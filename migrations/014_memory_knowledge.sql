-- 014_memory_knowledge.sql
--
-- Wave-3 track 3-d: agent memory persistence + knowledge/RAG foundation.
--
-- All statements are idempotent (CREATE TABLE IF NOT EXISTS / IF NOT EXISTS
-- pattern of migrations 001-011) so cmd/migrate can be re-run safely.
--
-- NOTE: no pgvector — embeddings are stored as JSON float arrays (jsonb) and
-- similarity is computed in Go over org-scoped candidate sets. This keeps the
-- schema portable across any Postgres; a pgvector upgrade can migrate the
-- jsonb columns forward without a rewrite of the table shape.

-- ---------------------------------------------------------------------------
-- Agent memory: short_term|long_term snippets, org-scoped, optionally bound
-- to one agent (agent_id NULL = shared organization-level memory).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory_snippets (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    agent_id TEXT,
    scope TEXT NOT NULL DEFAULT 'long_term',
    content TEXT NOT NULL,
    importance DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    expires_at TIMESTAMPTZ,
    embedding JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_memory_snippets_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_memory_snippets_agent FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

-- ---------------------------------------------------------------------------
-- Knowledge base: documents chunked at ingest time (~800 chars, ~15% overlap,
-- cut on content boundaries); every chunk stores its text and (optionally) an
-- embedding vector for retrieval scoring in Go.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS knowledge_documents (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    title TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    metadata JSONB,
    chunk_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_knowledge_documents_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS knowledge_chunks (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    document_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL DEFAULT 0,
    content TEXT NOT NULL,
    embedding JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_knowledge_chunks_document FOREIGN KEY (document_id) REFERENCES knowledge_documents(id) ON DELETE CASCADE,
    CONSTRAINT fk_knowledge_chunks_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    UNIQUE (document_id, ordinal)
);

-- Tenant-scoped indexes: every hot query filters on organization_id.
CREATE INDEX IF NOT EXISTS idx_memory_snippets_org_created ON memory_snippets(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_memory_snippets_org_agent ON memory_snippets(organization_id, agent_id);
CREATE INDEX IF NOT EXISTS idx_memory_snippets_org_expires ON memory_snippets(organization_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_knowledge_documents_org_created ON knowledge_documents(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_org_document ON knowledge_chunks(organization_id, document_id, ordinal);
