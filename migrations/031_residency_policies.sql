-- 031_residency_policies.sql
--
-- Wave-9 issue #140: org region pinning with data-flow-aware enforcement
-- + posture surface (internal/tenant/residency.go).
--
-- Idempotent (CREATE TABLE IF NOT EXISTS / IF NOT EXISTS pattern of
-- migrations 001-027) so the migration runner can be re-run safely.
--
-- Model (mirrors internal/tenant residency types):
--   * residency_policies — ONE policy per organization (organization_id is
--       the PRIMARY KEY, so the service upsert is UPDATE-then-INSERT and a
--       foreign org can never read another tenant's row). The organizations
--       schema itself is deliberately UNTOUCHED: the residency store is the
--       source of truth, keyed by organization_id.
--       - pinned_region   '' = no pin; otherwise one of the six operating
--         buckets (eu-central, eu-west, us-east, us-west, ap-south,
--         ap-northeast) or 'custom' (BYOC: deny all public-cloud endpoints
--         under strict mode).
--       - allowed_regions TEXT holding a JSON array (same TEXT-JSON storage
--         convention as notification_subscriptions.event_types in 024):
--         additional allowed regions, e.g. ["us-east"].
--       - strict_mode     FALSE = observe (flag + audit only, allowed);
--         TRUE = enforce (deny out-of-region AND unclassifiable endpoints,
--         fail-closed).
--       Empty pin + empty list = the documented "unpinned" default (checks
--       record audit evidence only).
--   * residency_violations — append-only out-of-region EVIDENCE rows
--       (flagged allows in observe mode AND strict denies). POSTure reads
--       the latest row (ORDER BY created_at DESC); the full trail is
--       retained for compliance review. url_host stores the HOST ONLY —
--       never the full URL or its query string (queries can carry secrets).

CREATE TABLE IF NOT EXISTS residency_policies (
    organization_id TEXT PRIMARY KEY,
    pinned_region TEXT NOT NULL DEFAULT '',
    allowed_regions TEXT NOT NULL DEFAULT '[]',
    strict_mode BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_residency_policies_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT ck_residency_pinned_region CHECK (pinned_region IN ('', 'eu-central', 'eu-west', 'us-east', 'us-west', 'ap-south', 'ap-northeast', 'custom'))
);

CREATE TABLE IF NOT EXISTS residency_violations (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    -- Evidence carries the bare host (port/userinfo/query stripped upstream).
    url_host TEXT NOT NULL,
    region TEXT NOT NULL DEFAULT 'unknown',
    reason TEXT NOT NULL,
    strict_mode BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_residency_violations_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Tenant-scoped hot paths: latest-violation lookup + per-org evidence walks.
CREATE INDEX IF NOT EXISTS idx_residency_violations_org_created
    ON residency_violations(organization_id, created_at DESC);
