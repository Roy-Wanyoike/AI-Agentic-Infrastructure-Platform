package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Tenant guard: every statement filters on organization_id. The document +
// chunk insert pair runs in one transaction (all-or-nothing, so a document
// never exists without its chunks).
const (
	sqlInsertDocument = `INSERT INTO knowledge_documents (id, organization_id, title, source, metadata, chunk_count, created_at, updated_at) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)`
	sqlInsertChunk    = `INSERT INTO knowledge_chunks (id, organization_id, document_id, ordinal, content, embedding, created_at) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`
	// Tenant guard: listings filter on organization_id (+created_at index).
	sqlSelectDocumentsByOrg = `SELECT id, organization_id, title, source, COALESCE(metadata::text, ''), chunk_count, created_at, updated_at FROM knowledge_documents WHERE organization_id = $1 ORDER BY created_at DESC, id ASC`
	// Tenant guard: single-document reads are scoped to one organization_id.
	sqlSelectDocument = `SELECT id, organization_id, title, source, COALESCE(metadata::text, ''), chunk_count, created_at, updated_at FROM knowledge_documents WHERE id = $1 AND organization_id = $2`
	// Tenant guard: chunk reads join the document scope through organization_id.
	sqlSelectChunksByDocument = `SELECT id, organization_id, document_id, ordinal, content, COALESCE(embedding::text, ''), created_at FROM knowledge_chunks WHERE document_id = $1 AND organization_id = $2 ORDER BY ordinal ASC, id ASC`
	// Retrieval candidate set: chunks joined with their document's citation
	// fields, bounded by LIMIT (ranking happens in Go, contract track 3-d).
	sqlSelectCandidates = `SELECT c.id, c.organization_id, c.document_id, c.ordinal, c.content, COALESCE(c.embedding::text, ''), c.created_at, d.title, COALESCE(d.source, '') FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id AND d.organization_id = c.organization_id WHERE c.organization_id = $1 ORDER BY c.created_at ASC, c.id ASC LIMIT $2`
)

// pgStore is the Postgres-backed Store implementation (migration 014).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("knowledge: database is nil")
	}
	return nil
}

// CreateDocument inserts the document row and all chunk rows in one
// transaction (all-or-nothing).
func (s *pgStore) CreateDocument(ctx context.Context, doc *Document, chunks []Chunk) error {
	if err := s.guard(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, sqlInsertDocument,
		doc.ID, doc.OrganizationID, doc.Title, doc.Source,
		metadataParam(doc.Metadata), doc.ChunkCount, doc.CreatedAt, doc.UpdatedAt); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err := tx.ExecContext(ctx, sqlInsertChunk,
			c.ID, doc.OrganizationID, c.DocumentID, c.Ordinal, c.Content,
			embeddingParam(c.Embedding), c.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *pgStore) ListDocuments(ctx context.Context, orgID string) ([]*Document, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectDocumentsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Document, 0)
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

func (s *pgStore) GetDocument(ctx context.Context, orgID, id string) (*Document, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	doc, err := scanDocument(s.db.QueryRowContext(ctx, sqlSelectDocument, id, orgID))
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func (s *pgStore) GetChunks(ctx context.Context, orgID, documentID string) ([]Chunk, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectChunksByDocument, documentID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Chunk, 0)
	for rows.Next() {
		var c Chunk
		var embeddingRaw string
		if err := rows.Scan(&c.ID, &c.OrganizationID, &c.DocumentID, &c.Ordinal,
			&c.Content, &embeddingRaw, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Embedding = embeddingFromParam(embeddingRaw)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *pgStore) ListChunksWithDocuments(ctx context.Context, orgID string, limit int) ([]ChunkRow, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = searchCandidateLimit
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectCandidates, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ChunkRow, 0)
	for rows.Next() {
		var row ChunkRow
		var embeddingRaw string
		if err := rows.Scan(&row.ID, &row.OrganizationID, &row.DocumentID, &row.Ordinal,
			&row.Content, &embeddingRaw, &row.CreatedAt,
			&row.DocumentTitle, &row.DocumentSource); err != nil {
			return nil, err
		}
		row.Embedding = embeddingFromParam(embeddingRaw)
		out = append(out, row)
	}
	return out, rows.Err()
}

// scanner abstracts *sql.Rows and *sql.Row for the document scan helper.
type scanner interface {
	Scan(dest ...any) error
}

func scanDocument(sc scanner) (*Document, error) {
	var d Document
	var metadataRaw string
	if err := sc.Scan(&d.ID, &d.OrganizationID, &d.Title, &d.Source,
		&metadataRaw, &d.ChunkCount, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDocumentNotFound
		}
		return nil, err
	}
	d.Metadata = metadataFromParam(metadataRaw)
	return &d, nil
}

// embeddingParam marshals the vector for the JSONB column; nil stays NULL.
func embeddingParam(vec []float64) any {
	if len(vec) == 0 {
		return nil
	}
	b, err := json.Marshal(vec)
	if err != nil {
		return nil
	}
	return string(b)
}

func embeddingFromParam(raw string) []float64 {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var vec []float64
	if err := json.Unmarshal([]byte(raw), &vec); err != nil {
		return nil
	}
	return vec
}
