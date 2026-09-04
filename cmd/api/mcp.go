package main

// Issue #50 (wave 6-e) HTTP wiring — the MCP server endpoint.
//
// Endpoint (registered on apiMux by registerMcpRoutes; served under BOTH
// /v1 and /api/v1):
//
//      POST /mcp — JSON-RPC 2.0 over HTTP, single stateless endpoint
//                  (MCP methods: initialize, tools/list, tools/call, ping)
//
// Design (mirrors the platform handler conventions, see cmd/api/tools.go
// and cmd/api/secrets.go):
//   - Authentication is the standard middleware chain: RequireAuthOrAPIKey
//     (Bearer JWT or X-API-Key; API keys authenticate as OWNER). Method-level
//     permission for the tool surface (tools/list + tools/call) reuses the
//     EXISTING runs.execute grant (matrix exactly OWNER/ADMIN/MEMBER —
//     viewers cannot list or call) through the real auth.RequirePermission
//     middleware, applied per JSON-RPC method inside the handler; denials
//     surface as the in-band JSON-RPC code -32000 so MCP clients get a
//     protocol-level error instead of an HTTP-level one. initialize and
//     ping need authentication only (they expose no tool data).
//   - Tenant scope comes exclusively from the auth claims; the endpoint
//     accepts no client-supplied organization ids. The tool catalog itself
//     is a process-wide runtime surface (see cmd/api/tools.go): identical
//     for every tenant, so the payload carries no organization fields and
//     there is nothing to leak across orgs.
//   - Tool execution happens through the EXISTING internal/tools registry
//     path (mcp.NewDefaultService -> tools.DefaultRegistry + registry.Get +
//     ContextAware dispatch) — the same built-ins the agent runtime and
//     GET /v1/tools advertise, so tool-side validation applies natively.
//   - Audit: EVERY tools/call with parsable params writes a best-effort
//     audit row (action "mcp.tool_call", resource "tools/<tool id>",
//     actor = caller principal, metadata {"channel":"mcp","tool":...} plus
//     the outcome: ok / denied / unknown_tool). Arguments are never logged
//     (they can embed secret material).
//
// Batching and notifications are handled inside internal/mcp (batch →
// -32600, notifications → 204); this file only moves bytes and enforces
// the platform auth/audit semantics.

import (
	"context"
	"io"
	"net/http"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/mcp"
)

// maxMCPRequestBody caps the JSON-RPC request body (tools/call arguments
// are capped far lower by the tools themselves; this bounds parsing work).
const maxMCPRequestBody = 1 << 20

// registerMcpRoutes mounts the MCP endpoint on apiMux behind the standard
// auth middleware with the audit wiring. The service is constructed here so
// the main.go wiring diff stays a single registration line (the
// registerToolsRoutes precedent of building tools.DefaultRegistry() at the
// call site).
func registerMcpRoutes(apiMux *http.ServeMux, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	registerMcpServiceRoutes(apiMux, mcp.NewDefaultService(), authSvc, apiKeysSvc, auditSvc)
}

// registerMcpServiceRoutes mounts a caller-supplied MCP service (test seam:
// custom registries). Authentication wraps the whole endpoint; the
// method-level permission check happens per JSON-RPC method inside the
// handler, where the request (and therefore the auth claims) is available.
func registerMcpServiceRoutes(apiMux *http.ServeMux, svc *mcp.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	apiMux.Handle("POST /mcp", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		http.HandlerFunc(mcpHandler(svc, authSvc, auditSvc))))
}

// mcpHandler adapts the HTTP transport to the stateless MCP service:
// claims -> Identity, body -> Outcome, Outcome -> audit row + HTTP response.
func mcpHandler(svc *mcp.Service, authSvc *auth.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			// Unreachable behind RequireAuthOrAPIKey; defends the contract
			// if the wiring ever changes.
			writeMcpJSON(w, http.StatusUnauthorized,
				mcp.BuildErrorResponse(mcp.NullID, mcp.CodeForbidden, err.Error()))
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPRequestBody+1))
		if err != nil {
			writeMcpJSON(w, http.StatusBadRequest,
				mcp.BuildErrorResponse(mcp.NullID, mcp.CodeParseError, "parse error: request body could not be read"))
			return
		}
		if len(body) > maxMCPRequestBody {
			writeMcpJSON(w, http.StatusRequestEntityTooLarge,
				mcp.BuildErrorResponse(mcp.NullID, mcp.CodeInvalidRequest, "invalid request: request body too large"))
			return
		}

		identity := mcp.Identity{
			UserID:         claims.UserID,
			OrganizationID: claims.OrganizationID,
			Email:          claims.Email,
			Role:           claims.Role,
		}
		// Per-method permission: the EXISTING RequirePermission semantics
		// (current DB user lookup first, role-claims fallback for API keys)
		// applied against THIS request, so the claims flow through unchanged.
		authorize := func(_ context.Context, _ mcp.Identity) bool {
			return mcpPermissionAllowed(authSvc, r)
		}
		out := svc.Handle(r.Context(), identity, body, authorize)

		// Best-effort audit row for every tools/call attempt (see file
		// comment): channel marker "mcp" + tool id + caller principal.
		// Arguments are deliberately absent — they may embed secret values.
		if rec := out.ToolCall; rec != nil && auditSvc != nil {
			metadata := map[string]any{"channel": "mcp", "tool": rec.Tool}
			switch {
			case rec.Denied:
				metadata["denied"] = true
			case rec.Unknown:
				metadata["error"] = "unknown_tool"
			default:
				metadata["ok"] = rec.OK
			}
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "mcp.tool_call",
				claims.OrganizationID, "tools/"+rec.Tool, metadata)
		}

		if out.Status == http.StatusNoContent {
			// JSON-RPC notification: no response body by spec.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeMcpJSON(w, out.Status, out.Body)
	}
}

// writeMcpJSON renders one JSON payload with the given status.
func writeMcpJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// mcpPermissionAllowed reuses the real auth.RequirePermission middleware to
// decide the verdict without committing a response: the middleware runs
// against this request with a discarding ResponseWriter and the status it
// would have written decides. The probe starts at 200 because the success
// path never writes a header (the wrapped handler is a no-op); only an
// explicit 401/403 WriteHeader from the middleware means denial.
func mcpPermissionAllowed(authSvc *auth.Service, r *http.Request) bool {
	probe := &mcpProbeResponseWriter{status: http.StatusOK}
	auth.RequirePermission(authSvc, auth.PermissionRunsExecute)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	).ServeHTTP(probe, r)
	return probe.status == http.StatusOK
}

// mcpProbeResponseWriter captures the status the permission middleware
// would have written while discarding everything else.
type mcpProbeResponseWriter struct {
	header http.Header
	status int
}

func (p *mcpProbeResponseWriter) Header() http.Header {
	if p.header == nil {
		p.header = http.Header{}
	}
	return p.header
}

func (p *mcpProbeResponseWriter) Write(b []byte) (int, error) { return len(b), nil }

func (p *mcpProbeResponseWriter) WriteHeader(code int) { p.status = code }
