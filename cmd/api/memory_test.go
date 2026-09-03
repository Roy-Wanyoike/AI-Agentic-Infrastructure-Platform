package main

// Track 3-d handler tests — memory half: auth (401/403), PUT replace + GET
// list flows through the registered middleware chain, agent scoping,
// short-term expiry, validation errors, and tenant isolation. All in-memory.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/memory"
)

// memoryHandlerEnv wires the handler stack and returns bearer tokens for one
// tenant's OWNER/VIEWER plus a foreign tenant's OWNER.
type memoryHandlerEnv struct {
	mux         *http.ServeMux
	svc         *memory.Service
	orgID       string
	ownerToken  string
	viewerToken string
	otherToken  string
}

func newMemoryHandlerEnv(t *testing.T) *memoryHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	ownerToken, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken(owner) returned error: %v", err)
	}
	viewerToken, err := authSvc.GenerateToken(&auth.User{
		ID: "user-viewer", Organization: owner.Organization, Email: "viewer@acme.test", Role: "VIEWER",
	})
	if err != nil {
		t.Fatalf("GenerateToken(viewer) returned error: %v", err)
	}
	_, foreign, err := authSvc.Register("OtherCo", "owner@other.test", "secret123")
	if err != nil {
		t.Fatalf("Register(foreign) returned error: %v", err)
	}
	otherToken, err := authSvc.GenerateToken(foreign)
	if err != nil {
		t.Fatalf("GenerateToken(foreign) returned error: %v", err)
	}

	svc := memory.NewService()
	mux := http.NewServeMux()
	registerMemoryRoutes(mux, svc, authSvc, apiKeysSvc)

	return &memoryHandlerEnv{
		mux:         mux,
		svc:         svc,
		orgID:       owner.Organization,
		ownerToken:  ownerToken,
		viewerToken: viewerToken,
		otherToken:  otherToken,
	}
}

func (e *memoryHandlerEnv) do(t *testing.T, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	var decoded map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
	}
	return rr, decoded
}

func snippetViews(t *testing.T, decoded map[string]any) []map[string]any {
	t.Helper()
	raw, ok := decoded["snippets"].([]any)
	if !ok {
		t.Fatalf("response must carry a snippets array: %v", decoded)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		sn, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("snippet must be an object: %v", item)
		}
		out = append(out, sn)
	}
	return out
}

func TestMemoryPutListRoundTrip(t *testing.T) {
	env := newMemoryHandlerEnv(t)
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	rr, decoded := env.do(t, "PUT", "/memory", env.ownerToken, `{
                "agent_id": "agent-1",
                "snippets": [
                        {"scope": "short_term", "content": "user prefers concise answers", "importance": 0.9, "expires_at": "`+future+`"},
                        {"content": "user works at Acme on billing"}
                ]
        }`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /memory should be 200, got %d: %v", rr.Code, rr.Body.String())
	}
	stored := snippetViews(t, decoded)
	if len(stored) != 2 {
		t.Fatalf("expected 2 stored snippets, got %d", len(stored))
	}
	if stored[0]["scope"] != "short_term" || stored[0]["importance"] != 0.9 {
		t.Fatalf("snippet fields not normalized: %v", stored[0])
	}
	if stored[0]["agent_id"] != "agent-1" {
		t.Fatalf("agent_id not carried: %v", stored[0])
	}
	if _, leaked := stored[0]["organization_id"]; leaked {
		t.Fatalf("snippet view must not leak organization_id: %v", stored[0])
	}
	if _, leaked := stored[0]["embedding"]; leaked {
		t.Fatalf("snippet view must not leak the embedding vector: %v", stored[0])
	}
	for _, key := range []string{"id", "agent_id", "scope", "content", "importance", "expires_at", "created_at", "updated_at"} {
		if _, present := stored[0][key]; !present {
			t.Fatalf("snippet view missing %q: %v", key, stored[0])
		}
	}

	// GET with agent filter.
	rr, decoded = env.do(t, "GET", "/memory?agent_id=agent-1", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /memory should be 200, got %d", rr.Code)
	}
	if got := snippetViews(t, decoded); len(got) != 2 {
		t.Fatalf("expected 2 agent snippets, got %d", len(got))
	}

	// Org-level scope is separate from agent scopes.
	rr, decoded = env.do(t, "PUT", "/memory", env.ownerToken,
		`{"snippets": [{"content": "org-wide policy note"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("org-level PUT should be 200, got %d", rr.Code)
	}
	if got := snippetViews(t, decoded); got[0]["agent_id"] != "" {
		t.Fatalf("org-level snippets carry empty agent_id: %v", got[0])
	}

	// Unfiltered GET lists the whole organization.
	rr, decoded = env.do(t, "GET", "/memory", env.ownerToken, "")
	if got := snippetViews(t, decoded); len(got) != 3 {
		t.Fatalf("unfiltered GET should list org + agent snippets, got %d", len(got))
	}
}

func TestMemoryPutReplacesScopeSet(t *testing.T) {
	env := newMemoryHandlerEnv(t)
	env.do(t, "PUT", "/memory", env.ownerToken,
		`{"agent_id": "agent-1", "snippets": [{"content": "old fact"}]}`)
	rr, decoded := env.do(t, "PUT", "/memory", env.ownerToken,
		`{"agent_id": "agent-1", "snippets": [{"content": "new fact"}, {"content": "another fact"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second PUT should be 200, got %d", rr.Code)
	}
	for _, sn := range snippetViews(t, decoded) {
		if sn["content"] == "old fact" {
			t.Fatal("PUT must replace the previous set")
		}
	}
	// Other agents are untouched.
	rr, _ = env.do(t, "GET", "/memory?agent_id=agent-1", env.ownerToken, "")
	if got := snippetViews(t, rrToMap(t, rr)); len(got) != 2 {
		t.Fatalf("expected 2 snippets for agent-1 after replace, got %d", len(got))
	}
}

func TestMemoryShortTermExpiryHonored(t *testing.T) {
	env := newMemoryHandlerEnv(t)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rr, _ := env.do(t, "PUT", "/memory", env.ownerToken, `{
                "snippets": [
                        {"scope": "short_term", "content": "expired scratch", "expires_at": "`+past+`"},
                        {"content": "durable fact"}
                ]
        }`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT with mixed expiry should be 200, got %d", rr.Code)
	}
	rr, decoded := env.do(t, "GET", "/memory", env.ownerToken, "")
	got := snippetViews(t, decoded)
	if len(got) != 1 || got[0]["content"] != "durable fact" {
		t.Fatalf("expired snippet must not be listed: %v", got)
	}
}

func TestMemoryValidationErrors(t *testing.T) {
	env := newMemoryHandlerEnv(t)

	rr, decoded := env.do(t, "PUT", "/memory", env.ownerToken,
		`{"snippets": [{"content": "x", "scope": "forever"}]}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown scope must be 422, got %d", rr.Code)
	}
	if code := errCodeKnb(decoded); code != "VALIDATION_ERROR" {
		t.Fatalf("expected VALIDATION_ERROR, got %v", decoded)
	}

	rr, _ = env.do(t, "PUT", "/memory", env.ownerToken,
		`{"snippets": [{"content": "x", "expires_at": "not-a-time"}]}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed expires_at must be 422, got %d", rr.Code)
	}

	rr, _ = env.do(t, "PUT", "/memory", env.ownerToken, `{"snippets": [{"content": "  "}]}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank content must be 422, got %d", rr.Code)
	}

	rr, _ = env.do(t, "PUT", "/memory", env.ownerToken, `not json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body must be 400, got %d", rr.Code)
	}
}

func TestMemoryAuthAndRBAC(t *testing.T) {
	env := newMemoryHandlerEnv(t)

	rr, _ := env.do(t, "GET", "/memory", "", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list must be 401, got %d", rr.Code)
	}
	rr, _ = env.do(t, "PUT", "/memory", env.ownerToken, `{"snippets": [{"content": "x"}]}`)
	if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
		t.Fatalf("owner write should pass RBAC, got %d", rr.Code)
	}
	rr, _ = env.do(t, "PUT", "/memory", env.viewerToken, `{"snippets": [{"content": "x"}]}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer write must be 403, got %d", rr.Code)
	}
	rr, _ = env.do(t, "GET", "/memory", env.viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer read should pass, got %d", rr.Code)
	}
}

func TestMemoryTenantIsolation(t *testing.T) {
	env := newMemoryHandlerEnv(t)
	env.do(t, "PUT", "/memory", env.ownerToken,
		`{"snippets": [{"content": "secret of org-1"}]}`)

	rr, decoded := env.do(t, "GET", "/memory", env.otherToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign list should be 200 (empty), got %d", rr.Code)
	}
	if got := snippetViews(t, decoded); len(got) != 0 {
		t.Fatalf("cross-tenant memory leak: %v", got)
	}
}

func rrToMap(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
	}
	return decoded
}
