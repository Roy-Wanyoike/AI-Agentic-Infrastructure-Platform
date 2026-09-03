package knowledge

import (
	"strings"
	"testing"
)

// sentence builds "SentenceNN this is filler text." blocks of ~40 chars.
func fillerSentences(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("Sentence number ")
		b.WriteString(strings.Repeat("x", i%10)) // vary lengths slightly
		b.WriteString(" ends here. ")
	}
	return b.String()
}

func TestChunkTextEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t  \n"} {
		if got := ChunkText(in); got != nil {
			t.Fatalf("ChunkText(%q) should return nil, got %v", in, got)
		}
	}
}

func TestChunkTextShortReturnsSingleChunk(t *testing.T) {
	in := "  Short document about billing.  "
	got := ChunkText(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(got))
	}
	if got[0] != strings.TrimSpace(in) {
		t.Fatalf("chunk should be the trimmed input, got %q", got[0])
	}
}

func TestChunkTextLongSentenceBoundaries(t *testing.T) {
	in := fillerSentences(120) // ~5000 chars
	chunks := ChunkText(in)
	if len(chunks) < 2 {
		t.Fatalf("long text must produce multiple chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk == "" {
			t.Fatalf("chunk %d is empty", i)
		}
		if n := len([]rune(chunk)); n > DefaultChunkSize+DefaultChunkOverlap {
			t.Fatalf("chunk %d exceeds size+overlap: %d runes", i, n)
		}
		// Sentence-boundary preference: every chunk that was cut (not the
		// final tail) should end at a sentence end.
		if i < len(chunks)-1 && !strings.HasSuffix(chunk, ".") {
			t.Fatalf("chunk %d should end on a sentence boundary, got %q", i, tail(chunk, 40))
		}
	}
	// Full coverage: every sentence survives somewhere in the chunk set.
	joined := strings.Join(chunks, " ")
	for _, marker := range []string{"Sentence number x", "ends here."} {
		if !strings.Contains(joined, marker) {
			t.Fatalf("coverage lost: %q missing from chunks", marker)
		}
	}
}

func TestChunkTextOverlapBetweenConsecutiveChunks(t *testing.T) {
	in := fillerSentences(80)
	chunks := ChunkText(in)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	// The 15% overlap window guarantees consecutive chunks share a tail/
	// head region (word-aligned, so compare via containment of the first
	// few words of chunk i+1 inside chunk i's tail).
	for i := 0; i < len(chunks)-1; i++ {
		head := firstWords(chunks[i+1], 3)
		if head == "" {
			continue
		}
		if !strings.Contains(chunks[i], head) {
			t.Fatalf("chunk %d and %d share no overlap window: %q not in tail %q",
				i, i+1, head, tail(chunks[i], 120))
		}
	}
}

func TestChunkTextCustomSize(t *testing.T) {
	in := fillerSentences(60)
	chunks := chunkText(in, 200, 30)
	if len(chunks) < 5 {
		t.Fatalf("small custom size should produce many chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if n := len([]rune(chunk)); n > 200+30 {
			t.Fatalf("chunk %d exceeds size+overlap: %d", i, n)
		}
	}
}

func TestChunkTextNoBoundariesStillProgresses(t *testing.T) {
	in := strings.Repeat("a", 2500) // one giant word: no boundaries at all
	chunks := ChunkText(in)
	if len(chunks) < 2 {
		t.Fatalf("hard cut must still chunk, got %d", len(chunks))
	}
	total := 0
	for i, chunk := range chunks {
		if i < len(chunks)-1 && len([]rune(chunk)) != DefaultChunkSize {
			t.Fatalf("hard-cut chunks should be exactly size, got %d", len([]rune(chunk)))
		}
		total += len([]rune(chunk))
	}
	if total < len(in) {
		t.Fatalf("coverage lost under hard cuts: %d < %d", total, len(in))
	}
}

func TestChunkTextUnicodeSafety(t *testing.T) {
	in := strings.Repeat("知识库文档内容。", 400) // multi-byte runes, CJK sentence ends
	chunks := ChunkText(in)
	if len(chunks) < 2 {
		t.Fatalf("unicode text must chunk, got %d", len(chunks))
	}
	last := chunks[len(chunks)-1]
	if !strings.HasSuffix(last, "。") {
		t.Fatalf("final chunk should carry the tail, got %q", tail(last, 20))
	}
	// No mid-rune splits: joining chunks (plus overlap slack) keeps every
	// rune count sane and no replacement characters appear.
	for _, chunk := range chunks {
		if strings.ContainsRune(chunk, '�') {
			t.Fatal("chunk contains a corrupted rune")
		}
	}
}

func TestChunkTextDeterministic(t *testing.T) {
	in := fillerSentences(50)
	first := ChunkText(in)
	second := ChunkText(in)
	if len(first) != len(second) {
		t.Fatalf("chunking must be deterministic: %d vs %d chunks", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("chunk %d differs between runs", i)
		}
	}
}

func firstWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	if len(words) < n {
		n = len(words)
	}
	return strings.Join(words[:n], " ")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
