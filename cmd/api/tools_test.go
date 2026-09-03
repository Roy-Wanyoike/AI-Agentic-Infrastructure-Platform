package main

// Issue #18 handler tests — tools half: the listing flows through the real
// middleware chain (auth + RBAC + JSON shape) and renders honest empty
// states. All in-memory, no infrastructure.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/tools"
)

// toolsHandlerEnv wires the handler stack and returns bearer tokens for one
// tenant's OWNER/VIEWER plus a foreign tenant's OWNER.
type toolsHandlerEnv struct {
	mux         *http.ServeMux
	orgID       string
	ownerToken  string
	viewerToken string
	otherToken  string
}

func newToolsHandlerEnv(t *testing.T, reg *tools.Registry) *toolsHandlerEnv {
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

	mux := http.NewServeMux()
	registerToolsRoutes(mux, reg, authSvc, apiKeysSvc)

	return &toolsHandlerEnv{
		mux:         mux,
		orgID:       owner.Organization,
		ownerToken:  ownerToken,
		viewerToken: viewerToken,
		otherToken:  otherToken,
	}
}

func (e *toolsHandlerEnv) do(t *testing.T, method, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
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

func TestToolsListReturnsBuiltInCatalog(t *testing.T) {
	env := newToolsHandlerEnv(t, tools.DefaultRegistry())

	rr, decoded := env.do(t, "GET", "/tools", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /tools should be 200, got %d: %v", rr.Code, rr.Body.String())
	}
	list, ok := decoded["tools"].([]any)
	if !ok {
		t.Fatalf("response must carry a tools array: %v", decoded)
	}
	if len(list) != 2 {
		t.Fatalf("the built-in registry exposes calculator + http_request, got %d: %v", len(list), decoded)
	}
	byName := make(map[string]map[string]any, len(list))
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tool entry must be an object: %v", raw)
		}
		name, _ := entry["name"].(string)
		byName[name] = entry
	}
	calc, ok := byName["calculator"]
	if !ok {
		t.Fatalf("calculator missing from the catalog: %v", decoded)
	}
	if desc, _ := calc["description"].(string); desc == "" {
		t.Errorf("calculator should publish a description: %v", calc)
	}
	schema, ok := calc["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("calculator should publish an input_schema: %v", calc)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, present := props["expression"]; !present {
		t.Errorf("calculator input_schema should document expression: %v", schema)
	}

	httpTool, ok := byName["http_request"]
	if !ok {
		t.Fatalf("http_request missing from the catalog: %v", decoded)
	}
	if _, present := httpTool["input_schema"]; !present {
		t.Errorf("http_request should publish an input_schema: %v", httpTool)
	}
	// Tools are runtime surface, not tenant data: nothing to leak, but assert
	// the payload carries no organization fields by design.
	if _, leaked := decoded["organization_id"]; leaked {
		t.Fatalf("tools response must not carry organization fields: %v", decoded)
	}
}

func TestToolsAuthAndRBAC(t *testing.T) {
	env := newToolsHandlerEnv(t, tools.DefaultRegistry())

	rr, _ := env.do(t, "GET", "/tools", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list must be 401, got %d", rr.Code)
	}
	// agents.read is granted to every role, so reads are open to all
	// authenticated roles (same fallback as the knowledge routes).
	rr, _ = env.do(t, "GET", "/tools", env.viewerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer read should pass agents.read, got %d", rr.Code)
	}
	rr, _ = env.do(t, "GET", "/tools", env.otherToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign tenant read should pass agents.read, got %d", rr.Code)
	}
	// Wrong method on a method-scoped pattern is rejected by the mux.
	rr, _ = env.do(t, "POST", "/tools", env.ownerToken)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /tools must be 405, got %d", rr.Code)
	}
}

func TestToolsEmptyAndNilRegistryRenderEmptyList(t *testing.T) {
	env := newToolsHandlerEnv(t, tools.NewRegistry())
	rr, decoded := env.do(t, "GET", "/tools", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty registry should still be 200, got %d", rr.Code)
	}
	list, ok := decoded["tools"].([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("empty registry should render an empty tools array: %v", decoded)
	}

	nilEnv := newToolsHandlerEnv(t, nil)
	rr, decoded = nilEnv.do(t, "GET", "/tools", nilEnv.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("nil registry should still be 200, got %d", rr.Code)
	}
	if list, _ := decoded["tools"].([]any); list == nil {
		t.Fatalf("nil registry should render a non-nil empty tools array: %v", decoded)
	}
}
