-- 008_policies.sql
--
-- Governance layer (Task 2-c): policy records + idempotency keys.
--
-- Goals (all statements idempotent, consistent with the IF NOT EXISTS pattern
-- used by migrations 001-005 so cmd/migrate can be re-run safely):
--   * policies: tenant-scoped allow/deny rules with JSONB actions/conditions
--     consumed by the internal/policies evaluation engine
--   * idempotency_keys: stored POST responses (status + body) replayed by the
--     internal/httpx idempotency middleware for 24h
--   * tenant-scoped and maintenance indexes matching the hot query paths

-- Policies: one governance rule per organization. The evaluation engine
-- filters by organization_id, orders by priority DESC / created_at ASC and
-- resolves deny-over-allow on priority ties.
CREATE TABLE IF NOT EXISTS policies (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    name TEXT NOT NULL,
    effect TEXT NOT NULL CHECK (effect IN ('allow', 'deny')),
    resource_type TEXT NOT NULL DEFAULT '*',
    actions JSONB NOT NULL DEFAULT '[]'::jsonb,
    conditions JSONB NOT NULL DEFAULT '{}'::jsonb,
    priority INTEGER NOT NULL DEFAULT 0,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_policies_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Hot path: list + evaluate all policies of one tenant, highest priority first.
CREATE INDEX IF NOT EXISTS idx_policies_org_priority ON policies(organization_id, priority DESC, created_at ASC);

-- Idempotency keys: response replay storage for POSTs carrying an
-- Idempotency-Key header. One row per (organization, key, route scope); rows
-- expire after 24h (expires_at is filtered by reads and swept by the index).
CREATE TABLE IF NOT EXISTS idempotency_keys (
    organization_id TEXT NOT NULL,
    key TEXT NOT NULL,
    scope TEXT NOT NULL DEFAULT '',
    status_code INTEGER NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    response_body TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (organization_id, key, scope),
    CONSTRAINT fk_idempotency_keys_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Maintenance path: expired-row cleanup / expiry-filtered scans.
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_expires ON idempotency_keys(expires_at);
