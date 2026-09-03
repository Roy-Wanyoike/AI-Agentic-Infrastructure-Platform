package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/connectors"
)

// connectors.go mounts the connectors framework (issue #30) endpoints:
//
//	POST   /connectors            (connectors.write — OWNER/ADMIN)  create
//	GET    /connectors            (connectors.read  — MEMBER+)      list
//	GET    /connectors/{id}       (connectors.read  — MEMBER+)      get
//	DELETE /connectors/{id}       (connectors.write — OWNER/ADMIN)  delete
//	POST   /connectors/{id}/test  (connectors.write — OWNER/ADMIN)  live health check
//
// Routes are registered on apiMux so main.go serves them under both /v1 and
// /api/v1 (StripPrefix mounting). Tenant scope comes exclusively from auth
// claims — client-supplied organization ids are never trusted.
//
// RBAC (issue #30 contract: writes OWNER/ADMIN, reads MEMBER+): the dedicated
// additive permission pair connectors.read/connectors.write is registered by
// internal/auth/permissions_connectors.go following the per-track
// permissions_audit.go pattern. No existing read grant fits MEMBER+ (every
// existing read permission includes VIEWER), and connector listings expose
// integration topology (base URLs, auth styles, secret-ref names).
//
// SECURITY: secret VALUES never appear anywhere in this surface. The stored
// secret_ref is a NAME reference into the secrets store; config JSONB carries
// header TEMPLATES and auth style parameters only. Responses render an
// explicit field allowlist (connectorJSON), so even a future service-side
// field could not leak a value without touching this projection.

// registerConnectorsRoutes mounts all connector routes on apiMux.
func registerConnectorsRoutes(apiMux *http.ServeMux, svc *connectors.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	// auth wrap pattern from cmd/api/main.go: RequireAuthOrAPIKey outer,
	// RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("POST /connectors", wrap(auth.PermissionConnectorsWrite, http.HandlerFunc(createConnectorHandler(svc, auditSvc))))
	apiMux.Handle("GET /connectors", wrap(auth.PermissionConnectorsRead, http.HandlerFunc(listConnectorsHandler(svc))))
	apiMux.Handle("GET /connectors/{id}", wrap(auth.PermissionConnectorsRead, http.HandlerFunc(getConnectorHandler(svc))))
	apiMux.Handle("DELETE /connectors/{id}", wrap(auth.PermissionConnectorsWrite, http.HandlerFunc(deleteConnectorHandler(svc, auditSvc))))
	apiMux.Handle("POST /connectors/{id}/test", wrap(auth.PermissionConnectorsWrite, http.HandlerFunc(testConnectorHandler(svc, auditSvc))))
}

// writeJSONConnector serializes v with the given status (distinct name to
// avoid clashing with helpers in other handler files).
func writeJSONConnector(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeConnectorError emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeConnectorError(w http.ResponseWriter, status int, code, message string) {
	writeJSONConnector(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readConnectorJSON decodes the request body into dst, writing a 400 envelope
// on malformed JSON. Returns false when the response is already written.
func readConnectorJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeConnectorError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// connectorPayload mirrors the HTTP create/update body. Config is flattened
// onto the connector: auth_style plus optional header templates.
type connectorPayload struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	BaseURL      string            `json:"base_url"`
	AuthStyle    string            `json:"auth_style"`
	Headers      map[string]string `json:"headers"`
	APIKeyHeader string            `json:"api_key_header"`
	APIKeyPrefix string            `json:"api_key_prefix"`
	Username     string            `json:"username"`
	SecretRef    string            `json:"secret_ref"`
	Status       string            `json:"status"`
}

func (p *connectorPayload) toInput() connectors.CreateInput {
	return connectors.CreateInput{
		Name:    p.Name,
		Type:    p.Type,
		BaseURL: p.BaseURL,
		Config: connectors.Config{
			AuthStyle:    p.AuthStyle,
			Headers:      p.Headers,
			APIKeyHeader: p.APIKeyHeader,
			APIKeyPrefix: p.APIKeyPrefix,
			Username:     p.Username,
		},
		SecretRef: p.SecretRef,
		Status:    p.Status,
	}
}

// mapConnectorError converts connectors service errors into contract error
// responses. Validation failures are 422; unknown/foreign ids are 404 without
// an existence leak; anything else is a generic 500.
func mapConnectorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, connectors.ErrNotFound):
		writeConnectorError(w, http.StatusNotFound, "CONNECTOR_NOT_FOUND", "connector not found")
	case errors.Is(err, connectors.ErrDuplicate):
		writeConnectorError(w, http.StatusConflict, "CONNECTOR_ALREADY_EXISTS", "connector already exists")
	case errors.Is(err, connectors.ErrOrgRequired),
		errors.Is(err, connectors.ErrNameRequired),
		errors.Is(err, connectors.ErrNameTooLong),
		errors.Is(err, connectors.ErrTypeInvalid),
		errors.Is(err, connectors.ErrBaseURLRequired),
		errors.Is(err, connectors.ErrBaseURLInvalid),
		errors.Is(err, connectors.ErrStatusInvalid),
		errors.Is(err, connectors.ErrAuthStyleInvalid),
		errors.Is(err, connectors.ErrUpdatedByRequired):
		writeConnectorError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	default:
		writeConnectorError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// connectorJSON renders one connector through an explicit field allowlist.
// The config projection is structurally value-free (header templates and
// auth style parameters only) and secret_ref is a NAME reference.
func connectorJSON(c *connectors.Connector) map[string]any {
	out := map[string]any{
		"id":         c.ID,
		"name":       c.Name,
		"type":       c.Type,
		"base_url":   c.BaseURL,
		"secret_ref": c.SecretRef,
		"status":     c.Status,
		"config": map[string]any{
			"auth_style":     c.Config.AuthStyle,
			"headers":        c.Config.Headers,
			"api_key_header": c.Config.APIKeyHeader,
			"api_key_prefix": c.Config.APIKeyPrefix,
			"username":       c.Config.Username,
		},
		"created_by": c.CreatedBy,
		"created_at": c.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": c.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if c.LastCheckAt != nil {
		out["last_check_at"] = c.LastCheckAt.UTC().Format(time.RFC3339)
	} else {
		out["last_check_at"] = nil
	}
	out["last_check_status"] = c.LastCheckStatus
	return out
}

// createConnectorHandler validates and registers a new org-scoped connector.
// The request never carries secret VALUES — only the secret_ref name.
func createConnectorHandler(svc *connectors.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeConnectorError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		var req connectorPayload
		if !readConnectorJSON(w, r, &req) {
			return
		}
		// Tenant guard: the connector is created with the caller's
		// organization_id; client-supplied org ids are ignored by design.
		created, err := svc.Create(r.Context(), claims.OrganizationID, req.toInput(), claims.UserID)
		if err != nil {
			mapConnectorError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (no secret material involved)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "connector.created",
				claims.OrganizationID, "connectors/"+created.ID, map[string]any{"name": created.Name})
		}
		writeJSONConnector(w, http.StatusCreated, map[string]any{"connector": connectorJSON(created)})
	}
}

// listConnectorsHandler returns the caller's connectors (name ASC).
func listConnectorsHandler(svc *connectors.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeConnectorError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		list, err := svc.List(r.Context(), claims.OrganizationID)
		if err != nil {
			mapConnectorError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(list))
		for _, c := range list {
			items = append(items, connectorJSON(c))
		}
		writeJSONConnector(w, http.StatusOK, map[string]any{"connectors": items})
	}
}

// getConnectorHandler returns one connector within the caller's organization.
func getConnectorHandler(svc *connectors.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeConnectorError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeConnectorError(w, http.StatusNotFound, "CONNECTOR_NOT_FOUND", "connector not found")
			return
		}
		c, err := svc.Get(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapConnectorError(w, err)
			return
		}
		writeJSONConnector(w, http.StatusOK, map[string]any{"connector": connectorJSON(c)})
	}
}

// deleteConnectorHandler hard-deletes one connector within the caller's
// organization (foreign/unknown ids surface as 404 without an existence leak).
func deleteConnectorHandler(svc *connectors.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeConnectorError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeConnectorError(w, http.StatusNotFound, "CONNECTOR_NOT_FOUND", "connector not found")
			return
		}
		if err := svc.Delete(r.Context(), claims.OrganizationID, id); err != nil {
			mapConnectorError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "connector.deleted",
				claims.OrganizationID, "connectors/"+id, nil)
		}
		writeJSONConnector(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

// testConnectorHandler triggers the live health check (5s timeout, outcome
// recorded on the connector as last_check_at/last_check_status). The probe
// response never includes secret values — only the classified outcome.
func testConnectorHandler(svc *connectors.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeConnectorError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeConnectorError(w, http.StatusNotFound, "CONNECTOR_NOT_FOUND", "connector not found")
			return
		}
		result, err := svc.Test(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapConnectorError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (metadata names the outcome only)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "connector.tested",
				claims.OrganizationID, "connectors/"+id, map[string]any{"status": result.Status})
		}
		writeJSONConnector(w, http.StatusOK, map[string]any{"test": result})
	}
}
