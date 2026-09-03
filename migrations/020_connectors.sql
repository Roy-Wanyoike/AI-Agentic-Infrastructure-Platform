-- 020_connectors.sql
--
-- Issue #30: connectors framework — registered external integrations with
-- health checks and secret references.
--
-- One row per (organization_id, name). A connector is the governed registry
-- entry for an external system (CRM, internal API, SaaS webhook receiver):
--   type          TEXT  - 'webhook' (fire-and-forget receivers) or 'http'
--                         (request/response APIs). CHECK-constrained; the
--                         service layer (internal/connectors) validates too.
--   base_url      TEXT  - scheme-qualified origin every request is built
--                         from (http/https only, enforced in the service).
--   config        JSONB - non-secret request shaping: {"auth_style":
--                         none|bearer|basic|api_key_header, "headers": {...}
--                         (static header templates), "api_key_header",
--                         "api_key_prefix", "username" (for basic auth)}.
--                         Config NEVER carries secret VALUES — the sensitive
--                         material lives in the secrets store (migration 017)
--                         and is referenced by name through secret_ref.
--   secret_ref    TEXT  - NULLable NAME reference into the secrets store;
--                         resolved at request-build time through the injected
--                         SecretResolver seam (internal/connectors), never
--                         stored or echoed here.
--   status        TEXT  - 'active' | 'disabled' (CHECK). Disabled connectors
--                         refuse BuildRequest (governance switch) while test
--                         probes stay available for diagnostics.
--   last_check_at / last_check_status - written ONLY by the Test() health
--                         probe ("ok" on 2xx-3xx, "error" otherwise); regular
--                         updates leave both untouched.
--
-- Uniqueness: connectors are hard-deleted (no tombstones), so a full
-- UNIQUE(organization_id, name) constraint is correct here (unlike the
-- soft-deleting secrets table which needs a partial index). A violating
-- insert surfaces as SQLSTATE 23505 and maps to ErrDuplicate.
--
-- All statements are idempotent (CREATE TABLE IF NOT EXISTS, consistent with
-- migrations 001-017) so cmd/migrate can be re-run safely.

CREATE TABLE IF NOT EXISTS connectors (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    name TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('webhook', 'http')),
    base_url TEXT NOT NULL,
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    secret_ref TEXT,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    last_check_at TIMESTAMPTZ,
    last_check_status TEXT,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_connectors_org_name UNIQUE (organization_id, name),
    CONSTRAINT fk_connectors_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Tenant-scoped listing index: every hot query filters organization_id (the
-- UNIQUE constraint above already covers the (organization_id, name) lookup;
-- this one keeps list-by-org ordering cheap).
CREATE INDEX IF NOT EXISTS idx_connectors_org_created ON connectors(organization_id, created_at);
