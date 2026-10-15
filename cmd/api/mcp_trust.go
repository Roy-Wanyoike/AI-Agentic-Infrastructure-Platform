package main

// mcp_trust.go — wave-8 issue B HTTP wiring: the MCP registry TRUST layer
// endpoints (drift visibility, version pinning, change acceptance) on top
// of the org-scoped registry surface from mcp_registry.go.
//
// Endpoints (registered on apiMux by registerMcpTrustRoutes; served under
// BOTH /v1 and /api/v1 like every versioned surface):
//
//	GET  /mcp-servers/{id}/drift        drift history + current/pinned hash
//	POST /mcp-servers/{id}/pin          pin the CURRENT cached toolset hash
//	POST /mcp-servers/{id}/pin/accept   accept drift (re-probe; update pin+cache)
//
// The re-test endpoint (POST /mcp-servers/{id}/test, mcp_registry.go) now
// also honors ?accept_changes=true and renders a "drift" object on the
// response whenever drift was detected on that call (additive JSON only).
//
// Design (mirrors cmd/api/mcp_registry.go exactly):
//   - RBAC: the EXISTING connectors grants are reused (no new permission
//     enum) — drift reads are connectors.read (MEMBER+; drift is integration
//     topology), pin/accept writes are connectors.write (OWNER/ADMIN: these
//     decide which toolset agents are allowed to see).
//   - Tenant scope comes exclusively from the auth claims; cross-tenant ids
//     surface as 404 without an existence leak.
//   - Response rendering is an explicit field allowlist; hashes are sha256
//     hex (non-sensitive by construction) and drift diffs carry tool NAMES
//     only — never cached tool payloads or secret material.
//   - Audit: pin/accept write best-effort rows (mcp.server_pinned /
//     mcp.server_changes_accepted); drift itself is audited by the service
//     layer (mcp.toolset_drift through the Auditor seam).

import (
	"errors"
	"net/http"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/mcp"
)

// registerMcpTrustRoutes mounts the MCP trust-layer endpoints.
func registerMcpTrustRoutes(apiMux *http.ServeMux, svc *mcp.Registry, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go / mcp_registry.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner; the connectors
	// grants are reused (same OWNER/ADMIN write, MEMBER+ read matrix).
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("GET /mcp-servers/{id}/drift", wrap(auth.PermissionConnectorsRead, http.HandlerFunc(mcpServerDriftHandler(svc))))
	apiMux.Handle("POST /mcp-servers/{id}/pin", wrap(auth.PermissionConnectorsWrite, http.HandlerFunc(mcpServerPinHandler(svc, auditSvc))))
	apiMux.Handle("POST /mcp-servers/{id}/pin/accept", wrap(auth.PermissionConnectorsWrite, http.HandlerFunc(mcpServerAcceptChangesHandler(svc, auditSvc))))
}

// mapMcpTrustError converts trust service errors into contract error
// responses. Unknown/foreign ids are 404 without an existence leak; a pin
// on an empty toolset is a 422; anything else is a generic 500.
func mapMcpTrustError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mcp.ErrNotFound):
		writeMcpRegError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "mcp server not found")
	case errors.Is(err, mcp.ErrOrgRequired), errors.Is(err, mcp.ErrPinNoToolset):
		writeMcpRegError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	default:
		writeMcpRegError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// mcpTrustServerID extracts the path value or renders the 404 (no
// existence leak for empty/foreign ids), returning "" when the response is
// already written.
func mcpTrustServerID(w http.ResponseWriter, r *http.Request) string {
	id := r.PathValue("id")
	if id == "" {
		writeMcpRegError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "mcp server not found")
		return ""
	}
	return id
}

// driftJSON renders one DriftEvent through an explicit field allowlist
// (tool NAMES only — never cached payloads).
func driftJSON(ev mcp.DriftEvent) map[string]any {
	added := ev.Added
	if added == nil {
		added = []string{}
	}
	removed := ev.Removed
	if removed == nil {
		removed = []string{}
	}
	changed := ev.Changed
	if changed == nil {
		changed = []string{}
	}
	return map[string]any{
		"at":       ev.At.UTC().Format(time.RFC3339),
		"old_hash": ev.OldHash,
		"new_hash": ev.NewHash,
		"added":    added,
		"removed":  removed,
		"changed":  changed,
	}
}

// mcpServerDriftHandler returns the drift history plus the current and
// pinned toolset hashes for one server within the caller's organization.
func mcpServerDriftHandler(svc *mcp.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMcpRegError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := mcpTrustServerID(w, r)
		if id == "" {
			return
		}
		// Tenant guard + the current hash state (the same read path every
		// registry GET uses).
		srv, err := svc.GetServer(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapMcpTrustError(w, err)
			return
		}
		history, err := svc.DriftHistory(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapMcpTrustError(w, err)
			return
		}
		events := make([]map[string]any, 0, len(history))
		for _, ev := range history {
			events = append(events, driftJSON(ev))
		}
		out := map[string]any{
			"mcp_server_id": srv.ID,
			"toolset_hash":  srv.ToolsetHash,
			"pinned":        srv.PinnedToolsetHash != "",
			"drift_events":  events,
			"drift_count":   len(events),
		}
		if srv.PinnedToolsetHash != "" {
			out["pinned_toolset_hash"] = srv.PinnedToolsetHash
		}
		if srv.LastDriftAt != nil {
			out["last_drift_at"] = srv.LastDriftAt.UTC().Format(time.RFC3339)
		}
		writeJSONMcpReg(w, http.StatusOK, out)
	}
}

// mcpServerPinHandler pins the CURRENT cached toolset hash as the approved
// baseline. While pinned, a re-test that detects a differing fingerprint
// keeps the old cached tools (the drift is surfaced instead).
func mcpServerPinHandler(svc *mcp.Registry, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMcpRegError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := mcpTrustServerID(w, r)
		if id == "" {
			return
		}
		srv, err := svc.Pin(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapMcpTrustError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "mcp.server_pinned",
				claims.OrganizationID, "mcp_servers/"+id,
				map[string]any{"name": srv.Name, "pinned_toolset_hash": srv.PinnedToolsetHash, "tool_count": len(srv.Tools)})
		}
		writeJSONMcpReg(w, http.StatusOK, map[string]any{"mcp_server": mcpServerJSON(srv)})
	}
}

// mcpServerAcceptChangesHandler accepts the detected drift: re-probes the
// server and adopts the fresh toolset as BOTH the cached toolset and the
// pinned hash (the explicit "I have reviewed the change" action).
func mcpServerAcceptChangesHandler(svc *mcp.Registry, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMcpRegError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := mcpTrustServerID(w, r)
		if id == "" {
			return
		}
		srv, err := svc.AcceptChanges(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapMcpTrustError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "mcp.server_changes_accepted",
				claims.OrganizationID, "mcp_servers/"+id,
				map[string]any{"name": srv.Name, "toolset_hash": srv.ToolsetHash, "tool_count": len(srv.Tools)})
		}
		writeJSONMcpReg(w, http.StatusOK, map[string]any{"mcp_server": mcpServerJSON(srv)})
	}
}
