-- 013_durable_workflows.sql
--
-- Wave-3 track 3-c: durable workflow execution.
--
-- Additive hardening of the workflow_runs / workflow_node_runs tables created
-- by migration 006 so that node execution can be checkpointed idempotently,
-- orphaned runs recovered after a worker crash and over-deadline runs timed
-- out by a watchdog (see internal/workflows recovery + docs/wiring/
-- durable-workflows.md).
--
-- All statements are idempotent (ADD COLUMN IF NOT EXISTS / CREATE INDEX
-- IF NOT EXISTS, consistent with migrations 006-011) and forward-only: no
-- column is ever dropped or rewritten, so rows written before this migration
-- stay valid (attempt defaults to 0 for pre-existing rows; new rows start at
-- attempt 1).

-- Workflow runs: durability bookkeeping.
--   attempt      - number of recovery claims (crash-recovery passes) that
--                  re-kicked this run; 0 for never-recovered runs.
--   locked_at    - instant the run was claimed by a recovery pass or worker.
--   heartbeat_at - last liveness signal (execute, node begin/heartbeat,
--                  recovery claim); staleness is measured against this
--                  (COALESCE(heartbeat_at, updated_at) for legacy rows).
--   finished_at  - terminal transition instant (completed/failed/cancelled/
--                  timeout).
--   deadline_at  - optional wall-clock budget; the watchdog times the run out
--                  (status 'timeout') once this instant passes.
--   error_code   - machine-readable failure code (NODE_ORPHANED,
--                  WORKFLOW_RUN_TIMEOUT, ...); '' while healthy.
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS locked_at TIMESTAMPTZ;
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS heartbeat_at TIMESTAMPTZ;
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS finished_at TIMESTAMPTZ;
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS deadline_at TIMESTAMPTZ;
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS error_code TEXT NOT NULL DEFAULT '';

-- Node runs: per-attempt checkpoint columns. One row is written per
-- (workflow_run_id, node_id, attempt) execution attempt:
--   attempt      - 1-based execution attempt (0 for legacy pre-013 rows).
--   locked_at    - instant the attempt was claimed by a worker
--                  (BeginNodeRun); NULL while the row is a pending
--                  placeholder created by the DAG expansion.
--   heartbeat_at - last liveness signal of the in-flight attempt.
--   error_code   - machine-readable failure code (NODE_ORPHANED,
--                  WORKFLOW_RUN_TIMEOUT, ...); '' while healthy.
ALTER TABLE workflow_node_runs ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_node_runs ADD COLUMN IF NOT EXISTS locked_at TIMESTAMPTZ;
ALTER TABLE workflow_node_runs ADD COLUMN IF NOT EXISTS heartbeat_at TIMESTAMPTZ;
ALTER TABLE workflow_node_runs ADD COLUMN IF NOT EXISTS error_code TEXT NOT NULL DEFAULT '';

-- Idempotency key for node checkpoints: at most one checkpoint row per
-- (workflow run, node, attempt). The checkpoint INSERT uses
-- ON CONFLICT ... DO NOTHING against this arbiter, so replaying a task can
-- never duplicate a node execution.
-- Legacy rows (attempt 0) satisfy the constraint: the pre-013 executor wrote
-- each node at most once per workflow run.
CREATE UNIQUE INDEX IF NOT EXISTS uq_workflow_node_runs_attempt
    ON workflow_node_runs(workflow_run_id, node_id, attempt);

-- Recovery hot paths: the watchdog and the stale sweep filter non-terminal
-- runs by heartbeat (contract-pinned index shape: status + heartbeat), and
-- the orphan pass scans a run's non-terminal node runs by tenant + status.
CREATE INDEX IF NOT EXISTS idx_workflow_runs_status_heartbeat
    ON workflow_runs(status, heartbeat_at);
CREATE INDEX IF NOT EXISTS idx_workflow_node_runs_org_status
    ON workflow_node_runs(organization_id, status);
