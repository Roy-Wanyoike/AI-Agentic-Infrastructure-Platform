package mcp

// client.go — the OUTBOUND MCP client (issue #82): lets agents consume
// EXTERNAL MCP servers over the streamable-HTTP transport (JSON-RPC 2.0
// POST, one self-contained request per call — the same stateless envelope
// shapes the inbound server in service.go/protocol.go speaks).
//
// Deliberately stdlib-only (net/http + encoding/json): no MCP SDK. Three
// operations, exactly the surface the registry needs:
//
//      Initialize  handshake + protocol version negotiation
//      ListTools   the remote tool catalog (tools/list)
//      CallTool    one remote tool invocation (tools/call)
//
// Error mapping is GRACEFUL and typed so callers can classify outcomes:
//   - transport/HTTP failures  -> plain errors naming the endpoint + status
//   - JSON-RPC error objects   -> *RPCError (errors.As-able, code preserved)
//   - remote tool that ran but
//     reported failure         -> *ToolCallError (MCP isError contract)
//
// Auth: when the caller wires an AuthHeaderProvider the client resolves the
// credential PER REQUEST (fresh at call time so secret rotation is honored)
// and injects it as a single header. The provider is the seam the registry
// binds to the platform secrets service (org-scoped Resolve) — this package
// never sees secret stores, only the resolved name/value pair, and the value
// is never logged.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client defaults and limits.
const (
	// DefaultClientTimeout bounds every single outbound JSON-RPC request.
	DefaultClientTimeout = 15 * time.Second
	// maxClientResponseBody caps how much of a remote response is read
	// (remote servers are untrusted; keeps memory bounded).
	maxClientResponseBody = 4 << 20
	// ClientName identifies AgentOS in the initialize handshake.
	ClientName = "agentos"
)

// RemoteTool is one tools/list entry advertised by a remote MCP server
// (mirror of the inbound toolDescriptor, exported for the registry cache).
// InputSchema is the JSON-Schema-style input contract, passed through
// UNINTERPRETED so the platform tool catalog can re-serve it verbatim.
type RemoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// InitializeResult is the server side of the initialize handshake. The
// negotiated ProtocolVersion is whatever the SERVER answered with (the MCP
// negotiation rule: the server picks the version it will use; a client that
// cannot live with it would disconnect — AgentOS accepts and records it).
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ServerInfo      serverInfo     `json:"serverInfo"`
}

// ToolCallError reports a remote tool that EXECUTED but reported failure
// (tools/call returned isError:true — the MCP in-band error contract, not a
// protocol failure). The Text carries the remote's own error content.
type ToolCallError struct {
	Tool string
	Text string
}

// Error implements the error interface.
func (e *ToolCallError) Error() string {
	if strings.TrimSpace(e.Text) != "" {
		return fmt.Sprintf("remote tool %q failed: %s", e.Tool, e.Text)
	}
	return fmt.Sprintf("remote tool %q failed (no error detail provided)", e.Tool)
}

// AuthHeaderProvider returns the (header name, header value) pair injected
// into every request, resolved FRESH PER REQUEST. Returning an error aborts
// the call before any bytes hit the wire (e.g. the org-scoped secret could
// not be resolved). The value is used verbatim — store "Bearer xyz" as the
// secret value when the remote expects a scheme.
type AuthHeaderProvider func(ctx context.Context) (name, value string, err error)

// Client is one MCP streamable-HTTP client bound to ONE remote server
// endpoint. Safe for concurrent use; the request id is an atomic counter.
type Client struct {
	endpoint   string
	httpClient *http.Client
	timeout    time.Duration
	auth       AuthHeaderProvider
	nextID     atomic.Int64
}

// ClientOption configures a Client at construction.
type ClientOption func(*Client)

// WithClientTimeout bounds each request (<= 0 restores the default).
func WithClientTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithClientHTTPClient replaces the underlying *http.Client (tests inject
// httptest servers with short timeouts). Nil restores the default.
func WithClientHTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithClientAuthHeader wires the per-request credential provider.
func WithClientAuthHeader(p AuthHeaderProvider) ClientOption {
	return func(c *Client) {
		if p != nil {
			c.auth = p
		}
	}
}

// NewClient returns a client for one remote MCP endpoint (the FULL URL the
// JSON-RPC POSTs go to, e.g. "https://mcp.example.com/mcp").
func NewClient(endpoint string, opts ...ClientOption) *Client {
	c := &Client{
		endpoint: strings.TrimSpace(endpoint),
		timeout:  DefaultClientTimeout,
		httpClient: &http.Client{
			Timeout: DefaultClientTimeout,
			Transport: &http.Transport{
				// Proxy intentionally disabled: an environment proxy would
				// silently reroute governed outbound calls (connectors
				// precedent).
				Proxy: nil,
			},
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// Endpoint returns the remote URL this client POSTs to.
func (c *Client) Endpoint() string {
	if c == nil {
		return ""
	}
	return c.endpoint
}

// Initialize performs the MCP handshake: it sends the initialize request
// with the protocol version this client supports (ProtocolVersion) and
// returns the server's answer. The server's negotiated version is recorded
// on the result; a response without one is a protocol violation (error).
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	if c == nil || c.endpoint == "" {
		return nil, errors.New("mcp: client endpoint is not configured")
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": ClientName, "version": serverVersion()},
	}
	var result InitializeResult
	if err := c.do(ctx, MethodInitialize, params, &result); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.ProtocolVersion) == "" {
		return nil, fmt.Errorf("mcp: server %s answered initialize without a protocolVersion", c.endpoint)
	}
	return &result, nil
}

// ListTools fetches the remote tool catalog (tools/list). A server that
// advertises no tools yields an empty, non-nil slice.
func (c *Client) ListTools(ctx context.Context) ([]RemoteTool, error) {
	if c == nil || c.endpoint == "" {
		return nil, errors.New("mcp: client endpoint is not configured")
	}
	var result struct {
		Tools []RemoteTool `json:"tools"`
	}
	// Empty params object: the stateless client requests the full list
	// (no cursor pagination — matching the platform's own server, which
	// answers with the complete catalog).
	if err := c.do(ctx, MethodToolsList, map[string]any{}, &result); err != nil {
		return nil, err
	}
	out := result.Tools
	if out == nil {
		out = []RemoteTool{}
	}
	return out, nil
}

// CallTool executes one remote tool (tools/call) and maps the result onto
// the platform's standard tool-result shape:
//
//	"content"    joined text of the response content blocks ("" when none)
//	"structured" the response's structuredContent object, when present
//
// A remote tool that ran but reported isError:true surfaces as *ToolCallError
// (callers render it as a visible tool error; the run continues).
func (c *Client) CallTool(ctx context.Context, toolName string, args map[string]any) (map[string]any, error) {
	if c == nil || c.endpoint == "" {
		return nil, errors.New("mcp: client endpoint is not configured")
	}
	if strings.TrimSpace(toolName) == "" {
		return nil, fmt.Errorf("%w: tool name is required", ErrInvalidToolParams)
	}
	params := map[string]any{"name": toolName, "arguments": args}
	var result callToolResult
	if err := c.do(ctx, MethodToolsCall, params, &result); err != nil {
		return nil, err
	}
	if result.IsError {
		return nil, &ToolCallError{Tool: toolName, Text: joinContentText(result.Content)}
	}
	out := make(map[string]any, 2)
	out["content"] = joinContentText(result.Content)
	if result.StructuredContent != nil {
		out["structured"] = result.StructuredContent
	}
	return out, nil
}

// joinContentText concatenates the text of the response's text content
// blocks (the only block type the stateless client consumes) with newlines.
func joinContentText(content []textContent) string {
	texts := make([]string, 0, len(content))
	for _, block := range content {
		if block.Type == "text" || block.Type == "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// ErrInvalidToolParams marks client-side parameter problems (distinct from
// remote JSON-RPC -32602 answers).
var ErrInvalidToolParams = errors.New("mcp: invalid tool call parameters")

// do sends one JSON-RPC 2.0 request and decodes the response into result
// (which may be nil when the caller ignores the payload). It centralizes
// the envelope handling, timeout, auth-header injection and the typed
// error mapping documented at the top of the file.
func (c *Client) do(ctx context.Context, method string, params any, result any) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// Resolve the credential BEFORE building the request: a resolution
	// failure must abort the call, never leak into a half-authed request.
	var headerName, headerValue string
	if c.auth != nil {
		name, value, err := c.auth(ctx)
		if err != nil {
			return fmt.Errorf("mcp: auth header resolution failed: %w", err)
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t\r\n") ||
			strings.ContainsAny(value, "\r\n") {
			return errors.New("mcp: auth header resolution produced an invalid header name/value")
		}
		headerName, headerValue = name, value
	}

	id := c.nextID.Add(1)
	rawParams, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("mcp: cannot encode %s params: %w", method, err)
	}
	body, err := json.Marshal(Request{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  rawParams,
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
	})
	if err != nil {
		return fmt.Errorf("mcp: cannot encode %s request: %w", method, err)
	}

	reqCtx := ctx
	var cancel context.CancelFunc = func() {}
	if c.timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, c.timeout)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mcp: cannot build request for %s: %w", c.endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if headerName != "" {
		req.Header.Set(headerName, headerValue)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if reqCtx.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("mcp: %s %s timed out after %s", method, c.endpoint, c.timeout)
		}
		return fmt.Errorf("mcp: %s %s failed: %w", method, c.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxClientResponseBody+1))
	if err != nil {
		return fmt.Errorf("mcp: %s %s: reading response failed: %w", method, c.endpoint, err)
	}
	if len(raw) > maxClientResponseBody {
		return fmt.Errorf("mcp: %s %s response exceeds %d bytes", method, c.endpoint, maxClientResponseBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 256 {
			snippet = snippet[:256] + "..."
		}
		return fmt.Errorf("mcp: server %s returned HTTP %d for %s: %s", c.endpoint, resp.StatusCode, method, snippet)
	}

	var envelope Response
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("mcp: %s %s returned a non-JSON-RPC body: %w", method, c.endpoint, err)
	}
	if envelope.Error != nil {
		// Preserve the remote's code/message as a typed *RPCError so
		// callers can classify (errors.As) without string parsing.
		return &RPCError{Code: envelope.Error.Code, Message: envelope.Error.Message, Data: envelope.Error.Data}
	}
	if result == nil {
		return nil
	}
	if envelope.Result == nil {
		return fmt.Errorf("mcp: %s %s answered without a result or error", method, c.endpoint)
	}
	// The shared Response type decodes Result as generic any; re-encode and
	// decode into the caller's typed shape (single round-trip, cheap here).
	reshaped, err := json.Marshal(envelope.Result)
	if err != nil {
		return fmt.Errorf("mcp: %s %s result re-encode failed: %w", method, c.endpoint, err)
	}
	if err := json.Unmarshal(reshaped, result); err != nil {
		return fmt.Errorf("mcp: %s %s returned an unexpected result shape: %w", method, c.endpoint, err)
	}
	return nil
}
