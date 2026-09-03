package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubEmbedder returns canned vectors so ranking tests are exact; a nil map
// entry means "no vector" (drives the lexical fallback paths).
type stubEmbedder struct {
	vectors map[string][]float64
	batches int
}

func (s *stubEmbedder) Name() string { return "stub" }

func (s *stubEmbedder) Embed(_ context.Context, inputs []string) ([][]float64, error) {
	s.batches++
	out := make([][]float64, len(inputs))
	for i, text := range inputs {
		out[i] = append([]float64(nil), s.vectors[text]...)
	}
	return out, nil
}

// failEmbedder always errors (drives the non-fatal ingest path).
type failEmbedder struct{}

func (failEmbedder) Name() string { return "fail" }

func (failEmbedder) Embed(_ context.Context, _ []string) ([][]float64, error) {
	return nil, errors.New("embeddings backend down")
}

func TestServiceIngestDocumentValidation(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, _, err := svc.IngestDocument(ctx, "", IngestRequest{Title: "t", Content: "c"}); err == nil {
		t.Fatal("empty org id must be rejected")
	}
	if _, _, err := svc.IngestDocument(ctx, "org-1", IngestRequest{Title: "  ", Content: "c"}); err == nil {
		t.Fatal("blank title must be rejected")
	}
	if _, _, err := svc.IngestDocument(ctx, "org-1", IngestRequest{Title: "t", Content: "   "}); err == nil {
		t.Fatal("blank content must be rejected")
	}
}

func TestServiceIngestDocumentInMemory(t *testing.T) {
	svc := NewService()
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	doc, chunks, err := svc.IngestDocument(context.Background(), "org-1", IngestRequest{
		Title:   "Billing FAQ",
		Content: "Q: How do invoices work? A: Invoices are sent monthly by email.",
		Source:  "confluence/billing",
		Metadata: map[string]any{
			"team": "billing",
		},
	})
	if err != nil {
		t.Fatalf("IngestDocument returned error: %v", err)
	}
	if doc.ID == "" || doc.OrganizationID != "org-1" || doc.ChunkCount != 1 {
		t.Fatalf("unexpected document: %+v", doc)
	}
	if !doc.CreatedAt.Equal(now) || !doc.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps should be service-controlled: %+v", doc)
	}
	if len(chunks) != 1 || chunks[0].Ordinal != 0 || chunks[0].DocumentID != doc.ID {
		t.Fatalf("unexpected chunks: %+v", chunks)
	}
	if len(chunks[0].Embedding) != OfflineEmbeddingDim {
		t.Fatalf("offline embedder should attach a vector, got dim %d", len(chunks[0].Embedding))
	}
	if doc.Metadata["team"] != "billing" {
		t.Fatalf("metadata not carried through: %+v", doc.Metadata)
	}

	got, gotChunks, err := svc.GetDocument(context.Background(), "org-1", doc.ID)
	if err != nil {
		t.Fatalf("GetDocument returned error: %v", err)
	}
	if got.ID != doc.ID || len(gotChunks) != 1 {
		t.Fatalf("GetDocument round-trip failed: %+v", got)
	}
}

func TestServiceIngestEmbedFailureIsNonFatal(t *testing.T) {
	svc := newService(nil, failEmbedder{})
	doc, chunks, err := svc.IngestDocument(context.Background(), "org-1", IngestRequest{
		Title:   "Doc",
		Content: "Some content that still gets stored without vectors.",
	})
	if err == nil {
		t.Fatal("embedder failure must be surfaced to the caller")
	}
	if doc == nil || len(chunks) != 1 {
		t.Fatalf("document + chunks must still be stored, got %+v / %d", doc, len(chunks))
	}
	for _, c := range chunks {
		if len(c.Embedding) != 0 {
			t.Fatalf("chunks must be stored unembedded, got %d dims", len(c.Embedding))
		}
	}
	// Retrieval falls back to lexical scoring for the unembedded chunks.
	results, err := svc.Search(context.Background(), "org-1", "content stored", 5)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].Score <= 0 {
		t.Fatalf("lexical fallback should match, got %+v", results)
	}
}

func TestServiceListDocumentsNewestFirstAndTenantScoped(t *testing.T) {
	svc := NewService()
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }
	ctx := context.Background()

	first, _, err := svc.IngestDocument(ctx, "org-1", IngestRequest{Title: "first", Content: "alpha"})
	if err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	svc.now = func() time.Time { return base.Add(time.Hour) }
	if _, _, err := svc.IngestDocument(ctx, "org-1", IngestRequest{Title: "second", Content: "beta"}); err != nil {
		t.Fatalf("ingest 2: %v", err)
	}
	if _, _, err := svc.IngestDocument(ctx, "org-2", IngestRequest{Title: "foreign", Content: "gamma"}); err != nil {
		t.Fatalf("ingest foreign: %v", err)
	}

	docs, err := svc.ListDocuments(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListDocuments returned error: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 org-1 documents, got %d", len(docs))
	}
	if docs[0].Title != "second" || docs[1].Title != "first" {
		t.Fatalf("listing must be newest first: %q then %q", docs[0].Title, docs[1].Title)
	}
	if docs[1].ID != first.ID {
		t.Fatalf("document identity lost: %+v", docs[1])
	}
	foreign, err := svc.ListDocuments(ctx, "org-2")
	if err != nil {
		t.Fatalf("ListDocuments(org-2) returned error: %v", err)
	}
	if len(foreign) != 1 || foreign[0].Title != "foreign" {
		t.Fatalf("tenant isolation broken: %+v", foreign)
	}
}

func TestServiceGetDocumentNotFound(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, _, err := svc.IngestDocument(ctx, "org-1", IngestRequest{Title: "d", Content: "c"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, _, err := svc.GetDocument(ctx, "org-1", "missing"); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("unknown id must surface ErrDocumentNotFound, got %v", err)
	}
	if _, _, err := svc.GetDocument(ctx, "org-2", "missing"); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("foreign tenant lookup must surface ErrDocumentNotFound, got %v", err)
	}
}

func TestServiceSearchOfflineEmbeddingRanking(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, _, err := svc.IngestDocument(ctx, "org-1", IngestRequest{
		Title: "Invoices",
		Content: "Invoices are generated monthly and sent to the billing contact by email. " +
			"Late payments trigger a dunning sequence after fourteen days.",
	}); err != nil {
		t.Fatalf("ingest invoices: %v", err)
	}
	if _, _, err := svc.IngestDocument(ctx, "org-1", IngestRequest{
		Title:   "Gardening",
		Content: "Roses need full sun and well drained soil to bloom through the summer months.",
	}); err != nil {
		t.Fatalf("ingest gardening: %v", err)
	}
	if _, _, err := svc.IngestDocument(ctx, "org-2", IngestRequest{
		Title:   "Secret",
		Content: "Invoices are generated monthly and sent to the billing contact by email.",
	}); err != nil {
		t.Fatalf("ingest foreign: %v", err)
	}

	results, err := svc.Search(ctx, "org-1", "invoices monthly billing email", 1)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("k=1 must return exactly one result, got %d", len(results))
	}
	hit := results[0]
	if hit.DocumentTitle != "Invoices" {
		t.Fatalf("top hit should be the invoices document, got %q", hit.DocumentTitle)
	}
	if hit.Score <= 0.5 {
		t.Fatalf("offline cosine should score the overlap strongly, got %.3f", hit.Score)
	}
	if hit.ChunkOrdinal != 0 {
		t.Fatalf("unexpected ordinal: %d", hit.ChunkOrdinal)
	}
	if !strings.Contains(hit.Citation, "Invoices") || !strings.Contains(hit.Citation, "chunk 0") {
		t.Fatalf("citation should name the document and chunk, got %q", hit.Citation)
	}
	if hit.Content == "" {
		t.Fatal("result content must carry the chunk text")
	}

	// Disjoint vocabulary must not outrank anything: hashed pseudo-embeddings
	// carry documented collision noise (docs/wiring/knowledge.md), so the
	// assertion is relative (noise stays far below the true match above), and
	// strict zero-result behavior is covered by the stub-embedder tests.
	noise, err := svc.Search(ctx, "org-1", "zzz qqq unrelated vocabulary", 5)
	if err != nil {
		t.Fatalf("Search(noise) returned error: %v", err)
	}
	for _, hit := range noise {
		if hit.Score > 0.25 {
			t.Fatalf("collision noise should stay low, got %.3f", hit.Score)
		}
	}
}

func TestServiceSearchCitationIncludesSource(t *testing.T) {
	svc := NewService()
	if _, _, err := svc.IngestDocument(context.Background(), "org-1", IngestRequest{
		Title:   "Runbook",
		Content: "Restart the ingestion worker before scaling the queue consumers.",
		Source:  "wiki/runbooks",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	results, err := svc.Search(context.Background(), "org-1", "restart ingestion worker", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one hit")
	}
	if !strings.Contains(results[0].Citation, "wiki/runbooks") {
		t.Fatalf("citation should include the source, got %q", results[0].Citation)
	}
}

func TestServiceSearchValidationAndKBounds(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, err := svc.Search(ctx, "", "q", 5); err == nil {
		t.Fatal("empty org id must be rejected")
	}
	if _, err := svc.Search(ctx, "org-1", "   ", 5); err == nil {
		t.Fatal("blank query must be rejected")
	}
	// k<=0 falls back to the default; huge k is capped.
	for _, k := range []int{0, -3, MaxSearchK + 10} {
		results, err := svc.Search(ctx, "org-1", "anything", k)
		if err != nil {
			t.Fatalf("Search(k=%d) returned error: %v", k, err)
		}
		if len(results) > MaxSearchK {
			t.Fatalf("k must be capped at %d, got %d", MaxSearchK, len(results))
		}
	}
}

func TestServiceSearchMultiChunkReturnsOrderedOrdinals(t *testing.T) {
	svc := NewService()
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("Paragraph about the billing pipeline and invoice generation. ")
	}
	if _, _, err := svc.IngestDocument(context.Background(), "org-1", IngestRequest{
		Title:   "Long doc",
		Content: b.String(),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	results, err := svc.Search(context.Background(), "org-1", "billing pipeline invoice generation", 50)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("multi-chunk doc should yield several hits, got %d", len(results))
	}
	for _, r := range results {
		if r.DocumentTitle != "Long doc" {
			t.Fatalf("unexpected document in results: %q", r.DocumentTitle)
		}
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].Score < results[i].Score {
			t.Fatalf("results must be score-ordered: %.3f before %.3f", results[i-1].Score, results[i].Score)
		}
	}
}

func TestServiceTenantIsolationOnSearch(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, _, err := svc.IngestDocument(ctx, "org-1", IngestRequest{
		Title:   "Internal",
		Content: "The private pricing agreement with customer Acme caps usage discounts at 40 percent.",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	results, err := svc.Search(ctx, "org-2", "private pricing agreement Acme discounts", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("cross-tenant search leak: %+v", results)
	}
}

func TestServiceWithStoreIngestAndSearchUseStore(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	embedder := &stubEmbedder{vectors: map[string][]float64{
		"alpha beta":            {1, 0, 0},
		"alpha beta query here": {1, 0, 0},
	}}
	store := &fakeStore{
		doc: &Document{ID: "doc-1", OrganizationID: "org-1", Title: "Stored doc", ChunkCount: 1, CreatedAt: now, UpdatedAt: now},
		rows: []ChunkRow{
			{Chunk: Chunk{ID: "c1", OrganizationID: "org-1", DocumentID: "doc-1", Ordinal: 0, Content: "alpha beta", Embedding: []float64{1, 0, 0}, CreatedAt: now}, DocumentTitle: "Stored doc"},
		},
	}
	svc := newService(store, embedder)

	doc, chunks, err := svc.IngestDocument(context.Background(), "org-1", IngestRequest{
		Title:   "Stored doc",
		Content: "alpha beta",
	})
	if err != nil {
		t.Fatalf("IngestDocument: %v", err)
	}
	if store.created == nil || store.created.ID != doc.ID {
		t.Fatalf("store CreateDocument not invoked: %+v", store.created)
	}
	if len(store.createdChunks) != 1 || len(store.createdChunks[0].Embedding) != 3 {
		t.Fatalf("store should receive embedded chunks: %+v", store.createdChunks)
	}
	_ = chunks

	results, err := svc.Search(context.Background(), "org-1", "alpha beta query here", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if store.candidateOrg != "org-1" {
		t.Fatalf("candidates should be fetched for the caller's org, got %q", store.candidateOrg)
	}
	if len(results) != 1 || results[0].Score != 1 {
		t.Fatalf("cosine over stored vectors expected score 1, got %+v", results)
	}
	if results[0].DocumentID != "doc-1" || results[0].ChunkID != "c1" {
		t.Fatalf("result identity mismatch: %+v", results[0])
	}

	docs, err := svc.ListDocuments(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "doc-1" {
		t.Fatalf("listing should come from the store: %+v", docs)
	}
}

func TestServiceWithStoreSurfacesStoreErrors(t *testing.T) {
	store := &fakeStore{fail: errors.New("db down")}
	svc := newService(store, NewHashEmbedder())
	if _, _, err := svc.IngestDocument(context.Background(), "org-1", IngestRequest{Title: "t", Content: "c"}); err == nil {
		t.Fatal("store failure on ingest must propagate")
	}
	if _, err := svc.Search(context.Background(), "org-1", "q", 5); err == nil {
		t.Fatal("store failure on search must propagate")
	}
	if _, err := svc.ListDocuments(context.Background(), "org-1"); err == nil {
		t.Fatal("store failure on list must propagate")
	}
}

// fakeStore captures Store interactions for the dual-mode service tests.
type fakeStore struct {
	doc           *Document
	created       *Document
	createdChunks []Chunk
	rows          []ChunkRow
	candidateOrg  string
	fail          error
}

func (f *fakeStore) CreateDocument(_ context.Context, doc *Document, chunks []Chunk) error {
	if f.fail != nil {
		return f.fail
	}
	f.created, f.createdChunks = doc, chunks
	return nil
}

func (f *fakeStore) ListDocuments(_ context.Context, orgID string) ([]*Document, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	if f.doc == nil || f.doc.OrganizationID != orgID {
		return nil, nil
	}
	return []*Document{f.doc}, nil
}

func (f *fakeStore) ListChunksWithDocuments(_ context.Context, orgID string, _ int) ([]ChunkRow, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.candidateOrg = orgID
	return f.rows, nil
}

func (f *fakeStore) GetDocument(_ context.Context, orgID, id string) (*Document, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	if f.doc == nil || f.doc.ID != id || f.doc.OrganizationID != orgID {
		return nil, ErrDocumentNotFound
	}
	return f.doc, nil
}

func (f *fakeStore) GetChunks(_ context.Context, orgID, documentID string) ([]Chunk, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	out := make([]Chunk, 0)
	for _, row := range f.rows {
		if row.DocumentID == documentID && row.OrganizationID == orgID {
			out = append(out, row.Chunk)
		}
	}
	return out, nil
}
