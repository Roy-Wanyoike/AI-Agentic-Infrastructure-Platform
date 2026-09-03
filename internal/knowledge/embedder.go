package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// Embedding env knobs (contract track 3-d). When none of them is set the
// service runs in OFFLINE mode with the deterministic hashing embedder, so
// search works in tests and dev without external infrastructure.
const (
	// EnvEmbeddingBaseURL points at an OpenAI-compatible API root
	// (e.g. https://api.openai.com/v1). The client POSTs {base}/embeddings.
	EnvEmbeddingBaseURL = "AGENTOS_EMBEDDING_BASE_URL"
	// EnvEmbeddingModel selects the embeddings model (e.g. text-embedding-3-small).
	EnvEmbeddingModel = "AGENTOS_EMBEDDING_MODEL"
	// EnvEmbeddingAPIKey carries the bearer token for the embeddings API.
	EnvEmbeddingAPIKey = "AGENTOS_EMBEDDING_API_KEY"
)

// DefaultEmbeddingBaseURL is used when a model is configured without an
// explicit base URL (the OpenAI default).
const DefaultEmbeddingBaseURL = "https://api.openai.com/v1"

// OfflineEmbeddingDim matches internal/memory.EmbeddingDim so both packages'
// offline spaces stay interchangeable (documented deviation: pseudo-
// embeddings, NOT compatible with remote model vectors).
const OfflineEmbeddingDim = 256

// Embedder turns a batch of texts into vectors (batch mirrors the
// OpenAI-compatible /embeddings shape and keeps one HTTP round trip per
// document ingest).
type Embedder interface {
	// Name identifies the embedder in logs/wiring ("offline-hash-v1", the
	// model name for remote clients).
	Name() string
	// Embed returns one vector per input, in input order.
	Embed(ctx context.Context, inputs []string) ([][]float64, error)
}

// NewEmbedderFromEnv returns an OpenAI-compatible HTTP embedder when
// AGENTOS_EMBEDDING_MODEL (or AGENTOS_EMBEDDING_BASE_URL) is configured, and
// the deterministic offline hash embedder otherwise. This function is the
// single wiring point documented in docs/wiring/knowledge.md.
func NewEmbedderFromEnv() Embedder {
	model := strings.TrimSpace(os.Getenv(EnvEmbeddingModel))
	baseURL := strings.TrimSpace(os.Getenv(EnvEmbeddingBaseURL))
	if model == "" && baseURL == "" {
		return NewHashEmbedder()
	}
	if model == "" {
		model = "text-embedding-3-small" // OpenAI-compatible default
	}
	if baseURL == "" {
		baseURL = DefaultEmbeddingBaseURL
	}
	return NewOpenAIEmbedder(baseURL, model, os.Getenv(EnvEmbeddingAPIKey))
}

// ---------------------------------------------------------------------------
// OpenAI-compatible client (minimal, owned by this package — internal/models
// is deliberately untouched per the contract).
// ---------------------------------------------------------------------------

// OpenAIEmbedder is a minimal client for the OpenAI-compatible
// POST {baseURL}/embeddings endpoint.
type OpenAIEmbedder struct {
	BaseURL string
	Model   string
	APIKey  string
	HTTP    *http.Client
}

// NewOpenAIEmbedder returns a client for an OpenAI-compatible embeddings API.
func NewOpenAIEmbedder(baseURL, model, apiKey string) *OpenAIEmbedder {
	return &OpenAIEmbedder{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Model:   strings.TrimSpace(model),
		APIKey:  strings.TrimSpace(apiKey),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Name reports the embeddings model.
func (e *OpenAIEmbedder) Name() string { return e.Model }

type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// Embed POSTs the batch and returns vectors ordered by the response's index
// field. Errors surface the HTTP status so misconfiguration is visible.
func (e *OpenAIEmbedder) Embed(ctx context.Context, inputs []string) ([][]float64, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if e.BaseURL == "" || e.Model == "" {
		return nil, errors.New("knowledge: embeddings base URL and model are required")
	}
	body, err := json.Marshal(embeddingsRequest{Model: e.Model, Input: inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	httpClient := e.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("knowledge: embeddings request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("knowledge: embeddings API returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var parsed embeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("knowledge: embeddings API returned invalid JSON: %w", err)
	}
	if len(parsed.Data) != len(inputs) {
		return nil, fmt.Errorf("knowledge: embeddings API returned %d vectors for %d inputs", len(parsed.Data), len(inputs))
	}
	out := make([][]float64, len(inputs))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("knowledge: embeddings API returned out-of-range index %d", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	for i, vec := range out {
		if vec == nil {
			return nil, fmt.Errorf("knowledge: embeddings API returned no vector for input %d", i)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Offline mode: deterministic hashing pseudo-embeddings (documented deviation
// in docs/wiring/knowledge.md).
// ---------------------------------------------------------------------------

// HashEmbedder produces deterministic pseudo-embeddings without any network
// dependency: tokens are hashed into a fixed-size bag-of-words vector and
// L2-normalized, so identical text yields identical vectors and cosine
// similarity behaves like a soft token-overlap measure. The scheme is
// intentionally identical to internal/memory's offline embedder.
type HashEmbedder struct{}

// NewHashEmbedder returns the offline embedder.
func NewHashEmbedder() HashEmbedder { return HashEmbedder{} }

// Name identifies the offline embedder.
func (HashEmbedder) Name() string { return "offline-hash-v1" }

// Embed computes pseudo-embeddings for the batch. It never fails.
func (HashEmbedder) Embed(_ context.Context, inputs []string) ([][]float64, error) {
	out := make([][]float64, len(inputs))
	for i, text := range inputs {
		vec := make([]float64, OfflineEmbeddingDim)
		for _, token := range tokenize(text) {
			sum := sha256.Sum256([]byte(token))
			// Two independent buckets per token soften the collisions of a
			// 256-dim space while staying fully deterministic.
			b1 := int(binary.BigEndian.Uint32(sum[0:4]) % OfflineEmbeddingDim)
			b2 := int(binary.BigEndian.Uint32(sum[4:8]) % OfflineEmbeddingDim)
			vec[b1] += 1
			vec[b2] += 0.5
		}
		norm := 0.0
		for _, v := range vec {
			norm += v * v
		}
		if norm > 0 {
			norm = math.Sqrt(norm)
			for j := range vec {
				vec[j] /= norm
			}
		}
		out[i] = vec
	}
	return out, nil
}
