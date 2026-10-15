-- 026_budgets.sql
--
-- Wave-8 issue C: per-scope budget gates + showback.
--
-- Idempotent (CREATE TABLE IF NOT EXISTS / IF NOT EXISTS pattern of
-- migrations 001-025) so the migration runner can be re-run safely.
--
-- Model (mirrors internal/billing Budget):
--   * budgets — tenant-configured monthly spend ceilings over ONE scope:
--       scope='organization' (ref_id='') or scope='agent' (ref_id=agent id).
--       limit_runs / limit_cost_cents use the 0 = unlimited sentinel (same
--       as plans.included_quota). Money is USD CENTS — the single money
--       unit of the billing stack (plans.price_cents, invoices.amount_cents,
--       runs.cost_cents); budgets must reconcile 1:1 with the usage ledger.
--       At most one ACTIVE budget per (organization, scope, ref_id):
--       the service refuses with ErrBudgetConflict and the partial unique
--       index uq_budgets_one_active is the durable backstop (same shape as
--       uq_subscriptions_one_live in migration 016).
--
-- Showback needs NO new table: it aggregates the EXISTING runs ledger
-- (runs.cost_cents, idx_runs_org_created_cost from migration 012) —
-- deliberately no cost duplication, mirroring the invoice provenance.

CREATE TABLE IF NOT EXISTS budgets (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    -- 'organization' (whole tenant; ref_id = '') or 'agent' (ref_id = agent id)
    scope TEXT NOT NULL,
    ref_id TEXT NOT NULL DEFAULT '',
    -- Budget period; only the calendar month is supported today.
    period TEXT NOT NULL DEFAULT 'monthly',
    -- Monthly ceilings; 0 = unlimited (documented sentinel).
    limit_runs BIGINT NOT NULL DEFAULT 0,
    limit_cost_cents BIGINT NOT NULL DEFAULT 0,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_budgets_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT ck_budgets_scope CHECK (scope IN ('organization', 'agent')),
    CONSTRAINT ck_budgets_period CHECK (period IN ('monthly')),
    CONSTRAINT ck_budgets_limits CHECK (limit_runs >= 0 AND limit_cost_cents >= 0)
);

-- At most one ACTIVE budget per (organization, scope, ref_id): creating a
-- second one is refused by the service (ErrBudgetConflict) and this partial
-- unique index makes the invariant durable under concurrency.
CREATE UNIQUE INDEX IF NOT EXISTS uq_budgets_one_active
    ON budgets(organization_id, scope, ref_id)
    WHERE is_active;

-- Tenant-scoped hot path: evaluate/list lookups by org.
CREATE INDEX IF NOT EXISTS idx_budgets_org_created ON budgets(organization_id, created_at);
