package main

// Issue #18 handler tests — audit half: RBAC (OWNER/ADMIN only), org scoping
// from the auth claims, keyset pagination through the HTTP contract, error
// envelopes, and tenant isolation. All in-memory, no infrastructure.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
)

// auditHandlerEnv wires the handler stack and returns bearer tokens covering
// every role relevant to audit.read (OWNER/ADMIN pass, MEMBER/VIEWER fail).
type auditHandlerEnv struct {
	mux         *http.ServeMux
	svc         *audit.Service
	orgID       string
	ownerToken  string
	adminToken  string
	memberToken string
	viewerToken string
	otherToken  string
}

func newAuditHandlerEnv(t *testing.T) *auditHandlerEnv {
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

	svc := audit.NewService()
	mux := http.NewServeMux()
	registerAuditEventsRoutes(mux, svc, authSvc, apiKeysSvc)

	return &auditHandlerEnv{
		mux:         mux,
		svc:         svc,
		orgID:       owner.Organization,
		ownerToken:  token("user-owner", "owner@acme.test", "OWNER"),
		adminToken:  token("user-admin", "admin@acme.test", "ADMIN"),
		memberToken: token("user-member", "member@acme.test", "MEMBER"),
		viewerToken: token("user-viewer", "viewer@acme.test", "VIEWER"),
		otherToken:  otherToken,
	}
}

func (e *auditHandlerEnv) do(t *testing.T, path, token string) (*httptest.ResponseRecorder, map[string]any) {
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

// errCodeAud extracts error.code from the shared error envelope.
func errCodeAud(decoded map[string]any) string {
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}

func TestAuditEventsListShapeAndOrdering(t *testing.T) {
	env := newAuditHandlerEnv(t)
	// Three sequential LogCtx calls (monotonic wall-clock stamps, ns
	// precision): the last-logged action must surface first (newest first).
	env.svc.LogCtx(t.Context(), "alice", "agent.created", env.orgID, "agents/a-1", map[string]any{"name": "helper"})
	env.svc.LogCtx(t.Context(), "bob", "run.failed", env.orgID, "runs/r-9", nil)
	env.svc.LogCtx(t.Context(), "carol", "webhook.received", env.orgID, "webhooks/w-1", map[string]any{"status": "delivered"})

	rr, decoded := env.do(t, "/audit-events", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /audit-events should be 200, got %d: %v", rr.Code, rr.Body.String())
	}
	events, ok := decoded["events"].([]any)
	if !ok {
		t.Fatalf("response must carry an events array: %v", decoded)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %v", len(events), decoded)
	}
	first, ok := events[0].(map[string]any)
	if !ok {
		t.Fatalf("event must be an object: %v", events[0])
	}
	for _, key := range []string{"id", "actor", "action", "resource", "created_at"} {
		if _, present := first[key]; !present {
			t.Fatalf("event view missing %q: %v", key, first)
		}
	}
	if _, leaked := first["organization_id"]; leaked {
		t.Fatalf("event view must not leak organization_id: %v", first)
	}
	if first["actor"] != "carol" {
		t.Fatalf("listing must be newest first (last-logged actor first), got %v", first)
	}
	if first["action"] != "webhook.received" {
		t.Fatalf("unexpected event fields: %v", first)
	}
	if _, present := first["metadata"]; !present {
		t.Fatalf("metadata should be rendered when present: %v", first)
	}
	if next, _ := decoded["next_cursor"].(string); next != "" {
		t.Fatalf("exhausted trail must return an empty next_cursor, got %q", next)
	}
}

func TestAuditEventsRBACOwnerAdminOnly(t *testing.T) {
	env := newAuditHandlerEnv(t)

	rr, _ := env.do(t, "/audit-events", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read must be 401, got %d", rr.Code)
	}
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"OWNER", env.ownerToken, http.StatusOK},
		{"ADMIN", env.adminToken, http.StatusOK},
		{"MEMBER", env.memberToken, http.StatusForbidden},
		{"VIEWER", env.viewerToken, http.StatusForbidden},
	} {
		rr, _ = env.do(t, "/audit-events", tc.token)
		if rr.Code != tc.want {
			t.Fatalf("%s read: got %d want %d", tc.name, rr.Code, tc.want)
		}
	}
}

func TestAuditEventsTenantIsolation(t *testing.T) {
	env := newAuditHandlerEnv(t)
	env.svc.LogCtx(t.Context(), "alice", "agent.created", env.orgID, "agents/a-1", nil)
	env.svc.LogCtx(t.Context(), "mallory", "secret.deleted", "org-foreign", "agents/x", nil)

	rr, decoded := env.do(t, "/audit-events", env.otherToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign list should be 200, got %d", rr.Code)
	}
	events, _ := decoded["events"].([]any)
	if len(events) != 0 {
		t.Fatalf("cross-tenant audit leak: %v", decoded)
	}
	rr, decoded = env.do(t, "/audit-events", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner list should be 200, got %d", rr.Code)
	}
	if events, _ = decoded["events"].([]any); len(events) != 1 {
		t.Fatalf("owner should see exactly the tenant's own entry: %v", decoded)
	}
}

func TestAuditEventsPaginationOverHTTP(t *testing.T) {
	env := newAuditHandlerEnv(t)
	for i := 0; i < 7; i++ {
		if _, err := env.svc.LogCtx(t.Context(), "alice", fmt.Sprintf("agent.created.%d", i), env.orgID, "agents/a-1", nil); err != nil {
			t.Fatalf("LogCtx: %v", err)
		}
	}

	var gotActions []string
	cursor := ""
	pages := 0
	for {
		path := "/audit-events?limit=3"
		if cursor != "" {
			path = fmt.Sprintf("/audit-events?limit=3&cursor=%s", cursor)
		}
		rr, decoded := env.do(t, path, env.ownerToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("page %d should be 200, got %d: %v", pages, rr.Code, rr.Body.String())
		}
		events, _ := decoded["events"].([]any)
		if len(events) > 3 {
			t.Fatalf("page %d exceeded the limit: %d", pages, len(events))
		}
		for _, raw := range events {
			event := raw.(map[string]any)
			gotActions = append(gotActions, event["action"].(string))
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
	if len(gotActions) != 7 {
		t.Fatalf("expected 7 events across pages, got %d", len(gotActions))
	}
	seen := make(map[string]bool, len(gotActions))
	for i, action := range gotActions {
		if seen[action] {
			t.Errorf("event %s repeated across pages", action)
		}
		seen[action] = true
		// LogCtx stamps strictly increasing timestamps, so the newest run has
		// the highest index.
		if want := fmt.Sprintf("agent.created.%d", 6-i); action != want {
			t.Errorf("position %d: got %s want %s (newest-first violated)", i, action, want)
		}
	}
}

func TestAuditEventsBadRequestHandling(t *testing.T) {
	env := newAuditHandlerEnv(t)

	rr, decoded := env.do(t, "/audit-events?limit=abc", env.ownerToken)
	if rr.Code != http.StatusBadRequest || errCodeAud(decoded) != "INVALID_REQUEST" {
		t.Fatalf("non-integer limit must be 400 INVALID_REQUEST, got %d %v", rr.Code, decoded)
	}
	rr, decoded = env.do(t, "/audit-events?limit=0", env.ownerToken)
	if rr.Code != http.StatusBadRequest || errCodeAud(decoded) != "INVALID_REQUEST" {
		t.Fatalf("zero limit must be 400 INVALID_REQUEST, got %d %v", rr.Code, decoded)
	}
	rr, decoded = env.do(t, "/audit-events?limit=-3", env.ownerToken)
	if rr.Code != http.StatusBadRequest || errCodeAud(decoded) != "INVALID_REQUEST" {
		t.Fatalf("negative limit must be 400 INVALID_REQUEST, got %d %v", rr.Code, decoded)
	}
	rr, decoded = env.do(t, "/audit-events?cursor=@@broken@@", env.ownerToken)
	if rr.Code != http.StatusBadRequest || errCodeAud(decoded) != "INVALID_CURSOR" {
		t.Fatalf("malformed cursor must be 400 INVALID_CURSOR, got %d %v", rr.Code, decoded)
	}
	// Oversized limits clamp silently instead of erroring (service-side cap).
	env.svc.LogCtx(t.Context(), "alice", "agent.created", env.orgID, "agents/a-1", nil)
	rr, decoded = env.do(t, "/audit-events?limit=999999", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("oversized limit should clamp, not fail, got %d: %v", rr.Code, rr.Body.String())
	}
	if events, _ := decoded["events"].([]any); len(events) != 1 {
		t.Fatalf("clamped request should return the tenant's page: %v", decoded)
	}
}

func TestAuditEventsUnavailableService(t *testing.T) {
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
	registerAuditEventsRoutes(mux, nil, authSvc, apiKeysSvc)

	req := httptest.NewRequest(http.MethodGet, "/audit-events", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil audit service must be 503, got %d", rr.Code)
	}
}
