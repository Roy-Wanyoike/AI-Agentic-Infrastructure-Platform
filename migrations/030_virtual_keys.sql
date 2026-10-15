-- 030_virtual_keys.sql
--
-- Wave-9 issue #139: gateway virtual keys — model allow-lists + enforced
-- monthly spend caps (402 on overspend).
--
-- Idempotent (CREATE TABLE/INDEX IF NOT EXISTS pattern of migrations
-- 001-029) so the migration runner can be re-run safely.
--
-- Model (mirrors internal/models gateway.go):
--   * virtual_keys — one row per minted vk_ credential. Hash-at-rest scheme
--     identical to api_keys (002/005) and agent_api_keys (027):
--     key_hash = SHA-256 hex of the full secret; the plaintext exists only
--     in the create response; prefix is a 12-char display value that
--     authenticates nothing. UNIQUE index on key_hash is the authentication
--     path (credential-based tenant resolution).
--
--     models JSONB carries the model allow-list with the CONTRACTUAL
--     nil-vs-empty distinction (same shape as agent_api_keys.tool_allow_list):
--         SQL NULL -> allow-list DISABLED (every model allowed)
--         '[]'     -> allow-list ENABLED and empty (no model allowed)
--         else     -> exactly these model ids
--
--     monthly_limit_cents uses the 0 = unlimited sentinel (same as
--     budgets.limit_cost_cents / plans.included_quota). Money is USD CENTS
--     — the single money unit of the billing stack.
--
--     status is 'active' | 'revoked'; revocation is terminal (no
--     un-revoke) and keeps the row for audit.
--
--   * virtual_key_spend — the ACTUAL spend ledger, append-only (one row per
--     recorded completion; no aggregation-in-place so per-call attribution
--     survives, mirroring the audit-trail posture). Month is the calendar
--     month bucket in UTC ('YYYY-MM'); cost_cents is fractional (float64,
--     the ComputeCostCents output). Reads aggregate with SUM for one
--     (organization_id, key_id, month) cell — the guarded hot path indexed
--     below.

CREATE TABLE IF NOT EXISTS virtual_keys (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    label TEXT NOT NULL,
    prefix TEXT NOT NULL DEFAULT '',
    key_hash TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    models JSONB,
    -- Monthly spend ceiling in USD cents; 0 = unlimited (documented sentinel).
    monthly_limit_cents BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'active',
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_virtual_keys_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT ck_virtual_keys_status CHECK (status IN ('active', 'revoked')),
    CONSTRAINT ck_virtual_keys_limit CHECK (monthly_limit_cents >= 0)
);

-- Authentication path: credential-based tenant resolution by key hash.
CREATE UNIQUE INDEX IF NOT EXISTS uq_virtual_keys_hash ON virtual_keys(key_hash);

-- Management path: list/update/revoke within one tenant.
CREATE INDEX IF NOT EXISTS idx_virtual_keys_org ON virtual_keys(organization_id, created_at);

CREATE TABLE IF NOT EXISTS virtual_key_spend (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    -- Calendar month bucket in UTC, 'YYYY-MM' (enforced shape).
    month TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    cost_cents DOUBLE PRECISION NOT NULL DEFAULT 0,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_virtual_key_spend_key FOREIGN KEY (key_id) REFERENCES virtual_keys(id) ON DELETE CASCADE,
    CONSTRAINT fk_virtual_key_spend_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT ck_virtual_key_spend_month CHECK (month ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    CONSTRAINT ck_virtual_key_spend_cost CHECK (cost_cents >= 0)
);

-- Tenant-scoped hot path: the enforcement check sums one (org, key, month)
-- cell on every guarded completion.
CREATE INDEX IF NOT EXISTS idx_virtual_key_spend_key_month
    ON virtual_key_spend(organization_id, key_id, month);
