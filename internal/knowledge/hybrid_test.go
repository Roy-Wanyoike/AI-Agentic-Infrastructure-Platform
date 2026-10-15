package knowledge

import (
	"context"
	"math"
	"strings"
	"testing"
)

// fakeKeywordSource is a scripted KeywordSource leg.
type fakeKeywordSource struct {
	hits     []ScoredChunk
	err      error
	calls    int
	gotOrg   string
	gotQuery string
	gotLimit int
}

func (f *fakeKeywordSource) KeywordSearch(_ context.Context, orgID, query string, limit int) ([]ScoredChunk, error) {
	f.calls++
	f.gotOrg, f.gotQuery, f.gotLimit = orgID, query, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// hybridVectorStore is a scripted VectorStore leg.
type hybridVectorStore struct {
	hits  []ScoredChunk
	err   error
	calls int
	gotK  int
}

func (f *hybridVectorStore) UpsertDocumentWithVectors(context.Context, *Document, []Chunk) error {
	return nil
}

func (f *hybridVectorStore) VectorSearch(_ context.Context, _ string, _ []float32, k int) ([]ScoredChunk, error) {
	f.calls++
	f.gotK = k
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// hybridChunk builds a ScoredChunk with citation fields.
func hybridChunk(id, docID, title string, ordinal int, score float64) ScoredChunk {
	return ScoredChunk{
		Row: ChunkRow{
			Chunk:          Chunk{ID: id, OrganizationID: "org-1", DocumentID: docID, Ordinal: ordinal, Content: "content " + id},
			DocumentTitle:  title,
			DocumentSource: "confluence/billing",
		},
		Score: score,
	}
}

func assertClose(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s: got %.12f, want %.12f", what, got, want)
	}
}

func TestHybridWeightsFusion(t *testing.T) {
	keyword := &fakeKeywordSource{hits: []ScoredChunk{
		hybridChunk("kA", "doc-1", "Doc A", 0, 0.8),
		hybridChunk("kB", "doc-1", "Doc A", 1, 0.4),
		hybridChunk("kC", "doc-2", "Doc B", 0, 0.2),
	}}
	vectors := &hybridVectorStore{hits: []ScoredChunk{
		hybridChunk("kB", "doc-1", "Doc A", 1, 0.9),
		hybridChunk("kD", "doc-3", "Doc C", 0, 0.1),
	}}
	// With weights 0.5/0.5 the min-max normalized fusion yields:
	//   keyword norm: kA=1.0, kB=(0.4-0.2)/0.6=1/3, kC=0
	//   vector  norm: kB=1.0, kD=0
	//   fused: kB=0.5*(1/3)+0.5*1.0=2/3, kA=0.5, kC=0, kD=0
	searcher := NewHybridSearcher(keyword, vectors, NewOfflineHashEmbedder())

	results, err := searcher.Search(context.Background(), "org-1", "billing question", 10, SearchModeHybrid)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("dedup by chunk id failed: 5 leg entries must fuse to 4 results, got %d", len(results))
	}
	if results[0].ChunkID != "kB" {
		t.Fatalf("double-hit chunk must win, got %s", results[0].ChunkID)
	}
	assertClose(t, "kB", results[0].Score, 2.0/3.0)
	assertClose(t, "kA", results[1].Score, 0.5)
	// kC and kD tie at 0 -> deterministic chunk-id tie-break (kC < kD).
	if results[2].ChunkID != "kC" || results[3].ChunkID != "kD" {
		t.Fatalf("tie-break must be deterministic by chunk id: %s/%s", results[2].ChunkID, results[3].ChunkID)
	}
	assertClose(t, "kC", results[2].Score, 0)
	assertClose(t, "kD", results[3].Score, 0)
	// Citation fields flow through.
	if results[0].DocumentTitle != "Doc A" || results[0].Citation == "" {
		t.Fatalf("citations must be built: %+v", results[0])
	}
	// Tenant guard: org forwarded to both legs.
	if keyword.gotOrg != "org-1" {
		t.Fatalf("keyword leg lost the org scope: %q", keyword.gotOrg)
	}
}

func TestHybridWeightedSkew(t *testing.T) {
	keyword := &fakeKeywordSource{hits: []ScoredChunk{hybridChunk("kA", "d", "T", 0, 1.0)}}
	vectors := &hybridVectorStore{hits: []ScoredChunk{hybridChunk("kB", "d", "T", 0, 1.0)}}
	// 0.8/0.2: the keyword leg dominates.
	searcher := NewHybridSearcher(keyword, vectors, NewOfflineHashEmbedder()).WithWeights(0.8, 0.2)
	results, err := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeHybrid)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	assertClose(t, "kA", results[0].Score, 0.8)
	assertClose(t, "kB", results[1].Score, 0.2)
}

func TestHybridRRFFusion(t *testing.T) {
	keyword := &fakeKeywordSource{hits: []ScoredChunk{
		hybridChunk("kA", "d1", "T", 0, 0.9),
		hybridChunk("kB", "d1", "T", 1, 0.5),
		hybridChunk("kC", "d2", "T", 0, 0.3),
	}}
	vectors := &hybridVectorStore{hits: []ScoredChunk{
		hybridChunk("kB", "d1", "T", 1, 0.9),
		hybridChunk("kD", "d3", "T", 0, 0.4),
	}}
	searcher := NewHybridSearcher(keyword, vectors, NewOfflineHashEmbedder()).WithFusion(FusionRRF)
	results, err := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeHybrid)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	// RRF (k=60, weights 0.5/0.5, 1-based ranks):
	//   kB: kw rank 2 + vec rank 1 = 0.5/62 + 0.5/61
	//   kA: kw rank 1 = 0.5/61
	//   kD: vec rank 2 = 0.5/62
	//   kC: kw rank 3 = 0.5/63
	assertClose(t, "kB", results[0].Score, 0.5/62.0+0.5/61.0)
	assertClose(t, "kA", results[1].Score, 0.5/61.0)
	assertClose(t, "kD", results[2].Score, 0.5/62.0)
	assertClose(t, "kC", results[3].Score, 0.5/63.0)
	if len(results) != 4 {
		t.Fatalf("dedup failed: got %d results", len(results))
	}
}

func TestHybridDegenerateLegNormalization(t *testing.T) {
	// All keyword scores equal -> the leg normalizes to 1 for every hit.
	keyword := &fakeKeywordSource{hits: []ScoredChunk{
		hybridChunk("kA", "d", "T", 0, 0.5),
		hybridChunk("kB", "d", "T", 1, 0.5),
	}}
	vectors := &hybridVectorStore{hits: []ScoredChunk{
		hybridChunk("kC", "d", "T", 0, 0.7),
	}}
	searcher := NewHybridSearcher(keyword, vectors, NewOfflineHashEmbedder())
	results, err := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeHybrid)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	// kA/kB: 0.5*1 + 0 = 0.5 each; kC: 0.5*1 = 0.5 -> three-way tie.
	for _, r := range results {
		assertClose(t, r.ChunkID, r.Score, 0.5)
	}
}

func TestHybridModes(t *testing.T) {
	keyword := &fakeKeywordSource{hits: []ScoredChunk{
		hybridChunk("kA", "d1", "T", 0, 0.7),
		hybridChunk("kB", "d1", "T", 1, 0.3),
	}}
	vectors := &hybridVectorStore{hits: []ScoredChunk{
		hybridChunk("kC", "d2", "T", 0, 0.95),
		hybridChunk("kD", "d2", "T", 1, 0.4),
	}}
	searcher := NewHybridSearcher(keyword, vectors, NewOfflineHashEmbedder())

	t.Run("keyword mode never touches the vector leg", func(t *testing.T) {
		results, err := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeKeyword)
		if err != nil {
			t.Fatalf("Search returned error: %v", err)
		}
		if vectors.calls != 0 {
			t.Fatal("keyword mode must not call the vector store")
		}
		if len(results) != 2 || results[0].ChunkID != "kA" {
			t.Fatalf("keyword mode must return the raw lexical leg: %+v", results)
		}
		assertClose(t, "kA", results[0].Score, 0.7) // RAW score, not normalized
	})
	t.Run("semantic mode returns raw vector scores", func(t *testing.T) {
		results, err := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeSemantic)
		if err != nil {
			t.Fatalf("Search returned error: %v", err)
		}
		if keyword.calls != 1 { // only the keyword-mode call above
			t.Fatalf("semantic mode must not need the keyword leg beyond the earlier call, got %d", keyword.calls)
		}
		if len(results) != 2 || results[0].ChunkID != "kC" {
			t.Fatalf("semantic mode must return the vector leg: %+v", results)
		}
		assertClose(t, "kC", results[0].Score, 0.95)
	})
	t.Run("empty mode defaults to hybrid", func(t *testing.T) {
		if _, err := searcher.Search(context.Background(), "org-1", "q", 10, ""); err != nil {
			t.Fatalf("empty mode must default to hybrid, got %v", err)
		}
	})
	t.Run("unknown mode errors", func(t *testing.T) {
		if _, err := searcher.Search(context.Background(), "org-1", "q", 10, SearchMode("fuzzy")); err == nil {
			t.Fatal("unknown mode must be rejected")
		}
	})
}

func TestHybridGracefulKeywordOnlyDegradation(t *testing.T) {
	keywordHits := []ScoredChunk{
		hybridChunk("kA", "d1", "T", 0, 0.6),
		hybridChunk("kB", "d1", "T", 1, 0.2),
	}
	cases := []struct {
		name     string
		vectors  VectorStore
		provider EmbeddingProvider
	}{
		{"nil vector store (pgvector never installed)", nil, NewOfflineHashEmbedder()},
		{"nil provider (offline wiring)", &hybridVectorStore{hits: []ScoredChunk{hybridChunk("kC", "d", "T", 0, 0.9)}}, nil},
		{"provider embed failure", &hybridVectorStore{hits: []ScoredChunk{hybridChunk("kC", "d", "T", 0, 0.9)}}, mustFailProvider()},
		{"vector store error", &hybridVectorStore{err: context.DeadlineExceeded}, NewOfflineHashEmbedder()},
		{"zero vector matches (empty embedding_vec column)", &hybridVectorStore{hits: nil}, NewOfflineHashEmbedder()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyword := &fakeKeywordSource{hits: keywordHits}
			searcher := NewHybridSearcher(keyword, tc.vectors, tc.provider)
			results, err := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeHybrid)
			if err != nil {
				t.Fatalf("degradation must not error, got %v", err)
			}
			if len(results) != 2 || results[0].ChunkID != "kA" {
				t.Fatalf("degradation must return the keyword leg: %+v", results)
			}
			// RAW lexical scores, identical to mode=keyword semantics.
			assertClose(t, "kA", results[0].Score, 0.6)
			// The same holds for semantic mode.
			results, err = searcher.Search(context.Background(), "org-1", "q", 10, SearchModeSemantic)
			if err != nil {
				t.Fatalf("semantic degradation must not error, got %v", err)
			}
			if len(results) != 2 || results[0].ChunkID != "kA" {
				t.Fatalf("semantic degradation must return the keyword leg: %+v", results)
			}
		})
	}
}

func mustFailProvider() EmbeddingProvider {
	p := newStubEmbeddingProvider(3)
	p.failFirst = 1_000_000
	return p
}

func TestHybridKCapAndDeterminism(t *testing.T) {
	hits := make([]ScoredChunk, 0, 10)
	for i := 0; i < 10; i++ {
		hits = append(hits, hybridChunk(strings.Repeat("z", 9-i)+"id"+string(rune('a'+i)), "d", "T", i, float64(i)/10))
	}
	keyword := &fakeKeywordSource{hits: hits}
	vectors := &hybridVectorStore{hits: hits[:2]}
	searcher := NewHybridSearcher(keyword, vectors, NewOfflineHashEmbedder())
	results, err := searcher.Search(context.Background(), "org-1", "q", 3, SearchModeHybrid)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("k cap violated: got %d results", len(results))
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].Score < results[i].Score {
			t.Fatalf("results must be score-ordered: %v < %v", results[i-1].Score, results[i].Score)
		}
	}
	// k=0 defaults to DefaultSearchK, k>MaxSearchK caps.
	if _, err := searcher.Search(context.Background(), "org-1", "q", 0, SearchModeKeyword); err != nil {
		t.Fatalf("k=0 must default, got %v", err)
	}
	if _, err := searcher.Search(context.Background(), "org-1", "q", MaxSearchK+100, SearchModeKeyword); err != nil {
		t.Fatalf("oversized k must cap, got %v", err)
	}
	// Determinism: identical inputs, identical output order.
	again, err := searcher.Search(context.Background(), "org-1", "q", 3, SearchModeHybrid)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	for i := range results {
		if results[i].ChunkID != again[i].ChunkID {
			t.Fatalf("result order must be deterministic at %d: %s vs %s", i, results[i].ChunkID, again[i].ChunkID)
		}
	}
}

func TestHybridValidation(t *testing.T) {
	searcher := NewHybridSearcher(&fakeKeywordSource{}, &hybridVectorStore{}, NewOfflineHashEmbedder())
	if _, err := searcher.Search(context.Background(), "", "q", 5, SearchModeHybrid); err == nil {
		t.Fatal("empty org must be rejected (tenant guard)")
	}
	if _, err := searcher.Search(context.Background(), "org-1", "  ", 5, SearchModeHybrid); err == nil {
		t.Fatal("empty query must be rejected")
	}
	var nilSearcher *HybridSearcher
	if _, err := nilSearcher.Search(context.Background(), "org-1", "q", 5, SearchModeHybrid); err == nil {
		t.Fatal("nil searcher must be guarded")
	}
}

func TestHybridEnvConfig(t *testing.T) {
	keyword := &fakeKeywordSource{hits: []ScoredChunk{
		hybridChunk("kA", "d", "T", 0, 1.0),
		hybridChunk("kB", "d", "T", 1, 0.0),
	}}
	vectors := &hybridVectorStore{hits: []ScoredChunk{
		hybridChunk("kC", "d", "T", 0, 1.0),
		hybridChunk("kD", "d", "T", 1, 0.0),
	}}

	t.Run("weights and fusion from env", func(t *testing.T) {
		t.Setenv(EnvHybridKeywordWeight, "0.9")
		t.Setenv(EnvHybridVectorWeight, "0.1")
		t.Setenv(EnvHybridFusion, "rrf")
		searcher := HybridSearcherFromEnv(keyword, vectors, NewOfflineHashEmbedder())
		results, err := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeHybrid)
		if err != nil {
			t.Fatalf("Search returned error: %v", err)
		}
		// RRF with 0.9/0.1: kA = 0.9/61, kB = 0.9/62, kC = 0.1/61, kD = 0.1/62.
		assertClose(t, "kA", results[0].Score, 0.9/61.0)
		assertClose(t, "kB", results[1].Score, 0.9/62.0)
		assertClose(t, "kC", results[2].Score, 0.1/61.0)
		assertClose(t, "kD", results[3].Score, 0.1/62.0)
	})
	t.Run("defaults when env unset", func(t *testing.T) {
		t.Setenv(EnvHybridKeywordWeight, "")
		t.Setenv(EnvHybridVectorWeight, "")
		t.Setenv(EnvHybridFusion, "")
		searcher := HybridSearcherFromEnv(keyword, vectors, NewOfflineHashEmbedder())
		results, _ := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeHybrid)
		// weights fusion, 0.5/0.5, min-max over [1.0, 0.0]: kA/kC normalize to
		// 1, kB/kD to 0.
		assertClose(t, "kA", results[0].Score, 0.5)
		assertClose(t, "kC", results[1].Score, 0.5)
		assertClose(t, "kB", results[2].Score, 0.0)
		assertClose(t, "kD", results[3].Score, 0.0)
	})
	t.Run("invalid weights clamp", func(t *testing.T) {
		t.Setenv(EnvHybridKeywordWeight, "7")
		t.Setenv(EnvHybridVectorWeight, "-2")
		searcher := HybridSearcherFromEnv(keyword, vectors, NewOfflineHashEmbedder())
		results, _ := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeHybrid)
		// keyword weight clamps to 1 (degenerate leg -> 1.0), vector to 0.
		assertClose(t, "kA", results[0].Score, 1.0)
	})
	t.Run("unparseable weights keep defaults", func(t *testing.T) {
		t.Setenv(EnvHybridKeywordWeight, "not-a-number")
		t.Setenv(EnvHybridVectorWeight, "")
		searcher := HybridSearcherFromEnv(keyword, vectors, NewOfflineHashEmbedder())
		results, _ := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeHybrid)
		assertClose(t, "kA", results[0].Score, 0.5)
	})
	t.Run("both-zero weights fall back to defaults", func(t *testing.T) {
		searcher := NewHybridSearcher(keyword, vectors, NewOfflineHashEmbedder()).WithWeights(0, 0)
		results, _ := searcher.Search(context.Background(), "org-1", "q", 10, SearchModeHybrid)
		assertClose(t, "kA", results[0].Score, 0.5) // 0.5 * 1.0
	})
	t.Run("unknown fusion keeps weights mode", func(t *testing.T) {
		t.Setenv(EnvHybridFusion, "bogus")
		searcher := HybridSearcherFromEnv(keyword, vectors, NewOfflineHashEmbedder())
		if searcher.fusion != FusionWeights {
			t.Fatalf("unknown fusion must keep the weights default, got %q", searcher.fusion)
		}
	})
}

func TestServiceKeywordSearch(t *testing.T) {
	svc := NewServiceWithEmbedder(NewHashEmbedder())
	org := "org-1"
	if _, _, err := svc.IngestDocument(context.Background(), org, IngestRequest{
		Title:   "Billing FAQ",
		Content: "Customers prefer email invoices. The billing cycle runs monthly.",
	}); err != nil {
		t.Fatalf("IngestDocument returned error: %v", err)
	}
	if _, _, err := svc.IngestDocument(context.Background(), org, IngestRequest{
		Title:   "Deploy guide",
		Content: "Deployments roll out canaries gradually across regions.",
	}); err != nil {
		t.Fatalf("IngestDocument returned error: %v", err)
	}

	t.Run("lexical hits sorted best-first", func(t *testing.T) {
		hits, err := svc.KeywordSearch(context.Background(), org, "email invoices billing", 10)
		if err != nil {
			t.Fatalf("KeywordSearch returned error: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("expected lexical hits")
		}
		for i := 1; i < len(hits); i++ {
			if hits[i-1].Score < hits[i].Score {
				t.Fatalf("hits must be score-ordered: %v < %v", hits[i-1].Score, hits[i].Score)
			}
		}
		if hits[0].Row.DocumentTitle != "Billing FAQ" {
			t.Fatalf("billing content must rank first, got %q", hits[0].Row.DocumentTitle)
		}
		// Lexical scorer (Jaccard token overlap) bounds scores to (0,1].
		if hits[0].Score <= 0 || hits[0].Score > 1 {
			t.Fatalf("lexical score out of range: %v", hits[0].Score)
		}
	})
	t.Run("tenant guard", func(t *testing.T) {
		hits, err := svc.KeywordSearch(context.Background(), "org-foreign", "email invoices", 10)
		if err != nil {
			t.Fatalf("KeywordSearch returned error: %v", err)
		}
		if len(hits) != 0 {
			t.Fatalf("foreign org must see nothing, got %d hits", len(hits))
		}
	})
	t.Run("validation", func(t *testing.T) {
		if _, err := svc.KeywordSearch(context.Background(), "", "q", 5); err == nil {
			t.Fatal("empty org must be rejected")
		}
		if _, err := svc.KeywordSearch(context.Background(), org, "", 5); err == nil {
			t.Fatal("empty query must be rejected")
		}
	})
	t.Run("limit caps the leg", func(t *testing.T) {
		hits, err := svc.KeywordSearch(context.Background(), org, "email invoices billing deployments", 1)
		if err != nil {
			t.Fatalf("KeywordSearch returned error: %v", err)
		}
		if len(hits) != 1 {
			t.Fatalf("limit must cap the leg, got %d", len(hits))
		}
	})
}

func TestHybridWithRealServiceLeg(t *testing.T) {
	// Full-stack integration: a real dual-mode Service as the keyword leg +
	// a fake pgvector store + the offline provider through the #113 seam.
	svc := NewServiceWithEmbedder(NewHashEmbedder())
	org := "org-1"
	if _, _, err := svc.IngestDocument(context.Background(), org, IngestRequest{
		Title:   "Billing FAQ",
		Content: "Customers prefer email invoices. The billing cycle runs monthly.",
	}); err != nil {
		t.Fatalf("IngestDocument returned error: %v", err)
	}
	// The vector leg has nothing indexed yet -> graceful keyword degradation.
	vectors := &hybridVectorStore{hits: nil}
	searcher := NewHybridSearcher(svc, vectors, NewOfflineHashEmbedder())
	results, err := searcher.Search(context.Background(), org, "email invoices", 5, SearchModeHybrid)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) == 0 || results[0].DocumentTitle != "Billing FAQ" {
		t.Fatalf("hybrid must degrade to the service keyword leg: %+v", results)
	}
	// With the vector leg populated, fusion runs.
	vectors.hits = []ScoredChunk{hybridChunk("vec-chunk", "doc-vec", "Vector doc", 0, 0.99)}
	results, err = searcher.Search(context.Background(), org, "email invoices", 5, SearchModeHybrid)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("fused result set must include both legs, got %d", len(results))
	}
	// The vector hit enters the fused set.
	found := false
	for _, r := range results {
		if r.ChunkID == "vec-chunk" {
			found = true
		}
	}
	if !found {
		t.Fatal("vector leg hit must appear in the fused results")
	}
}
