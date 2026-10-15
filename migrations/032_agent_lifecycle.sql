-- 032_agent_lifecycle.sql
--
-- Issue #106: agent lifecycle state machine (DRAFT → BUILDING → ACTIVE →
-- PAUSED / DISABLED → ARCHIVED; internal/agents/lifecycle.go).
--
-- Idempotent (IF NOT EXISTS pattern of migrations 001-031) so the migration
-- runner can be re-run safely.
--
-- Schema notes:
--   * The agents.status column ALREADY EXISTS — migration 001 created it as
--     `status TEXT NOT NULL DEFAULT 'DRAFT'` and the store has always read
--     and written it (internal/agents/store.go). No column or data migration
--     is required for the state machine: legacy rows keep their 'DRAFT'
--     value, the state machine parses statuses case-insensitively
--     (ParseAgentState), and unparseable hand-edited values are rejected
--     fail-closed instead of being silently rewritten.
--   * What the lifecycle adds is a hot lookup path: agents of one tenant
--     filtered by state (dashboards, the run-creation state guard, bulk
--     ops). The (organization_id, status) index below serves it without
--     touching the existing organization_id + created_at index used by
--     listings.
--
-- Deliberately NO CHECK constraint on status: the versioning domain
-- (internal/agents/versions.go repointAgent) can restore historical snapshot
-- values, and a CHECK would turn that into a hard failure. The state machine
-- enforces transitions at the service layer, fail-closed.

CREATE INDEX IF NOT EXISTS idx_agents_org_status
    ON agents(organization_id, status);
