-- 015_canary_deployments.sql
--
-- Issue #13: canary deployments with deterministic traffic splitting.
--
-- Extends the deployments table (migration 007) with canary fields so ONE
-- deployment row can serve two versions of the same agent:
--   canary_version - the canary agent config version. 0 = no canary. Uses the
--                    same composite FK pattern as 007's fk_deployments_version
--                    (agent_id, canary_version) -> agent_versions(agent_id,
--                    version), so a canary can only reference a version of the
--                    SAME agent.
--   canary_weight  - percentage of traffic routed to the canary (0-100).
--                    updated_at already exists on deployments (007) and is
--                    maintained by the service on every canary transition.
--
-- The environment's traffic is resolved from the single HEALTHY deployment row
-- (partial unique index uq_deployments_one_healthy from 007): the canary fields
-- on non-healthy rows are staged config that does not serve traffic. The
-- selection strategy itself (FNV-1a hash of orgID/agentID vs the weight
-- threshold) lives in internal/deployments and needs no schema support.
--
-- All statements are idempotent, consistent with migrations 006-013: ADD
-- COLUMN IF NOT EXISTS plus DO-block guards for the named constraints (Postgres
-- has no ADD CONSTRAINT IF NOT EXISTS). Forward-only and additive: legacy rows
-- default to canary_version=0 / canary_weight=0 (= "no canary", always stable).

ALTER TABLE deployments ADD COLUMN IF NOT EXISTS canary_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deployments ADD COLUMN IF NOT EXISTS canary_weight INTEGER NOT NULL DEFAULT 0;

-- A canary must reference a config version of the SAME agent (composite FK on
-- the agent_versions natural key). Guarded DO block keeps the migration
-- re-runnable (the migration runner skips applied versions, but re-running the
-- file manually must not fail either).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_deployments_canary_version') THEN
        ALTER TABLE deployments ADD CONSTRAINT fk_deployments_canary_version
            FOREIGN KEY (agent_id, canary_version) REFERENCES agent_versions(agent_id, version) ON DELETE CASCADE;
    END IF;
END $$;

-- Weight is a percentage threshold: clamp the domain at the database layer.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_deployments_canary_weight') THEN
        ALTER TABLE deployments ADD CONSTRAINT ck_deployments_canary_weight
            CHECK (canary_weight BETWEEN 0 AND 100);
    END IF;
END $$;
