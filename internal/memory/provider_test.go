package memory

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// fakeBatchProvider is a deterministic EmbeddingProvider fake for the issue
// #113 seam tests: vectors echo a per-text marker so the adapter's widening
// and the service's embedding persistence are both assertable.
type fakeBatchProvider struct {
	fail error
	// vectors maps content -> the float32 vector Embed returns for it.
	vectors map[string][]float32
	calls   []string // every text seen by Embed, in call order
}

func (f *fakeBatchProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		f.calls = append(f.calls, text)
		out[i] = f.vectors[text]
	}
	return out, nil
}

func (f *fakeBatchProvider) Dimensions() int { return len(f.vectors["dim"]) }

func (f *fakeBatchProvider) Name() string { return "fake-batch-provider" }

func TestProviderEmbedderAdapter(t *testing.T) {
	provider := &fakeBatchProvider{vectors: map[string][]float32{
		"hello world": {0.5, -0.25, 2},
	}}
	embedder := providerEmbedder{provider: provider}

	t.Run("name forwards the provider identity", func(t *testing.T) {
		if got := embedder.Name(); got != "fake-batch-provider" {
			t.Fatalf("Name must forward the provider name, got %q", got)
		}
	})

	t.Run("embed widens float32 exactly", func(t *testing.T) {
		vec, err := embedder.Embed(context.Background(), "hello world")
		if err != nil {
			t.Fatalf("Embed returned error: %v", err)
		}
		// Exact widening: float64(float32) round-trips the same bits.
		want := []float64{float64(float32(0.5)), float64(float32(-0.25)), float64(float32(2))}
		if len(vec) != len(want) {
			t.Fatalf("vector length mismatch: got %d, want %d", len(vec), len(want))
		}
		for i := range want {
			if vec[i] != want[i] {
				t.Fatalf("widening lost precision at %d: got %v, want %v", i, vec[i], want[i])
			}
		}
		// The provider saw exactly one single-text batch.
		if len(provider.calls) != 1 || provider.calls[0] != "hello world" {
			t.Fatalf("provider must see one single-text call, got %v", provider.calls)
		}
	})

	t.Run("provider errors propagate", func(t *testing.T) {
		failing := providerEmbedder{provider: &fakeBatchProvider{fail: errors.New("backend down")}}
		if _, err := failing.Embed(context.Background(), "x"); err == nil {
			t.Fatal("provider failure must surface to the service embedder")
		}
	})

	t.Run("compile-time seam check", func(t *testing.T) {
		// The adapter must satisfy memory's legacy single-text Embedder.
		var _ Embedder = providerEmbedder{}
		// The batch seam shape (structural contract with internal/knowledge).
		var _ EmbeddingProvider = &fakeBatchProvider{}
	})
}

func TestNewServiceWithEmbeddingProviderNilFallsBackToOffline(t *testing.T) {
	// A nil provider must behave exactly like NewService: the deterministic
	// offline hash embedder (256 dims) stays the default.
	svc := NewServiceWithEmbeddingProvider(nil)
	if svc == nil {
		t.Fatal("nil provider must fall back to a working service")
	}
	snippets, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		Snippets: []SnippetInput{{Scope: ScopeLongTerm, Content: "alpha memory"}},
	})
	if err != nil {
		t.Fatalf("PutSnippets returned error: %v", err)
	}
	want, err := NewHashEmbedder().Embed(context.Background(), "alpha memory")
	if err != nil {
		t.Fatalf("offline embedder failed: %v", err)
	}
	if len(snippets) != 1 || !reflect.DeepEqual(snippets[0].Embedding, want) {
		t.Fatalf("nil provider must keep the offline hash embedder, got %+v", snippets)
	}
}

func TestNewServiceWithEmbeddingProviderEmbedsThroughSeam(t *testing.T) {
	alpha := []float32{0.5, 0.5, 0.5}
	beta := []float32{-1, 0, 1}
	provider := &fakeBatchProvider{vectors: map[string][]float32{
		"alpha memory": alpha,
		"beta memory":  beta,
	}}
	svc := NewServiceWithEmbeddingProvider(provider)

	snippets, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		Snippets: []SnippetInput{
			{Scope: ScopeLongTerm, Content: "alpha memory"},
			{Scope: ScopeLongTerm, Content: "beta memory"},
		},
	})
	if err != nil {
		t.Fatalf("PutSnippets returned error: %v", err)
	}
	if len(snippets) != 2 {
		t.Fatalf("expected 2 snippets, got %d", len(snippets))
	}
	// Both snippets carry the provider vectors (widened to float64) — the
	// JSONB embedding column shape is unchanged.
	if !reflect.DeepEqual(snippets[0].Embedding, []float64{float64(alpha[0]), float64(alpha[1]), float64(alpha[2])}) {
		t.Fatalf("alpha snippet must carry the provider vector, got %v", snippets[0].Embedding)
	}
	if !reflect.DeepEqual(snippets[1].Embedding, []float64{float64(beta[0]), float64(beta[1]), float64(beta[2])}) {
		t.Fatalf("beta snippet must carry the provider vector, got %v", snippets[1].Embedding)
	}
	// One provider batch per snippet (documented limitation of PutSnippets).
	if len(provider.calls) != 2 {
		t.Fatalf("provider must see one call per snippet, got %v", provider.calls)
	}

	// Retrieval scores through the same vectors: the query embeds via the
	// provider, cosine ranks the matching snippet first.
	results, err := svc.Retrieve(context.Background(), "org-1", "alpha memory", RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve returned error: %v", err)
	}
	if len(results) == 0 || results[0].Snippet.Content != "alpha memory" {
		t.Fatalf("provider-cosine retrieval must rank the exact match first, got %+v", results)
	}
	// Identical vectors score cosine 1 (allow float rounding noise).
	if results[0].Score < 0.9999 || results[0].Score > 1.0001 {
		t.Fatalf("exact vector match must score ~1, got %v", results[0].Score)
	}
}

func TestNewServiceWithStoreAndEmbeddingProvider(t *testing.T) {
	t.Run("nil provider falls back to the offline store service", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New returned error: %v", err)
		}
		defer db.Close()
		_ = mock
		svc := NewServiceWithStoreAndEmbeddingProvider(db, nil)
		if svc == nil {
			t.Fatal("nil provider must fall back to a working service")
		}
	})

	t.Run("provider vectors reach the JSONB embedding column", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New returned error: %v", err)
		}
		defer db.Close()

		vec := []float32{0.25, -0.75, 1}
		provider := &fakeBatchProvider{vectors: map[string][]float32{"provider memory": vec}}
		svc := NewServiceWithStoreAndEmbeddingProvider(db, provider)

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta(sqlDeleteAgentSnippets)).
			WithArgs("org-1", "").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta(sqlInsertSnippet)).
			WithArgs(sqlmock.AnyArg(), "org-1", nil, ScopeLongTerm, "provider memory", sqlmock.AnyArg(),
				nil, embeddingParam([]float64{float64(vec[0]), float64(vec[1]), float64(vec[2])}),
				sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
			Snippets: []SnippetInput{{Scope: ScopeLongTerm, Content: "provider memory"}},
		}); err != nil {
			t.Fatalf("PutSnippets returned error: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("pending expectations: %v", err)
		}
	})
}
