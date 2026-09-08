package mcp

// client_test.go — the outbound streamable-HTTP client (issue #82),
// exercised against the in-test record/replay fixture server: handshake
// negotiation, tools/list mapping, tools/call result mapping, the typed
// error surface (HTTP / JSON-RPC / remote-isError), auth-header injection
// and timeouts.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClientInitializeHandshake(t *testing.T) {
	f := newMCPFixture(nil)
	defer f.close()
	c := NewClient(f.srv.URL)

	res, err := c.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if res.ProtocolVersion != ProtocolVersion {
		t.Fatalf("negotiated version = %q, want %q", res.ProtocolVersion, ProtocolVersion)
	}
	if res.ServerInfo.Name != "fixture" || res.ServerInfo.Version != "1.0.0" {
		t.Fatalf("unexpected serverInfo: %+v", res.ServerInfo)
	}

	reqs := f.recorded()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly one request, got %d", len(reqs))
	}
	if reqs[0].Method != MethodInitialize {
		t.Fatalf("method = %q, want initialize", reqs[0].Method)
	}
	if reqs[0].Envelope.JSONRPC != JSONRPCVersion {
		t.Fatalf("envelope jsonrpc = %q, want %q", reqs[0].Envelope.JSONRPC, JSONRPCVersion)
	}
	// The client proposes the version it supports.
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	if err := unmarshalParams(reqs[0].Params, &params); err != nil {
		t.Fatalf("params decode failed: %v", err)
	}
	if params.ProtocolVersion != ProtocolVersion {
		t.Fatalf("client proposed %q, want %q", params.ProtocolVersion, ProtocolVersion)
	}
	if params.ClientInfo.Name != ClientName {
		t.Fatalf("clientInfo.name = %q, want %q", params.ClientInfo.Name, ClientName)
	}
}

// unmarshalParams decodes a recorded raw params value.
func unmarshalParams(raw []byte, dst any) error {
	if len(raw) == 0 {
		raw = []byte("null")
	}
	return json.Unmarshal(raw, dst)
}

func TestClientInitializeRecordsServerChosenVersion(t *testing.T) {
	// The MCP negotiation rule: the SERVER picks the version it will use.
	f := newMCPFixture(func(f *mcpFixture) { f.initVersion = "2025-03-26" })
	defer f.close()
	c := NewClient(f.srv.URL)

	res, err := c.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if res.ProtocolVersion != "2025-03-26" {
		t.Fatalf("expected the server-chosen version to be recorded, got %q", res.ProtocolVersion)
	}
}

func TestClientInitializeWithoutProtocolVersionFails(t *testing.T) {
	// A protocol violation: an initialize answer without a version.
	f := newMCPFixture(func(f *mcpFixture) {
		f.httpStatus = 200
		f.body = `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{},"serverInfo":{"name":"broken","version":"1.0.0"}}}`
	})
	defer f.close()
	c := NewClient(f.srv.URL)

	_, err := c.Initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "without a protocolVersion") {
		t.Fatalf("expected a missing-protocolVersion error, got %v", err)
	}
}

func TestClientListToolsMapsCatalog(t *testing.T) {
	f := newMCPFixture(func(f *mcpFixture) {
		f.tools = []RemoteTool{
			{
				Name:        "get_weather",
				Description: "Returns the weather",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []string{"city"},
				},
			},
			{Name: "no_schema_tool"},
		}
	})
	defer f.close()
	c := NewClient(f.srv.URL)

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "get_weather" || tools[0].Description != "Returns the weather" {
		t.Fatalf("tool mapping failed: %+v", tools[0])
	}
	if _, ok := tools[0].InputSchema["properties"]; !ok {
		t.Fatalf("inputSchema not passed through: %+v", tools[0].InputSchema)
	}
	if tools[1].Name != "no_schema_tool" || tools[1].InputSchema != nil {
		t.Fatalf("schema-less tool mapping failed: %+v", tools[1])
	}

	reqs := f.recorded()
	if len(reqs) != 1 || reqs[0].Method != MethodToolsList {
		t.Fatalf("expected one tools/list request, got %+v", reqs)
	}
}

func TestClientCallToolMapsResult(t *testing.T) {
	f := newMCPFixture(func(f *mcpFixture) {
		f.callResult = callToolResult{
			Content: []textContent{{Type: "text", Text: "22 degrees"}, {Type: "text", Text: "sunny"}},
		}
	})
	defer f.close()
	c := NewClient(f.srv.URL)

	out, err := c.CallTool(context.Background(), "get_weather", map[string]any{"city": "Berlin"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if out["content"] != "22 degrees\nsunny" {
		t.Fatalf("content mapping failed: %v", out["content"])
	}
	if _, has := out["structured"]; has {
		t.Fatalf("structured key must be absent when the server sent none: %v", out)
	}

	reqs := f.recorded()
	if len(reqs) != 1 || reqs[0].Method != MethodToolsCall {
		t.Fatalf("expected one tools/call request, got %+v", reqs)
	}
	var params callToolParams
	if err := unmarshalParams(reqs[0].Params, &params); err != nil {
		t.Fatalf("params decode failed: %v", err)
	}
	if params.Name != "get_weather" {
		t.Fatalf("remote tool name = %q", params.Name)
	}
	if params.Arguments["city"] != "Berlin" {
		t.Fatalf("arguments mapping failed: %v", params.Arguments)
	}
}

func TestClientCallToolStructuredContent(t *testing.T) {
	f := newMCPFixture(func(f *mcpFixture) {
		f.callResult = callToolResult{
			StructuredContent: map[string]any{"temp_c": 22},
		}
	})
	defer f.close()
	c := NewClient(f.srv.URL)

	out, err := c.CallTool(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	structured, ok := out["structured"].(map[string]any)
	if !ok || structured["temp_c"] != float64(22) {
		t.Fatalf("structured mapping failed: %v", out["structured"])
	}
}

func TestClientCallToolRemoteIsError(t *testing.T) {
	// The MCP error contract: a tool that RAN but failed answers
	// isError:true inside a successful envelope -> typed ToolCallError.
	f := newMCPFixture(func(f *mcpFixture) {
		f.callResult = callToolResult{
			Content: []textContent{{Type: "text", Text: "city not found"}},
			IsError: true,
		}
	})
	defer f.close()
	c := NewClient(f.srv.URL)

	_, err := c.CallTool(context.Background(), "get_weather", nil)
	var toolErr *ToolCallError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *ToolCallError, got %T (%v)", err, err)
	}
	if toolErr.Tool != "get_weather" || toolErr.Text != "city not found" {
		t.Fatalf("ToolCallError fields wrong: %+v", toolErr)
	}
}

func TestClientCallToolEmptyNameRejectedLocally(t *testing.T) {
	f := newMCPFixture(nil)
	defer f.close()
	c := NewClient(f.srv.URL)

	if _, err := c.CallTool(context.Background(), "  ", nil); !errors.Is(err, ErrInvalidToolParams) {
		t.Fatalf("blank tool name must be ErrInvalidToolParams, got %v", err)
	}
	if f.count() != 0 {
		t.Fatalf("no request must hit the wire, got %d", f.count())
	}
}

func TestClientErrorMapping(t *testing.T) {
	t.Run("jsonrpc error object", func(t *testing.T) {
		f := newMCPFixture(func(f *mcpFixture) {
			f.listErr = &RPCError{Code: CodeInvalidParams, Message: "bad cursor"}
		})
		defer f.close()
		c := NewClient(f.srv.URL)
		_, err := c.ListTools(context.Background())
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) {
			t.Fatalf("expected *RPCError, got %T (%v)", err, err)
		}
		if rpcErr.Code != CodeInvalidParams || rpcErr.Message != "bad cursor" {
			t.Fatalf("RPCError fields wrong: %+v", rpcErr)
		}
	})

	t.Run("http error status", func(t *testing.T) {
		f := newMCPFixture(func(f *mcpFixture) {
			f.httpStatus = 503
			f.body = "maintenance"
		})
		defer f.close()
		c := NewClient(f.srv.URL)
		_, err := c.ListTools(context.Background())
		if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
			t.Fatalf("expected an HTTP 503 error, got %v", err)
		}
	})

	t.Run("non-json body", func(t *testing.T) {
		f := newMCPFixture(func(f *mcpFixture) {
			f.httpStatus = 200
			f.body = "<html>not json-rpc</html>"
		})
		defer f.close()
		c := NewClient(f.srv.URL)
		_, err := c.ListTools(context.Background())
		if err == nil || !strings.Contains(err.Error(), "non-JSON-RPC") {
			t.Fatalf("expected a non-JSON-RPC body error, got %v", err)
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		// Closed endpoint: connection refused.
		f := newMCPFixture(nil)
		url := f.srv.URL
		f.close()
		c := NewClient(url)
		if _, err := c.ListTools(context.Background()); err == nil {
			t.Fatalf("expected a transport error for a dead endpoint")
		}
	})

	t.Run("empty envelope", func(t *testing.T) {
		f := newMCPFixture(func(f *mcpFixture) {
			f.httpStatus = 200
			f.body = `{"jsonrpc":"2.0","id":1}`
		})
		defer f.close()
		c := NewClient(f.srv.URL)
		if _, err := c.ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "without a result or error") {
			t.Fatalf("expected an empty-envelope error, got %v", err)
		}
	})
}

func TestClientAuthHeaderInjection(t *testing.T) {
	f := newMCPFixture(nil)
	defer f.close()
	c := NewClient(f.srv.URL, WithClientAuthHeader(func(ctx context.Context) (string, string, error) {
		return "Authorization", "Bearer tok-123", nil
	}))

	if _, err := c.CallTool(context.Background(), "t", nil); err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if got := f.header("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("auth header not injected: %q", got)
	}
	if got := f.header("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
}

func TestClientAuthHeaderResolvedPerRequest(t *testing.T) {
	// The provider must run FRESH for every request (secret rotation).
	f := newMCPFixture(nil)
	defer f.close()
	token := "tok-1"
	c := NewClient(f.srv.URL, WithClientAuthHeader(func(ctx context.Context) (string, string, error) {
		token += "0" // rotate: tok-10, tok-100
		return "Authorization", "Bearer " + token, nil
	}))

	_, _ = c.CallTool(context.Background(), "t", nil)
	_, _ = c.CallTool(context.Background(), "t", nil)
	_, _ = c.CallTool(context.Background(), "t", nil)
	if got := f.header("Authorization"); got != "Bearer tok-1000" {
		t.Fatalf("expected a fresh resolution per call (last tok-1000), got %q", got)
	}
}

func TestClientAuthHeaderFailureAbortsBeforeWire(t *testing.T) {
	f := newMCPFixture(nil)
	defer f.close()
	c := NewClient(f.srv.URL, WithClientAuthHeader(func(ctx context.Context) (string, string, error) {
		return "", "", errors.New("secret not found")
	}))

	_, err := c.CallTool(context.Background(), "t", nil)
	if err == nil || !strings.Contains(err.Error(), "auth header resolution failed") {
		t.Fatalf("expected an auth resolution error, got %v", err)
	}
	if f.count() != 0 {
		t.Fatalf("a failed auth resolution must never hit the wire, got %d requests", f.count())
	}
}

func TestClientAuthHeaderRejectsInjection(t *testing.T) {
	// CR/LF in the resolved value is a header-injection attempt: refused.
	f := newMCPFixture(nil)
	defer f.close()
	c := NewClient(f.srv.URL, WithClientAuthHeader(func(ctx context.Context) (string, string, error) {
		return "Authorization", "Bearer x\r\nX-Evil: 1", nil
	}))
	if _, err := c.CallTool(context.Background(), "t", nil); err == nil {
		t.Fatalf("expected a header-injection refusal")
	}
	if f.count() != 0 {
		t.Fatalf("nothing must hit the wire, got %d requests", f.count())
	}
}

func TestClientTimeout(t *testing.T) {
	f := newMCPFixture(func(f *mcpFixture) { f.delay = 400 * time.Millisecond })
	defer f.close()
	c := NewClient(f.srv.URL, WithClientTimeout(50*time.Millisecond))

	start := time.Now()
	_, err := c.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout not enforced promptly: %s", elapsed)
	}
}

func TestClientContextCancellationHonored(t *testing.T) {
	f := newMCPFixture(func(f *mcpFixture) { f.delay = 500 * time.Millisecond })
	defer f.close()
	c := NewClient(f.srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Initialize(ctx); err == nil {
		t.Fatalf("expected a cancellation error")
	}
}

func TestClientEndpointGuard(t *testing.T) {
	c := NewClient("")
	if _, err := c.Initialize(context.Background()); err == nil {
		t.Fatalf("expected an endpoint error for an empty endpoint")
	}
}
