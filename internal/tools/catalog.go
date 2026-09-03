package tools

import "strconv"

// catalog.go adds the read-only listing surface for the tool registry
// (issue #18: GET /v1/tools). The registry itself stays execution-focused;
// this file only adds metadata plumbing so the public API can describe what
// is registered without touching the execution path.

// ToolInfo is one entry of the read-only tool catalog: the tool's registry
// name, a human-readable description, and the JSON-Schema-style input
// contract a model must satisfy when invoking it.
type ToolInfo struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// Described is optionally implemented by tools that publish catalog metadata
// for the public registry listing. Tools that do not implement it still
// appear in listings, but with their name only (execution is unaffected).
type Described interface {
	DescribeTool() ToolInfo
}

// List returns the catalog of registered tools sorted by name (Name() sort
// order, matching Names()). The registry key is the authoritative tool name,
// so a Described implementation can never rename a tool in listings. Nil-safe:
// a nil (or empty) registry yields an empty, non-nil slice so HTTP handlers
// can render honest empty states.
func (r *Registry) List() []ToolInfo {
	out := make([]ToolInfo, 0)
	if r == nil {
		return out
	}
	for _, name := range r.Names() {
		info := ToolInfo{Name: name}
		if described, ok := r.tools[name].(Described); ok {
			if meta := described.DescribeTool(); meta.Name != "" || meta.Description != "" || meta.InputSchema != nil {
				info.Description = meta.Description
				info.InputSchema = meta.InputSchema
			}
		}
		out = append(out, info)
	}
	return out
}

// DefaultRegistry returns a fresh registry preloaded with the platform's
// built-in tools (the same set cmd/worker registers: calculator and
// http_request). Callers may register additional tools on top of it. It is
// used for the read-only /v1/tools listing; registering into it has no effect
// on any other process's registry.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(NewCalculatorTool())
	r.Register(NewHTTPRequestTool())
	return r
}

// DescribeTool publishes the calculator's catalog metadata (issue #18).
func (t *CalculatorTool) DescribeTool() ToolInfo {
	return ToolInfo{
		Name: t.Name(),
		Description: "Evaluates an arithmetic expression (integers and decimals " +
			"combined with +, -, *, /) and returns the numeric result.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"expression": map[string]any{
					"type":        "string",
					"description": `Arithmetic expression to evaluate, e.g. "12*3+4" or "100/8".`,
				},
			},
			"required": []string{"expression"},
		},
	}
}

// DescribeTool publishes the http_request tool's catalog metadata (issue #18).
func (t *HTTPRequestTool) DescribeTool() ToolInfo {
	return ToolInfo{
		Name: HTTPToolName,
		Description: "Performs a single outbound HTTP(S) request and returns " +
			"{status, body, headers}. SSRF protection refuses private/loopback/link-local " +
			"destinations; at most " + strconv.Itoa(maxHTTPRedirects) + " redirects are followed.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Absolute http(s) URL to request. Private network destinations are blocked.",
				},
				"method": map[string]any{
					"type":        "string",
					"enum":        []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
					"description": "HTTP method (defaults to GET).",
				},
				"headers": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Optional request headers.",
				},
				"body": map[string]any{
					"type":        "string",
					"description": "Optional request body (max 1 MiB).",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Per-call timeout in milliseconds (1ms-60s; defaults to 10s).",
				},
			},
			"required": []string{"url"},
		},
	}
}
