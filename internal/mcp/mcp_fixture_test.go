package mcp

// mcp_fixture_test.go — the in-test MCP server used as the record/replay
// fixture for the outbound client (issue #82): an httptest.Server speaking
// the same JSON-RPC 2.0 envelope as the platform's own inbound MCP server
// (protocol.go shapes), recording every request it receives and replaying
// canned responses driven by the test knobs.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// recordedRequest is one request the fixture received.
type recordedRequest struct {
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
	Envelope Request         `json:"-"`
	Headers  http.Header     `json:"-"`
}

// mcpFixture is the record/replay MCP server fixture.
type mcpFixture struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	// replay knobs (zero values = well-behaved server)
	initVersion string         // initialize answer ("" -> ProtocolVersion)
	tools       []RemoteTool   // tools/list answer (nil -> none)
	callResult  callToolResult // tools/call answer
	initErr     *RPCError      // JSON-RPC error for initialize
	listErr     *RPCError      // JSON-RPC error for tools/list
	callErr     *RPCError      // JSON-RPC error for tools/call
	httpStatus  int            // when != 0, answer with this raw HTTP status
	body        string         // raw body for the httpStatus answer
	delay       time.Duration  // sleep before answering
}

// newMCPFixture starts the fixture server.
func newMCPFixture(knob func(f *mcpFixture)) *mcpFixture {
	f := &mcpFixture{}
	if knob != nil {
		knob(f)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *mcpFixture) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var env Request
	_ = json.Unmarshal(raw, &env)

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method:   env.Method,
		Params:   env.Params,
		Envelope: env,
		Headers:  r.Header.Clone(),
	})
	knobs := fixtureKnobs{
		initVersion: f.initVersion,
		tools:       f.tools,
		callResult:  f.callResult,
		initErr:     f.initErr,
		listErr:     f.listErr,
		callErr:     f.callErr,
		httpStatus:  f.httpStatus,
		body:        f.body,
		delay:       f.delay,
	}
	f.mu.Unlock()

	if knobs.delay > 0 {
		time.Sleep(knobs.delay)
	}
	if knobs.httpStatus != 0 {
		w.WriteHeader(knobs.httpStatus)
		_, _ = w.Write([]byte(knobs.body))
		return
	}

	var result any
	var rpcErr *RPCError
	switch env.Method {
	case MethodInitialize:
		if knobs.initErr != nil {
			rpcErr = knobs.initErr
			break
		}
		version := knobs.initVersion
		if version == "" {
			version = ProtocolVersion
		}
		result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "fixture", "version": "1.0.0"},
		}
	case MethodToolsList:
		if knobs.listErr != nil {
			rpcErr = knobs.listErr
			break
		}
		tools := knobs.tools
		if tools == nil {
			tools = []RemoteTool{}
		}
		result = map[string]any{"tools": tools}
	case MethodToolsCall:
		if knobs.callErr != nil {
			rpcErr = knobs.callErr
			break
		}
		result = map[string]any{
			"content":           knobs.callResult.Content,
			"isError":           knobs.callResult.IsError,
			"structuredContent": knobs.callResult.StructuredContent,
		}
	default:
		rpcErr = &RPCError{Code: CodeMethodNotFound, Message: "fixture: unknown method"}
	}

	w.Header().Set("Content-Type", "application/json")
	resp := buildResponse(env.ID, result, rpcErr)
	_, _ = w.Write(marshalResponse(resp))
}

// fixtureKnobs snapshots the mutable knobs under the fixture lock so the
// handler reads a consistent view.
type fixtureKnobs struct {
	initVersion string
	tools       []RemoteTool
	callResult  callToolResult
	initErr     *RPCError
	listErr     *RPCError
	callErr     *RPCError
	httpStatus  int
	body        string
	delay       time.Duration
}

// recorded returns a copy of the recorded requests.
func (f *mcpFixture) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// mutate applies fn to the fixture knobs under the lock (tests re-arm the
// replay between calls; the handler snapshots consistently).
func (f *mcpFixture) mutate(fn func(f *mcpFixture)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// count returns how many requests arrived so far.
func (f *mcpFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// header returns the last value of the given request header across all
// recorded requests ("" when never set).
func (f *mcpFixture) header(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		if v := f.requests[i].Headers.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// close shuts the fixture down.
func (f *mcpFixture) close() { f.srv.Close() }
