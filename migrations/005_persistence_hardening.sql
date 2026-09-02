-- 005_persistence_hardening.sql
--
-- Persistence hardening for the AgentOS Postgres source of truth.
--
-- Goals (all statements idempotent, consistent with the IF NOT EXISTS pattern
-- used by migrations 001-004 so cmd/migrate can be re-run safely):
--   * extend agents / runs / run_steps / api_keys / audit_logs with the columns
--     the persistence layer needs (instructions, run input/output, step trace
--     metadata, key prefixes, audit actor/resource)
--   * add the usage_records table (append-only metering per tenant)
--   * create tenant-scoped indexes: every hot query filters on
--     organization_id (+ created_at), matching the tenant isolation rule.

-- Agents: description / instructions / current version pointer
ALTER TABLE agents ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS instructions TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS current_version_id TEXT;

-- Runs: input and output payload columns
ALTER TABLE runs ADD COLUMN IF NOT EXISTS input TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS output TEXT NOT NULL DEFAULT '';

-- Run steps: execution trace metadata. The payload/result columns from 004
-- are kept for backwards compatibility; new code writes input_meta/output_meta.
ALTER TABLE run_steps ADD COLUMN IF NOT EXISTS input_meta JSONB;
ALTER TABLE run_steps ADD COLUMN IF NOT EXISTS output_meta JSONB;
ALTER TABLE run_steps ADD COLUMN IF NOT EXISTS error TEXT NOT NULL DEFAULT '';
ALTER TABLE run_steps ADD COLUMN IF NOT EXISTS token_usage JSONB;
ALTER TABLE run_steps ADD COLUMN IF NOT EXISTS cost DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE run_steps ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

-- API keys: short visibility prefix (the secret hash stays in key_hash)
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS prefix TEXT NOT NULL DEFAULT '';

-- Audit logs: actor / resource mirrors (user_id, resource_type, resource_id
-- from 002 are kept for compatibility)
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS actor TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS resource TEXT NOT NULL DEFAULT '';

-- Usage records: append-only metering rows, one per tenant resource event
CREATE TABLE IF NOT EXISTS usage_records (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    resource TEXT NOT NULL,
    quantity BIGINT NOT NULL DEFAULT 0,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_usage_records_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Tenant-scoped indexes (organization_id + created_at hot paths)
CREATE INDEX IF NOT EXISTS idx_agents_org_created ON agents(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_org_created ON runs(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_org_status ON runs(organization_id, status);
CREATE INDEX IF NOT EXISTS idx_run_steps_run_id_idx ON run_steps(run_id, step_index);
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);
CREATE INDEX IF NOT EXISTS idx_api_keys_org_created ON api_keys(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_logs_org_created ON audit_logs(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_records_org_created ON usage_records(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_agent_versions_agent_version ON agent_versions(agent_id, version);
