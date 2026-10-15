package memory

import (
	"context"
	"database/sql"
)

// EmbeddingProvider is memory's view of the issue #113 provider seam. It
// mirrors knowledge.EmbeddingProvider STRUCTURALLY (same method set), so the
// knowledge implementations — knowledge.OpenAIEmbeddingProvider,
// knowledge.OfflineHashEmbedder — satisfy it without an import (Go
// consumer-side interfaces); any other batch float32 embedding backend does
// too.
type EmbeddingProvider interface {
	// Embed returns one vector per input, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimensions reports the vector dimensionality of the provider space.
	Dimensions() int
	// Name identifies the provider in logs/wiring.
	Name() string
}

// providerEmbedder adapts a batch EmbeddingProvider (float32, issue #113
// seam) to memory's single-text float64 Embedder. The widening float32 ->
// float64 is exact, so provider vectors round-trip through the existing
// JSONB embedding column and the Go-side cosine scorer unchanged.
//
// Known limitation (documented): PutSnippets embeds snippet-by-snippet, so a
// remote provider sees one request per snippet. Memory PUTs are small
// (snippet sets, not documents); batching here is a future optimization that
// does not change the seam.
type providerEmbedder struct {
	provider EmbeddingProvider
}

// Compile-time proof the adapter satisfies memory's legacy embedder seam.
var _ Embedder = providerEmbedder{}

// Name forwards the provider identity.
func (p providerEmbedder) Name() string { return p.provider.Name() }

// Embed embeds one text through the batch provider.
func (p providerEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	vecs, err := p.provider.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	vec := vecs[0]
	out := make([]float64, len(vec))
	for i, v := range vec {
		out[i] = float64(v)
	}
	return out, nil
}

// NewServiceWithEmbeddingProvider returns an in-memory service whose
// semantic retrieval embeds through the provider-backed embedder (issue #113
// seam: pass knowledge.OpenAIEmbeddingProvider or
// knowledge.OfflineHashEmbedder). The offline default stays NewService().
// A nil provider falls back to NewService.
func NewServiceWithEmbeddingProvider(provider EmbeddingProvider) *Service {
	if provider == nil {
		return NewService()
	}
	return newService(nil, providerEmbedder{provider: provider})
}

// NewServiceWithStoreAndEmbeddingProvider returns a Postgres-backed service
// (migration 014 memory_snippets) using the provider-backed embedder. The
// JSONB embedding column stores the provider vectors as plain float arrays —
// memory needs NO pgvector column (candidate retrieval stays org-scoped scan
// + Go-side cosine), so no migration accompanies this seam. A nil provider
// falls back to NewServiceWithStore (offline embedder).
func NewServiceWithStoreAndEmbeddingProvider(db *sql.DB, provider EmbeddingProvider) *Service {
	if provider == nil {
		return NewServiceWithStore(db)
	}
	return newService(NewPostgresStore(db), providerEmbedder{provider: provider})
}
