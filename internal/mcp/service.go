package mcp

// service.go — the stateless MCP dispatch loop (issue #50). Every HTTP
// request carries exactly one JSON-RPC 2.0 request; Handle parses it,
// authorizes the tool surface, executes MCP methods against the EXISTING
// internal/tools registry (the same execution path the agent runtime uses)
// and renders one JSON-RPC response.
//
// The service is intentionally transport-thin: it never touches
// net/http.Handler wiring, auth stores or audit persistence. Identity and
// authorization arrive as plain values / a callback (see Identity and
// Authorizer), so the package stays unit-testable and the cmd/api wiring
// layer owns the platform's real auth middleware semantics and the
// best-effort audit trail.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"agentos/internal/tools"
)

// Identity is the authenticated caller as resolved by the transport layer
// (auth middleware claims). It is passed verbatim to the Authorizer and is
// never trusted beyond that — authorization is always delegated.
type Identity struct {
	UserID         string
	OrganizationID string
	Email          string
	Role           string
}

// Authorizer reports whether the caller may use the tenant tool surface
// (tools/list AND tools/call — both are tool access). The cmd/api wiring
// binds this to the platform's runs.execute permission so the role matrix
// stays single-sourced (OWNER/ADMIN/MEMBER; viewers cannot list or call).
// A nil Authorizer allows every authenticated caller (test seam only).
type Authorizer func(ctx context.Context, identity Identity) bool

// ToolCallRecord surfaces one tools/call attempt to the transport layer so
// it can write the audit row ("mcp" channel + tool id + caller principal).
// Exactly one of Denied/Unknown is set when the call was rejected before
// execution; OK is only meaningful for executed calls.
type ToolCallRecord struct {
	Tool    string
	Denied  bool // authorization refused the tool surface
	Unknown bool // tool name not present in the registry
	OK      bool // execution succeeded (executed calls only)
}

// Outcome is the transport verdict for one request: the HTTP status to
// render, the (optional) JSON-RPC response body, and the tools/call record
// for auditing. Body is nil for notifications (204) and must be empty there.
type Outcome struct {
	Status   int
	Body     []byte
	ToolCall *ToolCallRecord
}

// DefaultToolCallTimeout bounds a single tools/call execution.
const DefaultToolCallTimeout = 30 * time.Second

// fallbackServerVersion is used when runtime/debug carries no useful module
// version (go run / go test builds report "(devel)" or "").
const fallbackServerVersion = "0.1.0"

// Service is the stateless MCP method dispatcher. Construct with
// NewService (custom registry) or NewDefaultService (the platform's
// built-in registry: calculator + http_request). The authorizer is a
// per-call Handle argument so the transport layer can bind it to the
// concrete request's auth context.
type Service struct {
	registry    *tools.Registry
	version     string
	toolTimeout time.Duration
}

// NewService builds an MCP dispatcher over the given tool registry. A nil
// registry yields an honest empty catalog with every call answering
// "unknown tool".
func NewService(registry *tools.Registry) *Service {
	return &Service{
		registry:    registry,
		version:     serverVersion(),
		toolTimeout: DefaultToolCallTimeout,
	}
}

// NewDefaultService builds an MCP dispatcher over the platform's default
// tool registry (the same built-ins the /v1/tools catalog advertises).
func NewDefaultService() *Service {
	return NewService(tools.DefaultRegistry())
}

// serverVersion prefers the build-time module version and falls back to a
// constant so serverInfo.version is never empty.
func serverVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return fallbackServerVersion
}

// Handle processes one JSON-RPC 2.0 request body and returns the transport
// outcome. authorize gates the tenant tool surface (tools/list + tools/call)
// and is invoked at most once per request with the caller's context; a nil
// authorizer allows every authenticated caller (test seam). Handle never
// panics on malformed input: every parse/validation failure maps to a
// JSON-RPC error response (see the package comment).
func (s *Service) Handle(ctx context.Context, identity Identity, body []byte, authorize Authorizer) Outcome {
	if ctx == nil {
		ctx = context.Background()
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return s.failure(CodeParseError, "parse error: request body is empty", nil, http.StatusBadRequest)
	}

	// Structural probe first: batch rejection and non-object rejection must
	// happen before the typed decode (JSON-RPC batches are arrays).
	var probe any
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return s.failure(CodeParseError, "parse error: request body is not valid JSON", nil, http.StatusBadRequest)
	}
	if _, isBatch := probe.([]any); isBatch {
		return s.failure(CodeInvalidRequest,
			"invalid request: batch requests are not supported (stateless v1)", nil, http.StatusBadRequest)
	}
	if _, isObject := probe.(map[string]any); !isObject {
		return s.failure(CodeInvalidRequest,
			"invalid request: expected a single JSON-RPC 2.0 request object", nil, http.StatusBadRequest)
	}

	var req Request
	if err := json.Unmarshal(trimmed, &req); err != nil {
		return s.failure(CodeParseError, "parse error: request body is not valid JSON", nil, http.StatusBadRequest)
	}

	// Envelope validation (JSON-RPC 2.0): exact version member, non-empty
	// method, id limited to string/number/null.
	if req.JSONRPC != JSONRPCVersion {
		return s.failure(CodeInvalidRequest, `invalid request: "jsonrpc" must be exactly "2.0"`, nil, http.StatusBadRequest)
	}
	if strings.TrimSpace(req.Method) == "" {
		return s.failure(CodeInvalidRequest, "invalid request: method is required", nil, http.StatusBadRequest)
	}
	if req.ID != nil && !validRequestID(req.ID) {
		return s.failure(CodeInvalidRequest, "invalid request: id must be a string, number or null", nil, http.StatusBadRequest)
	}

	// Notifications (no id) never produce a response body. Statelessness
	// makes every notification a no-op (notifications/initialized included);
	// unknown notification methods are dropped per JSON-RPC, not errors.
	if req.ID == nil {
		return Outcome{Status: http.StatusNoContent}
	}

	switch req.Method {
	case MethodInitialize:
		return s.handleInitialize(req.ID, req.Params)
	case MethodToolsList:
		return s.handleToolsList(ctx, identity, req.ID, authorize)
	case MethodToolsCall:
		return s.handleToolsCall(ctx, identity, req.ID, req.Params, authorize)
	case MethodPing:
		return successOutcome(req.ID, pingResult{})
	default:
		return s.failure(CodeMethodNotFound,
			fmt.Sprintf("method not found: %q", req.Method), req.ID, http.StatusOK)
	}
}

// handleInitialize performs the MCP handshake: version negotiation (echo a
// supported client version, otherwise answer with ours), capability
// advertisement (tools, listChanged false) and serverInfo.
func (s *Service) handleInitialize(id json.RawMessage, params json.RawMessage) Outcome {
	requested := ""
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(params, &p); err == nil {
			requested = strings.TrimSpace(p.ProtocolVersion)
		}
	}
	// Version negotiation per the MCP spec: respond with the client's
	// version when supported, otherwise the server's latest.
	version := ProtocolVersion
	if requested == ProtocolVersion {
		version = requested
	}
	return successOutcome(id, initializeResult{
		ProtocolVersion: version,
		Capabilities:    serverCapabilities{Tools: toolsCapability{ListChanged: false}},
		ServerInfo:      serverInfo{Name: ServerName, Version: s.version},
	})
}

// handleToolsList renders the caller-visible tool catalog. The registry
// (not the tenant) owns the catalog — tool registrations are a process-wide
// runtime surface, so the payload carries no organization fields (nothing
// to leak across tenants); access is still permission-gated.
func (s *Service) handleToolsList(ctx context.Context, identity Identity, id json.RawMessage, authorize Authorizer) Outcome {
	if !allowed(authorize, ctx, identity) {
		return s.failure(CodeForbidden, forbiddenMessage("tools/list"), id, http.StatusOK)
	}
	infos := s.registry.List()
	entries := make([]toolDescriptor, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, toolDescriptor{
			Name:        info.Name,
			Description: info.Description,
			InputSchema: info.InputSchema,
		})
	}
	return successOutcome(id, toolsListResult{Tools: entries})
}

// handleToolsCall executes one tool through the existing registry execution
// path: registry lookup, ContextAware dispatch with a per-call deadline
// (watchdog for tools without context support), result mapped to MCP text
// content. Protocol failures (bad params, unknown tool, denied) are
// JSON-RPC errors; tool EXECUTION failures are in-band isError results.
func (s *Service) handleToolsCall(ctx context.Context, identity Identity, id json.RawMessage, params json.RawMessage, authorize Authorizer) Outcome {
	var p callToolParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return s.failure(CodeInvalidParams,
				"invalid params: name must be a string and arguments an object", id, http.StatusOK)
		}
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		// No tool name at all: a client bug, not an invocation attempt —
		// nothing meaningful to surface for the audit layer.
		return s.failure(CodeInvalidParams, "invalid params: tool name is required", id, http.StatusOK)
	}
	record := &ToolCallRecord{Tool: name}

	if !allowed(authorize, ctx, identity) {
		record.Denied = true
		return withToolCallRecord(s.failure(CodeForbidden, forbiddenMessage("tools/call"), id, http.StatusOK), record)
	}

	tool, found := s.registry.Get(name)
	if !found {
		record.Unknown = true
		return withToolCallRecord(s.failure(CodeInvalidParams, fmt.Sprintf("invalid params: unknown tool %q", name), id, http.StatusOK), record)
	}

	arguments := p.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}

	callCtx, cancel := context.WithTimeout(ctx, s.toolTimeout)
	defer cancel()
	result, err := executeToolCall(callCtx, tool, arguments)
	record.OK = err == nil
	if err != nil {
		return withToolCallRecord(successOutcome(id, callToolResult{
			Content: []textContent{{Type: "text", Text: "error: " + err.Error()}},
			IsError: true,
		}), record)
	}
	return withToolCallRecord(successOutcome(id, callToolResult{
		Content: []textContent{{Type: "text", Text: toolResultText(result)}},
		IsError: false,
	}), record)
}

// withToolCallRecord attaches the tools/call audit record to an outcome so
// every return path above carries it.
func withToolCallRecord(out Outcome, record *ToolCallRecord) Outcome {
	out.ToolCall = record
	return out
}

// executeToolCall runs one tool call, preferring the context-aware path so
// the deadline/cancellation propagates into the tool (mirrors the agent
// runtime). Tools without context support run under a watchdog: the call is
// abandoned when the deadline fires (the goroutine cannot be killed, but
// the caller is unblocked).
func executeToolCall(ctx context.Context, tool tools.Tool, args map[string]any) (map[string]any, error) {
	// Deterministic fast path: an already-expired (or canceled) context
	// aborts before the tool runs at all.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tool call aborted before execution: %v", err)
	}
	if aware, ok := tool.(tools.ContextAware); ok {
		return aware.ExecuteContext(ctx, args)
	}
	done := make(chan struct{})
	var (
		result map[string]any
		err    error
	)
	go func() {
		defer close(done)
		result, err = tool.Execute(args)
	}()
	select {
	case <-done:
		return result, err
	case <-ctx.Done():
		return nil, fmt.Errorf("tool call did not finish within %s: %v", DefaultToolCallTimeout, ctx.Err())
	}
}

// toolResultText renders a tool result map as the MCP text content payload
// (compact JSON — the same observation format the agent runtime feeds to
// models). Non-serializable results fall back to their Go formatting.
func toolResultText(result map[string]any) string {
	if result == nil {
		return ""
	}
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("%v", result)
	}
	return string(b)
}

// allowed applies the per-call authorizer; a nil authorizer (test seam)
// allows every authenticated caller.
func allowed(authorize Authorizer, ctx context.Context, identity Identity) bool {
	if authorize == nil {
		return true
	}
	return authorize(ctx, identity)
}

// forbiddenMessage keeps the denial wording uniform for both tool methods.
func forbiddenMessage(method string) string {
	return "forbidden: the caller's role does not permit MCP " + method
}

// successOutcome renders a 200 JSON-RPC success envelope.
func successOutcome(id json.RawMessage, result any) Outcome {
	return Outcome{Status: http.StatusOK, Body: marshalResponse(buildResponse(id, result, nil))}
}

// failure renders a JSON-RPC error envelope with the given HTTP status.
// A nil id renders as null (parse/invalid-request failures have no id).
func (s *Service) failure(code int, message string, id json.RawMessage, status int) Outcome {
	return Outcome{
		Status: status,
		Body:   marshalResponse(buildResponse(id, nil, &RPCError{Code: code, Message: message})),
	}
}

// validRequestID reports whether raw is a legal JSON-RPC request id:
// string, number or null (objects and arrays are invalid).
func validRequestID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '{' || trimmed[0] == '[' {
		return false
	}
	var v any
	return json.Unmarshal(trimmed, &v) == nil
}
