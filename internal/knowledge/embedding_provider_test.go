package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingEmbeddingsServer is an OpenAI-compatible /v1/embeddings fake. The
// echo vector is [len(input), batchNumber]: distinct input lengths make the
// input-order round trip assertable across batch boundaries.
type recordingEmbeddingsServer struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []providerEmbeddingsRequest
	auths    []string
}

func newRecordingEmbeddingsServer(t *testing.T) *recordingEmbeddingsServer {
	t.Helper()
	fake := &recordingEmbeddingsServer{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req providerEmbeddingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("fake server: invalid request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.auths = append(fake.auths, r.Header.Get("Authorization"))
		batchNo := len(fake.requests) + 1 // 1-based
		fake.requests = append(fake.requests, req)
		resp := providerEmbeddingsResponse{}
		for i, input := range req.Input {
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: []float32{float32(len(input)), float32(batchNo)}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// distinctLengthInputs returns inputs of lengths 1..n so every vector's echo
// pins its exact input position.
func distinctLengthInputs(n int) []string {
	inputs := make([]string, n)
	for i := range inputs {
		inputs[i] = strings.Repeat("x", i+1)
	}
	return inputs
}

func TestOpenAIEmbeddingProviderSuccess(t *testing.T) {
	fake := newRecordingEmbeddingsServer(t)
	provider := NewOpenAIEmbeddingProvider(fake.server.URL, "text-embedding-3-small", "sk-test")

	inputs := []string{"first", "second", "thirdss"} // distinct lengths 5/6/7
	out, err := provider.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(fake.requests))
	}
	req := fake.requests[0]
	if req.Model != "text-embedding-3-small" {
		t.Fatalf("model not sent: %q", req.Model)
	}
	if len(req.Input) != 3 || req.Input[0] != "first" {
		t.Fatalf("input batch not sent: %v", req.Input)
	}
	if fake.auths[0] != "Bearer sk-test" {
		t.Fatalf("API key must be sent as bearer token, got %q", fake.auths[0])
	}
	// Vectors must come back in INPUT order (echo[0] = input length).
	for i, vec := range out {
		if len(vec) != 2 {
			t.Fatalf("vector %d malformed: %v", i, vec)
		}
		if vec[0] != float32(len(inputs[i])) {
			t.Fatalf("input order violated at %d: %v (want len %d)", i, vec, len(inputs[i]))
		}
		if vec[1] != 1 {
			t.Fatalf("vector %d came from batch %v (want 1)", i, vec[1])
		}
	}
	if provider.Name() != "text-embedding-3-small" {
		t.Fatalf("Name() should be the model: %q", provider.Name())
	}
	if provider.Dimensions() != 1536 {
		t.Fatalf("text-embedding-3-small must report 1536 dims, got %d", provider.Dimensions())
	}
}

func TestOpenAIEmbeddingProviderEndpointVariants(t *testing.T) {
	cases := []struct {
		base, want string
	}{
		{"https://api.example.com", "https://api.example.com/v1/embeddings"},
		{"https://api.example.com/v1", "https://api.example.com/v1/embeddings"},
		{"https://api.example.com/v1/", "https://api.example.com/v1/embeddings"},
		{"https://api.example.com/", "https://api.example.com/v1/embeddings"},
	}
	for _, tc := range cases {
		p := NewOpenAIEmbeddingProvider(tc.base, "m", "k")
		if got := p.endpoint(); got != tc.want {
			t.Fatalf("base %q -> %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestOpenAIEmbeddingProviderBatches(t *testing.T) {
	fake := newRecordingEmbeddingsServer(t)
	provider := NewOpenAIEmbeddingProvider(fake.server.URL, "text-embedding-3-small", "")
	provider.MaxBatchInputs = 3
	provider.MaxBatchTokens = 1_000_000

	// 8 inputs (lengths 1..8) -> batches of 3/3/2.
	inputs := distinctLengthInputs(8)
	out, err := provider.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("expected 3 batched requests, got %d", len(fake.requests))
	}
	if len(fake.requests[0].Input) != 3 || len(fake.requests[1].Input) != 3 || len(fake.requests[2].Input) != 2 {
		t.Fatalf("batch sizes wrong: %d/%d/%d", len(fake.requests[0].Input), len(fake.requests[1].Input), len(fake.requests[2].Input))
	}
	if len(out) != len(inputs) {
		t.Fatalf("expected %d vectors, got %d", len(inputs), len(out))
	}
	// Batches must interleave back into exact input order.
	for i, vec := range out {
		if vec[0] != float32(i+1) {
			t.Fatalf("input order violated at %d: %v", i, vec)
		}
		if vec[1] != float32(i/3+1) {
			t.Fatalf("vector %d came from batch %v (want %d)", i, vec[1], i/3+1)
		}
	}
}

func TestOpenAIEmbeddingProviderTokenBudgetBatching(t *testing.T) {
	t.Run("small batch fits the token budget", func(t *testing.T) {
		fake := newRecordingEmbeddingsServer(t)
		provider := NewOpenAIEmbeddingProvider(fake.server.URL, "text-embedding-3-small", "")
		provider.MaxBatchInputs = 100
		provider.MaxBatchTokens = 20 // 4 inputs x 1 estimated token = 4 <= 20
		inputs := []string{"aaaa", "bbbb", "cccc", "dddd"}
		out, err := provider.Embed(context.Background(), inputs)
		if err != nil {
			t.Fatalf("Embed returned error: %v", err)
		}
		if len(fake.requests) != 1 {
			t.Fatalf("single small batch expected, got %d requests", len(fake.requests))
		}
		if len(out) != 4 {
			t.Fatalf("expected 4 vectors, got %d", len(out))
		}
	})
	t.Run("token budget splits oversized batches", func(t *testing.T) {
		fake := newRecordingEmbeddingsServer(t)
		provider := NewOpenAIEmbeddingProvider(fake.server.URL, "text-embedding-3-small", "")
		provider.MaxBatchInputs = 100
		provider.MaxBatchTokens = 10 // 16-char inputs estimate 4 tokens each
		inputs := distinctLengthInputs(3)
		inputs[0] = strings.Repeat("a", 16)
		inputs[1] = strings.Repeat("b", 16)
		inputs[2] = strings.Repeat("c", 16)
		if _, err := provider.Embed(context.Background(), inputs); err != nil {
			t.Fatalf("Embed returned error: %v", err)
		}
		// 3x4 tokens vs a 10-token budget -> batches of 2 + 1.
		if len(fake.requests) != 2 {
			t.Fatalf("token budget must split into 2 requests, got %d", len(fake.requests))
		}
		if len(fake.requests[0].Input) != 2 || len(fake.requests[1].Input) != 1 {
			t.Fatalf("split sizes wrong: %d/%d", len(fake.requests[0].Input), len(fake.requests[1].Input))
		}
	})
}

func TestOpenAIEmbeddingProviderTruncatesToModelTokenLimit(t *testing.T) {
	fake := newRecordingEmbeddingsServer(t)
	provider := NewOpenAIEmbeddingProvider(fake.server.URL, "text-embedding-3-small", "")

	overlimit := strings.Repeat("x", 8191*charsPerToken+1000)
	if _, err := provider.Embed(context.Background(), []string{overlimit}); err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	got := fake.requests[0].Input[0]
	wantLen := 8191 * charsPerToken
	if len([]rune(got)) != wantLen {
		t.Fatalf("input must truncate to %d runes (documented ~4 chars/token heuristic), got %d", wantLen, len([]rune(got)))
	}
	if !strings.HasPrefix(overlimit, got) {
		t.Fatal("truncation must keep the input prefix")
	}
	// Short input passes through untouched.
	if _, err := provider.Embed(context.Background(), []string{"tiny"}); err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if fake.requests[1].Input[0] != "tiny" {
		t.Fatalf("short input must not be truncated: %q", fake.requests[1].Input[0])
	}
}

func TestOpenAIEmbeddingProviderRetry(t *testing.T) {
	t.Run("429 retried then succeeds", func(t *testing.T) {
		var mu sync.Mutex
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			calls++
			first := calls == 1
			mu.Unlock()
			if first {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limited"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(providerEmbeddingsResponse{
				Data: []struct {
					Index     int       `json:"index"`
					Embedding []float32 `json:"embedding"`
				}{{Index: 0, Embedding: []float32{1, 2}}},
			})
		}))
		defer server.Close()

		var waits []time.Duration
		provider := NewOpenAIEmbeddingProvider(server.URL, "m", "k")
		provider.MaxAttempts = 3
		provider.RetryWait = 5 * time.Millisecond
		provider.sleep = func(d time.Duration) { waits = append(waits, d) }
		out, err := provider.Embed(context.Background(), []string{"x"})
		if err != nil {
			t.Fatalf("retry must recover a 429: %v", err)
		}
		if len(out) != 1 || out[0][0] != 1 {
			t.Fatalf("recovered vectors malformed: %v", out)
		}
		if len(waits) != 1 || waits[0] != 5*time.Millisecond {
			t.Fatalf("first backoff must equal the base wait, got %v", waits)
		}
	})
	t.Run("Retry-After header overrides backoff", func(t *testing.T) {
		var waits []time.Duration
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer server.Close()
		provider := NewOpenAIEmbeddingProvider(server.URL, "m", "k")
		provider.MaxAttempts = 2
		provider.RetryWait = time.Second
		provider.sleep = func(d time.Duration) { waits = append(waits, d) }
		_, err := provider.Embed(context.Background(), []string{"x"})
		if err == nil {
			t.Fatal("exhausted retries must fail")
		}
		if len(waits) != 1 || waits[0] != 7*time.Second {
			t.Fatalf("Retry-After (7s) must override the base backoff, got %v", waits)
		}
	})
	t.Run("5xx retried then fails after budget", func(t *testing.T) {
		var mu sync.Mutex
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			requests++
			mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer server.Close()
		provider := NewOpenAIEmbeddingProvider(server.URL, "m", "k")
		provider.MaxAttempts = 3
		provider.RetryWait = time.Millisecond
		provider.sleep = func(time.Duration) {}
		_, err := provider.Embed(context.Background(), []string{"x"})
		if err == nil || !strings.Contains(err.Error(), "502") {
			t.Fatalf("5xx must surface after retries, got %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if requests != 3 {
			t.Fatalf("expected 3 attempts, got %d", requests)
		}
	})
	t.Run("401 fails fast without retry", func(t *testing.T) {
		var mu sync.Mutex
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			requests++
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		provider := NewOpenAIEmbeddingProvider(server.URL, "m", "k")
		provider.MaxAttempts = 3
		provider.RetryWait = time.Millisecond
		provider.sleep = func(time.Duration) {}
		_, err := provider.Embed(context.Background(), []string{"x"})
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("non-retriable status must surface, got %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if requests != 1 {
			t.Fatalf("client errors must not retry, got %d requests", requests)
		}
	})
	t.Run("context cancellation aborts backoff", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()
		provider := NewOpenAIEmbeddingProvider(server.URL, "m", "k")
		provider.MaxAttempts = 3
		provider.RetryWait = time.Hour // would hang without ctx cancellation
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := provider.Embed(ctx, []string{"x"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled context must abort the retry loop, got %v", err)
		}
	})
}

func TestOpenAIEmbeddingProviderValidation(t *testing.T) {
	t.Run("empty inputs issue no request", func(t *testing.T) {
		if _, err := NewOpenAIEmbeddingProvider("http://127.0.0.1:1", "m", "k").Embed(context.Background(), nil); err != nil {
			t.Fatalf("empty batch must short-circuit, got %v", err)
		}
	})
	t.Run("missing config", func(t *testing.T) {
		if _, err := NewOpenAIEmbeddingProvider("", "m", "k").Embed(context.Background(), []string{"x"}); err == nil {
			t.Fatal("missing base URL must error")
		}
	})
	t.Run("empty model defaults to the default model", func(t *testing.T) {
		p := NewOpenAIEmbeddingProvider("https://api.example.com", "", "k")
		if p.Model != DefaultEmbeddingModel || p.Dimensions() != 1536 {
			t.Fatalf("model default not applied: %q/%d", p.Model, p.Dimensions())
		}
	})
	t.Run("count mismatch errors", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
		}))
		defer server.Close()
		if _, err := NewOpenAIEmbeddingProvider(server.URL, "m", "k").Embed(context.Background(), []string{"a", "b"}); err == nil {
			t.Fatal("count mismatch must error")
		}
	})
	t.Run("missing vector errors", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[]}]}`))
		}))
		defer server.Close()
		if _, err := NewOpenAIEmbeddingProvider(server.URL, "m", "k").Embed(context.Background(), []string{"a"}); err == nil {
			t.Fatal("an empty vector must error")
		}
	})
	t.Run("out-of-range index errors", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":9,"embedding":[1]}]}`))
		}))
		defer server.Close()
		if _, err := NewOpenAIEmbeddingProvider(server.URL, "m", "k").Embed(context.Background(), []string{"a"}); err == nil {
			t.Fatal("out-of-range index must error")
		}
	})
}

func TestEmbeddingModelTable(t *testing.T) {
	cases := map[string]int{
		"text-embedding-3-small": 1536,
		"text-embedding-3-large": 3072,
		"text-embedding-ada-002": 1536,
		"unknown-custom-model":   1536, // documented default for unknown models
	}
	for model, want := range cases {
		p := NewOpenAIEmbeddingProvider("https://api.example.com", model, "")
		if p.Dimensions() != want {
			t.Fatalf("%s: Dimensions() = %d, want %d", model, p.Dimensions(), want)
		}
		if spec := embeddingModelSpecFor(model); spec.maxInputTokens != 8191 {
			t.Fatalf("%s: maxInputTokens = %d, want 8191", model, spec.maxInputTokens)
		}
	}
	if DefaultEmbeddingModel != "text-embedding-3-small" {
		t.Fatalf("default model drifted: %q", DefaultEmbeddingModel)
	}
}

func TestEstimateTokensAndTruncation(t *testing.T) {
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("empty text estimates 0 tokens, got %d", got)
	}
	if got := estimateTokens("abcd"); got != 1 {
		t.Fatalf("4 chars ~ 1 token, got %d", got)
	}
	if got := estimateTokens("abcde"); got != 2 {
		t.Fatalf("5 chars ~ 2 tokens, got %d", got)
	}
	// Truncation is rune-safe: a multibyte string never splits mid-rune.
	multibyte := strings.Repeat("é", 100)
	got := truncateToTokenLimit(multibyte, 10)
	if got != strings.Repeat("é", 40) {
		t.Fatalf("rune-safe truncation failed: %d runes", len([]rune(got)))
	}
	if truncateToTokenLimit("short", 10) != "short" {
		t.Fatal("fitting input must pass through unchanged")
	}
}

func TestOfflineHashEmbedderAdapter(t *testing.T) {
	adapter := NewOfflineHashEmbedder()
	if adapter.Name() != "offline-hash-v1" {
		t.Fatalf("unexpected name %q", adapter.Name())
	}
	if adapter.Dimensions() != OfflineEmbeddingDim {
		t.Fatalf("adapter must report the offline dimension %d, got %d", OfflineEmbeddingDim, adapter.Dimensions())
	}
	ctx := context.Background()
	inputs := []string{"The customer prefers email invoices monthly", "second text"}
	vecs, err := adapter.Embed(ctx, inputs)
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vecs))
	}
	// Exact float64->float32 projection of the legacy hash embedder.
	legacy, _ := NewHashEmbedder().Embed(ctx, inputs)
	for i := range legacy {
		if len(vecs[i]) != len(legacy[i]) {
			t.Fatalf("vector %d dimension mismatch: %d vs %d", i, len(vecs[i]), len(legacy[i]))
		}
		for j := range legacy[i] {
			if math.Abs(float64(vecs[i][j])-legacy[i][j]) > 1e-6 {
				t.Fatalf("adapter must mirror the hash embedder at [%d][%d]: %v vs %v", i, j, vecs[i][j], legacy[i][j])
			}
		}
	}
	// Determinism.
	again, _ := adapter.Embed(ctx, inputs)
	for i := range again {
		for j := range again[i] {
			if again[i][j] != vecs[i][j] {
				t.Fatalf("offline adapter must be deterministic at [%d][%d]", i, j)
			}
		}
	}
	empty, err := adapter.Embed(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty batch must return an empty result, got %v/%v", empty, err)
	}
	// Interface compliance.
	var _ EmbeddingProvider = adapter
	var _ EmbeddingProvider = NewOpenAIEmbeddingProvider("https://api.example.com", "", "")
}

func TestEmbeddingProviderFromEnv(t *testing.T) {
	t.Setenv(EnvEmbeddingProvider, "")
	t.Setenv(EnvEmbeddingModel, "")
	t.Setenv(EnvEmbeddingBaseURL, "")
	t.Setenv(EnvEmbeddingAPIKey, "")

	t.Run("unset selects the offline default", func(t *testing.T) {
		p := EmbeddingProviderFromEnv()
		if _, ok := p.(OfflineHashEmbedder); !ok {
			t.Fatalf("default provider must be the offline hash embedder, got %T", p)
		}
	})
	t.Run("explicit offline", func(t *testing.T) {
		t.Setenv(EnvEmbeddingProvider, "offline")
		if _, ok := EmbeddingProviderFromEnv().(OfflineHashEmbedder); !ok {
			t.Fatal("AGENTOS_EMBEDDING_PROVIDER=offline must select the offline embedder")
		}
	})
	t.Run("unknown value fails safe to offline", func(t *testing.T) {
		t.Setenv(EnvEmbeddingProvider, "mystery")
		if _, ok := EmbeddingProviderFromEnv().(OfflineHashEmbedder); !ok {
			t.Fatal("unknown provider values must fail safe to offline")
		}
	})
	t.Run("openai with env config", func(t *testing.T) {
		t.Setenv(EnvEmbeddingProvider, "openai")
		t.Setenv(EnvEmbeddingModel, "text-embedding-3-large")
		t.Setenv(EnvEmbeddingBaseURL, "https://embed.example.com")
		t.Setenv(EnvEmbeddingAPIKey, "sk-env")
		p, ok := EmbeddingProviderFromEnv().(*OpenAIEmbeddingProvider)
		if !ok {
			t.Fatalf("AGENTOS_EMBEDDING_PROVIDER=openai must select the OpenAI provider, got %T", EmbeddingProviderFromEnv())
		}
		if p.Model != "text-embedding-3-large" || p.BaseURL != "https://embed.example.com" || p.APIKey != "sk-env" {
			t.Fatalf("env config not carried through: %+v", p)
		}
		if p.Dimensions() != 3072 {
			t.Fatalf("model table dims missing: %d", p.Dimensions())
		}
	})
	t.Run("openai defaults", func(t *testing.T) {
		t.Setenv(EnvEmbeddingProvider, "openai")
		p, ok := EmbeddingProviderFromEnv().(*OpenAIEmbeddingProvider)
		if !ok {
			t.Fatal("openai selection failed")
		}
		if p.Model != DefaultEmbeddingModel || p.BaseURL != DefaultEmbeddingProviderBaseURL {
			t.Fatalf("openai defaults not applied: %+v", p)
		}
	})
	t.Run("selection is case-insensitive", func(t *testing.T) {
		t.Setenv(EnvEmbeddingProvider, "OpenAI")
		if _, ok := EmbeddingProviderFromEnv().(*OpenAIEmbeddingProvider); !ok {
			t.Fatal("selection must be case-insensitive")
		}
	})
}

func TestFloatVectorConversions(t *testing.T) {
	f64 := []float64{0.5, -0.25, 1.0 / 3.0}
	f32 := float64ToFloat32(f64)
	if len(f32) != 3 {
		t.Fatalf("length mismatch: %d", len(f32))
	}
	back := float32ToFloat64(f32)
	for i := range f64 {
		// Widening is exact for the float32 values; the round trip only loses
		// float32 precision (relative error ~1e-7).
		if math.Abs(back[i]-f64[i]) > 1e-6 {
			t.Fatalf("conversion round trip failed at %d: %v vs %v", i, back[i], f64[i])
		}
	}
	if float64ToFloat32(nil) != nil || float32ToFloat64(nil) != nil {
		t.Fatal("nil vectors must stay nil")
	}
	// NaN never leaks into vectors.
	nan := []float64{math.NaN(), 1}
	if got := float64ToFloat32(nan); got[0] != 0 || got[1] != 1 {
		t.Fatalf("NaN must map to 0, got %v", got)
	}
}

func TestBatchEmbedInputsShape(t *testing.T) {
	texts := []string{"a", "b", "c", "d", "e"}
	batches := batchEmbedInputs(texts, 2, 1_000_000)
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches of 2, got %d", len(batches))
	}
	if batches[0].offset != 0 || batches[1].offset != 2 || batches[2].offset != 4 {
		t.Fatalf("batch offsets wrong: %+v", batches)
	}
	// A single input is always admitted even when it exceeds every cap.
	solo := batchEmbedInputs([]string{"giant"}, 1, 0)
	if len(solo) != 1 || len(solo[0].texts) != 1 {
		t.Fatalf("oversized single input must still batch: %+v", solo)
	}
	// Zero inputs -> no batches.
	if got := batchEmbedInputs(nil, 2, 2); len(got) != 0 {
		t.Fatalf("nil input must produce no batches, got %v", got)
	}
	// Offsets always line up with the input positions.
	cursor := 0
	for _, b := range batchEmbedInputs(texts, 2, 1_000_000) {
		if b.offset != cursor {
			t.Fatalf("offset %d, want %d", b.offset, cursor)
		}
		cursor += len(b.texts)
	}
	if cursor != len(texts) {
		t.Fatalf("batches must cover every input, covered %d", cursor)
	}
}

func TestProviderEmbedderAdapterForLegacyService(t *testing.T) {
	// The provider-backed service must behave like the legacy embedder path:
	// deterministic search over the float32->float64 bridge.
	provider := NewOfflineHashEmbedder()
	svc := NewServiceWithEmbeddingProvider(provider)
	org := "org-1"
	if _, _, err := svc.IngestDocument(context.Background(), org, IngestRequest{
		Title:   "Billing FAQ",
		Content: "Customers prefer email invoices. Billing runs monthly on the first.",
	}); err != nil {
		t.Fatalf("IngestDocument returned error: %v", err)
	}
	results, err := svc.Search(context.Background(), org, "email invoices", 5)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("provider-backed service must retrieve the ingested chunk")
	}
	if results[0].Score <= 0 {
		t.Fatalf("top hit must score positively, got %v", results[0].Score)
	}
	// The bridge preserves the legacy space to float32 precision (the
	// provider space IS float32): widened values match the float64 hash
	// vectors within the float32 epsilon.
	legacyVecs, _ := NewHashEmbedder().Embed(context.Background(), []string{"email invoices"})
	bridge, _ := providerEmbedder{provider: provider}.Embed(context.Background(), []string{"email invoices"})
	for i := range legacyVecs[0] {
		if math.Abs(bridge[0][i]-legacyVecs[0][i]) > 1e-6 {
			t.Fatalf("float32 bridge drifted beyond float32 precision at %d: %v vs %v", i, bridge[0][i], legacyVecs[0][i])
		}
	}
	// Nil provider falls back to environment selection (offline default).
	nilSvc := NewServiceWithEmbeddingProvider(nil)
	if _, ok := nilSvc.embedder.(HashEmbedder); !ok {
		t.Fatalf("nil provider must fall back to the offline default, got %T", nilSvc.embedder)
	}
	// Interface compliance through the legacy seam.
	var _ Embedder = providerEmbedder{provider: provider}
}
