package a2a

// service_test.go — card registry behavior: validation, CRUD, org
// isolation (404 semantics without an existence leak), PATCH semantics,
// the canonical ToJSON shape, and the inbound message path through the
// RunSink seam. Table-driven where a matrix makes sense.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/audit"
)

// --- stubs -----------------------------------------------------------------

// stubAgentChecker validates agents by "orgID/agentID" membership.
type stubAgentChecker struct {
	mu       sync.Mutex
	existing map[string]bool
	calls    int
}

func newStubAgentChecker(pairs ...string) *stubAgentChecker {
	m := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		m[p] = true
	}
	return &stubAgentChecker{existing: m}
}

func (c *stubAgentChecker) GetAgentCtx(_ context.Context, orgID, id string) (*agents.Agent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.existing[orgID+"/"+id] {
		return &agents.Agent{ID: id, OrganizationID: orgID}, nil
	}
	return nil, agents.ErrAgentNotFound
}

// stubAuditor records audit rows so tests can assert actions and the
// no-payload guarantee.
type stubAuditor struct {
	mu      sync.Mutex
	entries []*audit.Entry
}

func (a *stubAuditor) LogCtx(_ context.Context, actor, action, orgID, resource string, metadata map[string]any) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, &audit.Entry{
		ID:             "audit-" + action,
		Actor:          actor,
		Action:         action,
		OrganizationID: orgID,
		Resource:       resource,
		Metadata:       metadata,
	})
	return a.entries[len(a.entries)-1], nil
}

func (a *stubAuditor) actions() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]int{}
	for _, e := range a.entries {
		out[e.Action]++
	}
	return out
}

func (a *stubAuditor) contains(s string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries {
		raw, _ := json.Marshal(e.Metadata)
		if strings.Contains(e.Actor, s) || strings.Contains(e.Resource, s) || strings.Contains(string(raw), s) {
			return true
		}
	}
	return false
}

// stubGate is the scripted DelegationGate.
type stubGate struct {
	verdict DelegationVerdict
	calls   int
}

func (g *stubGate) EvaluateDelegation(_ context.Context, _ string, _ *AgentCard, _ string) DelegationVerdict {
	g.calls++
	return g.verdict
}

// stubSink records StartRun calls through the RunSink seam.
type stubSink struct {
	mu     sync.Mutex
	calls  []string // "orgID/agentID:message"
	runID  string
	taskID string
	err    error
}

func (s *stubSink) StartRun(_ context.Context, orgID, agentID, message string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", "", s.err
	}
	s.calls = append(s.calls, orgID+"/"+agentID+":"+message)
	if s.runID == "" && s.taskID == "" {
		return "run-1", "task-1", nil
	}
	return s.runID, s.taskID, nil
}

func validInput(agent string) CreateCardInput {
	return CreateCardInput{
		AgentID:     agent,
		Name:        "Weather Agent",
		Description: "Talks about weather",
		Version:     "1.0.0",
		Provider:    CardProvider{Organization: "Acme", URL: "https://acme.example"},
		EndpointURL: "https://agents.example/weather",
		Capabilities: Capabilities{
			Streaming:         true,
			PushNotifications: false,
		},
		Skills:     []Skill{{ID: "forecast", Name: "Forecast", Description: "5-day forecast", Tags: []string{"weather"}}},
		AuthScheme: "bearer",
		CreatedBy:  "user-1",
	}
}

// --- validation -------------------------------------------------------------

func TestCreateCardValidation(t *testing.T) {
	checker := newStubAgentChecker("org-1/agent-1")
	svc := NewService()
	svc.SetAgentChecker(checker)

	tests := []struct {
		name    string
		orgID   string
		input   CreateCardInput
		wantErr error
	}{
		{"missing org", "", validInput("agent-1"), ErrOrgRequired},
		{"missing name", "org-1", func() CreateCardInput { in := validInput("agent-1"); in.Name = "  "; return in }(), ErrNameRequired},
		{"missing version", "org-1", func() CreateCardInput { in := validInput("agent-1"); in.Version = ""; return in }(), ErrVersionRequired},
		{"missing agent", "org-1", func() CreateCardInput { in := validInput("agent-1"); in.AgentID = ""; return in }(), ErrAgentRequired},
		{"unknown agent", "org-1", validInput("agent-9"), ErrAgentUnknown},
		{"cross-org agent", "org-2", validInput("agent-1"), ErrAgentUnknown},
		{"bad endpoint scheme", "org-1", func() CreateCardInput { in := validInput("agent-1"); in.EndpointURL = "ftp://x.io"; return in }(), ErrEndpointInvalid},
		{"bad endpoint no host", "org-1", func() CreateCardInput { in := validInput("agent-1"); in.EndpointURL = "/relative/only"; return in }(), ErrEndpointInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.CreateCard(context.Background(), tc.orgID, tc.input); !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}

	// Happy path: defaults applied, org scoping stamped, agent re-checked.
	card, err := svc.CreateCard(context.Background(), "org-1", func() CreateCardInput {
		in := validInput("agent-1")
		in.AuthScheme = ""
		in.PreferredTransport = ""
		return in
	}())
	if err != nil {
		t.Fatalf("happy create: %v", err)
	}
	if card.OrganizationID != "org-1" || card.ID == "" {
		t.Fatalf("card scope/id wrong: %+v", card)
	}
	if card.PreferredTransport != DefaultTransport {
		t.Fatalf("empty transport must default to %q, got %q", DefaultTransport, card.PreferredTransport)
	}
	if card.AuthScheme != DefaultAuthScheme {
		t.Fatalf("empty auth scheme must default to %q, got %q", DefaultAuthScheme, card.AuthScheme)
	}
	if checker.calls == 0 {
		t.Fatal("the agent-existence seam must be consulted on create")
	}
}

// --- CRUD + org isolation ----------------------------------------------------

func TestCardCRUDAndOrgIsolation(t *testing.T) {
	svc := NewService()
	svc.SetAgentChecker(newStubAgentChecker("org-1/agent-1"))
	ctx := context.Background()

	card, err := svc.CreateCard(ctx, "org-1", validInput("agent-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Get within the org works.
	got, err := svc.GetCard(ctx, "org-1", card.ID)
	if err != nil || got.ID != card.ID {
		t.Fatalf("get: %v (%+v)", err, got)
	}
	// Foreign/unknown org: 404 semantics, no existence leak.
	if _, err := svc.GetCard(ctx, "org-2", card.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org get must be ErrNotFound, got %v", err)
	}
	if _, err := svc.GetCard(ctx, "org-1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id get must be ErrNotFound, got %v", err)
	}
	if _, err := svc.GetCard(ctx, "org-1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty id get must be ErrNotFound, got %v", err)
	}

	// List is org-scoped.
	list, err := svc.ListCards(ctx, "org-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list org-1: %v (len %d)", err, len(list))
	}
	if list2, _ := svc.ListCards(ctx, "org-2"); len(list2) != 0 {
		t.Fatalf("org-2 list must be empty, got %d", len(list2))
	}

	// Update from a foreign org is a 404 too.
	if _, err := svc.UpdateCard(ctx, "org-2", card.ID, UpdateCardInput{Name: strPtr("hijack")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org update must be ErrNotFound, got %v", err)
	}
	// Delete from a foreign org is a 404 and must not remove the row.
	if err := svc.DeleteCard(ctx, "org-2", card.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete must be ErrNotFound, got %v", err)
	}
	if _, err := svc.GetCard(ctx, "org-1", card.ID); err != nil {
		t.Fatalf("card must survive the foreign delete attempt: %v", err)
	}

	// Org A deletes its own card; afterwards even org A gets 404.
	if err := svc.DeleteCard(ctx, "org-1", card.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetCard(ctx, "org-1", card.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted card must 404, got %v", err)
	}
	if err := svc.DeleteCard(ctx, "org-1", card.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete must be ErrNotFound, got %v", err)
	}
}

// --- PATCH semantics ---------------------------------------------------------

func TestUpdateCardPartialSemantics(t *testing.T) {
	svc := NewService()
	svc.SetAgentChecker(newStubAgentChecker("org-1/agent-1", "org-1/agent-2"))
	ctx := context.Background()
	card, err := svc.CreateCard(ctx, "org-1", validInput("agent-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	originalUpdatedAt := card.UpdatedAt

	// Name-only patch: everything else preserved.
	updated, err := svc.UpdateCard(ctx, "org-1", card.ID, UpdateCardInput{Name: strPtr("Weather v2")})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if updated.Name != "Weather v2" {
		t.Fatalf("name = %q", updated.Name)
	}
	if updated.Description != card.Description || updated.Version != card.Version ||
		updated.AgentID != "agent-1" || updated.EndpointURL != card.EndpointURL {
		t.Fatalf("patch must be partial: %+v", updated)
	}
	if !updated.UpdatedAt.After(originalUpdatedAt) {
		t.Fatal("updated_at must advance on patch")
	}
	if len(updated.Skills) != 1 || updated.Skills[0].ID != "forecast" {
		t.Fatalf("skills must survive a name-only patch: %+v", updated.Skills)
	}

	// Skills replace when provided.
	updated, err = svc.UpdateCard(ctx, "org-1", card.ID, UpdateCardInput{
		Skills: []Skill{{ID: "alerts", Name: "Alerts", Tags: []string{"push"}}},
	})
	if err != nil {
		t.Fatalf("skills patch: %v", err)
	}
	if len(updated.Skills) != 1 || updated.Skills[0].ID != "alerts" {
		t.Fatalf("skills must be replaced: %+v", updated.Skills)
	}

	// Agent re-link revalidates against the same org; failure leaves the card.
	if _, err := svc.UpdateCard(ctx, "org-1", card.ID, UpdateCardInput{AgentID: strPtr("agent-9")}); !errors.Is(err, ErrAgentUnknown) {
		t.Fatalf("unknown agent re-link must be ErrAgentUnknown, got %v", err)
	}
	if _, err := svc.UpdateCard(ctx, "org-1", card.ID, UpdateCardInput{AgentID: strPtr("agent-2")}); err != nil {
		t.Fatalf("same-org re-link: %v", err)
	}

	// Resulting state must still validate (empty name via patch is refused).
	if _, err := svc.UpdateCard(ctx, "org-1", card.ID, UpdateCardInput{Name: strPtr("   ")}); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("patched-out name must be refused, got %v", err)
	}
	// Unknown card -> 404.
	if _, err := svc.UpdateCard(ctx, "org-1", "nope", UpdateCardInput{Name: strPtr("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown card patch must be ErrNotFound, got %v", err)
	}
}

func strPtr(s string) *string { return &s }

// --- canonical JSON ----------------------------------------------------------

func TestAgentCardToJSONCanonicalShape(t *testing.T) {
	card := &AgentCard{
		Name:        "Weather Agent",
		Description: "Talks about weather",
		Version:     "1.2.3",
		Provider:    CardProvider{Organization: "Acme", URL: "https://acme.example"},
		EndpointURL: "https://agents.example/weather",
		Capabilities: Capabilities{
			Streaming:              true,
			PushNotifications:      true,
			StateTransitionHistory: true,
		},
		Skills:             []Skill{{ID: "forecast", Name: "Forecast", Description: "5-day forecast", Tags: []string{"weather", "api"}}},
		AuthScheme:         "bearer",
		PreferredTransport: TransportJSONRPC,
	}
	out := card.ToJSON()

	// Top-level canonical keys, exact camelCase vocabulary.
	for _, key := range []string{"name", "description", "version", "provider", "url", "capabilities", "skills", "authentication", "preferredTransport"} {
		if _, ok := out[key]; !ok {
			t.Errorf("canonical card must carry %q", key)
		}
	}
	// Platform fields must NOT leak into the canonical surface.
	for _, key := range []string{"id", "organization_id", "agent_id", "created_at", "created_by"} {
		if _, ok := out[key]; ok {
			t.Errorf("canonical card must not carry %q", key)
		}
	}
	provider := out["provider"].(map[string]any)
	if provider["organization"] != "Acme" || provider["url"] != "https://acme.example" {
		t.Errorf("provider block wrong: %v", provider)
	}
	caps := out["capabilities"].(map[string]any)
	if caps["streaming"] != true || caps["pushNotifications"] != true || caps["stateTransitionHistory"] != true {
		t.Errorf("capabilities block wrong: %v", caps)
	}
	skills := out["skills"].([]map[string]any)
	if len(skills) != 1 {
		t.Fatalf("skills length = %d", len(skills))
	}
	skill := skills[0]
	if skill["id"] != "forecast" || skill["name"] != "Forecast" || skill["description"] != "5-day forecast" {
		t.Errorf("skill entry wrong: %v", skill)
	}
	if tags := skill["tags"].([]string); len(tags) != 2 || tags[0] != "weather" {
		t.Errorf("skill tags wrong: %v", tags)
	}
	auth := out["authentication"].(map[string]any)
	schemes, ok := auth["schemes"].([]string)
	if !ok || len(schemes) != 1 || schemes[0] != "bearer" {
		t.Errorf("authentication block wrong: %v", auth)
	}
	if out["url"] != "https://agents.example/weather" {
		t.Errorf("url = %v", out["url"])
	}

	// A card without an auth scheme omits the authentication block, and a
	// nil skills list still renders as an honest empty list.
	minimal := &AgentCard{Name: "x", Version: "1", EndpointURL: "https://x.example"}
	mout := minimal.ToJSON()
	if _, ok := mout["authentication"]; ok {
		t.Error("empty auth scheme must omit the authentication block")
	}
	if skills := mout["skills"].([]map[string]any); len(skills) != 0 {
		t.Errorf("nil skills must render as an empty list, got %v", skills)
	}
}

// --- inbound messages --------------------------------------------------------

func TestReceiveMessageHappyPath(t *testing.T) {
	auditor := &stubAuditor{}
	sink := &stubSink{}
	svc := NewService()
	svc.SetAgentChecker(newStubAgentChecker("org-1/agent-1"))
	svc.SetAuditor(auditor)
	svc.SetRunSink(sink)
	ctx := context.Background()
	card, err := svc.CreateCard(ctx, "org-1", validInput("agent-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	msg := "inbound-secret-payload-42"
	inbound, err := svc.ReceiveMessage(ctx, "org-1", card.ID, msg, "user-1")
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if inbound.Status != StatusSubmitted {
		t.Fatalf("status = %q, want %q", inbound.Status, StatusSubmitted)
	}
	if inbound.RunID == "" || inbound.TaskID == "" || inbound.CardID != card.ID || inbound.AgentID != "agent-1" {
		t.Fatalf("inbound result wrong: %+v", inbound)
	}
	// The sink saw exactly the card's agent + the message, org-scoped.
	if len(sink.calls) != 1 || sink.calls[0] != "org-1/agent-1:"+msg {
		t.Fatalf("sink calls wrong: %v", sink.calls)
	}
}

func TestReceiveMessageValidation(t *testing.T) {
	svc := NewService()
	svc.SetAgentChecker(newStubAgentChecker("org-1/agent-1"))
	svc.SetRunSink(&stubSink{})
	ctx := context.Background()
	card, err := svc.CreateCard(ctx, "org-1", validInput("agent-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	tests := []struct {
		name    string
		orgID   string
		cardID  string
		message string
		wantErr error
	}{
		{"unknown card", "org-1", "nope", "hi", ErrNotFound},
		{"cross-org card", "org-2", card.ID, "hi", ErrNotFound},
		{"empty card id", "org-1", "", "hi", ErrNotFound},
		{"empty message", "org-1", card.ID, "   ", ErrMessageRequired},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.ReceiveMessage(ctx, tc.orgID, tc.cardID, tc.message, "user-1"); !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}

	// A card whose agent no longer exists must not route (404 semantics on
	// the discovery path would leak less, but the agent link is the truth).
	broken := NewService()
	broken.SetAgentChecker(newStubAgentChecker()) // nothing exists
	if _, err := broken.CreateCard(ctx, "org-1", validInput("agent-1")); !errors.Is(err, ErrAgentUnknown) {
		t.Fatalf("card for a missing agent must not be creatable, got %v", err)
	}

	// No sink wired -> explicit unavailability, not a silent drop.
	noSink := NewService()
	noSink.SetAgentChecker(newStubAgentChecker("org-1/agent-1"))
	card2, err := noSink.CreateCard(ctx, "org-1", validInput("agent-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := noSink.ReceiveMessage(ctx, "org-1", card2.ID, "hi", "user-1"); !errors.Is(err, ErrRunSinkUnavailable) {
		t.Fatalf("expected ErrRunSinkUnavailable, got %v", err)
	}

	// A sink error propagates (handler -> 502/400 path).
	failing := NewService()
	failing.SetAgentChecker(newStubAgentChecker("org-1/agent-1"))
	failing.SetRunSink(&stubSink{err: errors.New("queue down")})
	card3, err := failing.CreateCard(ctx, "org-1", validInput("agent-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := failing.ReceiveMessage(ctx, "org-1", card3.ID, "hi", "user-1"); err == nil || !strings.Contains(err.Error(), "queue down") {
		t.Fatalf("sink error must propagate, got %v", err)
	}
}
