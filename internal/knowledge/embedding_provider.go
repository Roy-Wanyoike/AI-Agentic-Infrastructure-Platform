package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// EmbeddingProvider env knob (issue #113). AGENTOS_EMBEDDING_PROVIDER selects
// the embeddings backend: "openai" uses the OpenAI-compatible HTTP provider,
// "offline" (or unset, or any unknown value — fail-safe) keeps the
// deterministic offline hash embedder as the DEFAULT dev fallback.
const (
	// EnvEmbeddingProvider selects the provider: openai|offline (default offline).
	EnvEmbeddingProvider = "AGENTOS_EMBEDDING_PROVIDER"
)

// EmbeddingProvider is the provider seam between AgentOS and a real
// embedding backend (issue #113). It widens the legacy float64 Embedder with
// explicit dimensionality (required to target the pgvector column) and stays
// batch-shaped so one ingest round trip covers a whole document.
//
// Implementations in this package:
//   - OpenAIEmbeddingProvider — OpenAI-compatible /v1/embeddings client.
//   - OfflineHashEmbedder — adapter over the deterministic offline hash
//     embedder (the default when AGENTOS_EMBEDDING_PROVIDER is "offline" or
//     unset).
type EmbeddingProvider interface {
	// Embed returns one vector per input, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimensions reports the vector dimensionality of this provider's space
	// (the model table value for remote providers, OfflineEmbeddingDim for
	// the offline adapter).
	Dimensions() int
	// Name identifies the provider in logs/wiring.
	Name() string
}

// Provider selection values for AGENTOS_EMBEDDING_PROVIDER.
const (
	// ProviderOpenAI selects the OpenAI-compatible HTTP provider.
	ProviderOpenAI = "openai"
	// ProviderOffline selects (or confirms) the offline hash embedder.
	ProviderOffline = "offline"
)

// DefaultEmbeddingModel is the default embeddings model (1536 dimensions,
// matching the knowledge_chunks.embedding_vec column created by migration
// 033 and the VectorColumnDim constant).
const DefaultEmbeddingModel = "text-embedding-3-small"

// DefaultEmbeddingProviderBaseURL is the OpenAI-compatible API root used when
// AGENTOS_EMBEDDING_BASE_URL is not set. The client POSTs {base}/v1/embeddings
// (a base URL that already ends in /v1 is honored as {base}/embeddings, so
// both the with-/v1 and without-/v1 conventions work).
const DefaultEmbeddingProviderBaseURL = "https://api.openai.com"

// embeddingModelSpec pins the per-model facts the provider needs.
type embeddingModelSpec struct {
	dimensions int
	// maxInputTokens is the documented per-input token limit of the model.
	maxInputTokens int
}

// embeddingModelTable is the model table backing Dimensions() and input
// truncation (issue #113: "dimensions() from model table"). Unknown models
// fall back to defaultEmbeddingModelSpec — the default model's shape — so a
// brand-new OpenAI-compatible model keeps working without a code change
// (dimensions can be narrowed later by adding a table row).
var embeddingModelTable = map[string]embeddingModelSpec{
	"text-embedding-3-small": {dimensions: 1536, maxInputTokens: 8191},
	"text-embedding-3-large": {dimensions: 3072, maxInputTokens: 8191},
	"text-embedding-ada-002": {dimensions: 1536, maxInputTokens: 8191},
}

// defaultEmbeddingModelSpec is used for models missing from the table.
var defaultEmbeddingModelSpec = embeddingModelSpec{dimensions: 1536, maxInputTokens: 8191}

// embeddingModelSpecFor returns the table row for a model (or the default).
func embeddingModelSpecFor(model string) embeddingModelSpec {
	if spec, ok := embeddingModelTable[model]; ok {
		return spec
	}
	return defaultEmbeddingModelSpec
}

// charsPerToken is the documented truncation heuristic: AgentOS does not
// ship a tokenizer, so tokens are ESTIMATED at ~4 characters each (a
// deliberate over-approximation for English prose; long non-ASCII inputs may
// estimate low, which is safe — the model limit is only approached, never
// relied upon for correctness). Input truncation trims to
// maxInputTokens*charsPerToken runes.
const charsPerToken = 4

// estimateTokens estimates the token count of a text (~4 chars/token).
func estimateTokens(text string) int {
	return (len([]rune(text)) + charsPerToken - 1) / charsPerToken
}

// truncateToTokenLimit trims an input to at most maxTokens estimated tokens
// without splitting a rune. It returns the input unchanged when it already
// fits.
func truncateToTokenLimit(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return text
	}
	runes := []rune(text)
	limit := maxTokens * charsPerToken
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// ---------------------------------------------------------------------------
// OpenAI-compatible provider (issue #113). Owns batching (max inputs per
// request + estimated total tokens per request), retry with backoff on
// 429/5xx (honoring Retry-After), and per-input truncation to the model's
// documented token limit.
// ---------------------------------------------------------------------------

// Batching/retry defaults for OpenAIEmbeddingProvider.
const (
	// DefaultMaxBatchInputs is the OpenAI embeddings batch limit (~100 inputs
	// per request).
	DefaultMaxBatchInputs = 100
	// DefaultMaxBatchTokens is the estimated total token budget per request
	// (OpenAI documents a 300k summed-token limit for /v1/embeddings; the
	// estimate uses the ~4 chars/token heuristic).
	DefaultMaxBatchTokens = 300_000
	// DefaultMaxAttempts is the retry budget per batch on 429/5xx.
	DefaultMaxAttempts = 3
	// DefaultRetryWait is the base exponential backoff (250ms, 500ms, ...).
	DefaultRetryWait = 250 * time.Millisecond
	// maxRetryWait caps exponential growth and Retry-After values.
	maxRetryWait = 30 * time.Second
)

// OpenAIEmbeddingProvider is a client for the OpenAI-compatible POST
// {baseURL}/v1/embeddings endpoint (issue #113). The zero value is not
// usable; use NewOpenAIEmbeddingProvider.
type OpenAIEmbeddingProvider struct {
	// BaseURL is the API root (e.g. https://api.openai.com or a self-hosted
	// OpenAI-compatible service). A trailing /v1 is tolerated.
	BaseURL string
	// Model is the embeddings model id (Name() reports it).
	Model string
	// APIKey is sent as the bearer token when non-empty.
	APIKey string
	// HTTP is the client used for requests (default: 30s timeout client).
	HTTP *http.Client

	// MaxBatchInputs caps the number of inputs per HTTP request.
	MaxBatchInputs int
	// MaxBatchTokens caps the ESTIMATED total tokens per HTTP request.
	MaxBatchTokens int
	// MaxAttempts is the retry budget per batch for 429/5xx responses.
	MaxAttempts int
	// RetryWait is the base exponential backoff duration.
	RetryWait time.Duration

	// dim is resolved from the model table at construction time.
	dim int

	// sleep is a test seam replacing the backoff timer (nil in production).
	sleep func(time.Duration)
}

// Compile-time proof the OpenAI provider satisfies the seam.
var _ EmbeddingProvider = (*OpenAIEmbeddingProvider)(nil)

// NewOpenAIEmbeddingProvider returns a provider for an OpenAI-compatible
// embeddings API. An empty model falls back to DefaultEmbeddingModel.
func NewOpenAIEmbeddingProvider(baseURL, model, apiKey string) *OpenAIEmbeddingProvider {
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultEmbeddingModel
	}
	return &OpenAIEmbeddingProvider{
		BaseURL:        strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Model:          model,
		APIKey:         strings.TrimSpace(apiKey),
		HTTP:           &http.Client{Timeout: 30 * time.Second},
		MaxBatchInputs: DefaultMaxBatchInputs,
		MaxBatchTokens: DefaultMaxBatchTokens,
		MaxAttempts:    DefaultMaxAttempts,
		RetryWait:      DefaultRetryWait,
		dim:            embeddingModelSpecFor(model).dimensions,
	}
}

// Name reports the embeddings model.
func (p *OpenAIEmbeddingProvider) Name() string { return p.Model }

// Dimensions reports the model table dimensionality of the configured model.
func (p *OpenAIEmbeddingProvider) Dimensions() int { return p.dim }

// embeddingsHTTPError carries the API failure details through the retry
// decision (retriable: 429 + 5xx; Retry-After honored when present).
type embeddingsHTTPError struct {
	status     int
	retryAfter time.Duration
	body       string
}

func (e *embeddingsHTTPError) Error() string {
	return fmt.Sprintf("knowledge: embeddings API returned status %d: %s", e.status, e.body)
}

// retriable reports whether the failure is worth another attempt (rate
// limiting and server-side/transient failures only — client errors like 401
// fail fast).
func (e *embeddingsHTTPError) retriable() bool {
	return e.status == http.StatusTooManyRequests || e.status >= 500
}

type providerEmbeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type providerEmbeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed embeds the batch: inputs are truncated to the model's token limit,
// split into request-sized batches, and each batch is POSTed with retry on
// 429/5xx. Vectors are returned in input order regardless of the response
// ordering.
func (p *OpenAIEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if p == nil || p.BaseURL == "" || p.Model == "" {
		return nil, errors.New("knowledge: embeddings base URL and model are required")
	}
	spec := embeddingModelSpecFor(p.Model)
	truncated := make([]string, len(texts))
	for i, text := range texts {
		truncated[i] = truncateToTokenLimit(text, spec.maxInputTokens)
	}

	out := make([][]float32, len(truncated))
	for _, batch := range batchEmbedInputs(truncated, p.maxBatchInputs(), p.maxBatchTokens()) {
		vecs, err := p.embedBatchWithRetry(ctx, batch)
		if err != nil {
			return nil, err
		}
		// Batch positions are absolute offsets, so results interleave in
		// input order even across multiple requests.
		for i, vec := range vecs {
			out[batch.offset+i] = vec
		}
	}
	return out, nil
}

func (p *OpenAIEmbeddingProvider) maxBatchInputs() int {
	if p.MaxBatchInputs > 0 {
		return p.MaxBatchInputs
	}
	return DefaultMaxBatchInputs
}

func (p *OpenAIEmbeddingProvider) maxBatchTokens() int {
	if p.MaxBatchTokens > 0 {
		return p.MaxBatchTokens
	}
	return DefaultMaxBatchTokens
}

// embedBatchInput is one request-sized slice of the input batch.
type embedBatchInput struct {
	offset int      // absolute index of texts[0] inside the full input
	texts  []string // already truncated to the model token limit
}

// batchEmbedInputs splits inputs into request-sized batches bounded by the
// max-inputs-per-request and estimated-tokens-per-request caps. At least one
// input is always admitted per batch (a single oversized input is sent
// alone; it was already truncated to the per-input token limit).
func batchEmbedInputs(texts []string, maxInputs, maxTokens int) []embedBatchInput {
	batches := make([]embedBatchInput, 0, (len(texts)+maxInputs-1)/maxInputs)
	cur := embedBatchInput{offset: 0}
	curTokens := 0
	for i, text := range texts {
		est := estimateTokens(text)
		full := len(cur.texts) >= maxInputs || (len(cur.texts) > 0 && curTokens+est > maxTokens)
		if full {
			batches = append(batches, cur)
			cur = embedBatchInput{offset: i}
			curTokens = 0
		}
		cur.texts = append(cur.texts, text)
		curTokens += est
	}
	if len(cur.texts) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

// embedBatchWithRetry POSTs one batch, retrying 429/5xx with exponential
// backoff (Retry-After wins when the server sends one). Context
// cancellation aborts the wait immediately.
func (p *OpenAIEmbeddingProvider) embedBatchWithRetry(ctx context.Context, batch embedBatchInput) ([][]float32, error) {
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	wait := p.RetryWait
	if wait <= 0 {
		wait = DefaultRetryWait
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		vecs, err := p.embedBatch(ctx, batch.texts)
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		var httpErr *embeddingsHTTPError
		if !errors.As(err, &httpErr) || !httpErr.retriable() || attempt == maxAttempts-1 {
			return nil, err
		}
		delay := wait << attempt // exponential: 1x, 2x, 4x ...
		if delay > maxRetryWait {
			delay = maxRetryWait
		}
		if httpErr.retryAfter > 0 && httpErr.retryAfter < maxRetryWait {
			delay = httpErr.retryAfter
		}
		if err := sleepCtx(ctx, p.sleep, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// sleepCtx waits d, honoring context cancellation; the sleep seam (tests)
// replaces the timer entirely.
func sleepCtx(ctx context.Context, sleep func(time.Duration), d time.Duration) error {
	if sleep != nil {
		sleep(d)
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// embedBatch performs one POST for the batch and returns vectors in INPUT
// order (the response's index field is validated and applied).
func (p *OpenAIEmbeddingProvider) embedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(providerEmbeddingsRequest{Model: p.Model, Input: inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	httpClient := p.HTTP
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
		httpErr := &embeddingsHTTPError{status: resp.StatusCode, body: strings.TrimSpace(string(snippet))}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, convErr := strconv.Atoi(strings.TrimSpace(ra)); convErr == nil && secs > 0 {
				httpErr.retryAfter = time.Duration(secs) * time.Second
			}
		}
		return nil, httpErr
	}
	var parsed providerEmbeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("knowledge: embeddings API returned invalid JSON: %w", err)
	}
	if len(parsed.Data) != len(inputs) {
		return nil, fmt.Errorf("knowledge: embeddings API returned %d vectors for %d inputs", len(parsed.Data), len(inputs))
	}
	out := make([][]float32, len(inputs))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("knowledge: embeddings API returned out-of-range index %d", d.Index)
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("knowledge: embeddings API returned no vector for input %d", d.Index)
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

// endpoint builds the POST URL: {base}/v1/embeddings, tolerating base URLs
// that already carry the /v1 prefix (both conventions exist in the wild).
func (p *OpenAIEmbeddingProvider) endpoint() string {
	base := strings.TrimRight(p.BaseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/embeddings"
	}
	return base + "/v1/embeddings"
}

// ---------------------------------------------------------------------------
// Offline adapter: the deterministic hash embedder behind the provider seam.
// ---------------------------------------------------------------------------

// OfflineHashEmbedder adapts the legacy float64 HashEmbedder to the
// EmbeddingProvider seam so the offline default and remote providers are
// interchangeable downstream (issue #113: "the current hash embedder must
// remain default when AGENTOS_EMBEDDING_PROVIDER=offline or unset").
type OfflineHashEmbedder struct {
	inner HashEmbedder
}

// Compile-time proof the offline adapter satisfies the seam.
var _ EmbeddingProvider = OfflineHashEmbedder{}

// NewOfflineHashEmbedder returns the offline provider adapter.
func NewOfflineHashEmbedder() OfflineHashEmbedder {
	return OfflineHashEmbedder{inner: NewHashEmbedder()}
}

// Name identifies the offline embedder.
func (OfflineHashEmbedder) Name() string { return "offline-hash-v1" }

// Dimensions reports the offline pseudo-embedding dimensionality.
func (OfflineHashEmbedder) Dimensions() int { return OfflineEmbeddingDim }

// Embed computes the deterministic pseudo-embeddings (float32 projection of
// the float64 hash space). It never fails.
func (e OfflineHashEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := e.inner.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(vecs))
	for i, vec := range vecs {
		out[i] = float64ToFloat32(vec)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Wiring (httpx.RedisClientFromEnv style): selection from the environment.
// ---------------------------------------------------------------------------

// EmbeddingProviderFromEnv returns the embeddings provider selected by
// AGENTOS_EMBEDDING_PROVIDER:
//
//   - "openai": OpenAIEmbeddingProvider configured from
//     AGENTOS_EMBEDDING_BASE_URL / AGENTOS_EMBEDDING_MODEL /
//     AGENTOS_EMBEDDING_API_KEY (defaults: https://api.openai.com,
//     text-embedding-3-small).
//   - "offline", unset, or an unknown value (warned, fail-safe): the
//     deterministic OfflineHashEmbedder — the zero-infrastructure default.
func EmbeddingProviderFromEnv() EmbeddingProvider {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEmbeddingProvider))) {
	case ProviderOpenAI:
		model := strings.TrimSpace(os.Getenv(EnvEmbeddingModel))
		if model == "" {
			model = DefaultEmbeddingModel
		}
		baseURL := strings.TrimSpace(os.Getenv(EnvEmbeddingBaseURL))
		if baseURL == "" {
			baseURL = DefaultEmbeddingProviderBaseURL
		}
		return NewOpenAIEmbeddingProvider(baseURL, model, os.Getenv(EnvEmbeddingAPIKey))
	case ProviderOffline, "":
		return NewOfflineHashEmbedder()
	default:
		slog.Warn("unknown AGENTOS_EMBEDDING_PROVIDER; falling back to the offline hash embedder",
			"provider", os.Getenv(EnvEmbeddingProvider))
		return NewOfflineHashEmbedder()
	}
}

// ---------------------------------------------------------------------------
// Bridge to the legacy float64 Embedder used by Service (batch embedding
// path) so provider-backed services reuse the existing search/scoring code.
// ---------------------------------------------------------------------------

// providerEmbedder adapts an EmbeddingProvider (float32, issue #113 seam) to
// the float64 Embedder interface consumed by Service.
type providerEmbedder struct {
	provider EmbeddingProvider
}

// Compile-time proof the adapter still satisfies the legacy interface.
var _ Embedder = providerEmbedder{}

// Name forwards the provider identity.
func (p providerEmbedder) Name() string { return p.provider.Name() }

// Embed converts the provider's float32 vectors into the float64 space used
// by the legacy scoring paths (exact widening; no precision loss).
func (p providerEmbedder) Embed(ctx context.Context, inputs []string) ([][]float64, error) {
	vecs, err := p.provider.Embed(ctx, inputs)
	if err != nil {
		return nil, err
	}
	out := make([][]float64, len(vecs))
	for i, vec := range vecs {
		out[i] = float32ToFloat64(vec)
	}
	return out, nil
}

// float64ToFloat32 narrows a float64 vector to the provider space (offline
// adapter direction).
func float64ToFloat32(vec []float64) []float32 {
	if vec == nil {
		return nil
	}
	out := make([]float32, len(vec))
	for i, v := range vec {
		if math.IsNaN(float64(v)) {
			continue
		}
		out[i] = float32(v)
	}
	return out
}

// float32ToFloat64 widens a float32 vector into the legacy space (provider
// adapter direction).
func float32ToFloat64(vec []float32) []float64 {
	if vec == nil {
		return nil
	}
	out := make([]float64, len(vec))
	for i, v := range vec {
		if math.IsNaN(float64(v)) {
			continue
		}
		out[i] = float64(v)
	}
	return out
}
