package knowledge

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newVectorMockDB(t *testing.T) (*pgVectorStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return &pgVectorStore{db: db, vectorDim: VectorColumnDim}, mock, func() { _ = db.Close() }
}

func vectorTestDocument() *Document {
	now := time.Date(2025, 7, 1, 9, 0, 0, 0, time.UTC)
	return &Document{
		ID:             "doc-vec-1",
		OrganizationID: "org-1",
		Title:          "Billing FAQ",
		Source:         "confluence/billing",
		Metadata:       map[string]any{"team": "billing"},
		ChunkCount:     2,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func vectorTestChunks() []Chunk {
	now := time.Date(2025, 7, 1, 9, 0, 0, 0, time.UTC)
	return []Chunk{
		{
			ID: "vc-1", OrganizationID: "org-1", DocumentID: "doc-vec-1", Ordinal: 0,
			Content:   "first chunk",
			Embedding: []float64{0.5, 0.25},    // JSONB mirror
			Vector:    []float32{0.5, 0.25, 1}, // provider space -> embedding_vec
			CreatedAt: now,
		},
		{
			ID: "vc-2", OrganizationID: "org-1", DocumentID: "doc-vec-1", Ordinal: 1,
			Content:   "second chunk",
			Embedding: nil,
			Vector:    []float32{1, 2, 3},
			CreatedAt: now,
		},
	}
}

func TestPgVectorUpsertDocumentWithVectors(t *testing.T) {
	store, mock, closeDB := newVectorMockDB(t)
	defer closeDB()
	// Narrow the test column to 3 dims so the fixture vectors are written as
	// real pgvector text literals (the VectorColumnDim mismatch path is
	// covered by TestVectorParam).
	store.vectorDim = 3

	doc, chunks := vectorTestDocument(), vectorTestChunks()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlUpsertDocument)).
		WithArgs(doc.ID, doc.OrganizationID, doc.Title, doc.Source,
			metadataParam(doc.Metadata), doc.ChunkCount, doc.CreatedAt, doc.UpdatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	for _, c := range chunks {
		mock.ExpectExec(regexp.QuoteMeta(sqlUpsertChunk)).
			WithArgs(c.ID, doc.OrganizationID, c.DocumentID, c.Ordinal, c.Content,
				embeddingParam(c.Embedding), vectorParam(c.Vector, 3), c.CreatedAt).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	// Stale-chunk prune: tenant guard on organization_id, ordinal >= len.
	mock.ExpectExec(regexp.QuoteMeta(sqlDeleteStaleChunks)).
		WithArgs(doc.OrganizationID, doc.ID, len(chunks)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if err := store.UpsertDocumentWithVectors(context.Background(), doc, chunks); err != nil {
		t.Fatalf("UpsertDocumentWithVectors returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPgVectorUpsertRollsBackOnChunkError(t *testing.T) {
	store, mock, closeDB := newVectorMockDB(t)
	defer closeDB()

	doc, chunks := vectorTestDocument(), vectorTestChunks()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlUpsertDocument)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(sqlUpsertChunk)).WillReturnError(errors.New("vector column missing"))
	mock.ExpectRollback()

	if err := store.UpsertDocumentWithVectors(context.Background(), doc, chunks); err == nil {
		t.Fatal("chunk upsert failure must abort the transaction")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPgVectorUpsertSQLShape(t *testing.T) {
	// Shape assertions demanded by the issue: plain-SQL vector params, the
	// idempotent upsert key, and tenant guards — pinned BEFORE any real
	// pgvector database runs this code.
	for _, tc := range []struct {
		name, sql, mustContain string
	}{
		{"document upsert is idempotent", sqlUpsertDocument, "ON CONFLICT (id) DO UPDATE"},
		{"chunk upsert targets (document_id, ordinal)", sqlUpsertChunk, "ON CONFLICT (document_id, ordinal) DO UPDATE"},
		{"vector travels as a cast text param", sqlUpsertChunk, "$7::vector"},
		{"jsonb mirror param", sqlUpsertChunk, "$6::jsonb"},
		{"chunk upsert is tenant-guarded", sqlUpsertChunk, "WHERE knowledge_chunks.organization_id = EXCLUDED.organization_id"},
		{"stale-chunk prune is org-scoped", sqlDeleteStaleChunks, "WHERE organization_id = $1 AND document_id = $2 AND ordinal >= $3"},
		{"vector search filters the tenant", sqlVectorSearch, "WHERE c.organization_id = $1 AND c.embedding_vec IS NOT NULL"},
		{"vector search uses cosine distance", sqlVectorSearch, "1 - (c.embedding_vec <=> $2::vector)"},
		{"vector search joins the document for citations", sqlVectorSearch, "JOIN knowledge_documents d ON d.id = c.document_id AND d.organization_id = c.organization_id"},
		{"vector search is bounded", sqlVectorSearch, "LIMIT $3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.sql, tc.mustContain) {
				t.Fatalf("SQL shape drift:\nwant substring: %s\ngot: %s", tc.mustContain, tc.sql)
			}
		})
	}
}

func TestVectorParam(t *testing.T) {
	if got := vectorParam([]float32{0.5, -0.25, 1}, 3); got != "[0.5,-0.25,1]" {
		t.Fatalf("pgvector text literal mismatch: %v", got)
	}
	if got := vectorParam([]float32{1, 2, 3}, 3).(string); got != "[1,2,3]" {
		t.Fatalf("integer floats must render without decimals: %v", got)
	}
	// Empty vectors -> SQL NULL.
	if vectorParam(nil, 3) != nil {
		t.Fatal("nil vector must map to SQL NULL")
	}
	if vectorParam([]float32{}, 3) != nil {
		t.Fatal("empty vector must map to SQL NULL")
	}
	// Dimension mismatch -> SQL NULL (documented degradation: mismatched
	// vectors stay on the JSONB column; the typed column only mirrors its
	// migrated provider space).
	if vectorParam([]float32{1, 2, 3}, 1536) != nil {
		t.Fatal("dimension mismatch must map to SQL NULL")
	}
	// columnDim <= 0 disables the check (defensive default).
	if got := vectorParam([]float32{1}, 0); got != "[1]" {
		t.Fatalf("disabled dim check must pass through: %v", got)
	}
	// Very small magnitudes keep full float32 precision ('g' shortest form).
	small := []float32{1e-45, 3.4e38}
	if got := vectorParam(small, 2).(string); got != "[1e-45,3.4e+38]" {
		t.Fatalf("float32 shortest rendering failed: %v", got)
	}
}

func TestPgVectorSearchScansAndClamps(t *testing.T) {
	store, mock, closeDB := newVectorMockDB(t)
	defer closeDB()
	// Narrow the test store to a 3-dim vector column so the fixture vectors
	// fit (the production column is VectorColumnDim; dimension handling is
	// asserted separately).
	store.vectorDim = 3

	query := []float32{1, 2, 3}
	rows := sqlmock.NewRows([]string{
		"id", "organization_id", "document_id", "ordinal", "content", "created_at",
		"title", "COALESCE(d.source, '')", "1 - (c.embedding_vec <=> $2::vector)",
	})
	chunk := vectorTestChunks()[0]
	rows.AddRow(chunk.ID, chunk.OrganizationID, chunk.DocumentID, chunk.Ordinal, chunk.Content,
		chunk.CreatedAt, "Billing FAQ", "confluence/billing", 0.83)
	// Anti-parallel vectors -> cosine similarity -1 -> clamps to 0.
	rows.AddRow("vc-9", "org-1", "doc-vec-1", 2, "negative chunk", chunk.CreatedAt, "Billing FAQ", "", -1.0)

	mock.ExpectQuery(regexp.QuoteMeta(sqlVectorSearch)).
		WithArgs("org-1", vectorParam(query, 3), 5).
		WillReturnRows(rows)

	got, err := store.VectorSearch(context.Background(), "org-1", query, 5)
	if err != nil {
		t.Fatalf("VectorSearch returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(got))
	}
	if got[0].Row.ID != chunk.ID || got[0].Row.DocumentTitle != "Billing FAQ" || got[0].Row.DocumentSource != "confluence/billing" {
		t.Fatalf("citation fields missing: %+v", got[0].Row)
	}
	if got[0].Score != 0.83 {
		t.Fatalf("score scan failed: %v", got[0].Score)
	}
	if got[1].Score != 0 {
		t.Fatalf("negative cosine similarity must clamp to 0, got %v", got[1].Score)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPgVectorSearchValidation(t *testing.T) {
	store, mock, closeDB := newVectorMockDB(t)
	defer closeDB()
	store.vectorDim = 3

	t.Run("empty org rejected", func(t *testing.T) {
		if _, err := store.VectorSearch(context.Background(), "  ", []float32{1}, 5); err == nil {
			t.Fatal("empty org id must be rejected (tenant guard)")
		}
	})
	t.Run("empty query vector rejected", func(t *testing.T) {
		if _, err := store.VectorSearch(context.Background(), "org-1", nil, 5); err == nil {
			t.Fatal("empty query embedding must be rejected")
		}
	})
	t.Run("cross-space query rejected before SQL", func(t *testing.T) {
		// The offline 256-dim space can never match the 1536-dim column.
		if _, err := store.VectorSearch(context.Background(), "org-1", make([]float32, 256), 5); err == nil {
			t.Fatal("dimension mismatch must fail fast")
		}
	})
	query := vectorParam([]float32{1, 2, 3}, 3)
	t.Run("k defaults and caps", func(t *testing.T) {
		mock.ExpectQuery(regexp.QuoteMeta(sqlVectorSearch)).
			WithArgs("org-1", query, DefaultSearchK).
			WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "document_id", "ordinal", "content", "created_at", "title", "COALESCE(d.source, '')", "score"}))
		got, err := store.VectorSearch(context.Background(), "org-1", []float32{1, 2, 3}, 0)
		if err != nil {
			t.Fatalf("VectorSearch returned error: %v", err)
		}
		if got == nil {
			t.Fatal("result slice must be non-nil")
		}
	})
	t.Run("zero rows is empty, not an error", func(t *testing.T) {
		mock.ExpectQuery(regexp.QuoteMeta(sqlVectorSearch)).
			WithArgs("org-1", query, 5).
			WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "document_id", "ordinal", "content", "created_at", "title", "COALESCE(d.source, '')", "score"}))
		got, err := store.VectorSearch(context.Background(), "org-1", []float32{1, 2, 3}, 5)
		if err != nil {
			t.Fatalf("VectorSearch returned error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected no hits, got %d", len(got))
		}
	})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPgVectorGuardNilDB(t *testing.T) {
	var store *pgVectorStore
	if err := store.guard(); err == nil {
		t.Fatal("nil database must be guarded")
	}
	if err := store.UpsertDocumentWithVectors(context.Background(), vectorTestDocument(), nil); err == nil {
		t.Fatal("nil database must fail the upsert")
	}
	if _, err := store.VectorSearch(context.Background(), "org-1", []float32{1}, 5); err == nil {
		t.Fatal("nil database must fail the search")
	}
	ready, _, _ := newVectorMockDB(t)
	if err := ready.guard(); err != nil {
		t.Fatalf("initialized store must pass the guard: %v", err)
	}
}

func TestNewPgVectorStoreDefaults(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	store := NewPgVectorStore(db)
	vs, ok := store.(*pgVectorStore)
	if !ok {
		t.Fatalf("NewPgVectorStore must return the pgVectorStore, got %T", store)
	}
	if vs.vectorDim != VectorColumnDim {
		t.Fatalf("constructor must pin VectorColumnDim (%d), got %d", VectorColumnDim, vs.vectorDim)
	}
	// Interface compliance.
	var _ VectorStore = NewPgVectorStore(db)
}

func TestPgVectorScanOfNullCreatedAt(t *testing.T) {
	store, mock, closeDB := newVectorMockDB(t)
	defer closeDB()
	store.vectorDim = 3

	query := []float32{1, 2, 3}
	rows := sqlmock.NewRows([]string{
		"id", "organization_id", "document_id", "ordinal", "content", "created_at",
		"title", "COALESCE(d.source, '')", "score",
	}).AddRow("vc-1", "org-1", "doc-vec-1", 0, "content", nil, "T", "", 0.5)
	mock.ExpectQuery(regexp.QuoteMeta(sqlVectorSearch)).
		WithArgs("org-1", vectorParam(query, 3), 5).
		WillReturnRows(rows)

	got, err := store.VectorSearch(context.Background(), "org-1", query, 5)
	if err != nil {
		t.Fatalf("VectorSearch returned error: %v", err)
	}
	if !reflect.DeepEqual(got[0].Row.CreatedAt, time.Time{}) {
		t.Fatalf("NULL created_at must scan to the zero time, got %v", got[0].Row.CreatedAt)
	}
}
