-- 025_mcp_registry.sql
--
-- Issue #82: MCP client — external MCP servers as org-scoped, agent-facing
-- tools. One row per (organization_id, name): the governed registry entry
-- for a REMOTE MCP server the org's agents consume (the inbound MCP server
-- endpoint of issue #50 has no table — this is the consuming side).
--
-- mcp_servers:
--   endpoint_url      TEXT  - the FULL URL the JSON-RPC 2.0 POSTs go to
--                             (http/https only, enforced in the service).
--   secret_ref        TEXT  - NULLable NAME reference into the secrets store
--                             (migration 017); resolved PER REQUEST through
--                             the injected SecretResolver seam and injected
--                             as the auth header. Secret VALUES never rest
--                             in this table.
--   auth_header       TEXT  - the header the resolved secret is injected
--                             into (default 'Authorization'; the value is
--                             used verbatim — store "Bearer xyz" as the
--                             secret value when the remote expects a
--                             scheme). CHECK-constrained to a non-empty
--                             header-token shape at the service layer.
--   status            TEXT  - 'ok' | 'unreachable' | 'disabled' (CHECK).
--                             'ok' = the last initialize handshake
--                             succeeded (its tools are exposed to agents);
--                             'unreachable' = the last handshake failed
--                             (tools NOT exposed); 'disabled' = governance
--                             switch (no probes, no tools). Written ONLY
--                             by the registration/update/re-test flows.
--   last_check_at     TIMESTAMPTZ - stamped by every registration/re-test
--                             probe.
--   protocol_version  TEXT  - the MCP protocolVersion negotiated at the
--                             last successful handshake ('' otherwise).
--   tools_cache       JSONB - the STATIC tools/list cache (issue #82 v1
--                             contract: tools are discovered at
--                             registration / explicit re-test, never
--                             dynamically per run): [{"name", "description",
--                             "inputSchema"}]. The agent-facing tool names
--                             (mcp_<server>_<tool>) are derived from the
--                             server name + cached tool name at build time,
--                             NOT stored here.
--   created_by        TEXT  - actor principal (auth claims), like every
--                             governance table.
--
-- Uniqueness: rows are hard-deleted (no tombstones), so a full
-- UNIQUE(organization_id, name) constraint is correct (migration 020
-- connectors precedent). A violating insert surfaces as SQLSTATE 23505 and
-- maps to ErrDuplicate.
--
-- All statements are idempotent (CREATE TABLE IF NOT EXISTS, migrations
-- 006-024 pattern) so cmd/migrate can be re-run safely.

CREATE TABLE IF NOT EXISTS mcp_servers (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    name TEXT NOT NULL,
    endpoint_url TEXT NOT NULL,
    secret_ref TEXT,
    auth_header TEXT NOT NULL DEFAULT 'Authorization',
    status TEXT NOT NULL DEFAULT 'unreachable' CHECK (status IN ('ok', 'unreachable', 'disabled')),
    last_check_at TIMESTAMPTZ,
    protocol_version TEXT,
    tools_cache JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_mcp_servers_org_name UNIQUE (organization_id, name),
    CONSTRAINT fk_mcp_servers_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Tenant-scoped listing index: every hot query filters organization_id (the
-- UNIQUE constraint above already covers the (organization_id, name)
-- lookup; this one keeps list-by-org ordering cheap).
CREATE INDEX IF NOT EXISTS idx_mcp_servers_org_created ON mcp_servers(organization_id, created_at);
