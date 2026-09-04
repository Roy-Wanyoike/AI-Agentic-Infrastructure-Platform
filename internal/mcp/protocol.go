// Package mcp exposes AgentOS tenant tools through the Model Context
// Protocol (MCP) — JSON-RPC 2.0 over a single stateless HTTP endpoint
// (issue #50).
//
// Supported MCP methods (spec protocolVersion "2025-06-18"):
//
//	initialize   protocol handshake + capability advertisement
//	tools/list   the tool catalog visible to the caller (permission-scoped)
//	tools/call   executes a tool through the EXISTING internal/tools
//	             execution path (registry lookup + ContextAware dispatch),
//	             so tool-side validation applies natively
//	ping         liveness probe (empty result)
//
// Protocol choices (documented v1 limits, mirrored in api/fragments/mcp.yaml):
//   - Stateless: no sessions, no SSE streams, no progress notifications;
//     every HTTP request is self-contained.
//   - Batch requests are rejected with -32600 (stateless v1 keeps handling
//     strictly one-shot).
//   - Notifications (requests without an id) never produce a JSON-RPC
//     response; the HTTP layer answers 204 No Content.
//   - Authorization denials use the JSON-RPC server-error code -32000
//     (reserved implementation-defined range) so MCP clients get an
//     in-band error instead of a transport-level one.
//   - Tool EXECUTION failures are reported inside a successful
//     tools/call result with "isError": true (MCP tools error-handling
//     contract); protocol-level problems (unknown tool, malformed
//     params) use JSON-RPC -32602.
package mcp

import "encoding/json"

// Protocol constants.
const (
	// JSONRPCVersion is the JSON-RPC envelope version this server speaks.
	JSONRPCVersion = "2.0"
	// ProtocolVersion is the MCP spec revision this server implements.
	// Clients requesting another version are answered with this one (the
	// spec's version-negotiation rule).
	ProtocolVersion = "2025-06-18"
	// ServerName is the MCP serverInfo.name advertised at initialize.
	ServerName = "agentos"
)

// JSON-RPC method names.
const (
	MethodInitialize = "initialize"
	MethodToolsList  = "tools/list"
	MethodToolsCall  = "tools/call"
	MethodPing       = "ping"
)

// JSON-RPC 2.0 error codes (spec section 5.1) plus the server-defined
// authorization code from the reserved -32000..-32099 implementation range.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// CodeForbidden is a server-defined code (reserved implementation
	// range): the authenticated caller lacks the permission for the
	// requested tool operation (e.g. a viewer calling tools/list).
	CodeForbidden = -32000
)

// Request is one JSON-RPC 2.0 request object. ID stays raw so string,
// number and null ids are echoed byte-exact; a nil ID marks a
// notification (no response per JSON-RPC).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
}

// Response is one JSON-RPC 2.0 response object. Exactly one of Result /
// Error is set. ID is always rendered ("null" when the request id was
// undetectable, per the JSON-RPC spec).
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements the error interface (handy for tests and logs).
func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// NullID is the JSON null request id, used for transport-level error
// envelopes where the request id was undetectable.
var NullID = json.RawMessage("null")

// nullID is the response id used when the request id was absent or
// undetectable (JSON-RPC spec: "id ... NULL if there was an error
// detecting the id in the Request object").
var nullID = NullID

// ---- MCP result shapes (wire names follow the MCP spec, camelCase) ----

// serverInfo identifies the MCP server implementation.
type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// toolsCapability advertises the tools feature. listChanged is false: the
// stateless v1 endpoint never pushes registry-change notifications.
type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// serverCapabilities advertises the features this server supports.
type serverCapabilities struct {
	Tools toolsCapability `json:"tools"`
}

// initializeResult is the initialize handshake payload.
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfo         `json:"serverInfo"`
}

// toolDescriptor is one tools/list entry (MCP Tool shape). description and
// inputSchema are omitted for tools that publish no Described metadata,
// mirroring the /v1/tools REST catalog behavior.
type toolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// toolsListResult is the tools/list payload. nextCursor is deliberately
// absent (no pagination in stateless v1).
type toolsListResult struct {
	Tools []toolDescriptor `json:"tools"`
}

// callToolParams is the tools/call request payload.
type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// textContent is the MCP text content block.
type textContent struct {
	Type string `json:"type"` // always "text" in v1
	Text string `json:"text"`
}

// callToolResult is the tools/call payload. Execution failures are
// reported IN-BAND with isError true (MCP contract), never as JSON-RPC
// errors.
type callToolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

// pingResult is the ping payload: the empty object.
type pingResult struct{}

// buildResponse renders one JSON-RPC response envelope.
func buildResponse(id json.RawMessage, result any, rpcErr *RPCError) Response {
	if len(id) == 0 {
		id = nullID
	}
	return Response{JSONRPC: JSONRPCVersion, ID: id, Result: result, Error: rpcErr}
}

// marshalResponse serializes a response envelope; the shapes above are
// all JSON-safe so encoding cannot fail.
func marshalResponse(resp Response) []byte {
	b, err := json.Marshal(resp)
	if err != nil {
		// Unreachable for the fixed shapes above; keep a valid JSON-RPC
		// body so the HTTP layer never emits a non-JSON error payload.
		fallback, _ := json.Marshal(buildResponse(nullID, nil, &RPCError{
			Code:    CodeInternalError,
			Message: "internal error: response encoding failed",
		}))
		return fallback
	}
	return b
}

// BuildErrorResponse renders a standalone JSON-RPC error envelope for
// transport-level failures detected by the wiring layer (unreadable body,
// body too large). A nil id renders as null.
func BuildErrorResponse(id json.RawMessage, code int, message string) []byte {
	return marshalResponse(buildResponse(id, nil, &RPCError{Code: code, Message: message}))
}
