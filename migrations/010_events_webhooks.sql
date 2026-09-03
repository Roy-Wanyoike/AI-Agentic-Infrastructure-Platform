-- 010_events_webhooks.sql
--
-- Track 2-e: standard event model (append-only audit) + outbound webhooks.
--
-- Goals (all statements idempotent, consistent with the IF NOT EXISTS pattern
-- used by migrations 001-005 so cmd/migrate can be re-run safely):
--   * webhooks: outbound endpoint registrations (url, subscribed event types,
--     HMAC secret stored as SHA-256 hash only — the raw secret is returned
--     exactly once at creation and never persisted)
--   * webhook_deliveries: per-(webhook, event) delivery records; the first
--     attempt inserts the row and every retry updates it, so attempts /
--     last_status_code / latency_ms / error always mirror the latest attempt
--   * events: append-only audit of every published event (no UPDATE/DELETE
--     code paths exist; retention is an operational concern)
--   * tenant-scoped indexes: every hot query filters on organization_id.

-- Outbound webhook endpoints registered by a tenant.
CREATE TABLE IF NOT EXISTS webhooks (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    url TEXT NOT NULL,
    events JSONB NOT NULL DEFAULT '[]'::jsonb,
    secret_hash TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_webhooks_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Delivery attempt history (status delivered|failed|retrying; one row per
-- (webhook, event) delivery, mutated in place by the delivery worker).
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    webhook_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'retrying' CHECK (status IN ('delivered', 'failed', 'retrying')),
    attempts INT NOT NULL DEFAULT 0,
    last_status_code INT NOT NULL DEFAULT 0,
    latency_ms BIGINT NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_webhook_deliveries_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_webhook_deliveries_webhook FOREIGN KEY (webhook_id) REFERENCES webhooks(id) ON DELETE CASCADE
);

-- Append-only audit trail of published AgentOS events (events.Event envelope).
CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    type TEXT NOT NULL,
    project_id TEXT,
    resource_type TEXT,
    resource_id TEXT,
    execution_id TEXT,
    trace_id TEXT,
    payload JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_events_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Tenant-scoped indexes (organization_id + created_at hot paths)
CREATE INDEX IF NOT EXISTS idx_webhooks_org_created ON webhooks(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_org_webhook_created ON webhook_deliveries(organization_id, webhook_id, created_at);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_event_id ON webhook_deliveries(event_id);
CREATE INDEX IF NOT EXISTS idx_events_org_created ON events(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_events_type_created ON events(type, created_at);
