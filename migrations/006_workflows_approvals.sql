-- 006_workflows_approvals.sql
--
-- Wave-2 track 2-a: workflow engine, approvals and run control persistence.
--
-- All statements are idempotent (consistent with the IF NOT EXISTS pattern of
-- migrations 001-005 so cmd/migrate can be re-run safely).
--
-- NOTE: the `workflows` table itself already exists since migration 004
-- (id/organization_id/name/status/definition/created_at/updated_at with the
-- organization FK). This migration only extends it with the columns the
-- versioned engine needs and creates the companion tables.

-- Workflows: description + pointer to the latest published immutable version.
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS current_version INTEGER NOT NULL DEFAULT 0;

-- Immutable DSL snapshots (draft -> published). A version row is written once
-- on publish and never mutated afterwards.
CREATE TABLE IF NOT EXISTS workflow_versions (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'published',
    dsl_snapshot JSONB,
    published_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_workflow_versions_workflow FOREIGN KEY (workflow_id) REFERENCES workflows(id) ON DELETE CASCADE,
    CONSTRAINT fk_workflow_versions_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    UNIQUE (workflow_id, version)
);

-- One workflow execution: the expansion of a DAG into agent runs.
CREATE TABLE IF NOT EXISTS workflow_runs (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    input TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_workflow_runs_workflow FOREIGN KEY (workflow_id) REFERENCES workflows(id) ON DELETE CASCADE,
    CONSTRAINT fk_workflow_runs_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Node -> run mapping recorded while expanding the DAG (execution trace).
CREATE TABLE IF NOT EXISTS workflow_node_runs (
    id TEXT PRIMARY KEY,
    workflow_run_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    run_id TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    error TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_workflow_node_runs_run FOREIGN KEY (workflow_run_id) REFERENCES workflow_runs(id) ON DELETE CASCADE,
    CONSTRAINT fk_workflow_node_runs_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Human-in-the-loop approval records; approving a pending approval resumes the
-- linked paused run (see internal/approvals).
CREATE TABLE IF NOT EXISTS approvals (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    run_id TEXT,
    workflow_run_id TEXT,
    resource TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    risk TEXT NOT NULL DEFAULT 'medium',
    status TEXT NOT NULL DEFAULT 'pending',
    requester TEXT NOT NULL DEFAULT '',
    approver TEXT NOT NULL DEFAULT '',
    decision_reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at TIMESTAMPTZ,
    CONSTRAINT fk_approvals_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Tenant-scoped indexes: every hot query filters on organization_id.
CREATE INDEX IF NOT EXISTS idx_workflows_org_created ON workflows(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_workflow_versions_org_workflow ON workflow_versions(organization_id, workflow_id, version);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_org_created ON workflow_runs(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_org_status ON workflow_runs(organization_id, status);
CREATE INDEX IF NOT EXISTS idx_workflow_node_runs_org_run ON workflow_node_runs(organization_id, workflow_run_id);
CREATE INDEX IF NOT EXISTS idx_workflow_node_runs_run_id ON workflow_node_runs(run_id);
CREATE INDEX IF NOT EXISTS idx_approvals_org_created ON approvals(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_approvals_org_status ON approvals(organization_id, status);
