package knowledge

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// stubEmbeddingProvider is a deterministic EmbeddingProvider fake with
// injectable failure counts (for retry tests).
type stubEmbeddingProvider struct {
	mu        sync.Mutex
	calls     int
	failFirst int // fail the first N Embed calls
	dim       int
}

func newStubEmbeddingProvider(dim int) *stubEmbeddingProvider {
	return &stubEmbeddingProvider{dim: dim}
}

func (s *stubEmbeddingProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.failFirst {
		return nil, errors.New("stub provider: transient failure")
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		// dim-sized vector echoing the input length in slot 0 so ordering is
		// assertable.
		vec := make([]float32, s.dim)
		vec[0] = float32(len(text))
		out[i] = vec
	}
	return out, nil
}

func (s *stubEmbeddingProvider) Dimensions() int { return s.dim }

func (s *stubEmbeddingProvider) Name() string { return "stub-embedder" }

func (s *stubEmbeddingProvider) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// fakeVectorStore records upserts for pipeline assertions.
type fakeVectorStore struct {
	mu        sync.Mutex
	docs      []*Document
	chunkSets [][]Chunk
	err       error
}

func (f *fakeVectorStore) UpsertDocumentWithVectors(_ context.Context, doc *Document, chunks []Chunk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.docs = append(f.docs, doc)
	f.chunkSets = append(f.chunkSets, chunks)
	return nil
}

func (f *fakeVectorStore) VectorSearch(context.Context, string, []float32, int) ([]ScoredChunk, error) {
	return nil, nil
}

func (f *fakeVectorStore) upserts() (int, *Document, []Chunk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.docs) == 0 {
		return 0, nil, nil
	}
	return len(f.docs), f.docs[len(f.docs)-1], f.chunkSets[len(f.chunkSets)-1]
}

func TestEmbeddingPipelineHappyPath(t *testing.T) {
	store := &fakeVectorStore{}
	provider := newStubEmbeddingProvider(4)
	var events []string
	pipeline := NewEmbeddingPipeline(store, provider).
		WithBatchSize(2).
		WithRetryWait(time.Millisecond).
		WithProgress(func(stage PipelineStage, done, total int) {
			events = append(events, string(stage)+":"+strconv.Itoa(done)+"/"+strconv.Itoa(total))
		})

	// Long enough to produce several chunks (the exact count is the
	// chunker's business; the pipeline must handle whatever it emits).
	content := strings.Repeat("The billing engine emits invoices monthly. ", 200)
	doc, chunks, err := pipeline.Ingest(context.Background(), PipelineDocument{
		OrganizationID: "org-1",
		Title:          "Billing FAQ",
		Source:         "confluence/billing",
		Metadata:       map[string]any{"team": "billing"},
		Content:        content,
	})
	if err != nil {
		t.Fatalf("Ingest returned error: %v", err)
	}
	upserts, stored, storedChunks := store.upserts()
	if upserts != 1 {
		t.Fatalf("expected exactly 1 upsert, got %d", upserts)
	}
	if len(storedChunks) < 2 {
		t.Fatalf("fixture must produce multiple chunks, got %d", len(storedChunks))
	}
	if doc.ID != stored.ID || doc.ChunkCount != len(storedChunks) {
		t.Fatalf("document metadata mismatch: %+v vs %+v", doc, stored)
	}
	if len(chunks) != len(storedChunks) {
		t.Fatalf("returned chunks must mirror the stored set")
	}
	seenOrdinals := map[int]bool{}
	for i, c := range chunks {
		if c.Ordinal != i {
			t.Fatalf("chunk ordinals must be dense 0..n, got %d at %d", c.Ordinal, i)
		}
		seenOrdinals[c.Ordinal] = true
		if len(c.Vector) == 0 || len(c.Embedding) == 0 {
			t.Fatalf("chunk %d must carry both vector forms (provider + JSONB mirror)", i)
		}
		if c.Vector[0] != float32(len(c.Content)) {
			t.Fatalf("chunk %d vector does not echo its content ordering", i)
		}
		if len(c.Embedding) != len(c.Vector) {
			t.Fatalf("chunk %d JSONB mirror dimension mismatch", i)
		}
		if c.OrganizationID != "org-1" || c.DocumentID != doc.ID {
			t.Fatalf("chunk %d tenant/document scoping broken: %+v", i, c)
		}
	}
	if len(seenOrdinals) != len(chunks) {
		t.Fatal("duplicate ordinals in the chunk set")
	}
	// Provider calls: ceil(chunks / batch size) with one batch each.
	wantCalls := (len(chunks) + 1) / 2
	if provider.callCount() != wantCalls {
		t.Fatalf("expected %d provider calls, got %d", wantCalls, provider.callCount())
	}
	// Progress: chunked once, embedded per batch (monotonic, ending at
	// total), stored once.
	if len(events) != wantCalls+2 {
		t.Fatalf("expected %d progress events, got %d: %v", wantCalls+2, len(events), events)
	}
	if !strings.HasPrefix(events[0], "chunked:") || !strings.HasPrefix(events[len(events)-1], "stored:") {
		t.Fatalf("progress stages out of order: %v", events)
	}
	lastEmbedded := events[len(events)-2]
	if !strings.HasPrefix(lastEmbedded, "embedded:"+strconv.Itoa(len(chunks))+"/"+strconv.Itoa(len(chunks))) {
		t.Fatalf("final embedded event must report done==total, got %q", lastEmbedded)
	}
}

func TestEmbeddingPipelineIdempotentDocumentKey(t *testing.T) {
	store := &fakeVectorStore{}
	provider := newStubEmbeddingProvider(3)
	pipeline := NewEmbeddingPipeline(store, provider).WithRetryWait(time.Millisecond)

	content := "Stable document content for the idempotency check."
	firstDoc, firstChunks, err := pipeline.Ingest(context.Background(), PipelineDocument{
		DocumentID:     "doc-stable-1",
		OrganizationID: "org-1",
		Title:          "Stable doc",
		Content:        content,
	})
	if err != nil {
		t.Fatalf("first Ingest returned error: %v", err)
	}
	secondDoc, secondChunks, err := pipeline.Ingest(context.Background(), PipelineDocument{
		DocumentID:     "doc-stable-1",
		OrganizationID: "org-1",
		Title:          "Stable doc (updated)",
		Content:        content,
	})
	if err != nil {
		t.Fatalf("second Ingest returned error: %v", err)
	}
	if firstDoc.ID != "doc-stable-1" || secondDoc.ID != "doc-stable-1" {
		t.Fatalf("a caller-supplied DocumentID must be honored for idempotent re-ingest: %q/%q", firstDoc.ID, secondDoc.ID)
	}
	if len(firstChunks) != len(secondChunks) {
		t.Fatalf("same content must produce the same chunk count: %d vs %d", len(firstChunks), len(secondChunks))
	}
	for i := range secondChunks {
		if secondChunks[i].DocumentID != "doc-stable-1" || secondChunks[i].Ordinal != i {
			t.Fatalf("re-ingest must upsert on (org, document, ordinal): %+v", secondChunks[i])
		}
	}
	upserts, _, _ := store.upserts()
	if upserts != 2 {
		t.Fatalf("expected 2 upserts (one per ingest), got %d", upserts)
	}
	// Without a caller-supplied id each ingest is a NEW document.
	autoDoc, _, err := pipeline.Ingest(context.Background(), PipelineDocument{
		OrganizationID: "org-1", Title: "Auto", Content: content,
	})
	if err != nil {
		t.Fatalf("auto-id Ingest returned error: %v", err)
	}
	if autoDoc.ID == "" || autoDoc.ID == "doc-stable-1" {
		t.Fatalf("auto id must be a fresh uuid, got %q", autoDoc.ID)
	}
}

func TestEmbeddingPipelineRetryPerBatch(t *testing.T) {
	t.Run("transient failure recovers", func(t *testing.T) {
		store := &fakeVectorStore{}
		provider := newStubEmbeddingProvider(3)
		provider.failFirst = 1
		var waits []time.Duration
		pipeline := NewEmbeddingPipeline(store, provider).
			WithRetryWait(2 * time.Millisecond).
			WithMaxAttempts(3)
		pipeline.sleep = func(d time.Duration) { waits = append(waits, d) }

		doc, chunks, err := pipeline.Ingest(context.Background(), PipelineDocument{
			OrganizationID: "org-1", Title: "T", Content: "short content",
		})
		if err != nil {
			t.Fatalf("retry must recover a transient batch failure: %v", err)
		}
		if len(chunks) == 0 || doc.ChunkCount != len(chunks) {
			t.Fatalf("ingest incomplete after retry: %+v", doc)
		}
		if len(waits) != 1 || waits[0] != 2*time.Millisecond {
			t.Fatalf("expected one base backoff wait, got %v", waits)
		}
	})
	t.Run("exhausted retries abort before any store write", func(t *testing.T) {
		store := &fakeVectorStore{}
		provider := newStubEmbeddingProvider(3)
		provider.failFirst = 100
		pipeline := NewEmbeddingPipeline(store, provider).
			WithRetryWait(time.Millisecond).
			WithMaxAttempts(3)
		pipeline.sleep = func(time.Duration) {}

		_, _, err := pipeline.Ingest(context.Background(), PipelineDocument{
			OrganizationID: "org-1", Title: "T", Content: "short content",
		})
		if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
			t.Fatalf("exhausted retries must fail with the attempt budget, got %v", err)
		}
		if upserts, _, _ := store.upserts(); upserts != 0 {
			t.Fatal("a failed pipeline must not write anything (atomicity)")
		}
	})
	t.Run("vector count mismatch fails the batch", func(t *testing.T) {
		store := &fakeVectorStore{}
		provider := &countMismatchProvider{}
		pipeline := NewEmbeddingPipeline(store, provider).WithRetryWait(time.Millisecond).WithMaxAttempts(2)
		pipeline.sleep = func(time.Duration) {}
		if _, _, err := pipeline.Ingest(context.Background(), PipelineDocument{
			OrganizationID: "org-1", Title: "T", Content: "short content",
		}); err == nil || !strings.Contains(err.Error(), "vectors for") {
			t.Fatalf("count mismatch must fail the batch, got %v", err)
		}
	})
}

func TestEmbeddingPipelineValidation(t *testing.T) {
	pipeline := NewEmbeddingPipeline(&fakeVectorStore{}, newStubEmbeddingProvider(3))
	cases := []struct {
		name string
		doc  PipelineDocument
		want string
	}{
		{"missing org", PipelineDocument{Title: "T", Content: "C"}, "organization id is required"},
		{"missing title", PipelineDocument{OrganizationID: "org-1", Content: "C"}, "title is required"},
		{"missing content", PipelineDocument{OrganizationID: "org-1", Title: "T"}, "content is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := pipeline.Ingest(context.Background(), tc.doc); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	t.Run("nil store", func(t *testing.T) {
		if _, _, err := NewEmbeddingPipeline(nil, newStubEmbeddingProvider(3)).Ingest(context.Background(), PipelineDocument{
			OrganizationID: "org-1", Title: "T", Content: "C",
		}); err == nil {
			t.Fatal("nil store must be rejected")
		}
	})
	t.Run("nil provider", func(t *testing.T) {
		if _, _, err := NewEmbeddingPipeline(&fakeVectorStore{}, nil).Ingest(context.Background(), PipelineDocument{
			OrganizationID: "org-1", Title: "T", Content: "C",
		}); err == nil {
			t.Fatal("nil provider must be rejected")
		}
	})
}

// countMismatchProvider returns fewer vectors than inputs.
type countMismatchProvider struct{}

func (countMismatchProvider) Embed(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (countMismatchProvider) Dimensions() int { return 3 }
func (countMismatchProvider) Name() string    { return "count-mismatch" }

func TestEmbeddingPipelineIntoPgVectorStore(t *testing.T) {
	// End-to-end shape check: pipeline -> pgVectorStore with the vector
	// traveling as a $n::vector text literal (sqlmock; no live pgvector).
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	store := NewPgVectorStore(db).(*pgVectorStore)
	store.vectorDim = 3

	provider := newStubEmbeddingProvider(3)
	pipeline := NewEmbeddingPipeline(store, provider).WithRetryWait(time.Millisecond)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlUpsertDocument)).
		WithArgs("doc-fixed", "org-1", "Fixed doc", "", nil, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(sqlUpsertChunk)).
		WithArgs(sqlmock.AnyArg(), "org-1", "doc-fixed", 0, "short content",
			sqlmock.AnyArg(), "[13,0,0]", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(sqlDeleteStaleChunks)).
		WithArgs("org-1", "doc-fixed", 1).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	doc, chunks, err := pipeline.Ingest(context.Background(), PipelineDocument{
		DocumentID:     "doc-fixed",
		OrganizationID: "org-1",
		Title:          "Fixed doc",
		Content:        "short content",
	})
	if err != nil {
		t.Fatalf("Ingest returned error: %v", err)
	}
	if doc.ID != "doc-fixed" || len(chunks) != 1 {
		t.Fatalf("unexpected pipeline result: %+v / %d chunks", doc, len(chunks))
	}
	// The chunk carries the JSONB mirror of the 3-dim provider vector.
	if !reflect.DeepEqual(chunks[0].Embedding, []float64{13, 0, 0}) {
		t.Fatalf("JSONB mirror wrong: %v", chunks[0].Embedding)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
