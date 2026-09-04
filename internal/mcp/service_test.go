package mcp

// Issue #50 unit tests — the stateless JSON-RPC 2.0 / MCP dispatch loop,
// exercised directly (no HTTP): handshake negotiation, permission-scoped
// catalog, end-to-end calculator execution through the existing tools
// registry, error-code mapping, batch/notifications semantics and the
// tools/call record surfaced for auditing.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// allowAll is the permissive test authorizer.
func allowAll(context.Context, Identity) bool { return true }

// allowNonViewers mirrors the platform's runs.execute matrix (the real one
// is enforced by the auth middleware in cmd/api; here the role arrives in
// the Identity value).
func allowNonViewers(_ context.Context, id Identity) bool { return id.Role != "VIEWER" }

// call runs one request body through the service with a MEMBER identity.
func call(t *testing.T, svc *Service, body string) Outcome {
	t.Helper()
	return svc.Handle(context.Background(), Identity{UserID: "u1", OrganizationID: "org-a", Role: "MEMBER"}, []byte(body), allowNonViewers)
}

// callAs runs one request body through the service with a chosen identity.
func callAs(t *testing.T, svc *Service, identity Identity, body string) Outcome {
	t.Helper()
	return svc.Handle(context.Background(), identity, []byte(body), allowNonViewers)
}

// decode parses an outcome body into a generic object.
func decode(t *testing.T, out Outcome) map[string]any {
	t.Helper()
	if len(out.Body) == 0 {
		t.Fatalf("expected a response body, got none (status %d)", out.Status)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Body, &m); err != nil {
		t.Fatalf("response body is not JSON: %v (%q)", err, out.Body)
	}
	return m
}

// errCode extracts the JSON-RPC error code from a response body.
func errCode(t *testing.T, out Outcome) int {
	t.Helper()
	m := decode(t, out)
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error object, got %v", m)
	}
	code, ok := errObj["code"].(float64)
	if !ok {
		t.Fatalf("error object carries no numeric code: %v", errObj)
	}
	return int(code)
}

// resultMap extracts the result object from a success response.
func resultMap(t *testing.T, out Outcome) map[string]any {
	t.Helper()
	m := decode(t, out)
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result object, got %v", m)
	}
	return res
}

func newDefaultTestService() *Service {
	return NewDefaultService()
}

func TestInitializeHandshake(t *testing.T) {
	svc := newDefaultTestService()
	out := call(t, svc, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-client","version":"1.0"}}}`)
	if out.Status != 200 {
		t.Fatalf("initialize should be 200, got %d", out.Status)
	}
	res := resultMap(t, out)
	if res["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion should be %q, got %v", ProtocolVersion, res["protocolVersion"])
	}
	caps, ok := res["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result must carry capabilities: %v", res)
	}
	toolsCap, ok := caps["tools"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities must advertise tools: %v", caps)
	}
	if listChanged, _ := toolsCap["listChanged"].(bool); listChanged {
		t.Fatalf("stateless v1 must advertise listChanged=false: %v", toolsCap)
	}
	info, ok := res["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result must carry serverInfo: %v", res)
	}
	if info["name"] != ServerName {
		t.Fatalf("serverInfo.name should be %q, got %v", ServerName, info["name"])
	}
	if v, _ := info["version"].(string); v == "" {
		t.Fatalf("serverInfo.version must never be empty: %v", info)
	}
}

func TestInitializeVersionNegotiation(t *testing.T) {
	svc := newDefaultTestService()
	// An unsupported client version is answered with the server's latest.
	out := call(t, svc, `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if res := resultMap(t, out); res["protocolVersion"] != ProtocolVersion {
		t.Fatalf("unsupported version should negotiate down to %q, got %v", ProtocolVersion, res["protocolVersion"])
	}
	// Missing/absent params also negotiate to the server's latest.
	out = call(t, svc, `{"jsonrpc":"2.0","id":"b","method":"initialize"}`)
	if res := resultMap(t, out); res["protocolVersion"] != ProtocolVersion {
		t.Fatalf("initialize without params should answer %q, got %v", ProtocolVersion, res["protocolVersion"])
	}
}

func TestToolsListCatalog(t *testing.T) {
	svc := newDefaultTestService()
	out := call(t, svc, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if out.Status != 200 {
		t.Fatalf("tools/list should be 200, got %d", out.Status)
	}
	res := resultMap(t, out)
	list, ok := res["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list result must carry a tools array: %v", res)
	}
	if len(list) != 2 {
		t.Fatalf("default registry exposes calculator + http_request, got %d: %v", len(list), res)
	}
	byName := map[string]map[string]any{}
	for _, raw := range list {
		entry, _ := raw.(map[string]any)
		byName[entry["name"].(string)] = entry
	}
	calc, ok := byName["calculator"]
	if !ok {
		t.Fatalf("calculator missing from the MCP catalog: %v", res)
	}
	if desc, _ := calc["description"].(string); desc == "" {
		t.Errorf("calculator should carry a description: %v", calc)
	}
	schema, ok := calc["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("calculator should publish inputSchema (MCP camelCase wire name): %v", calc)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, present := props["expression"]; !present {
		t.Errorf("calculator inputSchema should document expression: %v", schema)
	}
	// Stateless v1: no pagination cursor, and no tenant fields at all
	// (tools are process-wide runtime surface, not org data).
	if _, present := res["nextCursor"]; present {
		t.Errorf("stateless v1 must not emit nextCursor: %v", res)
	}
	for _, leaked := range []string{"organization_id", "org_id", "organizationId"} {
		if _, present := res[leaked]; present {
			t.Errorf("tools/list must not carry organization fields (%s): %v", leaked, res)
		}
	}
}

func TestToolsListEmptyRegistry(t *testing.T) {
	svc := NewService(nil)
	out := call(t, svc, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	res := resultMap(t, out)
	list, ok := res["tools"].([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("nil registry should render an empty tools array: %v", res)
	}
}

func TestToolsListDeniedForViewer(t *testing.T) {
	svc := newDefaultTestService()
	out := callAs(t, svc, Identity{UserID: "u2", OrganizationID: "org-a", Role: "VIEWER"},
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if out.Status != 200 {
		t.Fatalf("authorization denial is an in-band JSON-RPC error (HTTP 200), got %d", out.Status)
	}
	if code := errCode(t, out); code != CodeForbidden {
		t.Fatalf("viewer tools/list must be denied with %d, got %d", CodeForbidden, code)
	}
}

func TestToolsCallCalculatorEndToEnd(t *testing.T) {
	svc := newDefaultTestService()
	out := call(t, svc, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"2*3+4"}}}`)
	if out.Status != 200 {
		t.Fatalf("tools/call should be 200, got %d", out.Status)
	}
	res := resultMap(t, out)
	if isError, _ := res["isError"].(bool); isError {
		t.Fatalf("calculator call must not be an error result: %v", res)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("tools/call result must carry one content block: %v", res)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("v1 content blocks are text: %v", block)
	}
	text, _ := block["text"].(string)
	if !strings.Contains(text, "10") {
		t.Fatalf("2*3+4 must evaluate to 10, got %q", text)
	}
	if out.ToolCall == nil {
		t.Fatal("tools/call must surface a ToolCall record for the audit layer")
	}
	if out.ToolCall.Tool != "calculator" || out.ToolCall.Denied || out.ToolCall.Unknown || !out.ToolCall.OK {
		t.Fatalf("unexpected ToolCall record: %+v", out.ToolCall)
	}
}

func TestToolsCallExecutionErrorIsInBand(t *testing.T) {
	svc := newDefaultTestService()
	// Division by zero is a TOOL failure -> successful JSON-RPC response
	// with isError true (MCP contract), not a protocol error.
	out := call(t, svc, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"1/0"}}}`)
	res := resultMap(t, out)
	if isError, _ := res["isError"].(bool); !isError {
		t.Fatalf("execution failure must set isError=true: %v", res)
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("error result must explain itself in content: %v", res)
	}
	block, _ := content[0].(map[string]any)
	if text, _ := block["text"].(string); !strings.Contains(text, "division by zero") {
		t.Fatalf("error content should carry the tool error: %v", block)
	}
	if out.ToolCall == nil || out.ToolCall.OK {
		t.Fatalf("failed execution must surface ToolCall with OK=false: %+v", out.ToolCall)
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	svc := newDefaultTestService()
	out := call(t, svc, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`)
	if code := errCode(t, out); code != CodeInvalidParams {
		t.Fatalf("unknown tool must be %d (invalid params), got %d", CodeInvalidParams, code)
	}
	if out.ToolCall == nil || !out.ToolCall.Unknown {
		t.Fatalf("unknown tool must surface ToolCall.Unknown: %+v", out.ToolCall)
	}
}

func TestToolsCallInvalidParams(t *testing.T) {
	svc := newDefaultTestService()
	cases := []struct {
		name string
		body string
	}{
		{"missing name", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`},
		{"empty params object", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`},
		{"blank name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"   "}}`},
		{"arguments as string", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"calculator","arguments":"x"}}`},
		{"arguments as array", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"calculator","arguments":[1]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := call(t, svc, tc.body)
			if code := errCode(t, out); code != CodeInvalidParams {
				t.Fatalf("expected %d, got %d (body %s)", CodeInvalidParams, code, out.Body)
			}
		})
	}
}

func TestToolsCallDeniedForViewer(t *testing.T) {
	svc := newDefaultTestService()
	out := callAs(t, svc, Identity{UserID: "u2", OrganizationID: "org-a", Role: "VIEWER"},
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"1+1"}}}`)
	if out.Status != 200 {
		t.Fatalf("denial is in-band (HTTP 200), got %d", out.Status)
	}
	if code := errCode(t, out); code != CodeForbidden {
		t.Fatalf("viewer tools/call must be denied with %d, got %d", CodeForbidden, code)
	}
	if out.ToolCall == nil || !out.ToolCall.Denied {
		t.Fatalf("denied call must surface ToolCall.Denied for the audit layer: %+v", out.ToolCall)
	}
}

func TestToolsCallNullArgumentsIsAccepted(t *testing.T) {
	svc := newDefaultTestService()
	// JSON null arguments behave like an empty object (the calculator's own
	// validation rejects the missing expression as a tool error).
	out := call(t, svc, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"calculator","arguments":null}}`)
	res := resultMap(t, out)
	if isError, _ := res["isError"].(bool); !isError {
		t.Fatalf("missing expression should surface the tool's own error in-band: %v", res)
	}
}

func TestPing(t *testing.T) {
	svc := newDefaultTestService()
	out := call(t, svc, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if out.Status != 200 {
		t.Fatalf("ping should be 200, got %d", out.Status)
	}
	m := decode(t, out)
	if _, hasErr := m["error"]; hasErr {
		t.Fatalf("ping must not error: %v", m)
	}
	res, ok := m["result"].(map[string]any)
	if !ok || len(res) != 0 {
		t.Fatalf("ping result must be the empty object, got %v", m["result"])
	}
}

func TestMethodNotFound(t *testing.T) {
	svc := newDefaultTestService()
	out := call(t, svc, `{"jsonrpc":"2.0","id":10,"method":"resources/list"}`)
	if out.Status != 200 {
		t.Fatalf("method-not-found is an in-band JSON-RPC error (HTTP 200), got %d", out.Status)
	}
	if code := errCode(t, out); code != CodeMethodNotFound {
		t.Fatalf("expected %d, got %d", CodeMethodNotFound, code)
	}
}

func TestParseError(t *testing.T) {
	svc := newDefaultTestService()
	out := call(t, svc, `this is not json`)
	if out.Status != 400 {
		t.Fatalf("parse error should be HTTP 400, got %d", out.Status)
	}
	if code := errCode(t, out); code != CodeParseError {
		t.Fatalf("expected %d, got %d", CodeParseError, code)
	}
	// The response id must be null when the request id was undetectable.
	m := decode(t, out)
	if id, present := m["id"]; !present || id != nil {
		t.Fatalf("parse error response must carry id null, got %v", m["id"])
	}
}

func TestEmptyBodyIsParseError(t *testing.T) {
	svc := newDefaultTestService()
	out := svc.Handle(context.Background(), Identity{Role: "MEMBER"}, nil, allowNonViewers)
	if out.Status != 400 {
		t.Fatalf("empty body should be 400, got %d", out.Status)
	}
	if code := errCode(t, out); code != CodeParseError {
		t.Fatalf("expected %d, got %d", CodeParseError, code)
	}
}

func TestInvalidRequestMatrix(t *testing.T) {
	svc := newDefaultTestService()
	cases := []struct {
		name string
		body string
	}{
		{"json scalar", `42`},
		{"json string", `"hello"`},
		{"json null", `null`},
		{"wrong jsonrpc version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`},
		{"missing jsonrpc member", `{"id":1,"method":"ping"}`},
		{"missing method", `{"jsonrpc":"2.0","id":1}`},
		{"empty method", `{"jsonrpc":"2.0","id":1,"method":"  "}`},
		{"object id", `{"jsonrpc":"2.0","id":{"x":1},"method":"ping"}`},
		{"array id", `{"jsonrpc":"2.0","id":[1],"method":"ping"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := call(t, svc, tc.body)
			if out.Status != 400 {
				t.Fatalf("invalid request should be HTTP 400, got %d", out.Status)
			}
			if code := errCode(t, out); code != CodeInvalidRequest {
				t.Fatalf("expected %d, got %d", CodeInvalidRequest, code)
			}
		})
	}
}

func TestBatchRequestsRejected(t *testing.T) {
	svc := newDefaultTestService()
	out := call(t, svc, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`)
	if out.Status != 400 {
		t.Fatalf("batch must be rejected with HTTP 400, got %d", out.Status)
	}
	if code := errCode(t, out); code != CodeInvalidRequest {
		t.Fatalf("batch must be rejected with %d, got %d", CodeInvalidRequest, code)
	}
	// Exactly ONE error object (not an array of responses).
	m := decode(t, out)
	if _, isErr := m["error"]; !isErr {
		t.Fatalf("batch rejection must be a single JSON-RPC error object: %v", m)
	}
}

func TestNotificationsProduceNoResponse(t *testing.T) {
	svc := newDefaultTestService()
	cases := []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"ping"}`,       // notification ping: never answered
		`{"jsonrpc":"2.0","method":"tools/list"}`, // even privileged notifications are dropped
	}
	for _, body := range cases {
		out := call(t, svc, body)
		if out.Status != 204 {
			t.Fatalf("notification should answer 204, got %d (%q)", out.Status, body)
		}
		if len(out.Body) != 0 {
			t.Fatalf("notification must have no body, got %q", out.Body)
		}
	}
}

func TestRequestIDEcho(t *testing.T) {
	svc := newDefaultTestService()
	cases := []struct {
		body string
		want any
	}{
		{`{"jsonrpc":"2.0","id":"abc","method":"ping"}`, "abc"},
		{`{"jsonrpc":"2.0","id":7,"method":"ping"}`, float64(7)},
		{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, nil},
	}
	for _, tc := range cases {
		out := call(t, svc, tc.body)
		m := decode(t, out)
		id, present := m["id"]
		if !present {
			t.Fatalf("response must always carry id: %v", m)
		}
		if id != tc.want {
			t.Fatalf("id echo mismatch: want %v got %v (%s)", tc.want, id, tc.body)
		}
	}
}

func TestToolsCallHonorsCanceledContext(t *testing.T) {
	svc := NewDefaultService()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller went away before the call ran
	out := svc.Handle(ctx, Identity{UserID: "u1", OrganizationID: "org-a", Role: "MEMBER"},
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"1+1"}}}`), allowAll)
	res := resultMap(t, out)
	if isError, _ := res["isError"].(bool); !isError {
		t.Fatalf("canceled call must surface an in-band error result: %v", res)
	}
}

func TestToolCallRecordNotSetForOtherMethods(t *testing.T) {
	svc := newDefaultTestService()
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
	} {
		if out := call(t, svc, body); out.ToolCall != nil {
			t.Fatalf("only tools/call surfaces a ToolCall record (%s): %+v", body, out.ToolCall)
		}
	}
}
