package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// VectorColumnDim is the dimensionality of the knowledge_chunks.embedding_vec
// pgvector column created by migration 033 (issue #113). It matches the
// default embeddings model (text-embedding-3-small). Provider vectors of a
// different dimension (e.g. text-embedding-3-large) stay functional through
// the JSONB embedding column + Go-side scoring; only vectors matching the
// column are mirrored into the ANN-indexed vector space.
const VectorColumnDim = 1536

// ScoredChunk is one retrieval hit: the chunk (with its document citation
// fields via ChunkRow) plus its relevance score in [0,1] (cosine similarity
// for vector hits, lexical score for keyword hits).
type ScoredChunk struct {
	Row   ChunkRow
	Score float64
}

// VectorStore abstracts pgvector-backed persistence and ANN search for the
// knowledge base (issue #113). Implementations MUST scope every statement by
// organization_id (tenant guard) and stay idempotent under re-ingest.
type VectorStore interface {
	// UpsertDocumentWithVectors inserts or refreshes the document row and its
	// chunks atomically, keyed on (organization_id, document_id, ordinal) —
	// re-running with the same document id converges to the same state.
	UpsertDocumentWithVectors(ctx context.Context, doc *Document, chunks []Chunk) error
	// VectorSearch returns the top-k org-scoped chunks nearest to the query
	// embedding (cosine distance <=>, pgvector HNSW index). Chunks without a
	// stored vector never match; an empty result is not an error.
	VectorSearch(ctx context.Context, orgID string, queryEmbedding []float32, k int) ([]ScoredChunk, error)
}

// SQL for the pgvector store. The vector literal travels as a plain string
// param cast at the placeholder ($n::vector — pgvector's text form
// '[1,2,3]'), so no pgvector driver type is needed. Tenant guards:
// knowledge_chunks.organization_id on every write/read; the chunk upsert
// additionally refuses to UPDATE a row owned by another organization.
const (
	sqlUpsertDocument = `INSERT INTO knowledge_documents (id, organization_id, title, source, metadata, chunk_count, created_at, updated_at) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8) ON CONFLICT (id) DO UPDATE SET title = EXCLUDED.title, source = EXCLUDED.source, metadata = EXCLUDED.metadata, chunk_count = EXCLUDED.chunk_count, updated_at = EXCLUDED.updated_at`
	// Idempotent upsert on (document_id, ordinal) — the UNIQUE key from
	// migration 014; organization_id is carried in the insert and guarded in
	// the UPDATE arm so a chunk can never migrate tenants.
	sqlUpsertChunk = `INSERT INTO knowledge_chunks (id, organization_id, document_id, ordinal, content, embedding, embedding_vec, created_at) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::vector, $8) ON CONFLICT (document_id, ordinal) DO UPDATE SET content = EXCLUDED.content, embedding = EXCLUDED.embedding, embedding_vec = EXCLUDED.embedding_vec, created_at = EXCLUDED.created_at WHERE knowledge_chunks.organization_id = EXCLUDED.organization_id`
	// Re-chunking can shrink the chunk set: ordinals >= the new count are
	// stale and are removed (tenant guard on organization_id).
	sqlDeleteStaleChunks = `DELETE FROM knowledge_chunks WHERE organization_id = $1 AND document_id = $2 AND ordinal >= $3`
	// Vector search: cosine distance <=> over the HNSW-indexed column,
	// org-scoped, nearest-first, LIMIT k. Score returned as 1 - distance.
	sqlVectorSearch = `SELECT c.id, c.organization_id, c.document_id, c.ordinal, c.content, c.created_at, d.title, COALESCE(d.source, ''), 1 - (c.embedding_vec <=> $2::vector) FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id AND d.organization_id = c.organization_id WHERE c.organization_id = $1 AND c.embedding_vec IS NOT NULL ORDER BY c.embedding_vec <=> $2::vector LIMIT $3`
)

// pgVectorStore is the Postgres/pgvector-backed VectorStore implementation
// (migration 033 adds the embedding_vec column + HNSW index; the migration
// is pgvector-OPTIONAL, so on a plain Postgres the column never exists and
// this store degrades to "no vector matches" — hybrid search falls back to
// keyword-only, see hybrid.go).
type pgVectorStore struct {
	db *sql.DB
	// vectorDim is the expected embedding_vec dimensionality (VectorColumnDim
	// in production; narrowed in tests to exercise the mismatch path).
	vectorDim int
}

// Compile-time proof the pgvector store satisfies the seam.
var _ VectorStore = (*pgVectorStore)(nil)

// NewPgVectorStore returns a VectorStore backed by *sql.DB (lib/pq driver).
// Vectors are written as plain SQL text params ($n::vector), so no pgvector
// driver type is required.
func NewPgVectorStore(db *sql.DB) VectorStore {
	return &pgVectorStore{db: db, vectorDim: VectorColumnDim}
}

func (s *pgVectorStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("knowledge: database is nil")
	}
	return nil
}

// UpsertDocumentWithVectors writes the document row and all chunk rows in
// one transaction (all-or-nothing). Chunks are upserted on
// (document_id, ordinal) with their embedding_vec mirrored from
// Chunk.Vector when the dimension matches the column; ordinals beyond the
// new chunk count are deleted so a re-chunked document converges.
func (s *pgVectorStore) UpsertDocumentWithVectors(ctx context.Context, doc *Document, chunks []Chunk) error {
	if err := s.guard(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, sqlUpsertDocument,
		doc.ID, doc.OrganizationID, doc.Title, doc.Source,
		metadataParam(doc.Metadata), doc.ChunkCount, doc.CreatedAt, doc.UpdatedAt); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err := tx.ExecContext(ctx, sqlUpsertChunk,
			c.ID, doc.OrganizationID, c.DocumentID, c.Ordinal, c.Content,
			embeddingParam(c.Embedding), vectorParam(c.Vector, s.vectorDim), c.CreatedAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, sqlDeleteStaleChunks, doc.OrganizationID, doc.ID, len(chunks)); err != nil {
		return err
	}
	return tx.Commit()
}

// VectorSearch returns the top-k org-scoped chunks nearest to the query
// embedding (cosine similarity via the <=> distance operator; the score is
// 1 - distance clamped to [0,1]). Empty org/query vectors are rejected;
// zero rows yield an empty (non-nil) slice.
func (s *pgVectorStore) VectorSearch(ctx context.Context, orgID string, queryEmbedding []float32, k int) ([]ScoredChunk, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if len(queryEmbedding) == 0 {
		return nil, errors.New("query embedding is required")
	}
	// A query from a different vector space (e.g. the 256-dim offline
	// embedder against the 1536-dim column) can never match: pgvector rejects
	// cross-dimension <=> comparisons, so fail fast with a clear error and
	// let callers degrade (hybrid search falls back to keyword-only).
	if s.vectorDim > 0 && len(queryEmbedding) != s.vectorDim {
		return nil, fmt.Errorf("knowledge: query embedding has %d dimensions, vector column expects %d", len(queryEmbedding), s.vectorDim)
	}
	if k <= 0 {
		k = DefaultSearchK
	}
	if k > MaxSearchK {
		k = MaxSearchK
	}
	// Tenant guard: WHERE c.organization_id = $1 (+ ANN index on the column).
	rows, err := s.db.QueryContext(ctx, sqlVectorSearch, orgID, vectorParam(queryEmbedding, s.vectorDim), k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ScoredChunk, 0, k)
	for rows.Next() {
		var (
			hit       ScoredChunk
			score     float64
			createdAt sql.NullTime
		)
		if err := rows.Scan(&hit.Row.ID, &hit.Row.OrganizationID, &hit.Row.DocumentID,
			&hit.Row.Ordinal, &hit.Row.Content, &createdAt,
			&hit.Row.DocumentTitle, &hit.Row.DocumentSource, &score); err != nil {
			return nil, err
		}
		if createdAt.Valid {
			hit.Row.CreatedAt = createdAt.Time
		}
		if score < 0 {
			score = 0 // cosine similarity of anti-parallel vectors clamps to 0
		}
		if score > 1 {
			score = 1
		}
		hit.Score = score
		out = append(out, hit)
	}
	return out, rows.Err()
}

// vectorParam renders a float32 vector in pgvector's text literal form
// ("[1,2,3]") for $n::vector placeholders. Empty vectors — and vectors whose
// dimension does not match the embedding_vec column — map to SQL NULL: the
// typed column only mirrors the provider space it was migrated for
// (VectorColumnDim), and mismatched vectors remain fully searchable through
// the JSONB embedding column + Go-side scoring (documented degradation,
// issue #113).
func vectorParam(vec []float32, columnDim int) any {
	if len(vec) == 0 || (columnDim > 0 && len(vec) != columnDim) {
		return nil
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
