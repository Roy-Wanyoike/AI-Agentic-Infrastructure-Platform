package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/secrets"
)

// secrets.go mounts the wave-5 secrets track (issue #25) endpoints:
//
//	POST   /secrets              (agents.write   — OWNER/ADMIN)  create
//	GET    /secrets              (runs.execute   — MEMBER+)      metadata list
//	DELETE /secrets/{name}       (agents.write   — OWNER/ADMIN)  soft delete
//	POST   /secrets/{name}/reveal (organization.manage — OWNER)   value once
//
// Routes are registered on apiMux so main.go serves them under both /v1 and
// /api/v1 (StripPrefix mounting). Tenant scope comes exclusively from auth
// claims — client-supplied organization ids are never trusted.
//
// RBAC reuse (no new permission enum was introduced): writes are guarded by
// agents.write, whose existing grant matrix is exactly OWNER/ADMIN; listing is
// guarded by runs.execute, whose matrix is exactly OWNER/ADMIN/MEMBER (MEMBER+
// — viewers must not see even secret NAMES); reveal is guarded by
// organization.manage, whose matrix is exactly OWNER. API keys authenticate as
// OWNER, matching the platform-wide API-key model.
//
// SECURITY: secret values exist in exactly two places — the request body of
// POST /secrets and the one-time reveal response. They are never logged
// (handlers log nothing), never echoed in create/list/delete responses, and
// never written to audit metadata.

// registerSecretsRoutes mounts all secret routes on apiMux.
func registerSecretsRoutes(apiMux *http.ServeMux, svc *secrets.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	// auth wrap pattern from cmd/api/main.go: RequireAuthOrAPIKey outer,
	// RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("POST /secrets", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(createSecretHandler(svc, auditSvc))))
	apiMux.Handle("GET /secrets", wrap(auth.PermissionRunsExecute, http.HandlerFunc(listSecretsHandler(svc))))
	apiMux.Handle("DELETE /secrets/{name}", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(deleteSecretHandler(svc, auditSvc))))
	apiMux.Handle("POST /secrets/{name}/reveal", wrap(auth.PermissionOrgManage, http.HandlerFunc(revealSecretHandler(svc, auditSvc))))
}

// writeJSONSecret serializes v with the given status (distinct name to avoid
// clashing with helpers in other handler files).
func writeJSONSecret(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeSecretError emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeSecretError(w http.ResponseWriter, status int, code, message string) {
	writeJSONSecret(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readSecretJSON decodes the request body into dst, writing a 400 envelope on
// malformed JSON. Returns false when the response is already written.
func readSecretJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeSecretError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// mapSecretError converts secrets service errors into contract error responses.
// Crypto failures (wrong key / tampered data / unknown key version) collapse
// into a generic 500: the wording of ErrDecryptFailed must never leak oracle
// detail to callers.
func mapSecretError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		writeSecretError(w, http.StatusNotFound, "SECRET_NOT_FOUND", "secret not found")
	case errors.Is(err, secrets.ErrDuplicate):
		writeSecretError(w, http.StatusConflict, "SECRET_ALREADY_EXISTS", "secret already exists")
	case errors.Is(err, secrets.ErrOrgRequired),
		errors.Is(err, secrets.ErrNameRequired),
		errors.Is(err, secrets.ErrValueRequired),
		errors.Is(err, secrets.ErrNameInvalid),
		errors.Is(err, secrets.ErrValueTooLarge),
		errors.Is(err, secrets.ErrUpdatedByRequired):
		writeSecretError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, secrets.ErrDecryptFailed),
		errors.Is(err, secrets.ErrUnknownKeyVersion),
		errors.Is(err, secrets.ErrInvalidEnvelope),
		errors.Is(err, secrets.ErrInvalidMasterKey),
		errors.Is(err, secrets.ErrMasterKeyRequired):
		writeSecretError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	default:
		writeSecretError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// secretJSON renders one secret METADATA projection (value-free by type).
func secretJSON(s *secrets.Secret) map[string]any {
	return map[string]any{
		"name":        s.Name,
		"key_version": s.KeyVersion,
		"created_by":  s.CreatedBy,
		"created_at":  s.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":  s.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// createSecretHandler seals and stores a new org-scoped secret. The request
// value is consumed exactly once (seal -> store) and never echoed back.
func createSecretHandler(svc *secrets.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSecretError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		var req struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if !readSecretJSON(w, r, &req) {
			return
		}
		// Tenant guard: the secret is created with the caller's organization_id;
		// client-supplied org ids are ignored by design.
		meta, err := svc.Create(r.Context(), claims.OrganizationID, req.Name, req.Value, claims.UserID)
		if err != nil {
			mapSecretError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert; no value)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "secret.created",
				claims.OrganizationID, "secrets/"+meta.Name, nil)
		}
		writeJSONSecret(w, http.StatusCreated, map[string]any{"secret": secretJSON(meta)})
	}
}

// listSecretsHandler returns metadata ONLY (names + bookkeeping) for the
// caller's organization. The service/store projections carry no value field.
func listSecretsHandler(svc *secrets.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSecretError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		list, err := svc.List(r.Context(), claims.OrganizationID)
		if err != nil {
			mapSecretError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(list))
		for _, s := range list {
			items = append(items, secretJSON(s))
		}
		writeJSONSecret(w, http.StatusOK, map[string]any{"secrets": items})
	}
}

// deleteSecretHandler soft-deletes one secret within the caller's organization
// (tombstone; foreign/unknown names surface as 404 without an existence leak).
func deleteSecretHandler(svc *secrets.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSecretError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		name := r.PathValue("name")
		if name == "" {
			writeSecretError(w, http.StatusNotFound, "SECRET_NOT_FOUND", "secret not found")
			return
		}
		// Tenant guard: deletes require a matching organization_id.
		if err := svc.Delete(r.Context(), claims.OrganizationID, name); err != nil {
			mapSecretError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert; no value)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "secret.deleted",
				claims.OrganizationID, "secrets/"+name, nil)
		}
		writeJSONSecret(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

// revealSecretHandler returns the plaintext value EXACTLY ONCE to OWNER-role
// callers and writes an audit entry. The response is the only place a value
// ever leaves the API; nothing about it is logged or stored in metadata.
func revealSecretHandler(svc *secrets.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSecretError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		name := r.PathValue("name")
		if name == "" {
			writeSecretError(w, http.StatusNotFound, "SECRET_NOT_FOUND", "secret not found")
			return
		}
		// Tenant guard: both lookups are scoped to the caller's organization_id;
		// foreign secrets surface as 404 (no existence leak).
		meta, err := svc.Get(r.Context(), claims.OrganizationID, name)
		if err != nil {
			mapSecretError(w, err)
			return
		}
		value, err := svc.Resolve(r.Context(), claims.OrganizationID, name)
		if err != nil {
			mapSecretError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert; the VALUE is
			// deliberately absent from metadata)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "secret.revealed",
				claims.OrganizationID, "secrets/"+name, map[string]any{"name": name})
		}
		body := secretJSON(meta)
		body["value"] = value
		writeJSONSecret(w, http.StatusOK, map[string]any{"secret": body})
	}
}
