-- 012_cost_tracking.sql
--
-- Cost tracking columns (wave-3 track 3-b).
--
-- All statements are additive and idempotent (ADD COLUMN IF NOT EXISTS /
-- CREATE INDEX IF NOT EXISTS), consistent with the style of migrations
-- 005-011 so cmd/migrate can be re-run safely.
--
--   * runs.cost_cents        — per-run total cost (cents, USD*100), bumped
--                              additively by the runs store as costed run
--                              steps are recorded
--   * run_steps.cost_cents   — per-step cost in cents. Migration 005 already
--                              introduced run_steps.cost (same unit, same
--                              meaning); cost_cents is the contract-canonical
--                              name. The runs store writes BOTH columns with
--                              the same value so either name stays usable;
--                              `cost` is kept for backwards compatibility and
--                              is expected to be dropped by a later cleanup
--                              migration once no reader needs it.
--
-- Pricing semantics: 1 cent = 0.01 USD; a step/run with no pricing data is 0
-- (the pricing hook never fails a run — see internal/models/pricing.go).

ALTER TABLE run_steps ADD COLUMN IF NOT EXISTS cost_cents DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cost_cents DOUBLE PRECISION NOT NULL DEFAULT 0;

-- Aggregation hot path for GET /v1/usage/costs: every query filters on
-- organization_id + created_at window and aggregates cost_cents (+agent_id
-- for group_by=agent), so make the scan index-covered.
CREATE INDEX IF NOT EXISTS idx_runs_org_created_cost
    ON runs(organization_id, created_at) INCLUDE (agent_id, cost_cents);
