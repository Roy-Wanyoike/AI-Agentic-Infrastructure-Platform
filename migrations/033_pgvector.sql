-- 033_pgvector.sql
--
-- Issue #113: real embeddings + pgvector hybrid search for the knowledge base
-- (internal/knowledge/pgvector_store.go, hybrid.go, embedding_pipeline.go).
--
-- ---------------------------------------------------------------------------
-- PGVECTOR-OPTIONAL (READ BEFORE TOUCHING)
-- ---------------------------------------------------------------------------
-- This migration MUST apply cleanly on a plain Postgres WITHOUT the vector
-- extension installed (dev machines and the CI baseline run extension-less
-- Postgres). Every statement therefore lives inside a DO block guarded by an
-- pg_available_extensions check: when "vector" is not available the block
-- raises a NOTICE and applies nothing, and the migration still succeeds.
-- Hybrid search then degrades gracefully to keyword-only
-- (internal/knowledge/hybrid.go documents the degradation contract).
--
-- pgvector >= 0.5.0 is required for the HNSW index method (older extension
-- builds will fail the CREATE INDEX — pin the extension version in the
-- deployment image before enabling AGENTOS_EMBEDDING_PROVIDER=openai).
--
-- IDEMPOTENT: CREATE EXTENSION IF NOT EXISTS + ADD COLUMN IF NOT EXISTS +
-- CREATE INDEX IF NOT EXISTS — the migration runner skips applied versions,
-- and re-running this file manually (or applying it after the column was
-- created by an earlier partial run) is safe.
--
-- WHY A TYPED vector(1536) COLUMN BESIDE THE EXISTING JSONB COLUMN:
--   * knowledge_chunks.embedding (JSONB, migration 014) is KEPT unchanged:
--     it stores the offline hash pseudo-vectors (256d) AND mirrors provider
--     vectors, feeding the classic Go-side lexical/cosine scoring path.
--   * embedding_vec vector(1536) is the ANN-indexed mirror of the DEFAULT
--     provider space (text-embedding-3-small = 1536d). Vectors of a
--     different dimension are stored as NULL here (see vectorParam in
--     pgvector_store.go): the typed column enforces the provider space's
--     integrity, and mismatched vectors remain fully searchable via the
--     JSONB column. Switching to a different-dimension default model
--     requires a follow-up migration (documented limitation).
--   * memory_snippets needs NO vector column: memory retrieval stays an
--     org-scoped candidate scan + Go-side cosine over the existing JSONB
--     column, transparently upgraded in vector QUALITY by the provider seam
--     (internal/memory/provider.go).
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector') THEN
        CREATE EXTENSION IF NOT EXISTS vector;

        ALTER TABLE knowledge_chunks
            ADD COLUMN IF NOT EXISTS embedding_vec vector(1536);

        -- HNSW ANN index over the cosine distance operator <=> (used by
        -- pgVectorStore.VectorSearch: ORDER BY embedding_vec <=> $query).
        CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_vec_hnsw
            ON knowledge_chunks USING hnsw (embedding_vec vector_cosine_ops)
            WITH (m = 16, ef_construction = 64);
    ELSE
        RAISE NOTICE '033_pgvector: vector extension not available; skipping embedding_vec column + HNSW index (hybrid search degrades to keyword-only)';
    END IF;
END $$;
