package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/knowledge"
	"agentos/internal/policies"
)

// searchCtx carries the org scope knowledge search needs.
func searchCtx() context.Context {
	return policies.WithRunScope(context.Background(), policies.RunScope{
		OrganizationID: "org-1",
		Environment:    "production",
	})
}

// fakeKnowledgeService is a deterministic KnowledgeSearcher recording calls.
type fakeKnowledgeService struct {
	gotOrg, gotQuery string
	gotK             int
	hits             []knowledge.SearchResult
	err              error
}

func (f *fakeKnowledgeService) Search(_ context.Context, orgID, query string, k int) ([]knowledge.SearchResult, error) {
	f.gotOrg, f.gotQuery, f.gotK = orgID, query, k
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

func TestSearchToolWebModeContract(t *testing.T) {
	var seenQuery, seenK, seenAuth, seenAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.Query().Get("q")
		seenK = r.URL.Query().Get("k")
		seenAuth = r.Header.Get("Authorization")
		seenAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
                        {"title":"AgentOS docs","url":"https://docs.example.com/a","snippet":"first","score":0.9},
                        {"title":"Second","link":"https://docs.example.com/b","description":"aliased fields","relevance":"0.4"}
                ]}`))
	}))
	defer server.Close()

	tool := NewSearchTool(SearchConfig{EndpointURL: server.URL, APIKey: "sk-test"}, nil)
	result, err := tool.ExecuteContext(searchCtx(), map[string]any{
		"query": "how do agents work", "k": float64(2), "mode": "web",
	})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	if seenQuery != "how do agents work" || seenK != "2" {
		t.Fatalf("unexpected request params q=%q k=%q", seenQuery, seenK)
	}
	if seenAuth != "Bearer sk-test" {
		t.Fatalf("expected bearer api key, got %q", seenAuth)
	}
	if seenAccept != "application/json" {
		t.Fatalf("expected Accept json, got %q", seenAccept)
	}

	results, _ := result["results"].([]SearchResult)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d: %#v", len(results), result["results"])
	}
	if results[0].Title != "AgentOS docs" || results[0].URL != "https://docs.example.com/a" ||
		results[0].Snippet != "first" || results[0].Score != 0.9 || results[0].Source != "web" {
		t.Fatalf("unexpected normalized result 0: %#v", results[0])
	}
	// Aliases (link/description/relevance) normalize into the canonical shape.
	if results[1].URL != "https://docs.example.com/b" || results[1].Snippet != "aliased fields" || results[1].Score != 0.4 {
		t.Fatalf("alias normalization failed: %#v", results[1])
	}
}

func TestSearchToolWebModeBareArrayAndDefaults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"title":"only","url":"https://x.example/1","snippet":"s","score":1.5}]`))
	}))
	defer server.Close()

	tool := NewSearchTool(SearchConfig{EndpointURL: server.URL}, nil)
	// No explicit mode: defaults to web when the endpoint is configured, and
	// web mode needs no organization scope.
	result, err := tool.ExecuteContext(context.Background(), map[string]any{"query": "q"})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	if result["mode"] != "web" {
		t.Fatalf("expected default mode web, got %#v", result["mode"])
	}
	results, _ := result["results"].([]SearchResult)
	if len(results) != 1 || results[0].Title != "only" {
		t.Fatalf("unexpected results: %#v", result["results"])
	}
}

func TestSearchToolWebModeErrorPaths(t *testing.T) {
	// 500 from the endpoint.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	tool := NewSearchTool(SearchConfig{EndpointURL: server.URL}, nil)
	if _, err := tool.ExecuteContext(searchCtx(), map[string]any{"query": "q", "mode": "web"}); err == nil {
		t.Fatal("expected endpoint error to surface")
	}

	// Non-JSON body.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer bad.Close()
	tool2 := NewSearchTool(SearchConfig{EndpointURL: bad.URL}, nil)
	if _, err := tool2.ExecuteContext(searchCtx(), map[string]any{"query": "q", "mode": "web"}); err == nil {
		t.Fatal("expected parse error")
	}

	// Unconfigured endpoint fails closed with ErrSearchUnavailable.
	tool3 := NewSearchTool(SearchConfig{}, nil)
	_, err := tool3.ExecuteContext(searchCtx(), map[string]any{"query": "q", "mode": "web"})
	if !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("expected ErrSearchUnavailable, got %v", err)
	}
	// ...and with no mode at all when nothing is configured.
	if _, err := tool3.ExecuteContext(searchCtx(), map[string]any{"query": "q"}); !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("expected ErrSearchUnavailable for default mode, got %v", err)
	}
}

func TestSearchToolKnowledgeMode(t *testing.T) {
	fake := &fakeKnowledgeService{hits: []knowledge.SearchResult{{
		DocumentID:    "doc-1",
		DocumentTitle: "Runbook",
		ChunkID:       "chunk-9",
		ChunkOrdinal:  3,
		Content:       "restart the worker",
		Score:         0.87,
		Citation:      "Runbook p3",
	}}}
	tool := NewSearchTool(SearchConfig{}, fake)
	result, err := tool.ExecuteContext(searchCtx(), map[string]any{"query": "restart", "mode": "knowledge", "k": float64(3)})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	if fake.gotOrg != "org-1" || fake.gotQuery != "restart" || fake.gotK != 3 {
		t.Fatalf("unexpected delegation org=%q query=%q k=%d", fake.gotOrg, fake.gotQuery, fake.gotK)
	}
	results, _ := result["results"].([]SearchResult)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %#v", results)
	}
	r := results[0]
	if r.Source != "knowledge" || r.ID != "chunk-9" || r.DocumentID != "doc-1" ||
		r.ChunkOrdinal != 3 || r.Citation != "Runbook p3" || r.Snippet != "restart the worker" {
		t.Fatalf("knowledge mapping wrong: %#v", r)
	}

	// Missing org scope fails closed (the knowledge service is tenant-scoped).
	_, err = tool.ExecuteContext(context.Background(), map[string]any{"query": "restart", "mode": "knowledge"})
	if err == nil || !strings.Contains(err.Error(), "organization scope") {
		t.Fatalf("expected org-scope refusal, got %v", err)
	}

	// Unwired knowledge backend fails closed.
	toolNone := NewSearchTool(SearchConfig{}, nil)
	if _, err := toolNone.ExecuteContext(searchCtx(), map[string]any{"query": "x", "mode": "knowledge"}); !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("expected ErrSearchUnavailable, got %v", err)
	}
}

func TestSearchToolHybridMergeRerankDedup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
                        {"title":"web top","url":"https://a.example/1","snippet":"w1","score":0.9},
                        {"title":"web dup","url":"https://b.example/1","snippet":"w2","score":0.6},
                        {"title":"web low","url":"https://c.example/1","snippet":"w3","score":0.2}
                ]}`))
	}))
	defer server.Close()

	fake := &fakeKnowledgeService{hits: []knowledge.SearchResult{
		{DocumentID: "d1", DocumentTitle: "dup", ChunkID: "k1", Content: "dup", Score: 0.8, Citation: "c1"},
		{DocumentID: "d2", DocumentTitle: "kn top", ChunkID: "k2", Content: "k2", Score: 0.95, Citation: "c2"},
	}}
	tool := NewSearchTool(SearchConfig{EndpointURL: server.URL}, fake)
	result, err := tool.ExecuteContext(searchCtx(), map[string]any{"query": "merge", "mode": "hybrid", "k": float64(3)})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	results, _ := result["results"].([]SearchResult)
	if len(results) != 3 {
		t.Fatalf("expected 3 merged results, got %#v", results)
	}
	// Deterministic re-rank: 0.95 (knowledge), 0.9 (web), 0.8 (knowledge).
	wantOrder := []string{"kn top", "web top", "dup"}
	for i, want := range wantOrder {
		if results[i].Title != want {
			t.Fatalf("result %d should be %q, got %q (all: %#v)", i, want, results[i].Title, results)
		}
	}
	// The deduped candidate (same URL 0.6) and the lowest (0.2) were cut by k=3.
	if result["count"] != 3 {
		t.Fatalf("expected count 3, got %#v", result["count"])
	}
	if len(result["warnings"].([]string)) != 0 {
		t.Fatalf("no warnings expected, got %#v", result["warnings"])
	}
}

func TestSearchToolHybridDedupsByIDWhenNoURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	fake := &fakeKnowledgeService{hits: []knowledge.SearchResult{
		{ChunkID: "same", Content: "first", Score: 0.5},
		{ChunkID: "same", Content: "second", Score: 0.4},
	}}
	tool := NewSearchTool(SearchConfig{EndpointURL: server.URL}, fake)
	result, err := tool.ExecuteContext(searchCtx(), map[string]any{"query": "q", "mode": "hybrid"})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	results, _ := result["results"].([]SearchResult)
	if len(results) != 1 || results[0].Snippet != "first" {
		t.Fatalf("chunk-id dedup failed: %#v", results)
	}
}

func TestSearchToolHybridDegradation(t *testing.T) {
	// Web down, knowledge fine -> degrade with warning.
	fake := &fakeKnowledgeService{hits: []knowledge.SearchResult{{ChunkID: "k1", Content: "c", Score: 1}}}
	tool := NewSearchTool(SearchConfig{EndpointURL: "http://127.0.0.1:1/unreachable"}, fake)
	result, err := tool.ExecuteContext(searchCtx(), map[string]any{"query": "q", "mode": "hybrid"})
	if err != nil {
		t.Fatalf("degradation expected, got error: %v", err)
	}
	warns, _ := result["warnings"].([]string)
	if len(warns) != 1 || !strings.Contains(warns[0], "web search failed") {
		t.Fatalf("expected one web warning, got %#v", warns)
	}
	results, _ := result["results"].([]SearchResult)
	if len(results) != 1 || results[0].Source != "knowledge" {
		t.Fatalf("expected knowledge fallback result, got %#v", results)
	}

	// Both down -> error.
	fake.err = errors.New("knowledge offline")
	_, err = tool.ExecuteContext(searchCtx(), map[string]any{"query": "q", "mode": "hybrid"})
	if err == nil || !strings.Contains(err.Error(), "both backends") {
		t.Fatalf("expected both-backends error, got %v", err)
	}
}

func TestSearchToolValidation(t *testing.T) {
	fake := &fakeKnowledgeService{}
	tool := NewSearchTool(SearchConfig{}, fake)
	cases := []struct {
		name  string
		input map[string]any
	}{
		{"missing query", map[string]any{}},
		{"empty query", map[string]any{"query": "   "}},
		{"query too long", map[string]any{"query": strings.Repeat("x", maxSearchQueryBytes+1)}},
		{"bad mode", map[string]any{"query": "q", "mode": "bing"}},
		{"non-string mode", map[string]any{"query": "q", "mode": 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.ExecuteContext(searchCtx(), tc.input); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
	// k clamps to MaxSearchK.
	if got := searchKArg(float64(9999)); got != MaxSearchK {
		t.Fatalf("k should clamp to %d, got %d", MaxSearchK, got)
	}
	if got := searchKArg(nil); got != DefaultSearchK {
		t.Fatalf("k default should be %d, got %d", DefaultSearchK, got)
	}
}

func TestSearchToolWebResponseSizeCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"` + strings.Repeat("x", 2<<20) + `"}]}`))
	}))
	defer server.Close()

	tool := NewSearchTool(SearchConfig{EndpointURL: server.URL}, nil)
	_, err := tool.ExecuteContext(searchCtx(), map[string]any{"query": "q", "mode": "web"})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected response size cap error, got %v", err)
	}
}

func TestSearchToolRoundTripJSONShape(t *testing.T) {
	// The normalized result shape must serialize exactly as documented.
	r := SearchResult{Title: "t", URL: "u", Snippet: "s", Score: 1, Source: "web"}
	blob, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	for _, key := range []string{"title", "url", "snippet", "score", "source"} {
		if _, ok := back[key]; !ok {
			t.Fatalf("documented field %q missing from JSON shape: %s", key, blob)
		}
	}
}
