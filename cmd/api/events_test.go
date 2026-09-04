package main

// Issue #56 handler tests — events read half: RBAC (MEMBER+ via runs.execute,
// VIEWER 403), org scoping from the auth claims, keyset pagination through
// the HTTP contract, filters, error envelopes, and tenant isolation. All
// in-memory (MemoryStore), no infrastructure. Mirrors audit_events_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/events"
)

// eventsHandlerEnv wires the handler stack and returns bearer tokens covering
// every role relevant to runs.execute (OWNER/ADMIN/MEMBER pass, VIEWER fails).
type eventsHandlerEnv struct {
	mux         *http.ServeMux
	store       *events.MemoryStore
	orgID       string
	ownerToken  string
	adminToken  string
	memberToken string
	viewerToken string
	otherToken  string
}

func newEventsHandlerEnv(t *testing.T) *eventsHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	token := func(id, email, role string) string {
		t.Helper()
		generated, err := authSvc.GenerateToken(&auth.User{
			ID: id, Organization: owner.Organization, Email: email, Role: role,
		})
		if err != nil {
			t.Fatalf("GenerateToken(%s) returned error: %v", role, err)
		}
		return generated
	}
	_, foreign, err := authSvc.Register("OtherCo", "owner@other.test", "secret123")
	if err != nil {
		t.Fatalf("Register(foreign) returned error: %v", err)
	}
	otherToken, err := authSvc.GenerateToken(foreign)
	if err != nil {
		t.Fatalf("GenerateToken(foreign) returned error: %v", err)
	}

	store := events.NewMemoryStore()
	mux := http.NewServeMux()
	registerEventsRoutes(mux, store, authSvc, apiKeysSvc)

	return &eventsHandlerEnv{
		mux:         mux,
		store:       store,
		orgID:       owner.Organization,
		ownerToken:  token("user-owner", "owner@acme.test", "OWNER"),
		adminToken:  token("user-admin", "admin@acme.test", "ADMIN"),
		memberToken: token("user-member", "member@acme.test", "MEMBER"),
		viewerToken: token("user-viewer", "viewer@acme.test", "VIEWER"),
		otherToken:  otherToken,
	}
}

func (e *eventsHandlerEnv) do(t *testing.T, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	var decoded map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
	}
	return rr, decoded
}

// seedEv appends one contract-valid event with an exact timestamp so the
// newest-first assertions are deterministic.
func (e *eventsHandlerEnv) seedEv(t *testing.T, at time.Time, id, eventType, entityType, entityID string, payload map[string]any) events.Event {
	t.Helper()
	event := events.Event{
		ID:        id,
		Type:      eventType,
		TenantID:  e.orgID,
		Timestamp: at,
		Resource:  events.Resource{Type: entityType, ID: entityID},
		Payload:   payload,
	}
	if err := e.store.AppendEvent(context.Background(), &event); err != nil {
		t.Fatalf("AppendEvent(%s): %v", id, err)
	}
	return event
}

func (e *eventsHandlerEnv) seedForeign(t *testing.T, at time.Time, id, eventType string) {
	t.Helper()
	event := events.Event{
		ID:        id,
		Type:      eventType,
		TenantID:  "org-foreign",
		Timestamp: at,
		Resource:  events.Resource{Type: "run", ID: "x"},
	}
	if err := e.store.AppendEvent(context.Background(), &event); err != nil {
		t.Fatalf("AppendEvent(foreign %s): %v", id, err)
	}
}

// errCodeEv extracts error.code from the shared error envelope.
func errCodeEv(decoded map[string]any) string {
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}

func TestEventsListShapeAndOrdering(t *testing.T) {
	env := newEventsHandlerEnv(t)
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	env.seedEv(t, base, "e1", events.EventRunStarted, "run", "r-1", nil)
	env.seedEv(t, base.Add(time.Minute), "e2", events.EventAgentCreated, "agent", "a-1", nil)
	env.seedEv(t, base.Add(2*time.Minute), "e3", events.EventRunFailed, "run", "r-2", map[string]any{"reason": "timeout"})

	rr, decoded := env.do(t, "/events", env.memberToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /events should be 200, got %d: %v", rr.Code, rr.Body.String())
	}
	items, ok := decoded["events"].([]any)
	if !ok {
		t.Fatalf("response must carry an events array: %v", decoded)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 events, got %d: %v", len(items), decoded)
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("event must be an object: %v", items[0])
	}
	for _, key := range []string{"id", "type", "entity_type", "entity_id", "timestamp"} {
		if _, present := first[key]; !present {
			t.Fatalf("event view missing %q: %v", key, first)
		}
	}
	if _, leaked := first["organization_id"]; leaked {
		t.Fatalf("event view must not leak organization_id: %v", first)
	}
	if _, leaked := first["tenant_id"]; leaked {
		t.Fatalf("event view must not leak tenant_id: %v", first)
	}
	// Newest first (last-seeded, highest timestamp first).
	if first["id"] != "e3" || first["type"] != events.EventRunFailed {
		t.Fatalf("listing must be newest first, got %v", first)
	}
	if first["payload"].(map[string]any)["reason"] != "timeout" {
		t.Fatalf("payload should be rendered when present: %v", first)
	}
	if next, _ := decoded["next_cursor"].(string); next != "" {
		t.Fatalf("exhausted stream must return an empty next_cursor, got %q", next)
	}
}

func TestEventsRBACMemberPlus(t *testing.T) {
	env := newEventsHandlerEnv(t)

	rr, _ := env.do(t, "/events", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read must be 401, got %d", rr.Code)
	}
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"OWNER", env.ownerToken, http.StatusOK},
		{"ADMIN", env.adminToken, http.StatusOK},
		{"MEMBER", env.memberToken, http.StatusOK},
		{"VIEWER", env.viewerToken, http.StatusForbidden},
	} {
		rr, _ = env.do(t, "/events", tc.token)
		if rr.Code != tc.want {
			t.Fatalf("%s read: got %d want %d", tc.name, rr.Code, tc.want)
		}
	}
	// The 403 comes from the shared auth middleware (plain-text "forbidden",
	// platform-wide convention) — status is the contract here.
	rr, _ = env.do(t, "/events", env.viewerToken)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer denial must be 403, got %d", rr.Code)
	}
}

func TestEventsTenantIsolation(t *testing.T) {
	env := newEventsHandlerEnv(t)
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	env.seedEv(t, base, "mine", events.EventAgentCreated, "agent", "a-1", nil)
	// A foreign event that is NEWER must never surface in the owner's pages.
	env.seedForeign(t, base.Add(time.Hour), "theirs-newer", events.EventRunFailed)
	env.seedForeign(t, base.Add(-time.Hour), "theirs-older", events.EventRunFailed)

	rr, decoded := env.do(t, "/events", env.otherToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign list should be 200, got %d", rr.Code)
	}
	if items, _ := decoded["events"].([]any); len(items) != 0 {
		t.Fatalf("cross-tenant event leak: %v", decoded)
	}
	rr, decoded = env.do(t, "/events", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner list should be 200, got %d", rr.Code)
	}
	items, _ := decoded["events"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "mine" {
		t.Fatalf("owner should see exactly the tenant's own event: %v", decoded)
	}
}

func TestEventsPaginationOverHTTP(t *testing.T) {
	env := newEventsHandlerEnv(t)
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		env.seedEv(t, base.Add(time.Duration(i)*time.Second),
			fmt.Sprintf("e%d", i), events.EventRunStarted, "run", fmt.Sprintf("r-%d", i), nil)
	}

	var gotIDs []string
	cursor := ""
	pages := 0
	for {
		path := "/events?limit=3"
		if cursor != "" {
			path = fmt.Sprintf("/events?limit=3&cursor=%s", cursor)
		}
		rr, decoded := env.do(t, path, env.ownerToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("page %d should be 200, got %d: %v", pages, rr.Code, rr.Body.String())
		}
		items, _ := decoded["events"].([]any)
		if len(items) > 3 {
			t.Fatalf("page %d exceeded the limit: %d", pages, len(items))
		}
		for _, raw := range items {
			event := raw.(map[string]any)
			gotIDs = append(gotIDs, event["id"].(string))
		}
		pages++
		next, _ := decoded["next_cursor"].(string)
		if next == "" {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		cursor = next
	}
	if pages != 3 {
		t.Fatalf("7 events at limit 3 should take 3 pages, got %d", pages)
	}
	if len(gotIDs) != 7 {
		t.Fatalf("expected 7 events across pages, got %d", len(gotIDs))
	}
	seen := make(map[string]bool, len(gotIDs))
	for i, id := range gotIDs {
		if seen[id] {
			t.Errorf("event %s repeated across pages", id)
		}
		seen[id] = true
		// Seeded timestamps increase with the index, so the newest run has
		// the highest index.
		if want := fmt.Sprintf("e%d", 6-i); id != want {
			t.Errorf("position %d: got %s want %s (newest-first violated)", i, id, want)
		}
	}
}

func TestEventsBadRequestHandling(t *testing.T) {
	env := newEventsHandlerEnv(t)

	for _, tc := range []struct {
		name, query, code string
	}{
		{"non-integer limit", "limit=abc", "INVALID_REQUEST"},
		{"zero limit", "limit=0", "INVALID_REQUEST"},
		{"negative limit", "limit=-3", "INVALID_REQUEST"},
		{"unknown event type", "type=bogus.type", "INVALID_REQUEST"},
		{"malformed since", "since=yesterday", "INVALID_REQUEST"},
		{"malformed cursor", "cursor=@@broken@@", "INVALID_CURSOR"},
	} {
		rr, decoded := env.do(t, "/events?"+tc.query, env.ownerToken)
		if rr.Code != http.StatusBadRequest || errCodeEv(decoded) != tc.code {
			t.Fatalf("%s must be 400 %s, got %d %v", tc.name, tc.code, rr.Code, decoded)
		}
	}
	// Oversized limits clamp silently instead of erroring (service-side cap).
	env.seedEv(t, time.Now(), "only", events.EventRunStarted, "run", "r", nil)
	rr, decoded := env.do(t, "/events?limit=999999", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("oversized limit should clamp, not fail, got %d: %v", rr.Code, rr.Body.String())
	}
	if items, _ := decoded["events"].([]any); len(items) != 1 {
		t.Fatalf("clamped request should return the tenant's page: %v", decoded)
	}
}

func TestEventsFiltersOverHTTP(t *testing.T) {
	env := newEventsHandlerEnv(t)
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	env.seedEv(t, base, "run-start", events.EventRunStarted, "run", "r-1", nil)
	env.seedEv(t, base.Add(30*time.Second), "run-start-2", events.EventRunStarted, "run", "r-3", nil)
	env.seedEv(t, base.Add(time.Minute), "run-fail", events.EventRunFailed, "run", "r-2", nil)
	env.seedEv(t, base.Add(2*time.Minute), "agent-create", events.EventAgentCreated, "agent", "a-1", nil)

	ids := func(t *testing.T, query, token string) []string {
		t.Helper()
		rr, decoded := env.do(t, "/events"+query, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /events%s should be 200, got %d: %v", query, rr.Code, rr.Body.String())
		}
		items, _ := decoded["events"].([]any)
		out := make([]string, 0, len(items))
		for _, raw := range items {
			out = append(out, raw.(map[string]any)["id"].(string))
		}
		return out
	}

	if got := ids(t, "?type=run.failed", env.ownerToken); len(got) != 1 || got[0] != "run-fail" {
		t.Fatalf("type filter wrong: %v", got)
	}
	if got := ids(t, "?entity_type=agent", env.ownerToken); len(got) != 1 || got[0] != "agent-create" {
		t.Fatalf("entity_type filter wrong: %v", got)
	}
	if got := ids(t, "?entity_id=r-1", env.ownerToken); len(got) != 1 || got[0] != "run-start" {
		t.Fatalf("entity_id filter wrong: %v", got)
	}
	// since is RFC3339, inclusive: everything strictly older drops out
	// (run-start-2 at 12:00:30 and run-start at 12:00:00 are below the bound).
	if got := ids(t, "?since=2025-06-01T12:01:00Z", env.ownerToken); len(got) != 2 || got[0] != "agent-create" || got[1] != "run-fail" {
		t.Fatalf("since filter wrong: %v", got)
	}
	// Filters compose with cursor pagination without repeats.
	firstPage, firstNext := func() ([]string, string) {
		rr, decoded := env.do(t, "/events?type=run.started&limit=1", env.ownerToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("filtered page: %d %v", rr.Code, rr.Body.String())
		}
		items, _ := decoded["events"].([]any)
		next, _ := decoded["next_cursor"].(string)
		out := []string{items[0].(map[string]any)["id"].(string)}
		return out, next
	}()
	if len(firstPage) != 1 || firstPage[0] != "run-start-2" || firstNext == "" {
		t.Fatalf("filtered first page wrong: %v (next=%q)", firstPage, firstNext)
	}
	if got := ids(t, "?type=run.started&limit=10&cursor="+firstNext, env.ownerToken); len(got) != 1 || got[0] != "run-start" {
		t.Fatalf("filtered continuation wrong: %v", got)
	}
}

func TestEventsUnavailableLister(t *testing.T) {
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()
	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	token, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	mux := http.NewServeMux()
	registerEventsRoutes(mux, nil, authSvc, apiKeysSvc)

	req := httptest.NewRequest(http.MethodGet, "/events", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var decoded map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
	if rr.Code != http.StatusServiceUnavailable || errCodeEv(decoded) != "EVENTS_UNAVAILABLE" {
		t.Fatalf("nil lister must be 503 EVENTS_UNAVAILABLE, got %d %v", rr.Code, decoded)
	}
}

func TestEventsListerResolution(t *testing.T) {
	// No DB + nothing retaining events -> no lister (handler answers 503).
	if eventsLister(nil, events.NewNoopPublisher()) != nil {
		t.Fatal("noop publisher must not resolve a lister")
	}
	// The pass-through AuditPublisher (nil store) has no read to fall back
	// on: whatever it resolves to must honestly answer ErrListUnsupported
	// (the handler maps that to 503 EVENTS_UNAVAILABLE).
	if lister := eventsLister(nil, events.NewAuditPublisher(nil, nil)); lister != nil {
		if _, _, err := lister.ListEventsPaged(context.Background(), "org-1", events.EventFilter{}, 10, ""); !errors.Is(err, events.ErrListUnsupported) {
			t.Fatalf("pass-through AuditPublisher must answer ErrListUnsupported, got %v", err)
		}
	}
	// Zero-infrastructure retention: a MemoryStore behind the AuditPublisher.
	wrapped := events.NewAuditPublisher(events.NewMemoryStore(), events.NewNoopPublisher())
	if eventsLister(nil, wrapped) == nil {
		t.Fatal("AuditPublisher wrapping a MemoryStore must resolve a lister")
	}
	// NATS-down fallback: the MemoryPublisher ring is itself paged.
	if eventsLister(nil, events.NewMemoryPublisher()) == nil {
		t.Fatal("MemoryPublisher must resolve a lister")
	}
	// Database mode: a read handle over the shared events table.
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	if eventsLister(db, nil) == nil {
		t.Fatal("database mode must resolve a lister")
	}
}
