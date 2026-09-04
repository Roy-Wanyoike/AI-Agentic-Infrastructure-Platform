package marketplace

// In-memory service tests: the publish -> browse -> install round trip,
// cross-org isolation, slug uniqueness, install name-collision suffixing,
// permission-relevant status semantics and keyset pagination. The Postgres
// variants of the round trip live in store_test.go (sqlmock).

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentos/internal/agents"
)

// fakeAgents is a controllable AgentsDomain for snapshot-validation and
// error-path tests (the happy-path tests use the real *agents.Service).
type fakeAgents struct {
	orgAgents map[string][]*agents.Agent // orgID -> agents
}

func (f *fakeAgents) GetAgentCtx(_ context.Context, orgID, agentID string) (*agents.Agent, error) {
	for _, agent := range f.orgAgents[orgID] {
		if agent.ID == agentID {
			return agent, nil
		}
	}
	return nil, agents.ErrAgentNotFound
}

func (f *fakeAgents) ListAgentsCtx(_ context.Context, orgID string) ([]*agents.Agent, error) {
	return append([]*agents.Agent(nil), f.orgAgents[orgID]...), nil
}

func (f *fakeAgents) CreateAgentCtx(_ context.Context, orgID, name, description, instructions, model string) (*agents.Agent, error) {
	agent := &agents.Agent{
		ID:             "agent-" + name,
		OrganizationID: orgID,
		Name:           name,
		Description:    description,
		Instructions:   instructions,
		Model:          model,
		Status:         "DRAFT",
	}
	f.orgAgents[orgID] = append(f.orgAgents[orgID], agent)
	return agent, nil
}

// fakeVersions is a controllable VersionReader.
type fakeVersions struct {
	snapshot string
	err      error
}

func (f *fakeVersions) GetVersionCtx(_ context.Context, _, _ string, _ int) (*agents.ConfigVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &agents.ConfigVersion{ID: "cv-1", AgentID: "agent-1", Version: 2, Snapshot: f.snapshot, Status: agents.VersionStatusPublished}, nil
}

// newFixture wires the real agents + versions services (memory mode) and a
// marketplace on top — the closest to the production in-memory graph.
func newFixture(t *testing.T) (*Service, *agents.Service, *agents.VersionsService) {
	t.Helper()
	agentsSvc := agents.NewService()
	versionsSvc := agents.NewVersionsService(agentsSvc)
	svc := NewServiceWithStore(nil, agentsSvc, versionsSvc)
	return svc, agentsSvc, versionsSvc
}

// createSourceAgent creates the publisher-side agent (legacy v1) and
// snapshots a modified v2 config version, returning (agentID, versionNumber).
func createSourceAgent(t *testing.T, agentsSvc *agents.Service, versionsSvc *agents.VersionsService, orgID string) (string, int) {
	t.Helper()
	ctx := context.Background()
	agent, err := agentsSvc.CreateAgentCtx(ctx, orgID, "Support Bot", "Helps customers", "Be polite", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	agent.Instructions = "Be extremely polite and concise"
	if err := agentsSvc.UpdateAgentCtx(ctx, orgID, agent); err != nil {
		t.Fatalf("UpdateAgentCtx: %v", err)
	}
	v2, err := versionsSvc.CreateVersionCtx(ctx, orgID, agent.ID, "publisher-user")
	if err != nil {
		t.Fatalf("CreateVersionCtx: %v", err)
	}
	return agent.ID, v2.Version
}

func TestRoundTripPublishBrowseGetInstallAcrossOrgs(t *testing.T) {
	svc, agentsSvc, versionsSvc := newFixture(t)
	ctx := context.Background()
	agentID, v2 := createSourceAgent(t, agentsSvc, versionsSvc, "org-a")

	// Publish from the immutable v2 snapshot.
	listing, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{
		AgentID: agentID,
		Version: v2,
		Name:    "Support Bot Template",
		Tags:    []string{"Support", "RAG"},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if listing.Status != StatusPublished || listing.Slug != "support-bot-template" {
		t.Fatalf("unexpected listing: %+v", listing)
	}
	if listing.DownloadCount != 0 {
		t.Fatalf("fresh listing must have download_count 0, got %d", listing.DownloadCount)
	}

	// Browse is GLOBAL: org-b's owner sees org-a's listing.
	page, next, err := svc.Browse(ctx, BrowseOptions{})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if next != "" || len(page) != 1 || page[0].Slug != "support-bot-template" {
		t.Fatalf("unexpected browse page: %+v next=%q", page, next)
	}

	// Get by slug works cross-org for published listings.
	got, err := svc.GetBySlug(ctx, "org-b", "support-bot-template")
	if err != nil {
		t.Fatalf("GetBySlug (foreign org): %v", err)
	}
	if got.ID != listing.ID {
		t.Fatalf("GetBySlug returned a different listing")
	}

	// Install into org-b: a NEW agent is created there from the snapshot.
	result, err := svc.Install(ctx, "org-b", "support-bot-template")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	installed := result.Agent
	if installed.OrganizationID != "org-b" {
		t.Fatalf("install must create the agent in the CALLER org, got %q", installed.OrganizationID)
	}
	if installed.Instructions != "Be extremely polite and concise" || installed.Model != "gpt-4o-mini" {
		t.Fatalf("install did not replay the snapshot config: %+v", installed)
	}
	if installed.Status != "DRAFT" {
		t.Fatalf("installed agent must start in the agents service initial state, got %q", installed.Status)
	}
	if result.Listing.DownloadCount != 1 {
		t.Fatalf("install must increment download_count, got %d", result.Listing.DownloadCount)
	}

	// Cross-org isolation: org-b's agent list NEVER contains org-a's agents.
	orgBAgents, err := agentsSvc.ListAgentsCtx(ctx, "org-b")
	if err != nil {
		t.Fatalf("ListAgentsCtx(org-b): %v", err)
	}
	if len(orgBAgents) != 1 || orgBAgents[0].ID != installed.ID {
		t.Fatalf("org-b must only contain the installed agent, got %+v", orgBAgents)
	}
	orgAAgents, err := agentsSvc.ListAgentsCtx(ctx, "org-a")
	if err != nil {
		t.Fatalf("ListAgentsCtx(org-a): %v", err)
	}
	if len(orgAAgents) != 1 || orgAAgents[0].ID != agentID {
		t.Fatalf("org-a must only contain its own source agent, got %+v", orgAAgents)
	}

	// Self-install (publisher org) is allowed: templating your own agent;
	// the download counter accumulates across installs. The snapshot name
	// collides with the source agent itself in org-a, so the deterministic
	// suffix applies here too.
	selfResult, err := svc.Install(ctx, "org-a", "support-bot-template")
	if err != nil {
		t.Fatalf("self-install: %v", err)
	}
	if selfResult.Agent.OrganizationID != "org-a" || selfResult.Agent.Name != "Support Bot-2" {
		t.Fatalf("unexpected self-install result: %+v", selfResult.Agent)
	}
	if selfResult.Listing.DownloadCount != 2 {
		t.Fatalf("download_count must accumulate (1 -> 2), got %d", selfResult.Listing.DownloadCount)
	}
}

func TestPublishLiveConfigDefaultsToSnapshotDescription(t *testing.T) {
	svc, agentsSvc, _ := newFixture(t)
	ctx := context.Background()
	agent, err := agentsSvc.CreateAgentCtx(ctx, "org-a", "RAG Helper", "Answer from docs", "Cite sources", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	listing, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agent.ID, Name: "RAG Helper!"})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if listing.Slug != "rag-helper" {
		t.Fatalf("slug derivation failed: %q", listing.Slug)
	}
	if listing.Description != "Answer from docs" {
		t.Fatalf("description must default to the snapshot description, got %q", listing.Description)
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(listing.VersionSnapshot), &snap); err != nil {
		t.Fatalf("snapshot not valid json: %v", err)
	}
	if snap.Name != "RAG Helper" || snap.Instructions != "Cite sources" || snap.Status != "DRAFT" {
		t.Fatalf("unexpected live snapshot: %+v", snap)
	}
}

func TestPublishCrossTenantAgentIsAgentNotFound(t *testing.T) {
	svc, agentsSvc, _ := newFixture(t)
	ctx := context.Background()
	agent, err := agentsSvc.CreateAgentCtx(ctx, "org-a", "Secret Bot", "", "internal steps", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	if _, err := svc.Publish(ctx, "org-b", "user-b", PublishInput{AgentID: agent.ID, Name: "Stolen"}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("cross-tenant publish must be ErrAgentNotFound, got %v", err)
	}
}

func TestSlugUniquenessIsGlobal(t *testing.T) {
	svc, agentsSvc, _ := newFixture(t)
	ctx := context.Background()
	agent, err := agentsSvc.CreateAgentCtx(ctx, "org-a", "Bot One", "", "be one", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	other, err := agentsSvc.CreateAgentCtx(ctx, "org-a", "Bot Two", "", "be two", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx(2): %v", err)
	}
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agent.ID, Name: "Duplicate"}); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// Same slug via name derivation (case-insensitive) in the SAME org.
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: other.ID, Name: "DUPLICATE"}); !errors.Is(err, ErrDuplicateSlug) {
		t.Fatalf("expected ErrDuplicateSlug, got %v", err)
	}
	// Explicit slug collision from a DIFFERENT org (slug namespace is global).
	foreign, err := agentsSvc.CreateAgentCtx(ctx, "org-b", "Bot Three", "", "be three", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx(3): %v", err)
	}
	if _, err := svc.Publish(ctx, "org-b", "user-b", PublishInput{AgentID: foreign.ID, Name: "Whatever", Slug: "duplicate"}); !errors.Is(err, ErrDuplicateSlug) {
		t.Fatalf("cross-org slug collision must fail, got %v", err)
	}
}

func TestSlugValidation(t *testing.T) {
	for slug, ok := range map[string]bool{
		"a": true, "support-bot": true, "bot-123": true, "double--hyphen": true,
		strings.Repeat("a", MaxSlugLen): true,
		"":                              false, "-lead": false, "trail-": false, "has space": false, "UPPER": false,
		"under_score": false, strings.Repeat("a", MaxSlugLen+1): false,
	} {
		if got := validSlug(slug); got != ok {
			t.Errorf("validSlug(%q) = %v, want %v", slug, got, ok)
		}
	}
	for name, want := range map[string]string{
		"RAG Helper!":            "rag-helper",
		"  --Weird__Name!!--":    "weird-name",
		"123 456":                "123-456",
		"!!!":                    "agent",
		strings.Repeat("x", 200): strings.Repeat("x", MaxSlugLen),
	} {
		if got := Slugify(name); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestInstallNameCollisionDeterministicSuffix(t *testing.T) {
	svc, agentsSvc, _ := newFixture(t)
	ctx := context.Background()
	agent, err := agentsSvc.CreateAgentCtx(ctx, "org-a", "Copilot", "", "pair program", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agent.ID, Name: "Copilot"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// org-b already owns an agent with the snapshot's name.
	if _, err := agentsSvc.CreateAgentCtx(ctx, "org-b", "Copilot", "", "org-b original", "gpt-4o-mini"); err != nil {
		t.Fatalf("CreateAgentCtx(org-b): %v", err)
	}

	first, err := svc.Install(ctx, "org-b", "copilot")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if first.Agent.Name != "Copilot-2" {
		t.Fatalf("expected deterministic suffix Copilot-2, got %q", first.Agent.Name)
	}
	second, err := svc.Install(ctx, "org-b", "copilot")
	if err != nil {
		t.Fatalf("Install(2): %v", err)
	}
	if second.Agent.Name != "Copilot-3" {
		t.Fatalf("expected deterministic suffix Copilot-3, got %q", second.Agent.Name)
	}
	// The originals are untouched and the installs are separate agents.
	// The in-memory agents service lists in map iteration order, so the
	// assertion must be order-independent (a bare orgBAgents[0] check
	// flakes across runs).
	orgBAgents, _ := agentsSvc.ListAgentsCtx(ctx, "org-b")
	names := make(map[string]int, len(orgBAgents))
	for _, a := range orgBAgents {
		names[a.Name]++
	}
	if len(orgBAgents) != 3 || names["Copilot"] != 1 || names["Copilot-2"] != 1 || names["Copilot-3"] != 1 {
		t.Fatalf("unexpected org-b agent names: %v (counts=%v)", keysOf(names), names)
	}
}

// keysOf renders a name-count map in sorted order for stable failure output.
func keysOf(counts map[string]int) []string {
	out := make([]string, 0, len(counts))
	for name := range counts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestInstallCollisionExhaustion(t *testing.T) {
	domain := &fakeAgents{orgAgents: map[string][]*agents.Agent{}}
	// Pre-create the base name and every deterministic suffix candidate.
	domain.orgAgents["org-b"] = append(domain.orgAgents["org-b"], &agents.Agent{ID: "a-0", OrganizationID: "org-b", Name: "Bot"})
	for i := 2; i < 2+maxNameSuffixAttempts; i++ {
		name := "Bot-" + strconv.Itoa(i)
		domain.orgAgents["org-b"] = append(domain.orgAgents["org-b"], &agents.Agent{ID: name, OrganizationID: "org-b", Name: name})
	}
	svc := NewService(domain)
	svc.mu.Lock()
	svc.items["l-1"] = &Listing{
		ID: "l-1", PublisherOrgID: "org-a", Slug: "bot", Status: StatusPublished,
		VersionSnapshot: `{"name":"Bot","instructions":"x","model":"m"}`,
	}
	svc.mu.Unlock()
	if _, err := svc.Install(context.Background(), "org-b", "bot"); !errors.Is(err, ErrNameCollision) {
		t.Fatalf("expected ErrNameCollision, got %v", err)
	}
}

func TestDraftAndUnlistedVisibility(t *testing.T) {
	svc, agentsSvc, _ := newFixture(t)
	ctx := context.Background()
	agent, err := agentsSvc.CreateAgentCtx(ctx, "org-a", "WIP Bot", "", "not ready", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	draft, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agent.ID, Name: "WIP Bot", Status: StatusDraft})
	if err != nil {
		t.Fatalf("Publish(draft): %v", err)
	}

	// Drafts never appear in the global browse — not even for the publisher.
	page, _, err := svc.Browse(ctx, BrowseOptions{})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("drafts must not be browseable, got %+v", page)
	}
	// Foreign orgs cannot see or install drafts (no existence leak).
	if _, err := svc.GetBySlug(ctx, "org-b", draft.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign draft get must be ErrNotFound, got %v", err)
	}
	if _, err := svc.Install(ctx, "org-b", draft.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign draft install must be ErrNotFound, got %v", err)
	}
	// The publisher still sees its own draft by point lookup, but installing
	// it surfaces the distinct ErrNotPublished.
	if _, err := svc.GetBySlug(ctx, "org-a", draft.Slug); err != nil {
		t.Fatalf("publisher draft get: %v", err)
	}
	if _, err := svc.Install(ctx, "org-a", draft.Slug); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("publisher draft install must be ErrNotPublished, got %v", err)
	}

	// Unlist: publisher org only; then the listing disappears from browse and
	// from foreign point lookups, but stays visible to the publisher.
	published, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agent.ID, Name: "WIP Bot Two"})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := svc.Unlist(ctx, "org-b", published.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign unlist must be ErrNotFound, got %v", err)
	}
	if err := svc.Unlist(ctx, "org-a", published.Slug); err != nil {
		t.Fatalf("publisher unlist: %v", err)
	}
	page, _, err = svc.Browse(ctx, BrowseOptions{})
	if err != nil {
		t.Fatalf("Browse(2): %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("unlisted listings must leave the catalog, got %+v", page)
	}
	if _, err := svc.GetBySlug(ctx, "org-b", published.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign unlisted get must be ErrNotFound, got %v", err)
	}
	if _, err := svc.Install(ctx, "org-b", published.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign unlisted install must be ErrNotFound, got %v", err)
	}
	got, err := svc.GetBySlug(ctx, "org-a", published.Slug)
	if err != nil {
		t.Fatalf("publisher unlisted get: %v", err)
	}
	if got.Status != StatusUnlisted {
		t.Fatalf("unlisted status not persisted: %+v", got)
	}
}

func TestBrowseTextAndTagFilters(t *testing.T) {
	svc, agentsSvc, _ := newFixture(t)
	ctx := context.Background()
	for _, spec := range []struct {
		name        string
		description string
		tags        []string
	}{
		{"SQL Assistant", "writes postgres queries", []string{"sql", "database"}},
		{"RAG Helper", "answers from documents", []string{"rag"}},
		{"Summarizer", "summarizes long documents", nil},
	} {
		agent, err := agentsSvc.CreateAgentCtx(ctx, "org-a", spec.name, spec.description, "do it", "gpt-4o-mini")
		if err != nil {
			t.Fatalf("CreateAgentCtx: %v", err)
		}
		if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agent.ID, Name: spec.name, Tags: spec.tags}); err != nil {
			t.Fatalf("Publish(%s): %v", spec.name, err)
		}
	}

	cases := []struct {
		opts BrowseOptions
		want []string
	}{
		{BrowseOptions{}, []string{"sql-assistant", "rag-helper", "summarizer"}},
		{BrowseOptions{Query: "DOC"}, []string{"rag-helper", "summarizer"}}, // name or description, case-insensitive
		{BrowseOptions{Query: "postgres queries"}, []string{"sql-assistant"}},
		{BrowseOptions{Tags: []string{"RAG"}}, []string{"rag-helper"}},
		{BrowseOptions{Tags: []string{"sql", "rag"}}, []string{"sql-assistant", "rag-helper"}}, // ANY-overlap
		{BrowseOptions{Query: "summarizes", Tags: []string{"rag"}}, nil},
		{BrowseOptions{Query: "no-such-thing"}, nil},
	}
	for _, tc := range cases {
		page, next, err := svc.Browse(ctx, tc.opts)
		if err != nil {
			t.Fatalf("Browse(%+v): %v", tc.opts, err)
		}
		got := make([]string, 0, len(page))
		for _, listing := range page {
			got = append(got, listing.Slug)
		}
		if next != "" {
			t.Errorf("Browse(%+v): small catalog must be exhausted, got next cursor", tc.opts)
		}
		if len(got) != len(tc.want) {
			t.Errorf("Browse(%+v) = %v, want %v", tc.opts, got, tc.want)
			continue
		}
		wantSet := map[string]bool{}
		for _, slug := range tc.want {
			wantSet[slug] = true
		}
		for _, slug := range got {
			if !wantSet[slug] {
				t.Errorf("Browse(%+v) contains unexpected %q", tc.opts, slug)
			}
		}
	}
}

func TestBrowseKeysetPaginationWalksEveryListingExactlyOnce(t *testing.T) {
	svc, agentsSvc, _ := newFixture(t)
	ctx := context.Background()
	want := map[string]bool{}
	for i := 1; i <= 5; i++ {
		name := "Paged Bot " + strconv.Itoa(i)
		agent, err := agentsSvc.CreateAgentCtx(ctx, "org-a", name, "", "run "+strconv.Itoa(i), "gpt-4o-mini")
		if err != nil {
			t.Fatalf("CreateAgentCtx: %v", err)
		}
		listing, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agent.ID, Name: name})
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		want[listing.Slug] = true
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, next, err := svc.Browse(ctx, BrowseOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("Browse page %d: %v", pages+1, err)
		}
		pages++
		if len(page) > 2 {
			t.Fatalf("page %d exceeded limit: %d", pages, len(page))
		}
		for _, listing := range page {
			if seen[listing.Slug] {
				t.Fatalf("listing %s appeared twice", listing.Slug)
			}
			seen[listing.Slug] = true
		}
		if next == "" {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("pagination saw %d listings, want %d", len(seen), len(want))
	}
	for slug := range want {
		if !seen[slug] {
			t.Fatalf("pagination missed %s", slug)
		}
	}
	if pages != 3 {
		t.Fatalf("expected 3 pages of 2, got %d", pages)
	}
}

func TestPageListingsOrderingAndExactFit(t *testing.T) {
	base := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	items := []*Listing{
		{ID: "a", CreatedAt: base.Add(time.Second)},
		{ID: "b", CreatedAt: base},
		{ID: "c", CreatedAt: base},
		{ID: "d", CreatedAt: base.Add(-time.Second)},
	}
	// Newest first; ties broken by id DESC.
	page, next, err := pageListings(items, BrowseOptions{Limit: 4})
	if err != nil {
		t.Fatalf("pageListings: %v", err)
	}
	var ids []string
	for _, l := range page {
		ids = append(ids, l.ID)
	}
	if strings.Join(ids, ",") != "a,c,b,d" {
		t.Fatalf("unexpected order: %v", ids)
	}
	if next != "" {
		t.Fatal("exact-fit page must emit no cursor")
	}

	// Truncated page: cursor points at the last returned row.
	page, next, err = pageListings(items, BrowseOptions{Limit: 2})
	if err != nil {
		t.Fatalf("pageListings(2): %v", err)
	}
	if len(page) != 2 || page[0].ID != "a" || page[1].ID != "c" {
		t.Fatalf("unexpected first page: %+v", page)
	}
	if next == "" {
		t.Fatal("truncated page must emit a cursor")
	}
	cursorTime, cursorID, err := decodeCursor(next)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if cursorID != "c" || !cursorTime.Equal(base) {
		t.Fatalf("cursor must encode the last row of the page, got %s", next)
	}

	// Invalid cursor.
	if _, _, err := pageListings(items, BrowseOptions{Limit: 2, Cursor: "not-a-cursor"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("expected ErrInvalidCursor, got %v", err)
	}
}

func TestPublishVersionSnapshotValidation(t *testing.T) {
	cases := []struct {
		name      string
		snapshot  string
		wantError error
	}{
		{"complete", `{"name":"n","instructions":"i","model":"m"}`, nil},
		{"missing model", `{"name":"n","instructions":"i"}`, ErrInvalidSnapshot},
		{"missing instructions", `{"name":"n","model":"m"}`, ErrInvalidSnapshot},
		{"missing name", `{"instructions":"i","model":"m"}`, ErrInvalidSnapshot},
		{"not an object", `["nope"]`, ErrInvalidSnapshot},
		{"empty", "", ErrInvalidSnapshot},
		{"extra config keys tolerated", `{"name":"n","instructions":"i","model":"m","tools":["search"],"params":{"temperature":0.2}}`, nil},
	}
	for _, tc := range cases {
		if _, err := validateSnapshot(tc.snapshot); !errors.Is(err, tc.wantError) {
			t.Errorf("%s: validateSnapshot error = %v, want %v", tc.name, err, tc.wantError)
		}
	}

	// The publish path rejects an incomplete version snapshot from a wired
	// versions source and reports the versions-source requirement when none
	// is wired.
	ctx := context.Background()
	domain := &fakeAgents{orgAgents: map[string][]*agents.Agent{
		"org-a": {{ID: "agent-1", OrganizationID: "org-a", Name: "n", Instructions: "i", Model: "m"}},
	}}
	svc := NewServiceWithStore(nil, domain, &fakeVersions{snapshot: `{"name":"n","instructions":"i"}`})
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: "agent-1", Version: 2, Name: "x"}); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("expected ErrInvalidSnapshot from publish, got %v", err)
	}
	noVersions := NewServiceWithStore(nil, domain, nil)
	if _, err := noVersions.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: "agent-1", Version: 2, Name: "x"}); !errors.Is(err, ErrVersionSourceUnavailable) {
		t.Fatalf("expected ErrVersionSourceUnavailable, got %v", err)
	}
}

func TestPublishInputValidation(t *testing.T) {
	svc, agentsSvc, _ := newFixture(t)
	ctx := context.Background()
	agent, err := agentsSvc.CreateAgentCtx(ctx, "org-a", "Valid Bot", "", "be valid", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	longTag := strings.Repeat("t", MaxTagLen+1)
	for _, tc := range []struct {
		name string
		in   PublishInput
		want error
	}{
		{"missing agent", PublishInput{Name: "x"}, ErrAgentRequired},
		{"unknown agent", PublishInput{AgentID: "nope", Name: "x"}, ErrAgentNotFound},
		{"missing name", PublishInput{AgentID: agent.ID}, ErrNameRequired},
		{"name too long", PublishInput{AgentID: agent.ID, Name: strings.Repeat("n", MaxNameLen+1)}, ErrNameTooLong},
		{"bad slug", PublishInput{AgentID: agent.ID, Name: "x", Slug: "Bad Slug"}, ErrSlugInvalid},
		{"desc too long", PublishInput{AgentID: agent.ID, Name: "x", Description: strings.Repeat("d", MaxDescriptionLen+1)}, ErrDescTooLong},
		{"too many tags", PublishInput{AgentID: agent.ID, Name: "x", Tags: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}}, ErrTooManyTags},
		{"tag too long", PublishInput{AgentID: agent.ID, Name: "x", Tags: []string{longTag}}, ErrTagTooLong},
		{"bad status", PublishInput{AgentID: agent.ID, Name: "x", Status: StatusUnlisted}, ErrStatusInvalid},
	} {
		if _, err := svc.Publish(ctx, "org-a", "user-a", tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: Publish error = %v, want %v", tc.name, err, tc.want)
		}
	}
	// Missing caller identity.
	if _, err := svc.Publish(ctx, "", "user-a", PublishInput{AgentID: agent.ID, Name: "x"}); !errors.Is(err, ErrOrgRequired) {
		t.Errorf("missing org: got %v", err)
	}
	if _, err := svc.Publish(ctx, "org-a", "", PublishInput{AgentID: agent.ID, Name: "x"}); !errors.Is(err, ErrUserRequired) {
		t.Errorf("missing user: got %v", err)
	}
	// Tag normalization: trimmed, lowercased, deduped.
	listing, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agent.ID, Name: "Tagged", Tags: []string{" RAG ", "rag", "SQL"}})
	if err != nil {
		t.Fatalf("Publish(tagged): %v", err)
	}
	if strings.Join(listing.Tags, ",") != "rag,sql" {
		t.Fatalf("tags not normalized: %v", listing.Tags)
	}
}

func TestInstallRequiresAgentsDomain(t *testing.T) {
	svc := NewService(nil)
	if _, err := svc.Install(context.Background(), "org-b", "whatever"); !errors.Is(err, ErrAgentsRequired) {
		t.Fatalf("expected ErrAgentsRequired, got %v", err)
	}
	if _, err := svc.Publish(context.Background(), "org-a", "user-a", PublishInput{AgentID: "a", Name: "x"}); !errors.Is(err, ErrAgentsRequired) {
		t.Fatalf("expected ErrAgentsRequired, got %v", err)
	}
}
