-- 007_versions_deployments.sql
--
-- Track 2-b: agent config versions + deployments.
--
-- Goals (all statements idempotent, consistent with the IF NOT EXISTS pattern
-- used by migrations 001-006 so cmd/migrate can be re-run safely):
--   * extend agent_versions with immutable-snapshot columns (snapshot, status,
--     published_at, published_by). Legacy rows keep status='draft' and their
--     existing config JSONB is used as the snapshot fallback (COALESCE).
--   * add the deployments table: an append-only ledger of (agent, version,
--     environment) lifecycle rows. The environment's "current" deployment is
--     the row with status='healthy'; the partial unique index guarantees at
--     most one healthy deployment per agent+environment. A superseded healthy
--     row keeps its history via superseded_at (status becomes 'failed' with
--     health.superseded_by pointing at the replacement) so rollback can always
--     find the previous healthy version.

-- Agent versions: immutable published config snapshots.
ALTER TABLE agent_versions ADD COLUMN IF NOT EXISTS snapshot JSONB;
ALTER TABLE agent_versions ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'draft';
ALTER TABLE agent_versions ADD COLUMN IF NOT EXISTS published_at TIMESTAMPTZ;
ALTER TABLE agent_versions ADD COLUMN IF NOT EXISTS published_by TEXT NOT NULL DEFAULT '';

-- Version lookups filter by agent + status (published/current pointer).
CREATE INDEX IF NOT EXISTS idx_agent_versions_agent_status ON agent_versions(agent_id, status);

-- Deployments: lifecycle ledger per agent + environment.
CREATE TABLE IF NOT EXISTS deployments (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    environment TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'requested',
    health JSONB,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    superseded_at TIMESTAMPTZ,
    CONSTRAINT fk_deployments_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_deployments_agent FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE,
    CONSTRAINT fk_deployments_version FOREIGN KEY (agent_id, version) REFERENCES agent_versions(agent_id, version) ON DELETE CASCADE,
    CONSTRAINT ck_deployments_environment CHECK (environment IN ('development', 'staging', 'production')),
    CONSTRAINT ck_deployments_status CHECK (status IN ('requested', 'validated', 'deploying', 'healthy', 'failed'))
);

-- Exactly one healthy deployment per agent + environment: the partial unique
-- index backs the "current deployment" pointer; promotions demote the previous
-- healthy row (status='failed', superseded_at set) before the new one lands.
CREATE UNIQUE INDEX IF NOT EXISTS uq_deployments_one_healthy
    ON deployments(agent_id, environment)
    WHERE status = 'healthy';

-- Tenant-scoped indexes (organization_id + created_at hot path, plus the
-- per-agent environment history used by list + rollback queries).
CREATE INDEX IF NOT EXISTS idx_deployments_org_created ON deployments(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_deployments_agent_env ON deployments(agent_id, environment, created_at);
