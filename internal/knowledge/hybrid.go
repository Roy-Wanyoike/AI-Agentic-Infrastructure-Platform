package knowledge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Hybrid search env knobs (issue #113). The weights control the linear
// fusion of the keyword and vector legs (both clamped to [0,1]); the fusion
// selects between weighted min-max-normalized score fusion ("weights") and
// reciprocal rank fusion ("rrf").
const (
	// EnvHybridKeywordWeight weights the keyword leg (default 0.5).
	EnvHybridKeywordWeight = "AGENTOS_HYBRID_KEYWORD_WEIGHT"
	// EnvHybridVectorWeight weights the vector leg (default 0.5).
	EnvHybridVectorWeight = "AGENTOS_HYBRID_VECTOR_WEIGHT"
	// EnvHybridFusion selects the fusion algorithm: weights|rrf (default weights).
	EnvHybridFusion = "AGENTOS_HYBRID_FUSION"
)

// SearchMode selects the retrieval strategy of HybridSearcher.Search.
type SearchMode string

// Search modes (wire values of the knowledge search request's `mode` param).
const (
	// SearchModeKeyword runs the lexical text-search leg only.
	SearchModeKeyword SearchMode = "keyword"
	// SearchModeSemantic runs the vector leg only (pgvector cosine search).
	SearchModeSemantic SearchMode = "semantic"
	// SearchModeHybrid fuses both legs (default when mode is omitted).
	SearchModeHybrid SearchMode = "hybrid"
)

// FusionMode selects the score-combination algorithm of the hybrid mode.
type FusionMode string

// Fusion algorithms.
const (
	// FusionWeights linearly combines min-max-normalized leg scores
	// (default): score = kwW*norm(kw) + vecW*norm(vec).
	FusionWeights FusionMode = "weights"
	// FusionRRF combines reciprocal RANKS (constant 60): score =
	// kwW/(60+kwRank) + vecW/(60+vecRank). Rank fusion is scale-free and
	// robust to per-leg score distributions.
	FusionRRF FusionMode = "rrf"
)

// rrfConstant is the standard reciprocal-rank-fusion smoothing constant.
const rrfConstant = 60.0

// Default hybrid weights.
const (
	DefaultHybridKeywordWeight = 0.5
	DefaultHybridVectorWeight  = 0.5
)

// KeywordSource is the keyword (lexical text-search) leg of hybrid search.
// The knowledge Service implements it with the existing Go-side lexical
// scorer over the org-scoped candidate window (there is no tsvector column —
// the repo's text-search path is the Go lexical token-overlap scorer, so the
// hybrid keyword leg reuses exactly that path).
type KeywordSource interface {
	// KeywordSearch returns at most limit keyword hits for the query, scoped
	// to one organization.
	KeywordSearch(ctx context.Context, orgID, query string, limit int) ([]ScoredChunk, error)
}

// Compile-time proof the dual-mode Service is a valid keyword leg.
var _ KeywordSource = (*Service)(nil)

// HybridSearcher combines the keyword leg (KeywordSource) and the vector leg
// (pgvector VectorStore + EmbeddingProvider) into one ranked result set.
//
// GRACEFUL DEGRADATION (issue #113): when the vector leg is unavailable —
// nil store/provider, a query embedding failure, a VectorSearch error (e.g.
// the migration ran on a plain Postgres without pgvector, or an offline
// 256-dim provider against the 1536-dim column), or zero vector matches —
// every non-keyword mode degrades to keyword-only results carrying their RAW
// lexical scores (same semantics as SearchModeKeyword). Degradation is
// deliberate and documented: search is a read path, availability beats
// completeness.
type HybridSearcher struct {
	keyword  KeywordSource
	vectors  VectorStore
	provider EmbeddingProvider

	keywordWeight float64
	vectorWeight  float64
	fusion        FusionMode
}

// NewHybridSearcher returns a searcher with the default weights (0.5/0.5)
// and FusionWeights. Any of the legs may be nil: a nil keyword source
// disables keyword-only paths, a nil vector store or provider forces the
// keyword-only degradation.
func NewHybridSearcher(keyword KeywordSource, vectors VectorStore, provider EmbeddingProvider) *HybridSearcher {
	return &HybridSearcher{
		keyword:       keyword,
		vectors:       vectors,
		provider:      provider,
		keywordWeight: DefaultHybridKeywordWeight,
		vectorWeight:  DefaultHybridVectorWeight,
		fusion:        FusionWeights,
	}
}

// WithWeights overrides the fusion weights (clamped to [0,1]; both-zero
// inputs are rejected in favor of the defaults so fusion can never collapse
// to a constant zero score).
func (h *HybridSearcher) WithWeights(keywordWeight, vectorWeight float64) *HybridSearcher {
	h.keywordWeight = clampUnit(keywordWeight)
	h.vectorWeight = clampUnit(vectorWeight)
	if h.keywordWeight == 0 && h.vectorWeight == 0 {
		h.keywordWeight = DefaultHybridKeywordWeight
		h.vectorWeight = DefaultHybridVectorWeight
	}
	return h
}

// WithFusion overrides the fusion algorithm (unknown values keep the
// previous setting).
func (h *HybridSearcher) WithFusion(fusion FusionMode) *HybridSearcher {
	if fusion == FusionWeights || fusion == FusionRRF {
		h.fusion = fusion
	}
	return h
}

// HybridSearcherFromEnv wires a searcher from the environment
// (AGENTOS_HYBRID_KEYWORD_WEIGHT / AGENTOS_HYBRID_VECTOR_WEIGHT /
// AGENTOS_HYBRID_FUSION, defaults 0.5 / 0.5 / weights).
func HybridSearcherFromEnv(keyword KeywordSource, vectors VectorStore, provider EmbeddingProvider) *HybridSearcher {
	h := NewHybridSearcher(keyword, vectors, provider)
	if raw := strings.TrimSpace(os.Getenv(EnvHybridKeywordWeight)); raw != "" {
		if w, err := strconv.ParseFloat(raw, 64); err == nil {
			h.WithWeights(w, h.vectorWeight)
		}
	}
	if raw := strings.TrimSpace(os.Getenv(EnvHybridVectorWeight)); raw != "" {
		if w, err := strconv.ParseFloat(raw, 64); err == nil {
			h.WithWeights(h.keywordWeight, w)
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvHybridFusion))) {
	case string(FusionRRF):
		h.WithFusion(FusionRRF)
	case string(FusionWeights), "":
		h.WithFusion(FusionWeights)
	}
	return h
}

// Search runs the selected mode and returns the top-k results:
//
//   - keyword: the lexical leg only (raw lexical scores).
//   - semantic: the vector leg only (raw cosine similarities); degrades to
//     keyword when the vector leg is unavailable.
//   - hybrid (default): both legs fused per the configured fusion mode.
//
// Results are deduplicated by chunk id and ordered deterministically (score
// desc, then chunk id, then ordinal).
func (h *HybridSearcher) Search(ctx context.Context, orgID, query string, k int, mode SearchMode) ([]SearchResult, error) {
	if h == nil || h.keyword == nil {
		return nil, errors.New("knowledge: hybrid keyword source is required")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("query is required")
	}
	if mode == "" {
		mode = SearchModeHybrid
	}
	switch mode {
	case SearchModeKeyword, SearchModeSemantic, SearchModeHybrid:
	default:
		return nil, fmt.Errorf("knowledge: unknown search mode %q (want keyword|semantic|hybrid)", mode)
	}
	if k <= 0 {
		k = DefaultSearchK
	}
	if k > MaxSearchK {
		k = MaxSearchK
	}

	switch mode {
	case SearchModeKeyword:
		hits := h.keywordLeg(ctx, orgID, query, k)
		return toSearchResults(hits), nil
	case SearchModeSemantic:
		vectorHits, ok := h.vectorLeg(ctx, orgID, query, k)
		if !ok {
			// Graceful degradation: vector leg unavailable -> keyword-only.
			return toSearchResults(h.keywordLeg(ctx, orgID, query, k)), nil
		}
		return toSearchResults(vectorHits), nil
	default: // SearchModeHybrid
		hits := h.keywordLeg(ctx, orgID, query, searchCandidateLimit)
		vectorHits, ok := h.vectorLeg(ctx, orgID, query, k)
		if !ok {
			// Graceful degradation: fuse nothing, return the raw keyword leg.
			if len(hits) > k {
				hits = hits[:k]
			}
			return toSearchResults(hits), nil
		}
		var fused []ScoredChunk
		if h.fusion == FusionRRF {
			fused = fuseRRF(hits, vectorHits, h.keywordWeight, h.vectorWeight)
		} else {
			fused = fuseWeights(hits, vectorHits, h.keywordWeight, h.vectorWeight)
		}
		if len(fused) > k {
			fused = fused[:k]
		}
		return toSearchResults(fused), nil
	}
}

// keywordLeg fetches the keyword hits (deterministic order from the
// KeywordSource).
func (h *HybridSearcher) keywordLeg(ctx context.Context, orgID, query string, limit int) []ScoredChunk {
	hits, err := h.keyword.KeywordSearch(ctx, orgID, query, limit)
	if err != nil || len(hits) == 0 {
		return nil
	}
	return hits
}

// vectorLeg embeds the query and runs the vector search. The bool result is
// the availability flag: false means "degrade to keyword-only" (nil legs,
// embedding failure, store failure, or zero matches).
func (h *HybridSearcher) vectorLeg(ctx context.Context, orgID, query string, k int) ([]ScoredChunk, bool) {
	if h.vectors == nil || h.provider == nil {
		return nil, false
	}
	vecs, err := h.provider.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, false
	}
	hits, err := h.vectors.VectorSearch(ctx, orgID, vecs[0], k)
	if err != nil || len(hits) == 0 {
		return nil, false
	}
	return hits, true
}

// fuseWeights combines both legs with min-max normalization inside each leg
// and a weighted sum: score = kwW*norm_kw + vecW*norm_vec. Chunks hit by
// only one leg contribute 0 for the missing leg. A degenerate leg (all
// scores equal) normalizes every hit to 1. Results are deduplicated by
// chunk id and sorted deterministically.
func fuseWeights(keyword, vector []ScoredChunk, keywordWeight, vectorWeight float64) []ScoredChunk {
	kwNorm := minMaxNormalize(keyword)
	vecNorm := minMaxNormalize(vector)

	byID := make(map[string]*fusedHit, len(keyword)+len(vector))
	for _, hit := range keyword {
		byID[hit.Row.ID] = &fusedHit{hit: hit}
	}
	for _, hit := range vector {
		if _, ok := byID[hit.Row.ID]; !ok {
			byID[hit.Row.ID] = &fusedHit{hit: hit}
		}
	}
	for id, entry := range byID {
		score := 0.0
		if norm, ok := kwNorm[id]; ok {
			score += keywordWeight * norm
		}
		if norm, ok := vecNorm[id]; ok {
			score += vectorWeight * norm
		}
		entry.score = score
	}
	return sortedFused(byID)
}

// fuseRRF combines both legs with reciprocal rank fusion: score =
// kwW/(60+kwRank) + vecW/(60+vecRank) with 1-based ranks inside each leg
// (legs arrive sorted best-first). Results are deduplicated by chunk id and
// sorted deterministically.
func fuseRRF(keyword, vector []ScoredChunk, keywordWeight, vectorWeight float64) []ScoredChunk {
	byID := make(map[string]*fusedHit, len(keyword)+len(vector))
	for rank, hit := range keyword {
		byID[hit.Row.ID] = &fusedHit{hit: hit, score: keywordWeight / (rrfConstant + float64(rank+1))}
	}
	for rank, hit := range vector {
		contribution := vectorWeight / (rrfConstant + float64(rank+1))
		if entry, ok := byID[hit.Row.ID]; ok {
			entry.score += contribution
			continue
		}
		byID[hit.Row.ID] = &fusedHit{hit: hit, score: contribution}
	}
	return sortedFused(byID)
}

// fusedHit accumulates the fused score of one (deduplicated) chunk.
type fusedHit struct {
	hit   ScoredChunk
	score float64
}

// minMaxNormalize maps each hit's score into [0,1] via min-max inside the
// result set, keyed by chunk id. A degenerate set (max == min) maps every
// hit to 1 (they are tied at the best score).
func minMaxNormalize(hits []ScoredChunk) map[string]float64 {
	if len(hits) == 0 {
		return nil
	}
	min, max := math.Inf(1), math.Inf(-1)
	for _, hit := range hits {
		if hit.Score < min {
			min = hit.Score
		}
		if hit.Score > max {
			max = hit.Score
		}
	}
	out := make(map[string]float64, len(hits))
	span := max - min
	for _, hit := range hits {
		if span <= 0 {
			out[hit.Row.ID] = 1
			continue
		}
		out[hit.Row.ID] = (hit.Score - min) / span
	}
	return out
}

// sortedFused flattens the fused map deterministically: score desc, then
// chunk id, then chunk ordinal.
func sortedFused(byID map[string]*fusedHit) []ScoredChunk {
	out := make([]ScoredChunk, 0, len(byID))
	for _, entry := range byID {
		entry.hit.Score = entry.score
		out = append(out, entry.hit)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Row.ID != out[j].Row.ID {
			return out[i].Row.ID < out[j].Row.ID
		}
		return out[i].Row.Ordinal < out[j].Row.Ordinal
	})
	return out
}

// toSearchResults maps retrieval hits onto the wire shape (contract-pinned
// fields + citations).
func toSearchResults(hits []ScoredChunk) []SearchResult {
	results := make([]SearchResult, 0, len(hits))
	for _, hit := range hits {
		results = append(results, SearchResult{
			DocumentID:    hit.Row.DocumentID,
			DocumentTitle: hit.Row.DocumentTitle,
			ChunkID:       hit.Row.ID,
			ChunkOrdinal:  hit.Row.Ordinal,
			Content:       hit.Row.Content,
			Score:         hit.Score,
			Citation:      buildCitation(hit.Row.DocumentTitle, hit.Row.DocumentSource, hit.Row.Ordinal),
		})
	}
	return results
}

// KeywordSearch implements KeywordSource for the dual-mode Service: it scores
// the org-scoped chunk candidates against the query with the lexical
// token-overlap scorer (the existing text-search path) and returns the
// top-limit hits above the score floor, best first (ties broken by document
// title, then chunk ordinal, then chunk id).
func (s *Service) KeywordSearch(ctx context.Context, orgID, query string, limit int) ([]ScoredChunk, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("query is required")
	}
	candidates, err := s.candidates(ctx, orgID)
	if err != nil {
		return nil, err
	}
	queryTokens := tokenSet(query)
	scored := make([]ScoredChunk, 0, len(candidates))
	for _, row := range candidates {
		score := lexicalScore(queryTokens, row.Content)
		if score <= scoreFloor {
			continue // drop hash-collision noise, same as classic Search
		}
		scored = append(scored, ScoredChunk{Row: row, Score: score})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		if scored[i].Row.DocumentTitle != scored[j].Row.DocumentTitle {
			return scored[i].Row.DocumentTitle < scored[j].Row.DocumentTitle
		}
		if scored[i].Row.Ordinal != scored[j].Row.Ordinal {
			return scored[i].Row.Ordinal < scored[j].Row.Ordinal
		}
		return scored[i].Row.ID < scored[j].Row.ID
	})
	if limit > 0 && len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

// clampUnit clamps v into [0,1] (NaN maps to 0).
func clampUnit(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
