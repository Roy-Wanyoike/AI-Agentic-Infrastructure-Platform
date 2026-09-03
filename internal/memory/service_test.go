package memory

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func testService() *Service {
	svc := NewService()
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }
	return svc
}

// stubEmbedder returns canned vectors so ranking tests are exact.
type stubEmbedder struct {
	vectors map[string][]float64
}

func (s *stubEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	if v, ok := s.vectors[text]; ok {
		return append([]float64(nil), v...), nil
	}
	return nil, nil
}

func TestServicePutListRoundTrip(t *testing.T) {
	svc := testService()
	future := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	put, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		AgentID: "agent-1",
		Snippets: []SnippetInput{
			{Scope: ScopeShortTerm, Content: "user prefers concise answers", Importance: 0.9, ExpiresAt: &future},
			{Content: "user works at Acme on billing"}, // default scope long_term
		},
	})
	if err != nil {
		t.Fatalf("PutSnippets returned error: %v", err)
	}
	if len(put) != 2 {
		t.Fatalf("expected 2 snippets, got %d", len(put))
	}
	if put[0].Scope != ScopeShortTerm || put[0].Importance != 0.9 {
		t.Fatalf("snippet not normalized: %+v", put[0])
	}
	if put[1].Scope != ScopeLongTerm {
		t.Fatalf("default scope should be long_term, got %q", put[1].Scope)
	}
	if put[0].ID == "" || put[0].ID == put[1].ID {
		t.Fatalf("snippets need distinct ids: %v %v", put[0].ID, put[1].ID)
	}
	if len(put[0].Embedding) != EmbeddingDim {
		t.Fatalf("service should embed content offline, got dim %d", len(put[0].Embedding))
	}

	// Agent filter + newest-first ordering.
	list, err := svc.ListSnippets(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("ListSnippets returned error: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 snippets for agent-1, got %d", len(list))
	}
	if list[0].CreatedAt.Before(list[1].CreatedAt) {
		t.Fatalf("listing should be newest first")
	}

	// Unfiltered listing returns the whole org.
	all, err := svc.ListSnippets(context.Background(), "org-1", "")
	if err != nil {
		t.Fatalf("ListSnippets(all) returned error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 org snippets, got %d", len(all))
	}

	// Org-level memory (agent_id empty) is a separate scope.
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		Snippets: []SnippetInput{{Content: "org-wide policy note"}},
	}); err != nil {
		t.Fatalf("org-level PutSnippets returned error: %v", err)
	}
	orgLevel, err := svc.ListSnippets(context.Background(), "org-1", "")
	if err != nil {
		t.Fatalf("relist returned error: %v", err)
	}
	if len(orgLevel) != 3 {
		t.Fatalf("expected 3 org snippets, got %d", len(orgLevel))
	}
}

func TestServicePutReplacesPreviousSet(t *testing.T) {
	svc := testService()
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		AgentID:  "agent-1",
		Snippets: []SnippetInput{{Content: "old fact"}},
	}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		AgentID:  "agent-1",
		Snippets: []SnippetInput{{Content: "new fact"}, {Content: "another fact"}},
	}); err != nil {
		t.Fatalf("second put: %v", err)
	}
	list, _ := svc.ListSnippets(context.Background(), "org-1", "agent-1")
	if len(list) != 2 {
		t.Fatalf("PUT should replace, got %d snippets", len(list))
	}
	for _, sn := range list {
		if sn.Content == "old fact" {
			t.Fatal("old snippet survived a replacing PUT")
		}
	}
	// Other agents are untouched.
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		AgentID:  "agent-2",
		Snippets: []SnippetInput{{Content: "agent-2 fact"}},
	}); err != nil {
		t.Fatalf("agent-2 put: %v", err)
	}
	agent1, _ := svc.ListSnippets(context.Background(), "org-1", "agent-1")
	if len(agent1) != 2 {
		t.Fatalf("agent-1 snippets changed by agent-2 put: %d", len(agent1))
	}
}

func TestServiceShortTermExpiryHonored(t *testing.T) {
	svc := newService(nil, &stubEmbedder{vectors: map[string][]float64{
		"scratch note":         {1, 0, 0},
		"live scratch note":    {1, 0, 0},
		"expired scratch note": {1, 0, 0},
	}})
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }
	past := base.Add(-24 * time.Hour)
	future := base.Add(24 * time.Hour)
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		Snippets: []SnippetInput{
			{Scope: ScopeShortTerm, Content: "expired scratch note", ExpiresAt: &past},
			{Scope: ScopeShortTerm, Content: "live scratch note", ExpiresAt: &future},
			{Content: "durable long-term fact"},
		},
	}); err != nil {
		t.Fatalf("PutSnippets: %v", err)
	}
	list, _ := svc.ListSnippets(context.Background(), "org-1", "")
	if len(list) != 2 {
		t.Fatalf("expired snippet must not be listed, got %d", len(list))
	}
	scored, err := svc.Retrieve(context.Background(), "org-1", "scratch note", RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(scored) != 1 || scored[0].Snippet.Content != "live scratch note" {
		t.Fatalf("retrieval must skip expired snippets, got %+v", scored)
	}
	if scored[0].Score != 1 {
		t.Fatalf("identical vectors should score 1, got %.3f", scored[0].Score)
	}
}

func TestServiceRetrieveCosineOrdering(t *testing.T) {
	vPlan := []float64{1, 1, 0, 0}
	vInvoices := []float64{0, 1, 1, 0}
	vUnrelated := []float64{0, 0, 0, 1}
	vQuery := []float64{1, 0.5, 0, 0}
	svc := newService(nil, &stubEmbedder{vectors: map[string][]float64{
		"The customer's plan is enterprise with SSO enabled": vPlan,
		"The customer prefers email invoices monthly":        vInvoices,
		"Completely unrelated gardening tips about roses":    vUnrelated,
		"customer plan enterprise SSO":                       vQuery,
	}})
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		Snippets: []SnippetInput{
			{Content: "The customer's plan is enterprise with SSO enabled"},
			{Content: "The customer prefers email invoices monthly"},
			{Content: "Completely unrelated gardening tips about roses"},
		},
	}); err != nil {
		t.Fatalf("PutSnippets: %v", err)
	}
	scored, err := svc.Retrieve(context.Background(), "org-1", "customer plan enterprise SSO", RetrieveOptions{K: 2})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(scored) != 2 {
		t.Fatalf("k=2 should return 2 results, got %d", len(scored))
	}
	if scored[0].Snippet.Content != "The customer's plan is enterprise with SSO enabled" {
		t.Fatalf("cosine ranking mismatch, top hit: %q (score %.3f)", scored[0].Snippet.Content, scored[0].Score)
	}
	if scored[0].Score < scored[1].Score {
		t.Fatalf("results must be score-ordered: %.3f then %.3f", scored[0].Score, scored[1].Score)
	}
	want := cosineSimilarity(vQuery, vPlan)
	if math.Abs(scored[0].Score-want) > 1e-9 {
		t.Fatalf("cosine score mismatch: got %.6f want %.6f", scored[0].Score, want)
	}
}

func TestServiceRetrieveScopesAgentPlusOrgShared(t *testing.T) {
	vShared := []float64{1, 0, 0}
	vAgent := []float64{1, 0, 0}
	vAgent2 := []float64{0, 1, 0}
	vQuery := []float64{1, 0, 0}
	svc := newService(nil, &stubEmbedder{vectors: map[string][]float64{
		"shared org fact about billing":          vShared,
		"billing escalation contact is Dana":     vAgent,
		"unrelated agent-2 note about zk proofs": vAgent2,
		"billing":                                vQuery,
	}})
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		Snippets: []SnippetInput{{Content: "shared org fact about billing"}},
	}); err != nil {
		t.Fatalf("org-level put: %v", err)
	}
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		AgentID:  "agent-1",
		Snippets: []SnippetInput{{Content: "billing escalation contact is Dana"}},
	}); err != nil {
		t.Fatalf("agent put: %v", err)
	}
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		AgentID:  "agent-2",
		Snippets: []SnippetInput{{Content: "unrelated agent-2 note about zk proofs"}},
	}); err != nil {
		t.Fatalf("agent-2 put: %v", err)
	}
	scored, err := svc.Retrieve(context.Background(), "org-1", "billing", RetrieveOptions{AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(scored) != 2 {
		t.Fatalf("agent scope should include org-level memory and exclude other agents, got %+v", scored)
	}
	seenAgent := map[string]bool{}
	for _, hit := range scored {
		seenAgent[hit.Snippet.AgentID] = true
		if hit.Score != 1 {
			t.Fatalf("identical vectors should score 1, got %.3f", hit.Score)
		}
	}
	if !seenAgent["agent-1"] || !seenAgent[""] {
		t.Fatalf("expected the agent hit plus org-level shared memory, got %+v", seenAgent)
	}

	// No agent filter: whole organization scores (still no agent-2 hit:
	// its vector is orthogonal to the query).
	all, err := svc.Retrieve(context.Background(), "org-1", "billing", RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("org-wide retrieval expected 2 hits, got %d", len(all))
	}
}

func TestServiceRetrieveLexicalFallbackForForeignVectors(t *testing.T) {
	svc := newService(nil, &stubEmbedder{vectors: map[string][]float64{}})
	foreign := make([]float64, 8) // client-supplied embedding in another space
	// One org-level PUT carries both: a client-supplied foreign-space vector
	// and a snippet without any vector at all (the stub embedder returns
	// none, and it shares no tokens with the query).
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		Snippets: []SnippetInput{
			{Content: "billing escalation contact is Dana", Embedding: foreign},
			{Content: "summary of the weekly report"},
		},
	}); err != nil {
		t.Fatalf("PutSnippets: %v", err)
	}
	scored, err := svc.Retrieve(context.Background(), "org-1", "billing dana", RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(scored) != 1 || scored[0].Score <= 0 {
		t.Fatalf("lexical fallback should still match, got %+v", scored)
	}
	if scored[0].Snippet.Content != "billing escalation contact is Dana" {
		t.Fatalf("lexical match picked the wrong snippet: %q", scored[0].Snippet.Content)
	}
}

func TestServiceTenantIsolation(t *testing.T) {
	svc := testService()
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		Snippets: []SnippetInput{{Content: "secret of org-1"}},
	}); err != nil {
		t.Fatalf("PutSnippets: %v", err)
	}
	list, err := svc.ListSnippets(context.Background(), "org-2", "")
	if err != nil {
		t.Fatalf("ListSnippets: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("cross-tenant leak: %+v", list)
	}
	scored, err := svc.Retrieve(context.Background(), "org-2", "secret", RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(scored) != 0 {
		t.Fatalf("cross-tenant retrieval leak: %+v", scored)
	}
}

func TestServiceValidation(t *testing.T) {
	svc := testService()
	ctx := context.Background()
	if _, err := svc.PutSnippets(ctx, "", PutRequest{}); err == nil {
		t.Fatal("empty org id must be rejected")
	}
	if _, err := svc.PutSnippets(ctx, "org-1", PutRequest{Snippets: []SnippetInput{{Content: "  "}}}); err == nil {
		t.Fatal("blank content must be rejected")
	}
	if _, err := svc.PutSnippets(ctx, "org-1", PutRequest{Snippets: []SnippetInput{{Content: "x", Scope: "forever"}}}); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("invalid scope must surface ErrInvalidScope, got %v", err)
	}
	if _, err := svc.Retrieve(ctx, "org-1", "   ", RetrieveOptions{}); err == nil {
		t.Fatal("blank query must be rejected")
	}
	if _, err := svc.Retrieve(ctx, "", "q", RetrieveOptions{}); err == nil {
		t.Fatal("empty org id must be rejected on retrieve")
	}
	// Importance clamping.
	put, _ := svc.PutSnippets(ctx, "org-1", PutRequest{Snippets: []SnippetInput{{Content: "clamped", Importance: 42}}})
	if put[0].Importance != 1 {
		t.Fatalf("importance must clamp to [0,1], got %v", put[0].Importance)
	}
}

// fakeStore captures store interactions for the dual-mode service tests.
type fakeStore struct {
	replaced   []string // "org|agent" keys
	listedOrg  string
	listedAgnt string
	rows       []*Snippet
	fail       error
}

func (f *fakeStore) ReplaceAgentSnippets(_ context.Context, orgID, agentID string, snippets []*Snippet) error {
	if f.fail != nil {
		return f.fail
	}
	f.replaced = append(f.replaced, orgID+"|"+agentID)
	f.rows = snippets
	return nil
}

func (f *fakeStore) ListSnippets(_ context.Context, orgID, agentID string) ([]*Snippet, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.listedOrg, f.listedAgnt = orgID, agentID
	return f.rows, nil
}

func (f *fakeStore) ListSnippetsForAgent(_ context.Context, orgID, agentID string) ([]*Snippet, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.listedOrg, f.listedAgnt = orgID, agentID+"+shared"
	return f.rows, nil
}

func TestServiceWithStoreUsesStore(t *testing.T) {
	store := &fakeStore{}
	svc := newService(store, NewHashEmbedder())
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }

	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{
		AgentID:  "agent-1",
		Snippets: []SnippetInput{{Content: "persisted fact"}},
	}); err != nil {
		t.Fatalf("PutSnippets: %v", err)
	}
	if len(store.replaced) != 1 || store.replaced[0] != "org-1|agent-1" {
		t.Fatalf("store replacement not invoked correctly: %v", store.replaced)
	}
	if len(store.rows) != 1 || store.rows[0].Embedding == nil {
		t.Fatalf("store should receive embedded rows: %+v", store.rows)
	}

	store.rows = []*Snippet{{ID: "sn-1", OrganizationID: "org-1", AgentID: "agent-1", Scope: ScopeLongTerm, Content: "persisted fact", CreatedAt: base, UpdatedAt: base}}
	list, err := svc.ListSnippets(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("ListSnippets: %v", err)
	}
	if len(list) != 1 || list[0].ID != "sn-1" {
		t.Fatalf("listing should come from the store: %+v", list)
	}
	if store.listedAgnt != "agent-1" {
		t.Fatalf("list should pass the agent filter through, got %q", store.listedAgnt)
	}

	scored, err := svc.Retrieve(context.Background(), "org-1", "persisted", RetrieveOptions{AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if store.listedAgnt != "agent-1+shared" {
		t.Fatalf("retrieval should use the agent+shared candidate query, got %q", store.listedAgnt)
	}
	if len(scored) != 1 || scored[0].Score <= 0 {
		t.Fatalf("lexical fallback should match the stored snippet, got %+v", scored)
	}

	store.fail = errors.New("db down")
	if _, err := svc.PutSnippets(context.Background(), "org-1", PutRequest{Snippets: []SnippetInput{{Content: "x"}}}); err == nil {
		t.Fatal("store failure must propagate")
	}
}
