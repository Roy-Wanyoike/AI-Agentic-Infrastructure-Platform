-- 035_knowledge_episodes.sql — issues #130/#131: dynamic knowledge graph
-- memory (agent activity episodes) + ontology-first knowledge construction.
--
-- IDEMPOTENT: safe to re-apply (CREATE TABLE IF NOT EXISTS pattern of
-- migrations 001-034). All tables org-scoped (tenant isolation).
--
-- NOTE: episode embeddings are JSONB float arrays like knowledge_chunks
-- (migration 014); similarity is computed in Go over org-scoped candidate
-- windows weighted by TimeDecayRelevance. No FK on agent_id/run_id: run
-- retention policies must never cascade into long-lived memory.

-- ---------------------------------------------------------------------------
-- Agent activity episodes (issue #130): one row per CHUNK of one activity.
-- Content longer than the knowledge chunker limit is split at ingest; chunks
-- of one episode share agent/run/action/timestamp and the deterministic
-- fingerprint id base, differ by chunk_ordinal. action_type CHECK mirrors
-- the four EpisodeAction values in internal/knowledge/episodes.go.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS knowledge_episodes (
    id             TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    agent_id       TEXT NOT NULL,
    run_id         TEXT NOT NULL,
    action_type    TEXT NOT NULL CHECK (action_type IN ('tool_call','reasoning','decision','observation')),
    chunk_ordinal  INTEGER NOT NULL DEFAULT 0,
    content        TEXT NOT NULL,
    entities       JSONB NOT NULL DEFAULT '[]',
    embedding      JSONB,
    episode_ts     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_knowledge_episodes_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);
-- Temporal layer: window queries ("last N hours") are the hot path.
CREATE INDEX IF NOT EXISTS idx_knowledge_episodes_org_ts ON knowledge_episodes(organization_id, episode_ts);
-- Per-run drilldown (episode replay / memory prompt assembly).
CREATE INDEX IF NOT EXISTS idx_knowledge_episodes_org_run ON knowledge_episodes(organization_id, run_id);
CREATE INDEX IF NOT EXISTS idx_knowledge_episodes_org_agent ON knowledge_episodes(organization_id, agent_id);

-- ---------------------------------------------------------------------------
-- Knowledge ontologies (issue #131): org-scoped graph schemas. The whole
-- vocabulary (entity_types / edge_types / attribute_defs) lives in the
-- `schema` JSONB column so the row shape stays stable while the ontology
-- vocabulary evolves; naming conventions and the reserved attributes
-- (uuid, name, created_at) are enforced in Go (ValidateOntology).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS knowledge_ontologies (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    schema          JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_knowledge_ontologies_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);
-- Tenant guard: org-scoped listing is the only enumeration path.
CREATE INDEX IF NOT EXISTS idx_knowledge_ontologies_org ON knowledge_ontologies(organization_id);
