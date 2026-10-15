-- 036_rate_limits.sql
--
-- Issue #124: granular per-scope rate limits (Organization → Agent RPM/RPD →
-- Tool RPM → Model RPM/TPM). The policies package (internal/policies) has no
-- rate-limit condition type — its conditions are tool_allowlist /
-- max_cost_cents / environments / require_approval — so limit RESOLUTION
-- reads this dedicated table instead (internal/limiter resolver.go); the
-- policies package is intentionally untouched.
--
-- Idempotent (CREATE TABLE IF NOT EXISTS / IF NOT EXISTS pattern of
-- migrations 001-033) so the migration runner can be re-run safely.
--
-- Model (mirrors internal/limiter.Config):
--   * organization_id — tenant scope; every store read/write filters on it
--     (a foreign org can never read another tenant's rows).
--   * scope           — one of 'org' | 'agent' | 'tool' | 'model'. The org
--     scope has no subject (org-wide throttle); the other scopes resolve
--     subject-specific rows over the org default
--     (subject-specific > org default > unlimited).
--   * subject_id      — NULLABLE. NULL is the ORG DEFAULT of the scope; a
--     value is the agent id / tool id / model name. Uniqueness of
--     (organization_id, scope, subject_id) is enforced by the EXPRESSION
--     unique index ux_rate_limits_subject
--     (organization_id, scope, COALESCE(subject_id, '')) because a PRIMARY
--     KEY cannot span a nullable column in Postgres — the functional
--     equivalent of the issue's "PK (org_id, scope, subject_id)" while
--     keeping subject_id genuinely nullable. In-memory/API layers map NULL
--     <-> "" (internal/limiter store.go).
--   * rpm             — requests per minute window (0 = unlimited).
--   * rpd             — requests per ROLLING 24h window (0 = unlimited; the
--     rolling choice is documented in internal/limiter/limiter.go).
--   * tpm             — tokens per minute window, model scope (0 =
--     unlimited; post-call accounting via internal/limiter Report).
--   * enabled         — FALSE disables the row. A disabled SUBJECT row is
--     the documented per-subject OPT-OUT: it suppresses the org default for
--     that subject (resolution stops there); a disabled org default means
--     the whole scope is unlimited.
--
-- An explicitly staged row with all limits 0 is valid but inactive (the
-- platform default "unlimited" is represented by the ABSENCE of rows, so
-- zero-infrastructure mode enforces nothing until configured).

CREATE TABLE IF NOT EXISTS rate_limits (
    organization_id TEXT NOT NULL,
    scope TEXT NOT NULL,
    subject_id TEXT,
    rpm INTEGER NOT NULL DEFAULT 0,
    rpd INTEGER NOT NULL DEFAULT 0,
    tpm INTEGER NOT NULL DEFAULT 0,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_rate_limits_scope CHECK (scope IN ('org', 'agent', 'tool', 'model')),
    CONSTRAINT ck_rate_limits_org_has_no_subject CHECK (scope <> 'org' OR subject_id IS NULL),
    CONSTRAINT ck_rate_limits_non_negative CHECK (rpm >= 0 AND rpd >= 0 AND tpm >= 0)
);

-- Uniqueness across NULL subject ids: the expression index treats NULL as ''
-- (see the file header for why this stands in for a nullable PRIMARY KEY).
CREATE UNIQUE INDEX IF NOT EXISTS ux_rate_limits_subject
    ON rate_limits (organization_id, scope, COALESCE(subject_id, ''));
