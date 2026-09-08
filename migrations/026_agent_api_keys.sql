-- 026_agent_api_keys.sql
--
-- Wave-8, agent identity v1: agent-scoped API keys with tool allow-lists
-- (non-human identity / NHI least privilege). One row per minted agent key.
--
-- agent_api_keys mirrors api_keys (migration 002/005) with the same hash-at-
-- rest scheme (key_hash = SHA-256 hex of the secret; the plaintext exists
-- only in the create response) and the same 12-char display prefix, plus:
--
--   agent_id         TEXT  - the ONLY agent this credential authenticates as
--                            (subject = the agent, not a user; FK to agents).
--   tool_allow_list  JSONB - nil/NULL = allow-list DISABLED (every tool
--                            allowed); '[]' = allow-list ENABLED and empty
--                            (no tools); else the exact tool names. The
--                            nil-vs-empty distinction is contractual
--                            (internal/apikeys agent_keys.go) and round-trips
--                            losslessly through JSONB.
--   expires_at       TIMESTAMPTZ - NULL = never expires; enforced at auth time.
--
-- Lookup paths: key_hash (authentication, UNIQUE index) and
-- (organization_id, agent_id) for management/audit; every id-based UPDATE is
-- guarded by BOTH organization_id and agent_id in the store layer.
--
-- All statements are idempotent (CREATE TABLE/INDEX IF NOT EXISTS, migrations
-- 006-025 pattern) so cmd/migrate can be re-run safely.

CREATE TABLE IF NOT EXISTS agent_api_keys (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    name TEXT NOT NULL,
    prefix TEXT NOT NULL DEFAULT '',
    key_hash TEXT NOT NULL,
    tool_allow_list JSONB,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_agent_api_keys_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_agent_api_keys_agent FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

-- Authentication path: credential-based tenant resolution by key hash.
CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_api_keys_hash ON agent_api_keys(key_hash);

-- Management path: list/update/revoke within one (organization, agent) pair.
CREATE INDEX IF NOT EXISTS idx_agent_api_keys_org_agent ON agent_api_keys(organization_id, agent_id, created_at);
