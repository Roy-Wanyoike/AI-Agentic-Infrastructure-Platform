package main

// mcp_registry_test.go — issue #82 HTTP surface, exercised through the REAL
// middleware chain (RequireAuthOrAPIKey -> RequirePermission): registration
// health-check semantics (422 vs ?force), the CRUD + re-test contract, the
// RBAC matrix (reused connectors grants), tenant isolation, audit rows and
// the no-secret-values guarantee.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
	"agentos/internal/mcp"
	"agentos/internal/secrets"
)

// newMcpRegTestRouter wires the registry routes against a live in-test MCP
// fixture server plus an in-memory secrets service (for secret_ref probes).
func newMcpRegTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *apikeys.Service, *audit.Service, *mcp.Registry, *mcpRegFixture) {
	t.Helper()
	fixture := newMcpRegFixture()
	t.Cleanup(fixture.close)

	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	auditSvc := audit.NewService()
	reg := mcp.NewRegistry()

	apiMux := http.NewServeMux()
	registerMcpRegistryRoutes(apiMux, reg, authSvc, keysSvc, auditSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, keysSvc, auditSvc, reg, fixture
}

// mcpRegFixture is the local record/replay MCP server for the HTTP tests:
// initialize + tools/list (two canned tools) + tools/call, recording the
// requests it receives.
type mcpRegFixture struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []mcp.Request
	headers  []http.Header
	tools    []mcp.RemoteTool
}

func newMcpRegFixture() *mcpRegFixture {
	f := &mcpRegFixture{tools: []mcp.RemoteTool{
		{
			Name:        "get_weather",
			Description: "Returns the weather for a city",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []string{"city"},
			},
		},
		{Name: "ping"},
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *mcpRegFixture) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var env mcp.Request
	_ = json.Unmarshal(raw, &env)
	f.mu.Lock()
	f.requests = append(f.requests, env)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()

	var result any
	switch env.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": mcp.ProtocolVersion,
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "fixture", "version": "1.0.0"},
		}
	case "tools/list":
		f.mu.Lock()
		tools := append([]mcp.RemoteTool(nil), f.tools...)
		f.mu.Unlock()
		result = map[string]any{"tools": tools}
	case "tools/call":
		result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": "22C"}},
		}
	default:
		result = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(env.ID), "result": result})
}

func (f *mcpRegFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *mcpRegFixture) header(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.headers) - 1; i >= 0; i-- {
		if v := f.headers[i].Get(name); v != "" {
			return v
		}
	}
	return ""
}

func (f *mcpRegFixture) close() { f.srv.Close() }

func mcpRegTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&authpkg.User{
		ID:           "user-" + email,
		Organization: orgID,
		Email:        email,
		Role:         role,
	})
	if err != nil {
		t.Fatalf("generate token failed: %v", err)
	}
	return token
}

func doMcpRegReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeMcpRegBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

func mcpRegCreateBody(name, endpoint string) string {
	return fmt.Sprintf(`{"name":%q,"endpoint_url":%q,"secret_ref":"","auth_header":""}`, name, endpoint)
}

// createMcpServerViaHTTP registers one reachable server for org (owner
// token) and fails the test on any non-201.
func createMcpServerViaHTTP(t *testing.T, h http.Handler, token, name, endpoint string) map[string]any {
	t.Helper()
	rr := doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", token, mcpRegCreateBody(name, endpoint))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %s: expected 201, got %d body=%s", name, rr.Code, rr.Body.String())
	}
	return decodeMcpRegBody(t, rr)
}

func TestMcpRegistryAuthRequired(t *testing.T) {
	h, _, _, _, _, _ := newMcpRegTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/mcp-servers"},
		{http.MethodPost, "/api/v1/mcp-servers"},
		{http.MethodGet, "/api/v1/mcp-servers/some-id"},
		{http.MethodPut, "/api/v1/mcp-servers/some-id"},
		{http.MethodDelete, "/api/v1/mcp-servers/some-id"},
		{http.MethodPost, "/api/v1/mcp-servers/some-id/test"},
	} {
		rr := doMcpRegReq(t, h, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestMcpRegistryRoleMatrix(t *testing.T) {
	h, authSvc, _, _, reg, fixture := newMcpRegTestRouter(t)
	const org = "org-m"

	// Seed one server so reads have a target (service-level, RBAC-agnostic).
	if _, err := reg.CreateServer(context.Background(), org, mcp.CreateServerInput{
		Name: "seeded", EndpointURL: fixture.srv.URL,
	}, "seeder", false); err != nil {
		t.Fatalf("seed create failed: %v", err)
	}

	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = mcpRegTokenFor(t, authSvc, strings.ToLower(role)+"@x.io", org, role)
	}

	// Reads: connectors.read = MEMBER+ (VIEWER 403).
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER"} {
		rr := doMcpRegReq(t, h, http.MethodGet, "/api/v1/mcp-servers", tokens[role], "")
		if rr.Code != http.StatusOK {
			t.Errorf("GET as %s: expected 200, got %d", role, rr.Code)
		}
		rr = doMcpRegReq(t, h, http.MethodGet, "/api/v1/mcp-servers/whatever", tokens[role], "")
		if rr.Code != http.StatusNotFound {
			t.Errorf("GET {id} as %s: expected 404 (unknown id), got %d", role, rr.Code)
		}
	}
	if rr := doMcpRegReq(t, h, http.MethodGet, "/api/v1/mcp-servers", tokens["VIEWER"], ""); rr.Code != http.StatusForbidden {
		t.Errorf("GET as VIEWER: expected 403, got %d", rr.Code)
	}

	// Writes: connectors.write = OWNER/ADMIN only. Each role gets its own
	// uniquely named target so the matrix rows are order-independent.
	for _, role := range []string{"OWNER", "ADMIN"} {
		rr := doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", tokens[role], mcpRegCreateBody("srv-"+role, fixture.srv.URL))
		if rr.Code != http.StatusCreated {
			t.Errorf("POST as %s: expected 201, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}
	for _, role := range []string{"MEMBER", "VIEWER"} {
		rr := doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", tokens[role], mcpRegCreateBody("srv-"+role, fixture.srv.URL))
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST as %s: expected 403, got %d", role, rr.Code)
		}
		rr = doMcpRegReq(t, h, http.MethodDelete, "/api/v1/mcp-servers/whatever", tokens[role], "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("DELETE as %s: expected 403, got %d", role, rr.Code)
		}
		rr = doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers/whatever/test", tokens[role], "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("test as %s: expected 403, got %d", role, rr.Code)
		}
	}
}

func TestMcpRegistryCreateOkCachesToolsAndAudits(t *testing.T) {
	h, authSvc, _, auditSvc, reg, fixture := newMcpRegTestRouter(t)
	token := mcpRegTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	out := createMcpServerViaHTTP(t, h, token, "Weather MCP", fixture.srv.URL)
	srv := out["mcp_server"].(map[string]any)
	if srv["status"] != "ok" {
		t.Fatalf("status = %v, want ok", srv["status"])
	}
	if srv["protocol_version"] != mcp.ProtocolVersion {
		t.Fatalf("protocol_version = %v", srv["protocol_version"])
	}
	tools := srv["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools must be cached at registration: %v", tools)
	}
	first := tools[0].(map[string]any)
	if first["name"] != "get_weather" {
		t.Fatalf("cached tool name = %v", first["name"])
	}
	if _, has := first["inputSchema"]; !has {
		t.Fatalf("cached tool must expose inputSchema: %v", first)
	}
	if srv["last_check_at"] == nil {
		t.Fatalf("last_check_at must be stamped")
	}
	if srv["auth_header"] != "Authorization" {
		t.Fatalf("auth_header default = %v", srv["auth_header"])
	}
	if srv["secret_ref"] != "" {
		t.Fatalf("secret_ref echo = %v", srv["secret_ref"])
	}

	// Audit row: mcp.server_registered with the server ref.
	entries, err := auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one audit row, got %d (%v)", len(entries), err)
	}
	e := entries[0]
	if e.Action != "mcp.server_registered" || e.Actor != "user-owner@x.io" ||
		e.Resource != "mcp_servers/"+srv["id"].(string) {
		t.Fatalf("audit row wrong: %+v", e)
	}
	if e.Metadata["name"] != "Weather MCP" || e.Metadata["status"] != "ok" {
		t.Fatalf("audit metadata wrong: %+v", e.Metadata)
	}

	// The registration really probed the fixture (initialize + tools/list).
	if got := fixture.count(); got != 2 {
		t.Fatalf("expected 2 probe requests, got %d", got)
	}
	_ = reg
}

func TestMcpRegistryCreateUnreachableAndForce(t *testing.T) {
	h, authSvc, _, _, _, _ := newMcpRegTestRouter(t)
	token := mcpRegTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")
	dead := "http://127.0.0.1:1/mcp"

	// Unreachable: 422 MCP_SERVER_UNREACHABLE by default.
	rr := doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", token, mcpRegCreateBody("dead", dead))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeMcpRegBody(t, rr)
	errObj := body["error"].(map[string]any)
	if errObj["code"] != "MCP_SERVER_UNREACHABLE" {
		t.Fatalf("error code = %v", errObj["code"])
	}

	// ?force=true stores the server as unreachable.
	rr = doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers?force=true", token, mcpRegCreateBody("dead", dead))
	if rr.Code != http.StatusCreated {
		t.Fatalf("forced create: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	srv := decodeMcpRegBody(t, rr)["mcp_server"].(map[string]any)
	if srv["status"] != "unreachable" {
		t.Fatalf("forced status = %v, want unreachable", srv["status"])
	}
	if tools := srv["tools"].([]any); len(tools) != 0 {
		t.Fatalf("unreachable server must cache no tools: %v", tools)
	}
}

func TestMcpRegistryCreateValidationAndDuplicate(t *testing.T) {
	h, authSvc, _, _, reg, fixture := newMcpRegTestRouter(t)
	token := mcpRegTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	// Malformed JSON -> 400.
	if rr := doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", token, "{nope"); rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d", rr.Code)
	}
	// Missing name -> 422 VALIDATION_ERROR.
	rr := doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", token, `{"endpoint_url":"http://x.io"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing name: expected 422, got %d", rr.Code)
	}
	if code := decodeMcpRegBody(t, rr)["error"].(map[string]any)["code"]; code != "VALIDATION_ERROR" {
		t.Fatalf("error code = %v", code)
	}
	// Bad scheme -> 422.
	rr = doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", token, mcpRegCreateBody("bad", "ftp://x.io/mcp"))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad scheme: expected 422, got %d", rr.Code)
	}
	// Bad auth header -> 422.
	rr = doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", token, `{"name":"x","endpoint_url":"http://x.io","auth_header":"Bad Header"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad auth header: expected 422, got %d", rr.Code)
	}
	// Duplicate name -> 409.
	createMcpServerViaHTTP(t, h, token, "dup", fixture.srv.URL)
	rr = doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", token, mcpRegCreateBody("dup", fixture.srv.URL))
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate: expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if code := decodeMcpRegBody(t, rr)["error"].(map[string]any)["code"]; code != "MCP_SERVER_ALREADY_EXISTS" {
		t.Fatalf("error code = %v", code)
	}
	_ = reg
}

func TestMcpRegistryListGetAndUpdate(t *testing.T) {
	h, authSvc, _, auditSvc, _, fixture := newMcpRegTestRouter(t)
	token := mcpRegTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	created := createMcpServerViaHTTP(t, h, token, "Weather MCP", fixture.srv.URL)
	id := created["mcp_server"].(map[string]any)["id"].(string)

	// List contains the server.
	rr := doMcpRegReq(t, h, http.MethodGet, "/api/v1/mcp-servers", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rr.Code)
	}
	items := decodeMcpRegBody(t, rr)["mcp_servers"].([]any)
	if len(items) != 1 {
		t.Fatalf("list length = %d", len(items))
	}

	// Get by id.
	rr = doMcpRegReq(t, h, http.MethodGet, "/api/v1/mcp-servers/"+id, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", rr.Code)
	}
	// Unknown id -> 404 without an existence leak.
	rr = doMcpRegReq(t, h, http.MethodGet, "/api/v1/mcp-servers/nope", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id: expected 404, got %d", rr.Code)
	}

	// Update: renames and re-probes (200 + refreshed row + audit).
	req := fmt.Sprintf(`{"name":"Weather v2","endpoint_url":%q}`, fixture.srv.URL)
	rr = doMcpRegReq(t, h, http.MethodPut, "/api/v1/mcp-servers/"+id, token, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	updated := decodeMcpRegBody(t, rr)["mcp_server"].(map[string]any)
	if updated["name"] != "Weather v2" || updated["status"] != "ok" {
		t.Fatalf("update mismatch: %v", updated)
	}

	// Update to a dead endpoint: 422 unless forced.
	rr = doMcpRegReq(t, h, http.MethodPut, "/api/v1/mcp-servers/"+id, token, `{"name":"dead","endpoint_url":"http://127.0.0.1:1/mcp"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unreachable update: expected 422, got %d", rr.Code)
	}
	rr = doMcpRegReq(t, h, http.MethodPut, "/api/v1/mcp-servers/"+id+"?force=1", token, `{"name":"dead","endpoint_url":"http://127.0.0.1:1/mcp"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("forced update: expected 200, got %d", rr.Code)
	}

	// The three writes above each audited once (registered/updated x2).
	entries, err := auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit list failed: %v", err)
	}
	actions := map[string]int{}
	for _, e := range entries {
		actions[e.Action]++
	}
	if actions["mcp.server_registered"] != 1 || actions["mcp.server_updated"] != 2 {
		t.Fatalf("audit actions wrong: %v", actions)
	}
}

func TestMcpRegistryTestEndpointRefreshesAndAudits(t *testing.T) {
	h, authSvc, _, auditSvc, _, fixture := newMcpRegTestRouter(t)
	token := mcpRegTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	created := createMcpServerViaHTTP(t, h, token, "Weather MCP", fixture.srv.URL)
	id := created["mcp_server"].(map[string]any)["id"].(string)

	// The remote grows a tool; the explicit re-test refreshes the cache.
	fixture.mu.Lock()
	fixture.tools = append(fixture.tools, mcp.RemoteTool{Name: "get_forecast", Description: "5-day forecast"})
	fixture.mu.Unlock()
	rr := doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers/"+id+"/test", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	srv := decodeMcpRegBody(t, rr)["mcp_server"].(map[string]any)
	if tools := srv["tools"].([]any); len(tools) != 3 {
		t.Fatalf("re-test must refresh the cache, got %d tools", len(tools))
	}
	if srv["status"] != "ok" {
		t.Fatalf("re-test status = %v", srv["status"])
	}

	// The remote dies; the re-test flips the status (visible degradation).
	fixture.close()
	rr = doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers/"+id+"/test", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("test over dead remote: expected 200 (outcome is the payload), got %d", rr.Code)
	}
	if srv := decodeMcpRegBody(t, rr)["mcp_server"].(map[string]any); srv["status"] != "unreachable" {
		t.Fatalf("dead re-test status = %v, want unreachable", srv["status"])
	}

	entries, err := auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit list failed: %v", err)
	}
	tested := 0
	for _, e := range entries {
		if e.Action == "mcp.server_tested" {
			tested++
		}
	}
	if tested != 2 {
		t.Fatalf("expected two mcp.server_tested rows, got %d", tested)
	}
}

func TestMcpRegistryDeleteAndCrossOrgGuard(t *testing.T) {
	h, authSvc, _, auditSvc, _, fixture := newMcpRegTestRouter(t)
	orgA := mcpRegTokenFor(t, authSvc, "a@x.io", "org-a", "OWNER")
	orgB := mcpRegTokenFor(t, authSvc, "b@x.io", "org-b", "OWNER")

	created := createMcpServerViaHTTP(t, h, orgA, "A-only", fixture.srv.URL)
	id := created["mcp_server"].(map[string]any)["id"].(string)

	// Org B cannot read, update, test or delete org A's server (404, no
	// existence leak) — and org A's audit trail stays clean of org B.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/mcp-servers/" + id},
		{http.MethodPut, "/api/v1/mcp-servers/" + id},
		{http.MethodDelete, "/api/v1/mcp-servers/" + id},
		{http.MethodPost, "/api/v1/mcp-servers/" + id + "/test"},
	} {
		rr := doMcpRegReq(t, h, tc.method, tc.path, orgB, `{"name":"x","endpoint_url":"http://x.io"}`)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s cross-org: expected 404, got %d", tc.method, rr.Code)
		}
	}
	// Org B's list never contains org A's server.
	rr := doMcpRegReq(t, h, http.MethodGet, "/api/v1/mcp-servers", orgB, "")
	if items := decodeMcpRegBody(t, rr)["mcp_servers"].([]any); len(items) != 0 {
		t.Errorf("org B list must be empty, got %v", items)
	}

	// Org A deletes its own server; org B still gets 404 afterwards.
	rr = doMcpRegReq(t, h, http.MethodDelete, "/api/v1/mcp-servers/"+id, orgA, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", rr.Code)
	}
	if out := decodeMcpRegBody(t, rr); out["deleted"] != true || out["id"] != id {
		t.Fatalf("delete payload wrong: %v", out)
	}
	rr = doMcpRegReq(t, h, http.MethodGet, "/api/v1/mcp-servers/"+id, orgA, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("deleted server must 404, got %d", rr.Code)
	}

	entries, err := auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit list failed: %v", err)
	}
	var deleted int
	for _, e := range entries {
		if e.Action == "mcp.server_deleted" && e.Resource == "mcp_servers/"+id {
			deleted++
		}
	}
	if deleted != 1 {
		t.Fatalf("expected one mcp.server_deleted row, got %d", deleted)
	}
}

func TestMcpRegistryNeverEchoesSecretValues(t *testing.T) {
	h, authSvc, _, _, reg, fixture := newMcpRegTestRouter(t)
	token := mcpRegTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	// The org stores the token in the secrets service; the registration
	// references it by NAME only.
	secSvc := secrets.NewService()
	if _, err := secSvc.Create(context.Background(), "org-a", "WEATHER_API_KEY", "Bearer super-secret-tok-42", "user-1"); err != nil {
		t.Fatalf("secret create failed: %v", err)
	}
	reg.SetSecretResolver(secSvc)

	body := fmt.Sprintf(`{"name":"authed","endpoint_url":%q,"secret_ref":"WEATHER_API_KEY","auth_header":"Authorization"}`, fixture.srv.URL)
	rr := doMcpRegReq(t, h, http.MethodPost, "/api/v1/mcp-servers", token, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	// The registration probe carried the RESOLVED value (secret-backed
	// auth), but the HTTP surface echoes only the NAME.
	if got := fixture.header("Authorization"); got != "Bearer super-secret-tok-42" {
		t.Fatalf("probe auth header = %q", got)
	}
	if strings.Contains(rr.Body.String(), "super-secret-tok-42") {
		t.Fatalf("the response must never echo a secret value: %s", rr.Body.String())
	}
	if name := decodeMcpRegBody(t, rr)["mcp_server"].(map[string]any)["secret_ref"]; name != "WEATHER_API_KEY" {
		t.Fatalf("secret_ref name echo wrong: %v", name)
	}
}
