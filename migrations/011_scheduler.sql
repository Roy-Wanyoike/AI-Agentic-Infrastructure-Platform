-- 011_scheduler.sql
--
-- Schedules for the wave-2 scheduler track (2-f). Idempotent, consistent with
-- the IF NOT EXISTS pattern used by migrations 001-005 so cmd/migrate can be
-- re-run safely.
--
-- A schedule is one tenant-scoped trigger definition:
--   kind = 'once'      -> fires at run_at (RFC3339 instant), then status becomes
--                         'completed' (terminal)
--   kind = 'recurring' -> fires every interval_seconds (>= 60, enforced by the
--                         service), advancing next_run_at = last fire + interval
--   kind = 'cron'      -> fires on the 5-field cron_expr evaluated in the IANA
--                         timezone; next_run_at = next matching wall-clock minute
--
-- Catch-up protection: last_fired_at + the conditional claim UPDATE in
-- internal/scheduler/store.go guarantee a due slot is consumed at most once,
-- even across worker restarts or concurrent workers.

CREATE TABLE IF NOT EXISTS schedules (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    input TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    run_at TIMESTAMPTZ,
    interval_seconds INTEGER,
    cron_expr TEXT NOT NULL DEFAULT '',
    timezone TEXT NOT NULL DEFAULT 'UTC',
    status TEXT NOT NULL DEFAULT 'active',
    next_run_at TIMESTAMPTZ,
    last_run_id TEXT,
    last_fired_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_schedules_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_schedules_agent FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

-- Worker hot path: Due() filters on status + next_run_at (contract-pinned index).
CREATE INDEX IF NOT EXISTS idx_schedules_status_next_run ON schedules(status, next_run_at);

-- Tenant-scoped hot path (listings, audit).
CREATE INDEX IF NOT EXISTS idx_schedules_org_created ON schedules(organization_id, created_at);
