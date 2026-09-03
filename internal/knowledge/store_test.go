package knowledge

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockDB(t *testing.T) (*pgStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return &pgStore{db: db}, mock, func() { _ = db.Close() }
}

func testDocument() *Document {
	return &Document{
		ID:             "doc-1",
		OrganizationID: "org-1",
		Title:          "Billing FAQ",
		Source:         "confluence/billing",
		Metadata:       map[string]any{"team": "billing"},
		ChunkCount:     2,
		CreatedAt:      time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
	}
}

func testChunks() []Chunk {
	at := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	return []Chunk{
		{ID: "c-1", OrganizationID: "org-1", DocumentID: "doc-1", Ordinal: 0, Content: "first chunk", Embedding: []float64{0.5, 0.25}, CreatedAt: at},
		{ID: "c-2", OrganizationID: "org-1", DocumentID: "doc-1", Ordinal: 1, Content: "second chunk", Embedding: nil, CreatedAt: at},
	}
}

func TestPGCreateDocumentTransactional(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	doc, chunks := testDocument(), testChunks()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertDocument)).
		WithArgs(doc.ID, doc.OrganizationID, doc.Title, doc.Source,
			metadataParam(doc.Metadata), doc.ChunkCount, doc.CreatedAt, doc.UpdatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	for i, c := range chunks {
		mock.ExpectExec(regexp.QuoteMeta(sqlInsertChunk)).
			WithArgs(c.ID, doc.OrganizationID, c.DocumentID, c.Ordinal, c.Content,
				embeddingParam(c.Embedding), c.CreatedAt).
			WillReturnResult(sqlmock.NewResult(0, 1))
		_ = i
	}
	mock.ExpectCommit()

	if err := store.CreateDocument(context.Background(), doc, chunks); err != nil {
		t.Fatalf("CreateDocument returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGCreateDocumentRollsBackOnChunkError(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	doc, chunks := testDocument(), testChunks()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertDocument)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertChunk)).
		WillReturnError(errors.New("chunk insert failed"))
	mock.ExpectRollback()

	if err := store.CreateDocument(context.Background(), doc, chunks); err == nil {
		t.Fatal("chunk insert failure must abort the document creation")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func documentRows(doc *Document) *sqlmock.Rows {
	// Mirrors the production SELECT's COALESCE(metadata::text, ''): the mock
	// returns exactly what Postgres would (never NULL).
	metadata := ""
	if len(doc.Metadata) > 0 {
		metadata = `{"team":"billing"}`
	}
	return sqlmock.NewRows([]string{
		"id", "organization_id", "title", "source", "COALESCE(metadata::text, '')",
		"chunk_count", "created_at", "updated_at",
	}).AddRow(doc.ID, doc.OrganizationID, doc.Title, doc.Source, metadata,
		doc.ChunkCount, doc.CreatedAt, doc.UpdatedAt)
}

func TestPGListDocumentsScoped(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	doc := testDocument()
	// Tenant guard: WHERE organization_id = $1 (+ created_at index).
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectDocumentsByOrg)).
		WithArgs("org-1").
		WillReturnRows(documentRows(doc))

	got, err := store.ListDocuments(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListDocuments returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 document, got %d", len(got))
	}
	if got[0].Title != doc.Title || got[0].ChunkCount != doc.ChunkCount {
		t.Fatalf("unexpected document: %+v", got[0])
	}
	if got[0].Metadata["team"] != "billing" {
		t.Fatalf("metadata round-trip failed: %+v", got[0].Metadata)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGGetDocumentNotFoundMapsError(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	// Tenant guard: WHERE id = $1 AND organization_id = $2; zero rows scan as
	// sql.ErrNoRows, which the store must map to ErrDocumentNotFound.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectDocument)).
		WithArgs("doc-x", "org-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "organization_id", "title", "source", "COALESCE(metadata::text, '')",
			"chunk_count", "created_at", "updated_at",
		}))

	if _, err := store.GetDocument(context.Background(), "org-1", "doc-x"); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("unknown document must surface ErrDocumentNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGGetDocumentScanned(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	doc := testDocument()
	doc.Metadata = nil // NULL metadata path
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectDocument)).
		WithArgs("doc-1", "org-1").
		WillReturnRows(documentRows(doc))

	got, err := store.GetDocument(context.Background(), "org-1", "doc-1")
	if err != nil {
		t.Fatalf("GetDocument returned error: %v", err)
	}
	if got.ID != "doc-1" || got.Metadata != nil {
		t.Fatalf("unexpected document: %+v", got)
	}
}

func TestPGGetChunksOrdered(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	chunks := testChunks()
	rows := sqlmock.NewRows([]string{
		"id", "organization_id", "document_id", "ordinal", "content",
		"COALESCE(embedding::text, '')", "created_at",
	})
	rows.AddRow(chunks[0].ID, chunks[0].OrganizationID, chunks[0].DocumentID, chunks[0].Ordinal,
		chunks[0].Content, `[0.5,0.25]`, chunks[0].CreatedAt)
	// COALESCE(embedding::text, '') yields '' for NULL embeddings.
	rows.AddRow(chunks[1].ID, chunks[1].OrganizationID, chunks[1].DocumentID, chunks[1].Ordinal,
		chunks[1].Content, "", chunks[1].CreatedAt)
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectChunksByDocument)).
		WithArgs("doc-1", "org-1").
		WillReturnRows(rows)

	got, err := store.GetChunks(context.Background(), "org-1", "doc-1")
	if err != nil {
		t.Fatalf("GetChunks returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0].Embedding, []float64{0.5, 0.25}) {
		t.Fatalf("embedding round-trip failed: %v", got[0].Embedding)
	}
	if got[1].Embedding != nil {
		t.Fatalf("NULL embedding must scan to nil, got %v", got[1].Embedding)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGListChunksWithDocumentsJoin(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	chunk := testChunks()[0]
	rows := sqlmock.NewRows([]string{
		"id", "organization_id", "document_id", "ordinal", "content",
		"COALESCE(embedding::text, '')", "created_at", "title", "COALESCE(source, '')",
	}).AddRow(chunk.ID, chunk.OrganizationID, chunk.DocumentID, chunk.Ordinal,
		chunk.Content, `[0.5,0.25]`, chunk.CreatedAt, "Billing FAQ", "confluence/billing")
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectCandidates)).
		WithArgs("org-1", 500).
		WillReturnRows(rows)

	got, err := store.ListChunksWithDocuments(context.Background(), "org-1", 500)
	if err != nil {
		t.Fatalf("ListChunksWithDocuments returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate row, got %d", len(got))
	}
	if got[0].DocumentTitle != "Billing FAQ" || got[0].DocumentSource != "confluence/billing" {
		t.Fatalf("join fields missing: %+v", got[0])
	}
	if !reflect.DeepEqual(got[0].Embedding, []float64{0.5, 0.25}) {
		t.Fatalf("embedding round-trip failed: %v", got[0].Embedding)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGGuardNilDB(t *testing.T) {
	var store *pgStore
	if err := store.guard(); err == nil {
		t.Fatal("nil database must be guarded")
	}
}

func TestEmbeddingAndMetadataParams(t *testing.T) {
	if embeddingParam(nil) != nil {
		t.Fatal("nil embedding must map to SQL NULL")
	}
	raw, ok := embeddingParam([]float64{0.5, -0.25}).(string)
	if !ok {
		t.Fatalf("embedding param should marshal to a JSON string, got %T", embeddingParam([]float64{0.5}))
	}
	if got := embeddingFromParam(raw); !reflect.DeepEqual(got, []float64{0.5, -0.25}) {
		t.Fatalf("embedding round-trip mismatch: %v", got)
	}
	if embeddingFromParam("") != nil {
		t.Fatal("empty column must scan to nil")
	}
	if embeddingFromParam("not json") != nil {
		t.Fatal("corrupt column must scan to nil (degrade, never panic)")
	}

	if metadataParam(nil) != nil {
		t.Fatal("nil metadata must map to SQL NULL")
	}
	if metadataParam(map[string]any{}) != nil {
		t.Fatal("empty metadata must map to SQL NULL")
	}
	mraw, ok := metadataParam(map[string]any{"k": "v"}).(string)
	if !ok || mraw != `{"k":"v"}` {
		t.Fatalf("metadata should marshal to a JSON string, got %v", mraw)
	}
	if got := metadataFromParam(mraw); got["k"] != "v" {
		t.Fatalf("metadata round-trip failed: %+v", got)
	}
	if metadataFromParam("") != nil {
		t.Fatal("empty metadata column must scan to nil")
	}
	if metadataFromParam("not json") != nil {
		t.Fatal("corrupt metadata column must scan to nil")
	}
}
