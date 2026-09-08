package tools

// mcp_test.go — the MCP tool adapter (issue #82): registry contract,
// catalog metadata, result mapping and failure semantics through a fake
// MCPToolCaller (the *mcp.Client conformance itself is pinned in
// internal/mcp).

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeMCPCaller is a scripted MCPToolCaller.
type fakeMCPCaller struct {
	calls []fakeMCPCall
	res   map[string]any
	err   error
	// observeCtx records whether the context arrived (ContextAware path).
	cancelled bool
}

type fakeMCPCall struct {
	tool string
	args map[string]any
}

func (f *fakeMCPCaller) CallTool(ctx context.Context, toolName string, args map[string]any) (map[string]any, error) {
	if ctx != nil && ctx.Err() != nil {
		f.cancelled = true
	}
	f.calls = append(f.calls, fakeMCPCall{tool: toolName, args: args})
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func newMCPTestTool(caller MCPToolCaller) *MCPTool {
	return NewMCPTool(
		"mcp_acme_crm_get_contact",
		"srv-123",
		"Acme CRM",
		"get-contact",
		"Fetches one contact by email.",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{"email": map[string]any{"type": "string"}},
			"required":   []string{"email"},
		},
		caller,
	)
}

func TestMCPToolImplementsPlatformContracts(t *testing.T) {
	tool := newMCPTestTool(&fakeMCPCaller{})
	var _ Tool = tool
	var _ ContextAware = tool
	var _ Described = tool
	if tool.Name() != "mcp_acme_crm_get_contact" {
		t.Fatalf("Name = %q", tool.Name())
	}
	if tool.ServerID() != "srv-123" || tool.ServerName() != "Acme CRM" || tool.RemoteName() != "get-contact" {
		t.Fatalf("identity accessors wrong: %q %q %q", tool.ServerID(), tool.ServerName(), tool.RemoteName())
	}
}

func TestMCPToolDescribeToolServesCachedRemoteMetadata(t *testing.T) {
	tool := newMCPTestTool(&fakeMCPCaller{})
	info := tool.DescribeTool()
	if info.Name != tool.Name() {
		t.Fatalf("catalog name mismatch: %q", info.Name)
	}
	if info.Description != "Fetches one contact by email." {
		t.Fatalf("catalog description mismatch: %q", info.Description)
	}
	if _, ok := info.InputSchema["required"]; !ok {
		t.Fatalf("inputSchema must pass through uninterpreted: %v", info.InputSchema)
	}
}

func TestMCPToolExecuteMapsRemoteResult(t *testing.T) {
	caller := &fakeMCPCaller{res: map[string]any{"content": "Jane Doe <jane@acme.com>"}}
	tool := newMCPTestTool(caller)

	out, err := tool.Execute(map[string]any{"email": "jane@acme.com"})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if out["content"] != "Jane Doe <jane@acme.com>" {
		t.Fatalf("result mapping failed: %v", out)
	}
	if len(caller.calls) != 1 || caller.calls[0].tool != "get-contact" {
		t.Fatalf("the REMOTE tool name must be invoked, got %+v", caller.calls)
	}
	if caller.calls[0].args["email"] != "jane@acme.com" {
		t.Fatalf("arguments must pass through: %v", caller.calls[0].args)
	}
}

func TestMCPToolExecuteContextHonorsCancellation(t *testing.T) {
	caller := &fakeMCPCaller{}
	tool := newMCPTestTool(caller)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.ExecuteContext(ctx, nil); err != nil {
		// The adapter itself is context-transparent: it must still CALL
		// through (deadline policy belongs to the caller/client); assert
		// the canceled context actually reached the caller.
		t.Fatalf("ExecuteContext returned an unexpected error: %v", err)
	}
	if !caller.cancelled {
		t.Fatalf("the canceled context must reach the caller (ContextAware contract)")
	}
}

func TestMCPToolErrorsSurfaceServerAndToolIdentity(t *testing.T) {
	caller := &fakeMCPCaller{err: errors.New("connection refused")}
	tool := newMCPTestTool(caller)

	_, err := tool.Execute(nil)
	if err == nil {
		t.Fatalf("expected an error")
	}
	for _, want := range []string{"mcp_acme_crm_get_contact", "Acme CRM", "get-contact", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name %q", err, want)
		}
	}
}

func TestMCPToolNilCallerFailsCleanly(t *testing.T) {
	tool := NewMCPTool("mcp_x_y", "srv", "X", "y", "", nil, nil)
	if _, err := tool.Execute(nil); err == nil || !strings.Contains(err.Error(), "no caller wired") {
		t.Fatalf("expected a no-caller error, got %v", err)
	}
}

func TestMCPToolNilResultCoerced(t *testing.T) {
	caller := &fakeMCPCaller{res: nil}
	tool := newMCPTestTool(caller)
	out, err := tool.Execute(nil)
	if err != nil || out == nil {
		t.Fatalf("a nil remote result must coerce to an empty map, got %v (%v)", out, err)
	}
	if len(out) != 0 {
		t.Fatalf("expected an empty map, got %v", out)
	}
}

func TestMCPToolNilReceiverSafe(t *testing.T) {
	var tool *MCPTool
	if tool.Name() != "" || tool.ServerID() != "" || tool.RemoteName() != "" {
		t.Fatalf("nil receiver accessors must be empty")
	}
	if _, err := tool.Execute(nil); err == nil {
		t.Fatalf("nil receiver Execute must fail")
	}
}
