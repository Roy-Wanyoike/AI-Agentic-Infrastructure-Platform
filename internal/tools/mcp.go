package tools

// mcp.go — the MCP tool adapter (issue #82): exposes tools served by
// EXTERNAL MCP servers as native entries of the platform tools.Registry.
//
// An MCPTool is created by the internal/mcp registry service from the
// static tools/list cache captured at registration time; it carries the
// remote server/tool identity, the cached catalog metadata and a narrow
// caller seam that performs the outbound tools/call (the internal/mcp
// streamable-HTTP client satisfies it structurally — this file stays
// import-free of internal/mcp, mirroring the connectors SecretResolver
// pattern).
//
// Everything the runtime already does for native tools applies to MCPTool
// untouched because it implements the SAME contracts:
//   - Tool (Name + Execute) — registry membership + execution,
//   - ContextAware — per-call deadlines honored directly,
//   - Described — catalog metadata for GET /v1/tools (the cached remote
//     description + inputSchema, re-served verbatim).
//
// Failure semantics: an unreachable remote or a remote tool that reports
// failure surfaces a regular error — the runtime renders it as a visible
// failed tool step and the run CONTINUES (same behavior as any other tool
// failure). A disabled/unreachable server simply contributes no tools to
// the registry (built by the registry service, status-gated).

import (
	"context"
	"errors"
	"fmt"
)

// MCPToolCaller performs one remote tools/call on behalf of an MCPTool.
// It is satisfied structurally by the internal/mcp streamable-HTTP client
// (*mcp.Client), keeping this package decoupled from the MCP transport.
type MCPToolCaller interface {
	// CallTool executes the remote tool named toolName with args and maps
	// the MCP result onto the standard tool-result map. Remote-reported
	// failures (isError) and transport failures surface as errors.
	CallTool(ctx context.Context, toolName string, args map[string]any) (map[string]any, error)
}

// MCPTool is one remote MCP tool as seen by the platform tool registry.
type MCPTool struct {
	registryName string // registry key: mcp_<server>_<tool> (sanitized)
	serverID     string // MCP server registry row id (the "server ref" for audit)
	serverName   string // human server name (as registered)
	toolName     string // remote tool name (verbatim from tools/list)
	description  string // cached remote description
	inputSchema  map[string]any
	caller       MCPToolCaller
}

// NewMCPTool builds one adapter. registryName must be unique within the
// target registry (the registry service deduplicates deterministically).
func NewMCPTool(registryName, serverID, serverName, toolName, description string, inputSchema map[string]any, caller MCPToolCaller) *MCPTool {
	return &MCPTool{
		registryName: registryName,
		serverID:     serverID,
		serverName:   serverName,
		toolName:     toolName,
		description:  description,
		inputSchema:  inputSchema,
		caller:       caller,
	}
}

// Name implements Tool: the sanitized flat registry name
// (mcp_<server>_<tool>) the model invokes.
func (t *MCPTool) Name() string {
	if t == nil {
		return ""
	}
	return t.registryName
}

// ServerID returns the MCP server registry row id (audit/server ref).
func (t *MCPTool) ServerID() string {
	if t == nil {
		return ""
	}
	return t.serverID
}

// ServerName returns the human MCP server name.
func (t *MCPTool) ServerName() string {
	if t == nil {
		return ""
	}
	return t.serverName
}

// RemoteName returns the remote tool name (verbatim from tools/list).
func (t *MCPTool) RemoteName() string {
	if t == nil {
		return ""
	}
	return t.toolName
}

// Execute implements Tool using a background context.
func (t *MCPTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware: the remote call honors the
// caller's deadline/cancellation (the outbound client applies its own
// request timeout on top), so the runtime's per-call tool timeout binds
// MCP tools exactly like native ones.
func (t *MCPTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if t == nil {
		return nil, errors.New("tools: mcp tool is not initialized")
	}
	if t.caller == nil {
		return nil, fmt.Errorf("tools: mcp tool %q has no caller wired", t.registryName)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := t.caller.CallTool(ctx, t.toolName, input)
	if err != nil {
		// Wrap with the server ref + tool identity so the visible failed
		// step names exactly which external integration failed. The remote
		// error types (*mcp.ToolCallError, *mcp.RPCError) stay reachable
		// through the chain for callers that classify.
		return nil, fmt.Errorf("mcp tool %s (server %s, remote %s): %w", t.registryName, t.serverName, t.toolName, err)
	}
	if res == nil {
		res = map[string]any{}
	}
	return res, nil
}

// DescribeTool implements Described: the cached remote metadata is re-served
// through the platform catalog (GET /v1/tools) with the MCP inputSchema
// passed through uninterpreted, so a model sees the external tool like a
// native one.
func (t *MCPTool) DescribeTool() ToolInfo {
	if t == nil {
		return ToolInfo{}
	}
	return ToolInfo{
		Name:        t.registryName,
		Description: t.description,
		InputSchema: t.inputSchema,
	}
}
