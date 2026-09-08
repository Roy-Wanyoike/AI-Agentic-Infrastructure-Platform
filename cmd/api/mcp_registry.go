package main

// mcp_registry.go — issue #82 HTTP wiring: the org-scoped MCP server
// registry endpoints (the CONSUMING side; the inbound MCP server endpoint
// lives in mcp.go / issue #50).
//
// Endpoints (registered on apiMux by registerMcpRegistryRoutes; served
// under BOTH /v1 and /api/v1):
//
//	POST   /mcp-servers        register + health-check (initialize ping)
//	GET    /mcp-servers        list (name ASC)
//	GET    /mcp-servers/{id}   get one
//	PUT    /mcp-servers/{id}   update + re-check (tool cache refresh)
//	DELETE /mcp-servers/{id}   delete (tools stop being exposed)
//	POST   /mcp-servers/{id}/test  explicit re-test (status + tool cache refresh)
//
// Registration health check: the service runs the initialize handshake and
// tools/list against the endpoint. An unreachable server is a 422
// MCP_SERVER_UNREACHABLE unless ?force=true — forcing stores the server as
// status "unreachable" (no cached tools) so the operator can fix the
// endpoint and re-test later.
//
// Design (mirrors cmd/api/connectors.go):
//   - RBAC: the EXISTING connectors.read/connectors.write grants are reused
//     (no new permission enum). This is an integration-governance surface
//     with the exact connectors role matrix: writes OWNER/ADMIN, reads
//     MEMBER+ (a listing exposes integration topology — endpoint URLs,
//     secret-ref names).
//   - Tenant scope comes exclusively from the auth claims; cross-tenant ids
//     surface as 404 without an existence leak.
//   - SECRET HANDLING: only the secret_ref NAME is accepted/returned; the
//     VALUE is resolved per call through the secrets service inside the
//     registry service and injected as the auth header. Responses render an
//     explicit field allowlist.
//   - Audit: registration/update/deletion/test write best-effort rows
//     (mcp.server_*); every external TOOL invocation is audited by the
//     registry service itself via the Auditor seam (mcp.tool_call with the
//     server ref + remote tool name).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/mcp"
)

// registerMcpRegistryRoutes mounts the MCP server registry endpoints.
func registerMcpRegistryRoutes(apiMux *http.ServeMux, svc *mcp.Registry, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go: RequireAuthOrAPIKey outer,
	// RequirePermission inner. The connectors grants are reused (see file
	// comment) — same OWNER/ADMIN write, MEMBER+ read matrix.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("POST /mcp-servers", wrap(auth.PermissionConnectorsWrite, http.HandlerFunc(createMcpServerHandler(svc, auditSvc))))
	apiMux.Handle("GET /mcp-servers", wrap(auth.PermissionConnectorsRead, http.HandlerFunc(listMcpServersHandler(svc))))
	apiMux.Handle("GET /mcp-servers/{id}", wrap(auth.PermissionConnectorsRead, http.HandlerFunc(getMcpServerHandler(svc))))
	apiMux.Handle("PUT /mcp-servers/{id}", wrap(auth.PermissionConnectorsWrite, http.HandlerFunc(updateMcpServerHandler(svc, auditSvc))))
	apiMux.Handle("DELETE /mcp-servers/{id}", wrap(auth.PermissionConnectorsWrite, http.HandlerFunc(deleteMcpServerHandler(svc, auditSvc))))
	apiMux.Handle("POST /mcp-servers/{id}/test", wrap(auth.PermissionConnectorsWrite, http.HandlerFunc(testMcpServerHandler(svc, auditSvc))))
}

// writeJSONMcpReg serializes v with the given status (distinct helper name
// to avoid clashing with helpers in other handler files).
func writeJSONMcpReg(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeMcpRegError emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeMcpRegError(w http.ResponseWriter, status int, code, message string) {
	writeJSONMcpReg(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readMcpRegJSON decodes the request body into dst, writing a 400 envelope
// on malformed JSON. Returns false when the response is already written.
func readMcpRegJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeMcpRegError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// mcpRegForce reads the ?force flag (registration/update tolerate an
// unreachable endpoint when explicitly forced).
func mcpRegForce(r *http.Request) bool {
	raw := strings.TrimSpace(r.URL.Query().Get("force"))
	if raw == "" {
		return false
	}
	forced, err := strconv.ParseBool(raw)
	if err != nil {
		return false
	}
	return forced
}

// mcpServerPayload mirrors the HTTP create/update body. The secret_ref is a
// NAME reference only — secret values are never accepted by this surface.
type mcpServerPayload struct {
	Name        string `json:"name"`
	EndpointURL string `json:"endpoint_url"`
	SecretRef   string `json:"secret_ref"`
	AuthHeader  string `json:"auth_header"`
	Status      string `json:"status"`
}

func (p *mcpServerPayload) toInput() mcp.CreateServerInput {
	return mcp.CreateServerInput{
		Name:        p.Name,
		EndpointURL: p.EndpointURL,
		SecretRef:   p.SecretRef,
		AuthHeader:  p.AuthHeader,
		Status:      p.Status,
	}
}

// mapMcpRegistryError converts registry service errors into contract error
// responses. Validation failures are 422; the unreachable-registration
// refusal is its own 422 code; unknown/foreign ids are 404 without an
// existence leak; anything else is a generic 500.
func mapMcpRegistryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mcp.ErrNotFound):
		writeMcpRegError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "mcp server not found")
	case errors.Is(err, mcp.ErrDuplicate):
		writeMcpRegError(w, http.StatusConflict, "MCP_SERVER_ALREADY_EXISTS", "mcp server already exists")
	case errors.Is(err, mcp.ErrServerUnreachable):
		writeMcpRegError(w, http.StatusUnprocessableEntity, "MCP_SERVER_UNREACHABLE", err.Error())
	case errors.Is(err, mcp.ErrOrgRequired),
		errors.Is(err, mcp.ErrNameRequired),
		errors.Is(err, mcp.ErrNameTooLong),
		errors.Is(err, mcp.ErrEndpointRequired),
		errors.Is(err, mcp.ErrEndpointInvalid),
		errors.Is(err, mcp.ErrStatusInvalid),
		errors.Is(err, mcp.ErrAuthHeaderInvalid),
		errors.Is(err, mcp.ErrUpdatedByRequired):
		writeMcpRegError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	default:
		writeMcpRegError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// mcpServerJSON renders one server through an explicit field allowlist.
// secret_ref is a NAME reference; the cached tools carry the remote
// name/description/inputSchema verbatim (no secret material exists on the
// row).
func mcpServerJSON(s *mcp.Server) map[string]any {
	out := map[string]any{
		"id":           s.ID,
		"name":         s.Name,
		"endpoint_url": s.EndpointURL,
		"secret_ref":   s.SecretRef,
		"auth_header":  s.AuthHeader,
		"status":       s.Status,
		"created_by":   s.CreatedBy,
		"created_at":   s.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":   s.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if s.LastCheckAt != nil {
		out["last_check_at"] = s.LastCheckAt.UTC().Format(time.RFC3339)
	} else {
		out["last_check_at"] = nil
	}
	out["protocol_version"] = s.ProtocolVersion
	tools := make([]map[string]any, 0, len(s.Tools))
	for _, t := range s.Tools {
		entry := map[string]any{"name": t.Name}
		if t.Description != "" {
			entry["description"] = t.Description
		}
		if t.InputSchema != nil {
			entry["inputSchema"] = t.InputSchema
		}
		tools = append(tools, entry)
	}
	out["tools"] = tools
	return out
}

// createMcpServerHandler registers and health-checks a new org-scoped MCP
// server. Unreachable endpoints answer 422 MCP_SERVER_UNREACHABLE unless
// ?force=true (then the server is stored as "unreachable").
func createMcpServerHandler(svc *mcp.Registry, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMcpRegError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		var req mcpServerPayload
		if !readMcpRegJSON(w, r, &req) {
			return
		}
		// Tenant guard: the server is created with the caller's
		// organization_id; client-supplied org ids are ignored by design.
		created, err := svc.CreateServer(r.Context(), claims.OrganizationID, req.toInput(), claims.UserID, mcpRegForce(r))
		if err != nil {
			mapMcpRegistryError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit row (endpoint URL is topology, not secret)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "mcp.server_registered",
				claims.OrganizationID, "mcp_servers/"+created.ID,
				map[string]any{"name": created.Name, "endpoint_url": created.EndpointURL, "status": created.Status})
		}
		writeJSONMcpReg(w, http.StatusCreated, map[string]any{"mcp_server": mcpServerJSON(created)})
	}
}

// listMcpServersHandler returns the caller's MCP servers (name ASC).
func listMcpServersHandler(svc *mcp.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMcpRegError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		list, err := svc.ListServers(r.Context(), claims.OrganizationID)
		if err != nil {
			mapMcpRegistryError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(list))
		for _, s := range list {
			items = append(items, mcpServerJSON(s))
		}
		writeJSONMcpReg(w, http.StatusOK, map[string]any{"mcp_servers": items})
	}
}

// getMcpServerHandler returns one MCP server within the caller's organization.
func getMcpServerHandler(svc *mcp.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMcpRegError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeMcpRegError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "mcp server not found")
			return
		}
		s, err := svc.GetServer(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapMcpRegistryError(w, err)
			return
		}
		writeJSONMcpReg(w, http.StatusOK, map[string]any{"mcp_server": mcpServerJSON(s)})
	}
}

// updateMcpServerHandler replaces the mutable fields and re-runs the health
// check (refreshing the tool cache). Unreachable re-checks answer 422
// unless ?force=true.
func updateMcpServerHandler(svc *mcp.Registry, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMcpRegError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeMcpRegError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "mcp server not found")
			return
		}
		var req mcpServerPayload
		if !readMcpRegJSON(w, r, &req) {
			return
		}
		updated, err := svc.UpdateServer(r.Context(), claims.OrganizationID, id, req.toInput(), claims.UserID, mcpRegForce(r))
		if err != nil {
			mapMcpRegistryError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "mcp.server_updated",
				claims.OrganizationID, "mcp_servers/"+id,
				map[string]any{"name": updated.Name, "endpoint_url": updated.EndpointURL, "status": updated.Status})
		}
		writeJSONMcpReg(w, http.StatusOK, map[string]any{"mcp_server": mcpServerJSON(updated)})
	}
}

// deleteMcpServerHandler hard-deletes one MCP server within the caller's
// organization (foreign/unknown ids surface as 404 without an existence
// leak). Its tools stop being exposed on the next tool build.
func deleteMcpServerHandler(svc *mcp.Registry, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMcpRegError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeMcpRegError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "mcp server not found")
			return
		}
		if err := svc.DeleteServer(r.Context(), claims.OrganizationID, id); err != nil {
			mapMcpRegistryError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "mcp.server_deleted",
				claims.OrganizationID, "mcp_servers/"+id, nil)
		}
		writeJSONMcpReg(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	}
}

// testMcpServerHandler re-runs the health check (initialize ping +
// tools/list refresh) for one server and returns the refreshed row.
func testMcpServerHandler(svc *mcp.Registry, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMcpRegError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeMcpRegError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "mcp server not found")
			return
		}
		s, err := svc.TestServer(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapMcpRegistryError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "mcp.server_tested",
				claims.OrganizationID, "mcp_servers/"+id,
				map[string]any{"name": s.Name, "status": s.Status, "tool_count": len(s.Tools)})
		}
		writeJSONMcpReg(w, http.StatusOK, map[string]any{"mcp_server": mcpServerJSON(s)})
	}
}
