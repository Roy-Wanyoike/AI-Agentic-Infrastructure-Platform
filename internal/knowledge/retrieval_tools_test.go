// Package knowledge_test hosts the retrieval tools' external contract tests:
// the compile-time proof that the tier tools satisfy the PLATFORM tool
// interface (internal/tools.Tool + ContextAware) and the tier behavior,
// decomposition, synthesis, scoping and metrics assertions (issue #132).
// The tools themselves live in package knowledge (internal/tools imports
// internal/knowledge, so the reverse import would cycle — the asserts must
// therefore live in this external test package).
package knowledge_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentos/internal/knowledge"
	"agentos/internal/tools"

	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------------------
// Compile-time interface compliance (issue #132: "interface compliance
// (compile-time asserts)"). Each tool must register like a platform built-in.
// ---------------------------------------------------------------------------
var (
	_ tools.Tool         = (*knowledge.QuickSearchTool)(nil)
	_ tools.Tool         = (*knowledge.PanoramaSearchTool)(nil)
	_ tools.Tool         = (*knowledge.InsightForgeTool)(nil)
	_ tools.ContextAware = (*knowledge.QuickSearchTool)(nil)
	_ tools.ContextAware = (*knowledge.PanoramaSearchTool)(nil)
	_ tools.ContextAware = (*knowledge.InsightForgeTool)(nil)

	_ knowledge.RetrievalTool   = (*knowledge.QuickSearchTool)(nil)
	_ knowledge.RetrievalTiered = (*knowledge.QuickSearchTool)(nil)
	_ knowledge.RetrievalTool   = (*knowledge.PanoramaSearchTool)(nil)
	_ knowledge.RetrievalTiered = (*knowledge.PanoramaSearchTool)(nil)
	_ knowledge.RetrievalTool   = (*knowledge.InsightForgeTool)(nil)
	_ knowledge.RetrievalTiered = (*knowledge.InsightForgeTool)(nil)

	_ knowledge.TierCandidateSource = knowledge.TierCandidateSource(nil)
	_ knowledge.TierClassifier      = knowledge.DefaultTierClassifier{}
	_ knowledge.Decomposer          = knowledge.RuleBasedDecomposer{}
	_ knowledge.Synthesizer         = knowledge.DeterministicSynthesizer{}
	_ knowledge.RetrievalMetrics    = knowledge.NoopRetrievalMetrics{}
	_ knowledge.RetrievalMetrics    = (*knowledge.MemoryRetrievalMetrics)(nil)
)

// fakeTierSource is a scripted TierCandidateSource: candidates grouped by
// org (the org scoping the real sources enforce) plus a record of the
// orgs/limits it was asked for.
type fakeTierSource struct {
	byOrg   map[string][]knowledge.TierCandidate
	seen    []string // "orgID:limit" per call, in order
	fail    error
	lastOrg string
}

func newFakeTierSource() *fakeTierSource {
	return &fakeTierSource{byOrg: make(map[string][]knowledge.TierCandidate)}
}

func (f *fakeTierSource) add(orgID string, candidates ...knowledge.TierCandidate) {
	f.byOrg[orgID] = append(f.byOrg[orgID], candidates...)
}

func (f *fakeTierSource) TierCandidates(_ context.Context, orgID string, limit int) ([]knowledge.TierCandidate, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.seen = append(f.seen, orgID+":"+itoa(limit))
	f.lastOrg = orgID
	rows := f.byOrg[orgID] // tenant guard: foreign orgs see nothing
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return append([]knowledge.TierCandidate(nil), rows...), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// tierChunk builds one candidate chunk row.
func tierChunk(orgID, docID, chunkID, title, content string, ordinal int) knowledge.TierCandidate {
	return knowledge.TierCandidate{
		ChunkRow: knowledge.ChunkRow{
			Chunk: knowledge.Chunk{
				ID:             chunkID,
				OrganizationID: orgID,
				DocumentID:     docID,
				Ordinal:        ordinal,
				Content:        content,
			},
			DocumentTitle: title,
		},
	}
}

func withMeta(c knowledge.TierCandidate, meta map[string]any) knowledge.TierCandidate {
	c.DocumentMetadata = meta
	return c
}

// ---------------------------------------------------------------------------
// Quick tier
// ---------------------------------------------------------------------------

func TestQuickSearchToolDefaultTopK(t *testing.T) {
	src := newFakeTierSource()
	for i := 0; i < 8; i++ {
		c := tierChunk("org-1", "doc-a", "c-a-"+itoa(i), "Alpha Doc", "billing refund policy line "+strings.Repeat("alpha ", i), i)
		c.Chunk.Content = "billing refund policy alpha"
		src.add("org-1", c)
	}
	tool := knowledge.NewQuickSearchTool(src)

	out, err := tool.Execute(map[string]any{"org_id": "org-1", "query": "billing refund policy"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got, want := out["tier"], "quick"; got != want {
		t.Fatalf("tier = %v, want %v", got, want)
	}
	if got, want := out["count"], 5; got != want { // default top-K is 5
		t.Fatalf("count = %v, want %v", got, want)
	}
	results := out["results"].([]map[string]any)
	if len(results) != 5 {
		t.Fatalf("len(results) = %d, want 5", len(results))
	}
	for i, r := range results {
		if r["content"] != "billing refund policy alpha" {
			t.Fatalf("results[%d][content] = %v, want the queried content", i, r["content"])
		}
		if _, ok := r["document_metadata"]; ok {
			t.Fatalf("quick results must not carry panorama context (results[%d])", i)
		}
	}
}

func TestQuickSearchToolRespectsTopK(t *testing.T) {
	src := newFakeTierSource()
	for i := 0; i < 6; i++ {
		c := tierChunk("org-1", "doc-a", "c-"+itoa(i), "Alpha", "vector search index", i)
		src.add("org-1", c)
	}
	tool := knowledge.NewQuickSearchTool(src)

	out, err := tool.Execute(map[string]any{"org_id": "org-1", "query": "vector search", "k": 2})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := out["count"]; got != 2 {
		t.Fatalf("count = %v, want 2", got)
	}
	results := out["results"].([]map[string]any)
	// Deterministic order: equal scores break by ordinal.
	if results[0]["chunk_id"] != "c-0" || results[1]["chunk_id"] != "c-1" {
		t.Fatalf("unexpected order: %v, %v", results[0]["chunk_id"], results[1]["chunk_id"])
	}
}

func TestQuickSearchExcludesArchivedAndExpired(t *testing.T) {
	src := newFakeTierSource()
	src.add("org-1",
		withMeta(tierChunk("org-1", "doc-live", "c-live", "Live", "the lease renewal process", 0), nil),
		withMeta(tierChunk("org-1", "doc-arch", "c-arch", "Archive", "the lease renewal process archived", 0),
			map[string]any{"archived": true}),
		withMeta(tierChunk("org-1", "doc-status", "c-status", "Status", "the lease renewal process status", 0),
			map[string]any{"status": "archived"}),
		withMeta(tierChunk("org-1", "doc-exp", "c-exp", "Expiring", "the lease renewal process expired", 0),
			map[string]any{"expires_at": "2020-01-01T00:00:00Z"}),
	)
	frozen := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	tool := knowledge.NewQuickSearchTool(src).WithClock(func() time.Time { return frozen })

	out, err := tool.Execute(map[string]any{"org_id": "org-1", "query": "lease renewal process"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	results := out["results"].([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want only the active chunk", len(results))
	}
	if results[0]["chunk_id"] != "c-live" {
		t.Fatalf("chunk_id = %v, want c-live", results[0]["chunk_id"])
	}
}

func TestPanoramaSearchIncludesArchivedWhenFlagged(t *testing.T) {
	src := newFakeTierSource()
	archivedAt := time.Date(2024, 1, 15, 8, 30, 0, 0, time.UTC)
	src.add("org-1",
		withMeta(tierChunk("org-1", "doc-live", "c-live", "Live", "sso configuration guide", 0), nil),
		withMeta(tierChunk("org-1", "doc-arch", "c-arch", "Archive", "sso configuration guide (superseded)", 0),
			map[string]any{"archived": true, "reason": "superseded"}),
	)
	src.byOrg["org-1"][1].DocumentCreatedAt = archivedAt
	frozen := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	tool := knowledge.NewPanoramaSearchTool(src).WithClock(func() time.Time { return frozen })

	// Flag off: active-only (same scoping as the quick tier).
	out, err := tool.Execute(map[string]any{"org_id": "org-1", "query": "sso configuration"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := out["include_archived"]; got != false {
		t.Fatalf("include_archived = %v, want false", got)
	}
	if results := out["results"].([]map[string]any); len(results) != 1 || results[0]["chunk_id"] != "c-live" {
		t.Fatalf("default window must be active-only, got %v", out["results"])
	}

	// Flag on: archived content joins the window with FULL context.
	out, err = tool.Execute(map[string]any{"org_id": "org-1", "query": "sso configuration", "include_archived": true})
	if err != nil {
		t.Fatalf("Execute(include_archived) returned error: %v", err)
	}
	if got := out["include_archived"]; got != true {
		t.Fatalf("include_archived = %v, want true", got)
	}
	results := out["results"].([]map[string]any)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want the live and archived chunks", len(results))
	}
	var archived map[string]any
	for _, r := range results {
		if r["chunk_id"] == "c-arch" {
			archived = r
		}
	}
	if archived == nil {
		t.Fatalf("archived chunk missing from panorama results: %v", results)
	}
	if archived["document_metadata"].(map[string]any)["reason"] != "superseded" {
		t.Fatalf("archived hit missing document metadata: %v", archived)
	}
	if archived["document_source"] != "" || archived["document_title"] != "Archive" {
		t.Fatalf("archived hit context wrong: %v", archived)
	}
	if archived["created_at"] != "2024-01-15T08:30:00Z" {
		t.Fatalf("created_at = %v, want the document timestamp", archived["created_at"])
	}
}

// ---------------------------------------------------------------------------
// Insight tier: decomposition + synthesis
// ---------------------------------------------------------------------------

func TestRuleBasedDecomposerDeterministicAndDocumented(t *testing.T) {
	d := knowledge.RuleBasedDecomposer{}
	query := "What is the refund policy and how do overrides work? Also, where are invoices stored?"
	first := d.Decompose(query)
	for i := 0; i < 20; i++ {
		if got := d.Decompose(query); !reflect.DeepEqual(got, first) {
			t.Fatalf("decomposition not deterministic: run %d = %#v, first = %#v", i, got, first)
		}
	}
	want := []string{"What is the refund policy", "how do overrides work", "where are invoices stored"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("Decompose = %#v, want %#v", first, want)
	}

	// No split points -> the single original query (tier still runs).
	if got := d.Decompose("single clause question"); !reflect.DeepEqual(got, []string{"single clause question"}) {
		t.Fatalf("single-clause decomposition = %#v", got)
	}
	// Duplicates collapse (case-insensitive, first occurrence wins).
	got := d.Decompose("How does billing work? HOW DOES BILLING WORK and how does billing work?")
	if len(got) != 1 || got[0] != "How does billing work" {
		t.Fatalf("dedup decomposition = %#v", got)
	}
}

func TestRuleBasedDecomposerCapsSubQuestions(t *testing.T) {
	// Clauses long enough to survive the fragment filter; the cap bounds
	// the fan-out regardless of how many splits the query carries.
	query := "alpha and beta and gamma and delta and epsilon and zeta and eta and theta"
	got := knowledge.RuleBasedDecomposer{}.Decompose(query)
	if len(got) != 6 { // MaxSubQuestions
		t.Fatalf("len = %d, want capped 6: %#v", len(got), got)
	}
}

func TestInsightForgeDecomposesRetrievesSynthesizes(t *testing.T) {
	src := newFakeTierSource()
	src.add("org-1",
		tierChunk("org-1", "doc-billing", "c-refund", "Billing", "the refund policy allows 30 days", 0),
		tierChunk("org-1", "doc-billing", "c-override", "Billing", "how do refunds work when managers override refund limits", 1),
	)
	metrics := knowledge.NewMemoryRetrievalMetrics()
	tool := knowledge.NewInsightForgeTool(src).WithMetrics(metrics)

	out, err := tool.Execute(map[string]any{"org_id": "org-1", "query": "What is the refund policy and how do refunds work?"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	subs := out["sub_questions"].([]string)
	if len(subs) != 2 {
		t.Fatalf("sub_questions = %v, want 2 sub-questions", subs)
	}
	findings := out["findings"].(map[string][]map[string]any)
	if len(findings[subs[0]]) == 0 || len(findings[subs[1]]) == 0 {
		t.Fatalf("expected findings for both sub-questions: %v", findings)
	}
	synthesis := out["synthesis"].(string)
	if !strings.Contains(synthesis, "refund policy allows 30 days") || !strings.Contains(synthesis, "override refund limits") {
		t.Fatalf("synthesis missing evidence: %q", synthesis)
	}
	// Deterministic synthesis: identical input -> identical text.
	out2, err := tool.Execute(map[string]any{"org_id": "org-1", "query": "What is the refund policy and how do refunds work?"})
	if err != nil {
		t.Fatalf("second Execute returned error: %v", err)
	}
	if out2["synthesis"] != synthesis {
		t.Fatalf("synthesis not deterministic:\n%q\n%q", out2["synthesis"], synthesis)
	}
	// Metrics: one call, distinct evidence counted, tokens estimated.
	usage := metrics.Usage()[knowledge.TierInsight]
	if usage.Calls != 2 {
		t.Fatalf("insight calls = %d, want 2", usage.Calls)
	}
	if usage.EstimatedTokens <= 0 {
		t.Fatalf("estimated tokens = %d, want positive", usage.EstimatedTokens)
	}
}

// tierHit builds one ranked hit for the synthesizer assertions.
func tierHit(docID, chunkID, title, content string) knowledge.TierHit {
	return knowledge.TierHit{
		DocumentID:    docID,
		DocumentTitle: title,
		ChunkID:       chunkID,
		Content:       content,
		Score:         scoreFor(content),
		Citation:      title + ", chunk 0",
	}
}

// scoreFor derives a hit score from the content so ranking tests are
// explicit.
func scoreFor(content string) float64 {
	if strings.Contains(content, "high") {
		return 0.9
	}
	return 0.1
}

func TestInsightForgeSynthesisDedupsAndRanks(t *testing.T) {
	// Both sub-questions surfaced both chunks: the deterministic synthesizer
	// must collapse the duplicated chunk into ONE bullet each (dedup) and
	// order by score, not by sub-question order.
	synthesis := knowledge.DeterministicSynthesizer{}.Synthesize(
		"query",
		[]string{"sub one", "sub two"},
		map[string][]knowledge.TierHit{
			"sub one": {
				tierHit("doc-a", "c-high", "Alpha", "shared evidence high"),
				tierHit("doc-b", "c-low", "Beta", "shared evidence low"),
			},
			"sub two": {
				tierHit("doc-a", "c-high", "Alpha", "shared evidence high"),
				tierHit("doc-b", "c-low", "Beta", "shared evidence low"),
			},
		},
	)
	if strings.Count(synthesis, "shared evidence high") != 1 || strings.Count(synthesis, "shared evidence low") != 1 {
		t.Fatalf("synthesis did not dedup chunk evidence: %q", synthesis)
	}
	// Two evidence bullets, highest score first.
	if !strings.Contains(synthesis, "1. Alpha, chunk 0 — shared evidence high") ||
		!strings.Contains(synthesis, "2. Beta, chunk 0 — shared evidence low") {
		t.Fatalf("synthesis ranking wrong: %q", synthesis)
	}
	// Deterministic across runs (and independent of the findings map's
	// iteration order).
	again := knowledge.DeterministicSynthesizer{}.Synthesize(
		"query",
		[]string{"sub one", "sub two"},
		map[string][]knowledge.TierHit{
			"sub two": {
				tierHit("doc-b", "c-low", "Beta", "shared evidence low"),
				tierHit("doc-a", "c-high", "Alpha", "shared evidence high"),
			},
			"sub one": {
				tierHit("doc-a", "c-high", "Alpha", "shared evidence high"),
			},
		},
	)
	if again != synthesis {
		t.Fatalf("synthesis depends on map iteration order:\n%q\n%q", again, synthesis)
	}
}

func TestInsightForgeUsesSynthesizerSeam(t *testing.T) {
	src := newFakeTierSource()
	src.add("org-1", tierChunk("org-1", "doc-a", "c-1", "Alpha", "retrieval seam evidence", 0))

	stub := &synthesizerStub{out: "LLM composition"}
	tool := knowledge.NewInsightForgeTool(src).WithSynthesizer(stub)
	out, err := tool.Execute(map[string]any{"org_id": "org-1", "query": "what is here"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if out["synthesis"] != "LLM composition" {
		t.Fatalf("synthesizer seam ignored: %v", out["synthesis"])
	}
	if stub.gotQuery != "what is here" || len(stub.gotSubs) == 0 {
		t.Fatalf("synthesizer seam got query=%q subs=%v", stub.gotQuery, stub.gotSubs)
	}
}

type synthesizerStub struct {
	out      string
	gotQuery string
	gotSubs  []string
}

func (s *synthesizerStub) Synthesize(query string, subQuestions []string, _ map[string][]knowledge.TierHit) string {
	s.gotQuery = query
	s.gotSubs = subQuestions
	return s.out
}

// ---------------------------------------------------------------------------
// Org scoping + validation
// ---------------------------------------------------------------------------

func TestTierToolsAreOrgScoped(t *testing.T) {
	src := newFakeTierSource()
	src.add("org-1", tierChunk("org-1", "doc-a", "c-1", "Alpha", "tenant isolated content", 0))
	src.add("org-2", tierChunk("org-2", "doc-b", "c-2", "Beta", "other tenant content", 0))

	quick := knowledge.NewQuickSearchTool(src)
	pano := knowledge.NewPanoramaSearchTool(src).WithClassifier(knowledge.DefaultTierClassifier{})
	insight := knowledge.NewInsightForgeTool(src)

	for _, tc := range []struct {
		name string
		run  func() (map[string]any, error)
	}{
		{"quick", func() (map[string]any, error) {
			return quick.Execute(map[string]any{"org_id": "org-1", "query": "content"})
		}},
		{"panorama", func() (map[string]any, error) {
			return pano.Execute(map[string]any{"org_id": "org-1", "query": "content", "include_archived": true})
		}},
		{"insight", func() (map[string]any, error) {
			return insight.Execute(map[string]any{"org_id": "org-1", "query": "what content exists"})
		}},
	} {
		out, err := tc.run()
		if err != nil {
			t.Fatalf("%s: Execute returned error: %v", tc.name, err)
		}
		if out["org_id"] != "org-1" {
			t.Fatalf("%s: org_id = %v, want org-1", tc.name, out["org_id"])
		}
		if src.lastOrg != "org-1" {
			t.Fatalf("%s: source queried org %q, want org-1", tc.name, src.lastOrg)
		}
	}

	// Cross-tenant reads return nothing (the source is org-guarded) — and
	// the tool still answers honestly with an empty result set.
	out, err := quick.Execute(map[string]any{"org_id": "org-9", "query": "content"})
	if err != nil {
		t.Fatalf("foreign org Execute returned error: %v", err)
	}
	if got := out["count"]; got != 0 {
		t.Fatalf("foreign org count = %v, want 0", got)
	}

	// Fail closed on missing org/query.
	if _, err := quick.Execute(map[string]any{"query": "x"}); err == nil {
		t.Fatal("missing org_id must fail")
	}
	if _, err := quick.Execute(map[string]any{"org_id": "org-1"}); err == nil {
		t.Fatal("missing query must fail")
	}
}

func TestTierToolsFailClosedWithoutSource(t *testing.T) {
	quick := knowledge.NewQuickSearchTool(nil)
	if _, err := quick.Execute(map[string]any{"org_id": "org-1", "query": "x"}); err == nil {
		t.Fatal("nil source must fail closed")
	}
}

func TestTierSourceErrorsPropagate(t *testing.T) {
	src := newFakeTierSource()
	src.fail = errors.New("store down")
	pano := knowledge.NewPanoramaSearchTool(src)
	if _, err := pano.Execute(map[string]any{"org_id": "org-1", "query": "x"}); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("source error must propagate, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Metrics seam
// ---------------------------------------------------------------------------

func TestTierMetricsCounters(t *testing.T) {
	src := newFakeTierSource()
	src.add("org-1", tierChunk("org-1", "doc-a", "c-1", "Alpha", "metric probe content", 0))

	metrics := knowledge.NewMemoryRetrievalMetrics()
	quick := knowledge.NewQuickSearchTool(src).WithMetrics(metrics)
	pano := knowledge.NewPanoramaSearchTool(src).WithMetrics(metrics)

	if _, err := quick.Execute(map[string]any{"org_id": "org-1", "query": "metric probe"}); err != nil {
		t.Fatalf("quick Execute: %v", err)
	}
	if _, err := quick.Execute(map[string]any{"org_id": "org-1", "query": "metric probe"}); err != nil {
		t.Fatalf("quick Execute: %v", err)
	}
	if _, err := pano.Execute(map[string]any{"org_id": "org-1", "query": "metric probe"}); err != nil {
		t.Fatalf("panorama Execute: %v", err)
	}
	usage := metrics.Usage()
	if usage[knowledge.TierQuick].Calls != 2 {
		t.Fatalf("quick calls = %d, want 2", usage[knowledge.TierQuick].Calls)
	}
	if usage[knowledge.TierQuick].EstimatedTokens <= 0 {
		t.Fatal("quick tokens not estimated")
	}
	if usage[knowledge.TierPanorama].Calls != 1 {
		t.Fatalf("panorama calls = %d, want 1", usage[knowledge.TierPanorama].Calls)
	}
	// The no-op default never records anything (and never panics).
	noop := knowledge.NoopRetrievalMetrics{}
	noop.ObserveTierCall(knowledge.TierQuick, "org-1", 3, 10)
}

func TestEstimateTokensMatchesPlatformHeuristic(t *testing.T) {
	if got := knowledge.EstimateTokens(""); got != 0 {
		t.Fatalf("EstimateTokens(\"\") = %d, want 0", got)
	}
	// ~4 characters per token, rounded up (platform budgeting heuristic).
	if got, want := knowledge.EstimateTokens("12345678"), 2; got != want {
		t.Fatalf("EstimateTokens = %d, want %d", got, want)
	}
	if got, want := knowledge.EstimateTokens("123456789"), 3; got != want {
		t.Fatalf("EstimateTokens = %d, want %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// Tier identity + registration
// ---------------------------------------------------------------------------

func TestTierNamesAndMapping(t *testing.T) {
	if got := knowledge.NewQuickSearchTool(nil).TierName(); got != knowledge.TierQuick {
		t.Fatalf("quick TierName = %v", got)
	}
	if got := knowledge.NewPanoramaSearchTool(nil).TierName(); got != knowledge.TierPanorama {
		t.Fatalf("panorama TierName = %v", got)
	}
	if got := knowledge.NewInsightForgeTool(nil).TierName(); got != knowledge.TierInsight {
		t.Fatalf("insight TierName = %v", got)
	}
	if tier, ok := knowledge.TierForTool("panorama_search"); !ok || tier != knowledge.TierPanorama {
		t.Fatalf("TierForTool(panorama_search) = %v, %v", tier, ok)
	}
	if _, ok := knowledge.TierForTool("unknown_tool"); ok {
		t.Fatal("unknown tool must not map to a tier")
	}
}

func TestRegisterRetrievalToolsIntoPlatformRegistry(t *testing.T) {
	src := newFakeTierSource()
	src.add("org-1", tierChunk("org-1", "doc-a", "c-1", "Alpha", "registry probe", 0))

	reg := tools.NewRegistry()
	// THE integrator wiring line: each concrete tool is passed as the
	// registry's tool interface (tools.Tool) — the conversion is the
	// compiler-enforced interface-compliance check, T is inferred from the
	// registry.
	names := knowledge.RegisterRetrievalTools(reg,
		tools.Tool(knowledge.NewQuickSearchTool(src)),
		tools.Tool(knowledge.NewPanoramaSearchTool(src)),
		tools.Tool(knowledge.NewInsightForgeTool(src)))
	want := []string{"quick_search", "panorama_search", "insight_forge"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("registered = %v, want %v", names, want)
	}
	for _, name := range want {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		if tool.Name() != name {
			t.Fatalf("registry key %q != tool name %q", name, tool.Name())
		}
	}
	// End-to-end through the platform registry: Execute works like a built-in.
	tool, _ := reg.Get("quick_search")
	out, err := tool.Execute(map[string]any{"org_id": "org-1", "query": "registry probe"})
	if err != nil {
		t.Fatalf("registry Execute returned error: %v", err)
	}
	if out["count"] != 1 {
		t.Fatalf("registry Execute count = %v, want 1", out["count"])
	}
	// Additive + idempotent: re-registering overwrites the same names.
	again := knowledge.RegisterRetrievalTools(reg, tools.Tool(knowledge.NewQuickSearchTool(src)))
	if !reflect.DeepEqual(again, []string{"quick_search"}) {
		t.Fatalf("re-register = %v", again)
	}
	if _, ok := reg.Get("insight_forge"); !ok {
		t.Fatal("re-register must not drop other tools")
	}
}

// ---------------------------------------------------------------------------
// Production source adapters
// ---------------------------------------------------------------------------

func TestServiceTierSource(t *testing.T) {
	svc := knowledge.NewService()
	ctx := context.Background()
	if _, _, err := svc.IngestDocument(ctx, "org-1", knowledge.IngestRequest{
		Title:    "Billing FAQ",
		Content:  "billing refund policy alpha beta gamma delta epsilon zeta",
		Metadata: map[string]any{"team": "billing"},
	}); err != nil {
		t.Fatalf("IngestDocument: %v", err)
	}
	if _, _, err := svc.IngestDocument(ctx, "org-2", knowledge.IngestRequest{
		Title:   "Foreign",
		Content: "other org content",
	}); err != nil {
		t.Fatalf("IngestDocument: %v", err)
	}

	src := knowledge.NewServiceTierSource(svc)
	rows, err := src.TierCandidates(ctx, "org-1", 100)
	if err != nil {
		t.Fatalf("TierCandidates: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected candidates for org-1")
	}
	for _, row := range rows {
		if row.OrganizationID != "org-1" {
			t.Fatalf("cross-tenant leak: chunk %q org %q", row.ID, row.OrganizationID)
		}
		if row.DocumentMetadata["team"] != "billing" {
			t.Fatalf("metadata not joined for chunk %q: %v", row.ID, row.DocumentMetadata)
		}
		if row.DocumentTitle != "Billing FAQ" {
			t.Fatalf("title not joined for chunk %q: %q", row.ID, row.DocumentTitle)
		}
	}
	// The quick tier over the service source answers with citations.
	tool := knowledge.NewQuickSearchTool(src)
	out, err := tool.Execute(map[string]any{"org_id": "org-1", "query": "refund policy"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	results := out["results"].([]map[string]any)
	if len(results) == 0 || results[0]["citation"] == "" {
		t.Fatalf("expected cited results, got %v", out["results"])
	}
}

func TestStoreTierSourceJoinsMetadata(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := knowledge.NewPostgresStore(db)

	at := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	docs := sqlmock.NewRows([]string{"id", "organization_id", "title", "source", "metadata", "chunk_count", "created_at", "updated_at"}).
		AddRow("doc-1", "org-1", "Billing FAQ", "confluence/billing", `{"team":"billing"}`, 1, at, at)
	chunks := sqlmock.NewRows([]string{"id", "organization_id", "document_id", "ordinal", "content", "embedding", "created_at", "title", "source"}).
		AddRow("c-1", "org-1", "doc-1", 0, "billing refund policy", ``, at, "Billing FAQ", "confluence/billing")
	mock.ExpectQuery("SELECT").WillReturnRows(docs)
	mock.ExpectQuery("SELECT").WillReturnRows(chunks)

	src := knowledge.NewStoreTierSource(store)
	rows, err := src.TierCandidates(context.Background(), "org-1", 100)
	if err != nil {
		t.Fatalf("TierCandidates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].DocumentMetadata["team"] != "billing" || rows[0].DocumentSource != "confluence/billing" {
		t.Fatalf("join failed: %+v", rows[0])
	}
	if rows[0].DocumentCreatedAt != at {
		t.Fatalf("DocumentCreatedAt = %v, want %v", rows[0].DocumentCreatedAt, at)
	}
}

// ---------------------------------------------------------------------------
// Classifier convention
// ---------------------------------------------------------------------------

func TestDefaultTierClassifier(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	cls := knowledge.DefaultTierClassifier{}
	base := tierChunk("org-1", "doc", "c", "T", "content", 0)

	cases := []struct {
		name string
		meta map[string]any
		want knowledge.TierVisibility
	}{
		{"no metadata", nil, knowledge.VisibilityActive},
		{"archived bool", map[string]any{"archived": true}, knowledge.VisibilityArchived},
		{"archived string", map[string]any{"archived": "true"}, knowledge.VisibilityArchived},
		{"status archived", map[string]any{"status": "Archived"}, knowledge.VisibilityArchived},
		{"status expired", map[string]any{"status": "expired"}, knowledge.VisibilityExpired},
		{"expired past", map[string]any{"expires_at": "2025-01-01T00:00:00Z"}, knowledge.VisibilityExpired},
		{"expiry future", map[string]any{"expires_at": "2099-01-01T00:00:00Z"}, knowledge.VisibilityActive},
		{"expiry garbage", map[string]any{"expires_at": "not-a-date"}, knowledge.VisibilityActive},
		{"archived wins over expiry", map[string]any{"archived": true, "expires_at": "2020-01-01T00:00:00Z"}, knowledge.VisibilityArchived},
	}
	for _, tc := range cases {
		base.DocumentMetadata = tc.meta
		if got := cls.Classify(base, now); got != tc.want {
			t.Fatalf("%s: Classify = %v, want %v", tc.name, got, tc.want)
		}
	}
}
