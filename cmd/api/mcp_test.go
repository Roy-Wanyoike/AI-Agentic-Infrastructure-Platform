package main

// Issue #50 handler tests — the MCP endpoint flows through the real
// middleware chain (RequireAuthOrAPIKey + the real runs.execute permission
// check) and the real internal/mcp service over the real default tool
// registry. All in-memory except one sqlmock test that pins the audit row
// in Postgres store mode.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/mcp"
)

// mcpHandlerEnv wires the endpoint with the production registration helper
// and returns bearer tokens for one tenant's OWNER/MEMBER/VIEWER, a foreign
// tenant's OWNER, and one API key of the primary tenant.
type mcpHandlerEnv struct {
	mux         *http.ServeMux
	authSvc     *auth.Service
	auditSvc    *audit.Service
	orgID       string
	ownerID     string
	ownerToken  string
	memberToken string
	viewerToken string
	otherToken  string
	apiKeyValue string
}

func newMcpHandlerEnv(t *testing.T) *mcpHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()
	auditSvc := audit.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	ownerToken, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken(owner) returned error: %v", err)
	}
	memberToken, err := authSvc.GenerateToken(&auth.User{
		ID: "user-member", Organization: owner.Organization, Email: "member@acme.test", Role: "MEMBER",
	})
	if err != nil {
		t.Fatalf("GenerateToken(member) returned error: %v", err)
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
	key, err := apiKeysSvc.Create(owner.Organization, owner.ID, "mcp-key")
	if err != nil {
		t.Fatalf("apiKeysSvc.Create returned error: %v", err)
	}

	mux := http.NewServeMux()
	registerMcpServiceRoutes(mux, mcp.NewDefaultService(), authSvc, apiKeysSvc, auditSvc)

	return &mcpHandlerEnv{
		mux:         mux,
		authSvc:     authSvc,
		auditSvc:    auditSvc,
		orgID:       owner.Organization,
		ownerID:     owner.ID,
		ownerToken:  ownerToken,
		memberToken: memberToken,
		viewerToken: viewerToken,
		otherToken:  otherToken,
		apiKeyValue: key.Value,
	}
}

// post sends one JSON-RPC body with optional bearer token / API key.
func (e *mcpHandlerEnv) post(t *testing.T, body, token, apiKey string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	var decoded map[string]any
	if rr.Body.Len() > 0 {
		// Best-effort decode: transport-level middleware rejections (401)
		// are plain text, not JSON — callers assert on rr.Code there.
		_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
	}
	return rr, decoded
}

// mcpErrCode extracts the JSON-RPC error code from a decoded response.
func mcpErrCode(t *testing.T, decoded map[string]any) int {
	t.Helper()
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error object, got %v", decoded)
	}
	code, ok := errObj["code"].(float64)
	if !ok {
		t.Fatalf("error object carries no numeric code: %v", errObj)
	}
	return int(code)
}

func TestMcpEndpointRequiresAuth(t *testing.T) {
	env := newMcpHandlerEnv(t)
	rr, _ := env.post(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /mcp must be 401, got %d", rr.Code)
	}
	rr, _ = env.post(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "not-a-token", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("invalid bearer token must be 401, got %d", rr.Code)
	}
}

func TestMcpInitializeHandshakeEndToEnd(t *testing.T) {
	env := newMcpHandlerEnv(t)
	rr, decoded := env.post(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude","version":"1"}}}`,
		env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("initialize should be 200, got %d: %v", rr.Code, decoded)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("responses must be application/json, got %q", ct)
	}
	res, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize must return a result object: %v", decoded)
	}
	if res["protocolVersion"] != mcp.ProtocolVersion {
		t.Fatalf("protocolVersion should be %q, got %v", mcp.ProtocolVersion, res["protocolVersion"])
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info == nil || info["name"] != mcp.ServerName {
		t.Fatalf("serverInfo.name should be agentos: %v", res)
	}
}

func TestMcpToolsListRBACMatrix(t *testing.T) {
	env := newMcpHandlerEnv(t)

	// OWNER and MEMBER pass runs.execute and see the catalog.
	for name, token := range map[string]string{"owner": env.ownerToken, "member": env.memberToken} {
		rr, decoded := env.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, token, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s tools/list should be 200, got %d", name, rr.Code)
		}
		res, _ := decoded["result"].(map[string]any)
		tools, _ := res["tools"].([]any)
		if len(tools) != 2 {
			t.Fatalf("%s should see calculator + http_request, got %v", name, decoded)
		}
	}

	// VIEWER lacks runs.execute: in-band -32000, never a catalog.
	rr, decoded := env.post(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, env.viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer denial is an in-band JSON-RPC error (HTTP 200), got %d", rr.Code)
	}
	if code := mcpErrCode(t, decoded); code != mcp.CodeForbidden {
		t.Fatalf("viewer tools/list must be denied with %d, got %d", mcp.CodeForbidden, code)
	}
	if _, hasResult := decoded["result"]; hasResult {
		t.Fatalf("denied tools/list must not leak a result: %v", decoded)
	}

	// An API key authenticates as OWNER and may list.
	rr, decoded = env.post(t, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, "", env.apiKeyValue)
	if rr.Code != http.StatusOK {
		t.Fatalf("api-key tools/list should be 200, got %d", rr.Code)
	}
	res, _ := decoded["result"].(map[string]any)
	if tools, _ := res["tools"].([]any); len(tools) != 2 {
		t.Fatalf("api-key caller should see the catalog: %v", decoded)
	}
}

func TestMcpToolsCallCalculatorEndToEndAndAudit(t *testing.T) {
	env := newMcpHandlerEnv(t)
	rr, decoded := env.post(t,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"6*7"}}}`,
		env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call should be 200, got %d: %v", rr.Code, decoded)
	}
	res, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call must return a result object: %v", decoded)
	}
	if isError, _ := res["isError"].(bool); isError {
		t.Fatalf("6*7 must not be an error result: %v", res)
	}
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("result must carry exactly one content block: %v", res)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("v1 content blocks are text: %v", block)
	}
	if text, _ := block["text"].(string); !strings.Contains(text, "42") {
		t.Fatalf("6*7 must evaluate to 42, got %q", text)
	}

	// The audit row: channel marker "mcp" + tool id + caller principal.
	entries := env.auditSvc.List()
	if len(entries) != 1 {
		t.Fatalf("exactly one audit row expected, got %d: %v", len(entries), entries)
	}
	e := entries[0]
	if e.Action != "mcp.tool_call" {
		t.Errorf("audit action should be mcp.tool_call, got %q", e.Action)
	}
	if e.Resource != "tools/calculator" {
		t.Errorf("audit resource should be tools/calculator, got %q", e.Resource)
	}
	if e.Actor != env.ownerID {
		t.Errorf("audit actor should be the caller principal %q, got %q", env.ownerID, e.Actor)
	}
	if e.OrganizationID != env.orgID {
		t.Errorf("audit row must be tenant-scoped to the caller org, got %q", e.OrganizationID)
	}
	if e.Metadata["channel"] != "mcp" || e.Metadata["tool"] != "calculator" || e.Metadata["ok"] != true {
		t.Errorf("audit metadata should mark the mcp channel + tool + ok, got %v", e.Metadata)
	}
}

func TestMcpToolsCallDeniedForViewerIsAudited(t *testing.T) {
	env := newMcpHandlerEnv(t)
	rr, decoded := env.post(t,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"1+1"}}}`,
		env.viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer denial is in-band (HTTP 200), got %d", rr.Code)
	}
	if code := mcpErrCode(t, decoded); code != mcp.CodeForbidden {
		t.Fatalf("viewer tools/call must be denied with %d, got %d", mcp.CodeForbidden, code)
	}
	entries := env.auditSvc.List()
	if len(entries) != 1 {
		t.Fatalf("denied attempt must be audited exactly once, got %d", len(entries))
	}
	if entries[0].Actor != "user-viewer" {
		t.Errorf("denied attempt must record the caller principal, got %q", entries[0].Actor)
	}
	if entries[0].Metadata["denied"] != true {
		t.Errorf("denied attempt must be marked denied in metadata: %v", entries[0].Metadata)
	}
}

func TestMcpToolsCallUnknownToolIsAudited(t *testing.T) {
	env := newMcpHandlerEnv(t)
	rr, decoded := env.post(t,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"no_such_tool"}}`,
		env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("unknown tool is an in-band error (HTTP 200), got %d", rr.Code)
	}
	if code := mcpErrCode(t, decoded); code != mcp.CodeInvalidParams {
		t.Fatalf("unknown tool must be %d, got %d", mcp.CodeInvalidParams, code)
	}
	entries := env.auditSvc.List()
	if len(entries) != 1 || entries[0].Metadata["error"] != "unknown_tool" {
		t.Fatalf("unknown-tool attempt must be audited with error metadata: %v", entries)
	}
}

func TestMcpCrossTenantCatalogIdenticalAndOrgFree(t *testing.T) {
	env := newMcpHandlerEnv(t)
	_, first := env.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, env.ownerToken, "")
	_, second := env.post(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, env.otherToken, "")

	// The catalog is process-wide runtime surface: both tenants see the
	// identical payload and neither carries organization fields.
	firstJSON, _ := json.Marshal(first["result"])
	secondJSON, _ := json.Marshal(second["result"])
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("catalog must be identical across tenants:\n%s\n%s", firstJSON, secondJSON)
	}
	if strings.Contains(string(firstJSON), "organization") || strings.Contains(string(firstJSON), "org") {
		t.Fatalf("catalog payload must not carry organization fields: %s", firstJSON)
	}
}

func TestMcpJSONRPCErrorMapping(t *testing.T) {
	env := newMcpHandlerEnv(t)

	// -32601 method not found (in-band, HTTP 200).
	rr, decoded := env.post(t, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, env.ownerToken, "")
	if rr.Code != http.StatusOK || mcpErrCode(t, decoded) != mcp.CodeMethodNotFound {
		t.Fatalf("unknown method must be 200 + %d, got %d / %v", mcp.CodeMethodNotFound, rr.Code, decoded)
	}

	// -32700 parse error (HTTP 400).
	rr, decoded = env.post(t, `{not json`, env.ownerToken, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("parse error must be HTTP 400, got %d", rr.Code)
	}
	if code := mcpErrCode(t, decoded); code != mcp.CodeParseError {
		t.Fatalf("parse error code must be %d, got %d", mcp.CodeParseError, code)
	}

	// -32600 invalid request: batches are rejected (stateless v1).
	rr, decoded = env.post(t, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, env.ownerToken, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("batch must be rejected with HTTP 400, got %d", rr.Code)
	}
	if code := mcpErrCode(t, decoded); code != mcp.CodeInvalidRequest {
		t.Fatalf("batch code must be %d, got %d", mcp.CodeInvalidRequest, code)
	}

	// -32602 invalid params: unknown tool + malformed arguments.
	rr, decoded = env.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"calculator","arguments":"nope"}}`, env.ownerToken, "")
	if rr.Code != http.StatusOK || mcpErrCode(t, decoded) != mcp.CodeInvalidParams {
		t.Fatalf("malformed arguments must be 200 + %d, got %d / %v", mcp.CodeInvalidParams, rr.Code, decoded)
	}
}

func TestMcpWrongHTTPMethodRejected(t *testing.T) {
	env := newMcpHandlerEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+env.ownerToken)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp must be 405, got %d", rr.Code)
	}
}

func TestMcpNotificationProducesNoBody(t *testing.T) {
	env := newMcpHandlerEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+env.ownerToken)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("notification should answer 204, got %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("notification must have no body, got %q", rr.Body.String())
	}
}

// TestMcpToolsCallAuditPostgresMode pins the durable audit row in store
// mode: the mcp.tool_call INSERT binds the tenant scope, the caller
// principal, the action/resource pair and the "mcp" channel metadata.
func TestMcpToolsCallAuditPostgresMode(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	authSvc := auth.NewService("test-secret")
	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	auditSvc := audit.NewServiceWithStore(audit.NewPostgresStore(db))

	mux := http.NewServeMux()
	registerMcpServiceRoutes(mux, mcp.NewDefaultService(), authSvc, apikeys.NewService(), auditSvc)

	// encoding/json renders map keys sorted, so the metadata bind is
	// deterministic: channel < ok < tool.
	mock.ExpectExec("INSERT INTO audit_logs").
		WithArgs(
			sqlmock.AnyArg(),   // entry id
			owner.Organization, // organization_id (tenant guard)
			owner.ID,           // actor = caller principal
			"mcp.tool_call",    // action
			"tools/calculator", // resource = tool id
			`{"channel":"mcp","ok":true,"tool":"calculator"}`, // metadata (channel marker, no arguments)
			sqlmock.AnyArg(), // created_at
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"2+2"}}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call should be 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("audit INSERT expectations not met: %v", err)
	}
}
