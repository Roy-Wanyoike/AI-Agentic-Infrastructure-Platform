-- 028_a2a_agent_cards.sql
--
-- Issue #136: A2A (Agent-to-Agent) protocol surface — the durable side of
-- the org-scoped Agent Card registry (internal/a2a). One row per published
-- card: an existing platform agent of the org, published for agent-to-agent
-- DISCOVERY (GET /a2a/agents/{id}/card renders the canonical A2A Agent
-- Card) and governed A2A invocation (inbound messages + outbound
-- delegation).
--
-- a2a_agent_cards mirrors mcp_servers (migration 025) — an integration-
-- topology governance table with NO secret material:
--
--   agent_id            TEXT  - the org agent this card publishes. Validated
--                               at create/update time by the service through
--                               the agent-existence seam (the agent MUST
--                               exist in the SAME org); the FK (027
--                               agent_api_keys precedent) is the durable
--                               backstop and cascades on agent deletion (a
--                               card whose agent is gone must not route).
--   provider_*          TEXT  - the A2A provider block split into its two
--                               scalars (organization name + url); '' = unset.
--   endpoint_url        TEXT  - where A2A message envelopes are POSTed. May
--                               be '' (a card is publishable without an
--                               endpoint; delegation refuses it later).
--                               http/https enforced in the service.
--   capabilities        JSONB - the A2A capabilities block, stored verbatim
--                               as the canonical card renders it:
--                               {"streaming","pushNotifications",
--                               "stateTransitionHistory"} (booleans;
--                               serialized explicitly — absent vs false must
--                               not be ambiguous on the wire).
--   skills              JSONB - the A2A skills list
--                               [{"id","name","description","tags"}] —
--                               discovery metadata only, never credentials.
--   auth_scheme         TEXT  - the SCHEME NAME the endpoint expects
--                               ("bearer", "oauth2", "none", ...). A name,
--                               NEVER a credential value; no secret column
--                               exists on this table by design.
--   preferred_transport TEXT  - A2A transport vocabulary ("JSONRPC", "GRPC",
--                               "HTTP+JSON"); service defaults to "JSONRPC".
--   created_by          TEXT  - actor principal (auth claims), like every
--                               governance table.
--
-- No UNIQUE(organization_id, name): cards are identity-free publications of
-- an agent (the same agent may publish several cards / versions); rows are
-- hard-deleted and re-publishing is a new id.
--
-- All statements are idempotent (CREATE TABLE/INDEX IF NOT EXISTS,
-- migrations 006-027 pattern) so cmd/migrate can be re-run safely.

CREATE TABLE IF NOT EXISTS a2a_agent_cards (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    version TEXT NOT NULL,
    provider_organization TEXT NOT NULL DEFAULT '',
    provider_url TEXT NOT NULL DEFAULT '',
    endpoint_url TEXT NOT NULL DEFAULT '',
    capabilities JSONB NOT NULL DEFAULT '{}'::jsonb,
    skills JSONB NOT NULL DEFAULT '[]'::jsonb,
    auth_scheme TEXT NOT NULL DEFAULT '',
    preferred_transport TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_a2a_agent_cards_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_a2a_agent_cards_agent FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

-- Tenant-scoped listing index: every hot query filters organization_id and
-- orders by created_at (the in-memory mode returns insertion order; the
-- store keeps the same contract with created_at ASC).
CREATE INDEX IF NOT EXISTS idx_a2a_agent_cards_org_created ON a2a_agent_cards(organization_id, created_at);
