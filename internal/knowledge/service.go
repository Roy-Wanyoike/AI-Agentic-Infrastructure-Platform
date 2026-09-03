package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Search bounds.
const (
	// DefaultSearchK is the number of chunks returned when the caller does
	// not specify k.
	DefaultSearchK = 5
	// MaxSearchK caps a single search request.
	MaxSearchK = 50
	// searchCandidateLimit bounds the org-scoped candidate scan per search.
	// Ranking is computed in Go (no pgvector, contract track 3-d), so the
	// candidate window is the scalability knob; oldest-first window keeps
	// results deterministic. A pgvector/ANN upgrade replaces this constant.
	searchCandidateLimit = 2000
	// scoreFloor drops hash-collision noise from the offline embedder:
	// chunks below 1% relevance never enter the top-k.
	scoreFloor = 0.01
)

// Typed errors surfaced to handlers for status mapping.
var (
	// ErrDocumentNotFound is returned when a document does not exist within
	// the caller's organization (tenant guard: foreign docs are "not found").
	ErrDocumentNotFound = errors.New("knowledge: document not found")
)

// Document is one ingested knowledge base entry (tenant-scoped).
type Document struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id,omitempty"`
	Title          string         `json:"title"`
	Source         string         `json:"source,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	ChunkCount     int            `json:"chunk_count"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// Chunk is one piece of a document: ~DefaultChunkSize runes of content with
// its positional ordinal and (optionally) the embedding vector used by
// retrieval scoring.
type Chunk struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id,omitempty"`
	DocumentID     string    `json:"document_id"`
	Ordinal        int       `json:"ordinal"`
	Content        string    `json:"content"`
	Embedding      []float64 `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

// ChunkRow joins a chunk with its document's citation fields.
type ChunkRow struct {
	Chunk
	DocumentTitle  string
	DocumentSource string
}

// SearchResult is the retrieval wire shape pinned by the wave-3 contract:
// {"document_id","chunk_ordinal","content","score","citation"} plus
// additive document_title/chunk_id fields.
type SearchResult struct {
	DocumentID    string  `json:"document_id"`
	DocumentTitle string  `json:"document_title"`
	ChunkID       string  `json:"chunk_id"`
	ChunkOrdinal  int     `json:"chunk_ordinal"`
	Content       string  `json:"content"`
	Score         float64 `json:"score"`
	Citation      string  `json:"citation"`
}

// IngestRequest is the caller-supplied shape of a document ingest.
type IngestRequest struct {
	Title    string
	Content  string
	Source   string
	Metadata map[string]any
}

// Store abstracts durable knowledge storage. Implementations MUST scope
// every tenant-facing query by organization_id.
type Store interface {
	// CreateDocument inserts the document row plus all chunk rows atomically.
	CreateDocument(ctx context.Context, doc *Document, chunks []Chunk) error
	// ListDocuments returns the documents of one tenant, newest first.
	ListDocuments(ctx context.Context, orgID string) ([]*Document, error)
	// ListChunksWithDocuments returns at most limit chunks (with document
	// title/source joined) of one tenant as retrieval candidates.
	ListChunksWithDocuments(ctx context.Context, orgID string, limit int) ([]ChunkRow, error)
	// GetDocument fetches one document within one tenant (ErrDocumentNotFound
	// for unknown ids or foreign tenants).
	GetDocument(ctx context.Context, orgID, id string) (*Document, error)
	// GetChunks returns the ordered chunks of one document within one tenant.
	GetChunks(ctx context.Context, orgID, documentID string) ([]Chunk, error)
}

// Service is the dual-mode knowledge service: pure in-memory maps (zero
// infrastructure mode) or Postgres-backed store. Both use the ingest
// pipeline: create document -> chunk -> embed -> store.
type Service struct {
	mu          sync.Mutex
	docs        map[string]*Document
	chunksByDoc map[string][]Chunk
	store       Store
	embedder    Embedder
	now         func() time.Time
}

// NewService returns the in-memory service (zero-infrastructure mode). The
// embedder is chosen from the environment (offline hashing when no
// AGENTOS_EMBEDDING_* knobs are set).
func NewService() *Service {
	return newService(nil, NewEmbedderFromEnv())
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store (migration 014 knowledge_documents/knowledge_chunks). The embedder
// is chosen from the environment; without configuration search degrades to
// the deterministic offline embeddings + lexical fallback.
func NewServiceWithStore(db *sql.DB) *Service {
	return newService(NewPostgresStore(db), NewEmbedderFromEnv())
}

// NewServiceWithEmbedder returns an in-memory service with a custom embedder
// (used by tests and by callers wiring a specific embeddings backend).
func NewServiceWithEmbedder(embedder Embedder) *Service {
	return newService(nil, embedder)
}

func newService(store Store, embedder Embedder) *Service {
	if embedder == nil {
		embedder = NewHashEmbedder()
	}
	return &Service{
		docs:        make(map[string]*Document),
		chunksByDoc: make(map[string][]Chunk),
		store:       store,
		embedder:    embedder,
		now:         time.Now,
	}
}

// IngestDocument runs the pipeline: create document -> chunk (~800 chars,
// 15% overlap, on boundaries) -> embed via the OpenAI-compatible client ->
// store chunks. An embedding backend failure is NON-FATAL: chunks are stored
// with NULL embeddings and retrieval falls back to lexical scoring for them
// (documented deviation; the embedder error is wrapped and surfaced to the
// caller for observability without losing the ingested document).
func (s *Service) IngestDocument(ctx context.Context, orgID string, req IngestRequest) (*Document, []Chunk, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(req.Title) == "" {
		return nil, nil, errors.New("title is required")
	}
	if strings.TrimSpace(req.Content) == "" {
		return nil, nil, errors.New("content is required")
	}

	texts := ChunkText(req.Content)
	if len(texts) == 0 {
		return nil, nil, errors.New("content is required")
	}

	now := s.now().UTC()
	doc := &Document{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Title:          strings.TrimSpace(req.Title),
		Source:         strings.TrimSpace(req.Source),
		Metadata:       req.Metadata,
		ChunkCount:     len(texts),
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	vectors, embedErr := s.embedder.Embed(ctx, texts)
	if embedErr != nil {
		vectors = nil // store chunks unembedded; search falls back lexically
	}
	chunks := make([]Chunk, len(texts))
	for i, text := range texts {
		chunks[i] = Chunk{
			ID:             uuid.NewString(),
			OrganizationID: orgID,
			DocumentID:     doc.ID,
			Ordinal:        i,
			Content:        text,
			CreatedAt:      now,
		}
		if vectors != nil && i < len(vectors) {
			chunks[i].Embedding = vectors[i]
		}
	}

	if s.store != nil {
		if err := s.store.CreateDocument(ctx, doc, chunks); err != nil {
			return nil, nil, err
		}
	} else {
		s.mu.Lock()
		s.docs[doc.ID] = doc
		s.chunksByDoc[doc.ID] = chunks
		s.mu.Unlock()
	}
	if embedErr != nil {
		return doc, chunks, embedErr
	}
	return doc, chunks, nil
}

// ListDocuments returns the documents of one tenant, newest first.
func (s *Service) ListDocuments(ctx context.Context, orgID string) ([]Document, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if s.store != nil {
		rows, err := s.store.ListDocuments(ctx, orgID)
		if err != nil {
			return nil, err
		}
		out := make([]Document, 0, len(rows))
		for _, d := range rows {
			out = append(out, *d)
		}
		return out, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Document, 0, len(s.docs))
	for _, d := range s.docs {
		if d.OrganizationID != orgID {
			continue
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GetDocument returns one document of one tenant with its ordered chunks.
func (s *Service) GetDocument(ctx context.Context, orgID, id string) (*Document, []Chunk, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, nil, ErrDocumentNotFound
	}
	if s.store == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		doc, ok := s.docs[id]
		if !ok || doc.OrganizationID != orgID {
			return nil, nil, ErrDocumentNotFound
		}
		chunks := append([]Chunk(nil), s.chunksByDoc[id]...)
		return doc, chunks, nil
	}
	doc, err := s.store.GetDocument(ctx, orgID, id)
	if err != nil {
		return nil, nil, err
	}
	chunks, err := s.store.GetChunks(ctx, orgID, id)
	if err != nil {
		return nil, nil, err
	}
	return doc, chunks, nil
}

// Search scores the org-scoped chunk candidates against the query and
// returns the top-k with citations. Scoring uses cosine similarity when both
// the query and the chunk carry embeddings of the same dimension, and
// lexical token overlap otherwise, so results stay meaningful across the
// offline hash embedder, remote model embeddings, and unembedded chunks.
func (s *Service) Search(ctx context.Context, orgID, query string, k int) ([]SearchResult, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("query is required")
	}
	if k <= 0 {
		k = DefaultSearchK
	}
	if k > MaxSearchK {
		k = MaxSearchK
	}

	var queryVec []float64
	if vecs, err := s.embedder.Embed(ctx, []string{query}); err == nil && len(vecs) > 0 {
		queryVec = vecs[0]
	}
	queryTokens := tokenSet(query)

	candidates, err := s.candidates(ctx, orgID)
	if err != nil {
		return nil, err
	}

	type scoredChunk struct {
		row   ChunkRow
		score float64
	}
	scored := make([]scoredChunk, 0, len(candidates))
	for _, row := range candidates {
		score := 0.0
		if len(queryVec) > 0 && len(row.Embedding) == len(queryVec) {
			score = cosineSimilarity(queryVec, row.Embedding)
		} else {
			score = lexicalScore(queryTokens, row.Content)
		}
		if score <= scoreFloor {
			continue
		}
		scored = append(scored, scoredChunk{row: row, score: score})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].row.DocumentTitle != scored[j].row.DocumentTitle {
			return scored[i].row.DocumentTitle < scored[j].row.DocumentTitle
		}
		return scored[i].row.Ordinal < scored[j].row.Ordinal
	})
	if len(scored) > k {
		scored = scored[:k]
	}

	results := make([]SearchResult, 0, len(scored))
	for _, hit := range scored {
		results = append(results, SearchResult{
			DocumentID:    hit.row.DocumentID,
			DocumentTitle: hit.row.DocumentTitle,
			ChunkID:       hit.row.ID,
			ChunkOrdinal:  hit.row.Ordinal,
			Content:       hit.row.Content,
			Score:         hit.score,
			Citation:      buildCitation(hit.row.DocumentTitle, hit.row.DocumentSource, hit.row.Ordinal),
		})
	}
	return results, nil
}

func (s *Service) candidates(ctx context.Context, orgID string) ([]ChunkRow, error) {
	if s.store != nil {
		return s.store.ListChunksWithDocuments(ctx, orgID, searchCandidateLimit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]ChunkRow, 0)
	for _, chunks := range s.chunksByDoc {
		for _, c := range chunks {
			if c.OrganizationID != orgID {
				continue
			}
			doc, ok := s.docs[c.DocumentID]
			if !ok || doc.OrganizationID != orgID {
				continue
			}
			rows = append(rows, ChunkRow{Chunk: c, DocumentTitle: doc.Title, DocumentSource: doc.Source})
		}
	}
	// Deterministic candidate window (oldest first) matching the SQL path.
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.Before(rows[j].CreatedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	if len(rows) > searchCandidateLimit {
		rows = rows[:searchCandidateLimit]
	}
	return rows, nil
}

func buildCitation(title, source string, ordinal int) string {
	var b strings.Builder
	b.WriteString(title)
	if source != "" {
		b.WriteString(" (")
		b.WriteString(source)
		b.WriteString(")")
	}
	b.WriteString(", chunk ")
	b.WriteString(strconv.Itoa(ordinal))
	return b.String()
}

// metadataParam marshals the document metadata for the JSONB column; nil
// stays NULL.
func metadataParam(metadata map[string]any) any {
	if len(metadata) == 0 {
		return nil
	}
	b, err := json.Marshal(metadata)
	if err != nil {
		return nil
	}
	return string(b)
}

func metadataFromParam(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}
