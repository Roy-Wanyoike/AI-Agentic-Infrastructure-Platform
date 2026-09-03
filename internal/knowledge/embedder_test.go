package knowledge

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHashEmbedderDeterministic(t *testing.T) {
	embedder := NewHashEmbedder()
	ctx := context.Background()
	a, err := embedder.Embed(ctx, []string{"The customer prefers email invoices monthly"})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	b, _ := embedder.Embed(ctx, []string{"The customer prefers email invoices monthly"})
	if len(a) != 1 || len(a[0]) != OfflineEmbeddingDim || len(b[0]) != OfflineEmbeddingDim {
		t.Fatalf("embedding dimension mismatch: %d/%d", len(a[0]), len(b[0]))
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("hash embedder must be deterministic at index %d", i)
		}
	}
	norm := 0.0
	for _, v := range a[0] {
		norm += v * v
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-9 {
		t.Fatalf("embedding should be L2-normalized, got norm %.6f", math.Sqrt(norm))
	}
	same, _ := embedder.Embed(ctx, []string{"email invoices"})
	if cosineSimilarity(a[0], same[0]) <= 0.1 {
		t.Fatalf("overlapping vocabulary should score positively, got %.3f", cosineSimilarity(a[0], same[0]))
	}
	other, _ := embedder.Embed(ctx, []string{"zzz qqq xxx"})
	if cosineSimilarity(a[0], other[0]) > 0.25 {
		t.Fatalf("disjoint vocabulary should score low, got %.3f", cosineSimilarity(a[0], other[0]))
	}
	empty, _ := embedder.Embed(ctx, []string{""})
	if cosineSimilarity(a[0], empty[0]) != 0 {
		t.Fatal("empty text must embed to a zero vector (cosine 0)")
	}
	if _, err := embedder.Embed(ctx, nil); err != nil || len(empty) != 1 {
		t.Fatal("batch embed must tolerate empty inputs")
	}
	if embedder.Name() != "offline-hash-v1" {
		t.Fatalf("unexpected embedder name: %q", embedder.Name())
	}
}

func TestNewEmbedderFromEnv(t *testing.T) {
	t.Setenv(EnvEmbeddingModel, "")
	t.Setenv(EnvEmbeddingBaseURL, "")
	t.Setenv(EnvEmbeddingAPIKey, "")
	if _, ok := NewEmbedderFromEnv().(HashEmbedder); !ok {
		t.Fatal("without env knobs the offline hash embedder must be selected")
	}
	t.Setenv(EnvEmbeddingModel, "text-embedding-3-small")
	remote, ok := NewEmbedderFromEnv().(*OpenAIEmbedder)
	if !ok {
		t.Fatal("with AGENTOS_EMBEDDING_MODEL the remote embedder must be selected")
	}
	if remote.Model != "text-embedding-3-small" {
		t.Fatalf("model not carried through: %q", remote.Model)
	}
	if remote.BaseURL != DefaultEmbeddingBaseURL {
		t.Fatalf("default base URL not applied: %q", remote.BaseURL)
	}
}

func TestOpenAIEmbedderSuccess(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	var gotInput []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var req embeddingsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		gotInput = req.Input
		_ = json.NewEncoder(w).Encode(embeddingsResponse{
			Data: []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			}{
				{Index: 1, Embedding: []float64{0, 1}},
				{Index: 0, Embedding: []float64{1, 0}},
			},
		})
	}))
	defer server.Close()

	embedder := NewOpenAIEmbedder(server.URL+"/v1", "text-embedding-3-small", "sk-test")
	out, err := embedder.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("client must POST {base}/embeddings, got %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("API key must be sent as bearer token, got %q", gotAuth)
	}
	if gotModel != "text-embedding-3-small" {
		t.Fatalf("model not sent: %q", gotModel)
	}
	if len(gotInput) != 2 || gotInput[0] != "first" {
		t.Fatalf("input batch not sent: %v", gotInput)
	}
	// Vectors must come back in INPUT order (server responded out of order).
	if len(out) != 2 || out[0][0] != 1 || out[1][1] != 1 {
		t.Fatalf("vectors must be reordered by the response index field: %v", out)
	}
	if embedder.Name() != "text-embedding-3-small" {
		t.Fatalf("Name() should be the model: %q", embedder.Name())
	}
}

func TestOpenAIEmbedderErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("http error status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
		}))
		defer server.Close()
		_, err := NewOpenAIEmbedder(server.URL, "m", "k").Embed(ctx, []string{"x"})
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("HTTP failures must surface the status code, got %v", err)
		}
	})
	t.Run("count mismatch", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
		}))
		defer server.Close()
		_, err := NewOpenAIEmbedder(server.URL, "m", "k").Embed(ctx, []string{"x", "y"})
		if err == nil || !strings.Contains(err.Error(), "for 2 inputs") {
			t.Fatalf("count mismatch must error, got %v", err)
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer server.Close()
		if _, err := NewOpenAIEmbedder(server.URL, "m", "k").Embed(ctx, []string{"x"}); err == nil {
			t.Fatal("invalid JSON must error")
		}
	})
	t.Run("missing vector for one input", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":null},{"index":1,"embedding":[1]}]}`))
		}))
		defer server.Close()
		if _, err := NewOpenAIEmbedder(server.URL, "m", "k").Embed(ctx, []string{"x", "y"}); err == nil {
			t.Fatal("a missing vector must error")
		}
	})
	t.Run("out of range index", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":5,"embedding":[1]}]}`))
		}))
		defer server.Close()
		if _, err := NewOpenAIEmbedder(server.URL, "m", "k").Embed(ctx, []string{"x"}); err == nil {
			t.Fatal("an out-of-range index must error")
		}
	})
	t.Run("empty inputs short-circuits", func(t *testing.T) {
		if _, err := NewOpenAIEmbedder("http://127.0.0.1:1", "m", "k").Embed(ctx, nil); err != nil {
			t.Fatalf("empty batch must not issue a request, got %v", err)
		}
	})
	t.Run("missing config", func(t *testing.T) {
		if _, err := NewOpenAIEmbedder("", "", "").Embed(ctx, []string{"x"}); err == nil {
			t.Fatal("missing base URL/model must error")
		}
	})
}
