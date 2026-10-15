package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Pipeline defaults.
const (
	// DefaultPipelineBatchSize mirrors the provider's per-request input cap
	// (~100 inputs, OpenAI embeddings limit): one pipeline batch = one
	// provider request in the common case, which bounds the blast radius of
	// a failed batch and keeps the progress callback granular.
	DefaultPipelineBatchSize = 100
	// DefaultPipelineMaxAttempts is the per-batch retry budget of the
	// pipeline (in addition to any retries inside the provider itself).
	DefaultPipelineMaxAttempts = 3
	// DefaultPipelineRetryWait is the base exponential backoff per batch.
	DefaultPipelineRetryWait = 200 * time.Millisecond
)

// PipelineStage identifies the phase reported through the progress callback.
type PipelineStage string

// Progress stages emitted by EmbeddingPipeline.Ingest.
const (
	// StageChunked fires once after the content has been chunked.
	StageChunked PipelineStage = "chunked"
	// StageEmbedded fires after each batch is embedded (done = vectors
	// completed so far, total = chunk count).
	StageEmbedded PipelineStage = "embedded"
	// StageStored fires once after the idempotent upsert succeeded.
	StageStored PipelineStage = "stored"
)

// ProgressFunc is the pipeline progress callback (issue #113). It is invoked
// synchronously; implementations must not block for long.
type ProgressFunc func(stage PipelineStage, done, total int)

// PipelineDocument is the caller-supplied input of one pipeline ingest. A
// stable DocumentID makes re-ingests idempotent: the same id re-runs the
// chunk->embed->upsert pipeline and converges (chunks upserted on
// (organization_id, document_id, ordinal), stale ordinals pruned). An empty
// DocumentID generates a fresh UUID (new document).
type PipelineDocument struct {
	// DocumentID is the stable document key for idempotent re-ingest
	// (optional; empty generates a UUID).
	DocumentID string
	// OrganizationID is the tenant scope (required).
	OrganizationID string
	Title          string
	Source         string
	Metadata       map[string]any
	// Content is the full document text; it is chunked by ChunkText.
	Content string
}

// EmbeddingPipeline is the batch embedding ingest pipeline (issue #113):
// chunk (ChunkText) -> batch (DefaultPipelineBatchSize) -> embed
// (EmbeddingProvider, retried per batch) -> idempotent upsert into a
// VectorStore with embeddings. It is independent of Service and can be wired
// to any VectorStore (pgvector-backed in production, fakes in tests).
type EmbeddingPipeline struct {
	store       VectorStore
	provider    EmbeddingProvider
	batchSize   int
	maxAttempts int
	retryWait   time.Duration
	onProgress  ProgressFunc
	// sleep is a test seam replacing the retry backoff timer.
	sleep func(time.Duration)
}

// NewEmbeddingPipeline returns a pipeline with production defaults
// (batch 100, 3 attempts per batch, 200ms base backoff).
func NewEmbeddingPipeline(store VectorStore, provider EmbeddingProvider) *EmbeddingPipeline {
	return &EmbeddingPipeline{
		store:       store,
		provider:    provider,
		batchSize:   DefaultPipelineBatchSize,
		maxAttempts: DefaultPipelineMaxAttempts,
		retryWait:   DefaultPipelineRetryWait,
	}
}

// WithBatchSize overrides the batch size (clamped to >= 1).
func (p *EmbeddingPipeline) WithBatchSize(n int) *EmbeddingPipeline {
	if n > 0 {
		p.batchSize = n
	}
	return p
}

// WithMaxAttempts overrides the per-batch retry budget (clamped to >= 1).
func (p *EmbeddingPipeline) WithMaxAttempts(n int) *EmbeddingPipeline {
	if n > 0 {
		p.maxAttempts = n
	}
	return p
}

// WithRetryWait overrides the base backoff between batch attempts.
func (p *EmbeddingPipeline) WithRetryWait(d time.Duration) *EmbeddingPipeline {
	if d > 0 {
		p.retryWait = d
	}
	return p
}

// WithProgress installs the progress callback (nil = no reporting).
func (p *EmbeddingPipeline) WithProgress(fn ProgressFunc) *EmbeddingPipeline {
	p.onProgress = fn
	return p
}

// Ingest runs the pipeline for one document:
//
//	validate -> ChunkText -> batches -> provider.Embed (retry per batch)
//	-> idempotent upsert (document + chunks with embeddings) -> return.
//
// The returned chunks carry both vector forms: Vector (float32, provider
// space -> embedding_vec pgvector column) and Embedding (float64 mirror ->
// JSONB column, keeping the classic lexical/cosine fallback search
// functional). A provider failure after exhausting retries aborts the ingest
// with the wrapped error and NOTHING is stored (the upsert is atomic), so a
// later re-run converges.
func (p *EmbeddingPipeline) Ingest(ctx context.Context, doc PipelineDocument) (*Document, []Chunk, error) {
	if p == nil || p.store == nil {
		return nil, nil, errors.New("knowledge: pipeline store is required")
	}
	if p.provider == nil {
		return nil, nil, errors.New("knowledge: pipeline embedding provider is required")
	}
	if strings.TrimSpace(doc.OrganizationID) == "" {
		return nil, nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(doc.Title) == "" {
		return nil, nil, errors.New("title is required")
	}
	if strings.TrimSpace(doc.Content) == "" {
		return nil, nil, errors.New("content is required")
	}

	texts := ChunkText(doc.Content)
	if len(texts) == 0 {
		return nil, nil, errors.New("content is required")
	}
	if p.onProgress != nil {
		p.onProgress(StageChunked, len(texts), len(texts))
	}

	documentID := strings.TrimSpace(doc.DocumentID)
	if documentID == "" {
		documentID = uuid.NewString()
	}
	now := time.Now().UTC()
	document := &Document{
		ID:             documentID,
		OrganizationID: doc.OrganizationID,
		Title:          strings.TrimSpace(doc.Title),
		Source:         strings.TrimSpace(doc.Source),
		Metadata:       doc.Metadata,
		ChunkCount:     len(texts),
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	vectors := make([][]float32, len(texts))
	done := 0
	for start := 0; start < len(texts); start += p.batchSize {
		end := start + p.batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]
		vecs, err := p.embedBatchWithRetry(ctx, batch)
		if err != nil {
			return nil, nil, err
		}
		copy(vectors[start:end], vecs)
		done = end
		if p.onProgress != nil {
			p.onProgress(StageEmbedded, done, len(texts))
		}
	}

	chunks := make([]Chunk, len(texts))
	for i, text := range texts {
		chunks[i] = Chunk{
			ID:             uuid.NewString(),
			OrganizationID: doc.OrganizationID,
			DocumentID:     documentID,
			Ordinal:        i,
			Content:        text,
			Vector:         vectors[i],
			Embedding:      float32ToFloat64(vectors[i]),
			CreatedAt:      now,
		}
	}

	if err := p.store.UpsertDocumentWithVectors(ctx, document, chunks); err != nil {
		return nil, nil, err
	}
	if p.onProgress != nil {
		p.onProgress(StageStored, len(chunks), len(chunks))
	}
	return document, chunks, nil
}

// embedBatchWithRetry embeds one batch with per-batch retry (exponential
// backoff, context-aware). Unlike the OpenAI provider's internal retry
// (transport-level 429/5xx), this loop retries ANY error from the provider —
// including fakes and alternate backends — so the pipeline is robust
// independent of the provider implementation.
func (p *EmbeddingPipeline) embedBatchWithRetry(ctx context.Context, batch []string) ([][]float32, error) {
	var lastErr error
	for attempt := 0; attempt < p.maxAttempts; attempt++ {
		vecs, err := p.provider.Embed(ctx, batch)
		if err == nil {
			if len(vecs) != len(batch) {
				return nil, fmt.Errorf("knowledge: embedding provider returned %d vectors for %d inputs", len(vecs), len(batch))
			}
			return vecs, nil
		}
		lastErr = err
		if attempt == p.maxAttempts-1 {
			break
		}
		delay := p.retryWait << attempt
		if delay > maxRetryWait {
			delay = maxRetryWait
		}
		if err := sleepCtx(ctx, p.sleep, delay); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("knowledge: embedding batch failed after %d attempts: %w", p.maxAttempts, lastErr)
}
