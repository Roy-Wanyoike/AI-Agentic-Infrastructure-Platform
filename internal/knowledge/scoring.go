package knowledge

import (
	"math"
	"strings"
	"unicode"
)

// Scoring helpers shared by the offline embedder and the retrieval fallback.
// They intentionally mirror internal/memory's implementations so the two
// packages' offline behavior stays identical (a shared helper can be
// extracted later without behavior changes).

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
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
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

// lexicalScore is the embedding-free fallback: Jaccard overlap between the
// query tokens and the candidate content tokens (bounded [0,1] and robust
// to long documents).
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
