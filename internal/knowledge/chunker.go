package knowledge

import (
	"strings"
	"unicode"
)

// Chunking knobs (contract track 3-d: ~800 chars, ~15% overlap, cut on
// boundaries so chunks never split words/sentences mid-token).
const (
	// DefaultChunkSize is the maximum chunk length in RUNES (~800 chars).
	DefaultChunkSize = 800
	// DefaultChunkOverlap is the character overlap between consecutive
	// chunks (15% of DefaultChunkSize). The overlap window starts at the
	// previous cut position so boundary context is preserved.
	DefaultChunkOverlap = DefaultChunkSize * 15 / 100
	// minChunkFraction is the smallest useful chunk produced by a boundary
	// cut (half the target size); below that the hard cut at `size` wins.
	// It keeps overlap windows from producing degenerate chunks.
	minChunkFraction = 2
)

// ChunkText splits text into overlapping chunks of at most DefaultChunkSize
// runes, preferring paragraph breaks, then sentence ends, then word spaces.
func ChunkText(text string) []string {
	return chunkText(text, DefaultChunkSize, DefaultChunkOverlap)
}

// chunkText is the deterministic implementation. Chunks are trimmed; empty
// chunks are never emitted. Progress is guaranteed even for pathological
// inputs (no boundaries at all -> hard cut at size).
func chunkText(text string, size, overlap int) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if size <= 0 {
		size = DefaultChunkSize
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 2 // never let the window exceed the chunk
	}
	runes := []rune(trimmed)
	if len(runes) <= size {
		return []string{trimmed}
	}

	chunks := make([]string, 0, len(runes)/size+1)
	start := 0
	for start < len(runes) {
		end := start + size
		if end >= len(runes) {
			// Final chunk: everything that remains.
			if chunk := strings.TrimSpace(string(runes[start:])); chunk != "" {
				chunks = append(chunks, chunk)
			}
			break
		}
		// Cut on the best boundary at or before the size limit, never
		// closer to start than half the target size (a hard cut beats a
		// tiny chunk).
		cut := findBoundary(runes, start+size/minChunkFraction, end)
		if chunk := strings.TrimSpace(string(runes[start:cut])); chunk != "" {
			chunks = append(chunks, chunk)
		}
		// Overlap window: back off from the cut, then re-align forward to
		// a word start so chunks never begin mid-word. The realignment is
		// bounded by the previous cut so boundary-less input (giant words,
		// CJK) cannot make the window run to the end of the text.
		next := cut - overlap
		if next <= start {
			next = start + 1 // hard progress guarantee
		}
		for next < cut && !unicode.IsSpace(runes[next]) && !unicode.IsSpace(runes[next-1]) {
			next++
		}
		for next < len(runes) && unicode.IsSpace(runes[next]) {
			next++
		}
		if next <= start {
			next = end // paranoia: never loop
		}
		start = next
	}
	return chunks
}

// findBoundary returns the exclusive end index of the best chunk that fits in
// [floor, end]: the last paragraph break, else the last sentence end, else
// the last word space, else `end` itself.
func findBoundary(runes []rune, floor, end int) int {
	if floor < 0 {
		floor = 0
	}
	if floor >= end {
		return end
	}
	// Paragraph break (covers "\n\n" too via the same rune).
	for i := end - 1; i >= floor; i-- {
		if runes[i] == '\n' {
			return i + 1
		}
	}
	// Sentence end: punctuation kept inside the chunk.
	for i := end - 1; i >= floor; i-- {
		switch runes[i] {
		case '.', '!', '?':
			return i + 1
		}
	}
	// Word boundary.
	for i := end - 1; i >= floor; i-- {
		if runes[i] == ' ' {
			return i
		}
	}
	return end
}
