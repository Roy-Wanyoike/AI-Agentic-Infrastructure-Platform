package main

// Issue #18 (wave 5-b) HTTP handlers — public tool registry half.
//
// Endpoint (registered on apiMux by registerToolsRoutes; served under BOTH
// /v1 and /api/v1):
//
//	GET /tools -> read-only catalog of the runtime's registered tools
//
// The listing mirrors the shape of the other track views: {"tools":[{"name",
// "description","input_schema"}]}. Tool registrations are a process-wide
// runtime surface (not tenant data), so the response carries no organization
// fields at all — there is nothing to leak; access is still authenticated and
// RBAC-gated.
//
// Permission fallback (documented deviation, mirrors the knowledge routes):
// internal/auth has no tools.read constant, so the closest existing grant
// agents.read is reused — the same read permission the knowledge listing and
// the agent listing require (every role holds it).
//
// The registry is injected by the wiring call site; cmd/worker registers the
// same built-in pair (calculator, http_request), and internal/tools.
// DefaultRegistry constructs exactly that set for the API process.

import (
	"encoding/json"
	"net/http"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/tools"
)

// writeToolJSON renders a JSON response (local helper, distinct name avoids
// collisions with other tracks' helpers in package main).
func writeToolJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// toolView is the wire shape of one catalog entry. description/input_schema
// are omitted for tools that publish no Described metadata.
type toolView struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// listToolsHandler serves GET /tools with the read-only registry catalog.
// A nil (or empty) registry renders an honest empty list, never a 5xx: the
// listing is observability, not control flow.
func listToolsHandler(reg *tools.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		infos := reg.List()
		views := make([]toolView, 0, len(infos))
		for _, info := range infos {
			views = append(views, toolView{
				Name:        info.Name,
				Description: info.Description,
				InputSchema: info.InputSchema,
			})
		}
		writeToolJSON(w, http.StatusOK, map[string]any{"tools": views})
	}
}

// registerToolsRoutes mounts the tool registry catalog on apiMux behind the
// agents.read permission (see the file-level permission fallback note).
func registerToolsRoutes(apiMux *http.ServeMux, reg *tools.Registry, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	apiMux.Handle("GET /tools", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionAgentsRead)(http.HandlerFunc(listToolsHandler(reg)))))
}
