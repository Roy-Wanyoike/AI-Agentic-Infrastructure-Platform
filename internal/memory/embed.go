package memory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"unicode"
)

// EmbeddingDim is the dimensionality of the deterministic offline embedding
// space. It is intentionally identical to internal/knowledge's offline
// embedder so a shared embedder can be extracted later without a migration.
const EmbeddingDim = 256

// HashEmbedder is the deterministic, dependency-free embedder used when no
// embeddings backend is configured (tests/dev — documented deviation in
// docs/wiring/knowledge.md). Tokens are hashed into a fixed-size bag-of-
// words vector, L2-normalized, so identical text yields identical vectors
// and cosine similarity behaves like a soft token-overlap measure.
type HashEmbedder struct{}

// NewHashEmbedder returns the offline embedder.
func NewHashEmbedder() HashEmbedder { return HashEmbedder{} }

// Embed computes the pseudo-embedding for one text. It never fails.
func (HashEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	vec := make([]float64, EmbeddingDim)
	for _, token := range tokenize(text) {
		sum := sha256.Sum256([]byte(token))
		// Two independent buckets per token soften the collisions of a
		// 256-dim space while staying fully deterministic.
		b1 := int(binary.BigEndian.Uint32(sum[0:4]) % EmbeddingDim)
		b2 := int(binary.BigEndian.Uint32(sum[4:8]) % EmbeddingDim)
		vec[b1] += 1
		vec[b2] += 0.5
	}
	norm := 0.0
	for _, v := range vec {
		norm += v * v
	}
	if norm == 0 {
		return vec, nil // empty text -> zero vector (cosine scores 0)
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] /= norm
	}
	return vec, nil
}

// cosineSimilarity returns the cosine of two equal-length vectors in [0,1]
// (negative similarity clamps to 0; zero vectors score 0).
func cosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if math.IsNaN(sim) || sim < 0 {
		return 0
	}
	return sim
}

// lexicalScore is the embedding-free fallback: the fraction of distinct query
// tokens present in the candidate content (Jaccard over token sets so long
// candidates are not over-favored).
func lexicalScore(queryTokens map[string]struct{}, content string) float64 {
	if len(queryTokens) == 0 {
		return 0
	}
	candidateTokens := tokenSet(content)
	if len(candidateTokens) == 0 {
		return 0
	}
	intersection := 0
	for token := range queryTokens {
		if _, ok := candidateTokens[token]; ok {
			intersection++
		}
	}
	union := len(queryTokens) + len(candidateTokens) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// tokenSet lowercases and splits text on non-letter/digit runes.
func tokenSet(text string) map[string]struct{} {
	tokens := tokenize(text)
	set := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		set[t] = struct{}{}
	}
	return set
}

func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return fields
}
