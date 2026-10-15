-- 037_run_checkpoints.sql
--
-- Issue #115: complete run lifecycle — durable checkpointing for pause,
-- resume and recovery.
--
-- Idempotent (CREATE TABLE IF NOT EXISTS / IF NOT EXISTS pattern of
-- migrations 001-033) so the migration runner can be re-run safely.
--
-- Model:
--   * run_checkpoints — one serializable mid-run snapshot per (run, step),
--     written by the runtime's Checkpointer seam AFTER each completed step
--     (conversation history + token accounting + step index; the runs
--     package persists, the runtime owns the document schema — state is an
--     opaque JSONB column).
--       - PRIMARY KEY (run_id, step_index) makes saves idempotent: a
--         replayed save overwrites in place instead of erroring.
--       - run_id FK cascades on run delete (checkpoints are run-scoped
--         artifacts, same lifecycle as run_steps).
--       - created_at doubles as the liveness heartbeat of the recovery
--         sweep (issue #115): a running run whose latest checkpoint is
--         fresher than AGENTOS_RUN_STALE_SECONDS is making progress and is
--         never swept; the paused-vs-failed decision of the sweep reads
--         whether ANY checkpoint exists (resumable vs not).
--   * idx_run_checkpoints_created — the sweep's "fresh checkpoint exists"
--     predicate and diagnostics walk the table by recency.

CREATE TABLE IF NOT EXISTS run_checkpoints (
    run_id TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    state JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_run_checkpoints PRIMARY KEY (run_id, step_index),
    CONSTRAINT fk_run_checkpoints_run FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_run_checkpoints_created
    ON run_checkpoints(created_at);
