-- 016_billing.sql
--
-- Issue #24: real billing & subscriptions — plans, subscription lifecycle,
-- usage invoicing.
--
-- All statements are idempotent (CREATE TABLE IF NOT EXISTS / IF NOT EXISTS
-- pattern of migrations 001-015) so cmd/migrate can be re-run safely.
--
-- Model (mirrors internal/billing):
--   * plans            — global (non-tenant) catalog of billable plans. A plan
--                        prices a monthly cycle: price_cents is the catalog
--                        price, included_quota is the included monthly run
--                        budget (0 = unlimited sentinel) and metadata JSONB
--                        carries optional knobs such as
--                        {"overage_run_rate_cents": N} used to price runs
--                        beyond the included quota on invoices.
--   * subscriptions    — one LIVE subscription per organization at most
--                        (trial|active|past_due), enforced by the partial
--                        unique index uq_subscriptions_one_live; canceled rows
--                        keep their history and a fresh subscribe re-opens a
--                        new lifecycle. Periods are half-open
--                        [period_start, period_end).
--   * invoices         — one billing document per (org, period); the partial
--                        unique index uq_invoices_org_period makes
--                        regeneration idempotent (voided invoices never block
--                        a regeneration).
--   * invoice_lines    — per-source breakdown (run|eval|overage). Line amounts
--                        are priced from EXISTING aggregates (runs.cost_cents
--                        via the usage-cost report); billing never stores or
--                        recomputes token costs.

-- ---------------------------------------------------------------------------
-- Plans: the billable catalog. Global rows — no organization_id on purpose;
-- subscribing tenants reference plans by id (fk_subscriptions_plan).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS plans (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    price_cents BIGINT NOT NULL DEFAULT 0,
    currency TEXT NOT NULL DEFAULT 'usd',
    -- Monthly included run budget; 0 = unlimited (documented sentinel, see
    -- internal/billing QuotaStatus docs).
    included_quota BIGINT NOT NULL DEFAULT 0,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_plans_price CHECK (price_cents >= 0),
    CONSTRAINT ck_plans_quota CHECK (included_quota >= 0)
);

-- ---------------------------------------------------------------------------
-- Subscriptions: the per-tenant lifecycle state machine
-- trial -> active -> past_due -> canceled (rollovers handled by the service).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS subscriptions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'trial',
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
    canceled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_subscriptions_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_subscriptions_plan FOREIGN KEY (plan_id) REFERENCES plans(id),
    CONSTRAINT ck_subscriptions_status CHECK (status IN ('trial', 'active', 'past_due', 'canceled')),
    CONSTRAINT ck_subscriptions_period CHECK (period_end > period_start)
);

-- At most one LIVE (trial|active|past_due) subscription per tenant: subscribe
-- becomes an error while a live row exists; after cancel a fresh subscribe
-- opens a new lifecycle row (history preserved).
CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_one_live
    ON subscriptions(organization_id)
    WHERE status IN ('trial', 'active', 'past_due');

-- Tenant-scoped hot path: "current subscription for org" lookups + history.
CREATE INDEX IF NOT EXISTS idx_subscriptions_org_created ON subscriptions(organization_id, created_at);
-- Rollover scan: due live subscriptions by period end.
CREATE INDEX IF NOT EXISTS idx_subscriptions_period_end ON subscriptions(period_end) WHERE status <> 'canceled';

-- ---------------------------------------------------------------------------
-- Invoices: one billing document per (organization, period). Amounts are
-- integer cents (sum of line amounts); currency is copied from the plan so a
-- mid-life plan change never mixes currencies inside one invoice.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS invoices (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    subscription_id TEXT,
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    subtotal_cents BIGINT NOT NULL DEFAULT 0,
    currency TEXT NOT NULL DEFAULT 'usd',
    status TEXT NOT NULL DEFAULT 'open',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_invoices_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_invoices_subscription FOREIGN KEY (subscription_id) REFERENCES subscriptions(id) ON DELETE SET NULL,
    CONSTRAINT ck_invoices_status CHECK (status IN ('open', 'paid', 'void')),
    CONSTRAINT ck_invoices_period CHECK (period_end > period_start),
    CONSTRAINT ck_invoices_subtotal CHECK (subtotal_cents >= 0)
);

-- Invoice regeneration is idempotent per (org, period): a non-void invoice
-- already covering the exact half-open period is returned as-is by
-- Service.GenerateInvoiceCtx and the index blocks accidental duplicates.
CREATE UNIQUE INDEX IF NOT EXISTS uq_invoices_org_period
    ON invoices(organization_id, period_start, period_end)
    WHERE status <> 'void';

CREATE INDEX IF NOT EXISTS idx_invoices_org_created ON invoices(organization_id, created_at);

-- ---------------------------------------------------------------------------
-- Invoice lines: the per-source breakdown. source=run are model-bucketed
-- token-cost lines priced from runs.cost_cents aggregates; source=eval is
-- reserved for evaluation cost metering (schema-supported seam); source=
-- overage prices metered runs beyond the plan's included quota using the
-- plan metadata overage_run_rate_cents knob. refs JSONB carries the pricing
-- provenance (model, included_quota, rate) for auditability.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS invoice_lines (
    id TEXT PRIMARY KEY,
    invoice_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    source TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    quantity BIGINT NOT NULL DEFAULT 0,
    amount_cents BIGINT NOT NULL DEFAULT 0,
    refs JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_invoice_lines_invoice FOREIGN KEY (invoice_id) REFERENCES invoices(id) ON DELETE CASCADE,
    CONSTRAINT fk_invoice_lines_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT ck_invoice_lines_source CHECK (source IN ('run', 'eval', 'overage')),
    CONSTRAINT ck_invoice_lines_amount CHECK (amount_cents >= 0)
);

CREATE INDEX IF NOT EXISTS idx_invoice_lines_invoice ON invoice_lines(invoice_id, created_at);
