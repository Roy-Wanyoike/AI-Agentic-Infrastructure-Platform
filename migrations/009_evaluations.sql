-- 009_evaluations.sql
--
-- Evaluations subsystem (wave 2, track 2-d): eval datasets with their cases,
-- synchronous evaluation runs against an agent, and per-case results.
--
-- Idempotent (CREATE TABLE IF NOT EXISTS / IF NOT EXISTS pattern of
-- migrations 001-005) so cmd/migrate can be re-run safely. Every table is
-- scoped by organization_id (tenant isolation rule) with tenant-scoped
-- indexes on the hot query paths.

CREATE TABLE IF NOT EXISTS eval_datasets (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_eval_datasets_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Cases belong to exactly one dataset; case_id is caller supplied (e.g. "c1")
-- and unique per dataset; position preserves the caller's case order.
CREATE TABLE IF NOT EXISTS eval_cases (
    dataset_id TEXT NOT NULL,
    case_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0,
    input TEXT NOT NULL DEFAULT '',
    expected TEXT NOT NULL DEFAULT '',
    scorer TEXT NOT NULL,
    params JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (dataset_id, case_id),
    CONSTRAINT fk_eval_cases_dataset FOREIGN KEY (dataset_id) REFERENCES eval_datasets(id) ON DELETE CASCADE,
    CONSTRAINT fk_eval_cases_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS eval_runs (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    dataset_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'running',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    CONSTRAINT fk_eval_runs_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_eval_runs_dataset FOREIGN KEY (dataset_id) REFERENCES eval_datasets(id) ON DELETE CASCADE
);

-- One row per executed case; case_index preserves execution order; scorer is
-- denormalized for by_scorer aggregations.
CREATE TABLE IF NOT EXISTS eval_results (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    case_id TEXT NOT NULL,
    scorer TEXT NOT NULL DEFAULT '',
    case_index INTEGER NOT NULL DEFAULT 0,
    output TEXT NOT NULL DEFAULT '',
    passed BOOLEAN NOT NULL DEFAULT FALSE,
    score DOUBLE PRECISION NOT NULL DEFAULT 0,
    latency_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
    cost_cents DOUBLE PRECISION NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_eval_results_run FOREIGN KEY (run_id) REFERENCES eval_runs(id) ON DELETE CASCADE,
    CONSTRAINT fk_eval_results_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Tenant-scoped indexes (organization_id + hot paths)
CREATE INDEX IF NOT EXISTS idx_eval_datasets_org_created ON eval_datasets(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_eval_cases_dataset_position ON eval_cases(dataset_id, position);
CREATE INDEX IF NOT EXISTS idx_eval_runs_org_created ON eval_runs(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_eval_runs_org_dataset ON eval_runs(organization_id, dataset_id);
CREATE INDEX IF NOT EXISTS idx_eval_results_run ON eval_results(run_id, case_index);
CREATE INDEX IF NOT EXISTS idx_eval_results_org_created ON eval_results(organization_id, created_at);
