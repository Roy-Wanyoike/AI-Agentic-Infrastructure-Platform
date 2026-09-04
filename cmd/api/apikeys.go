package main

// Issue #48 (wave 6-d) HTTP handlers — the API keys management surface.
//
// Endpoints (registered on apiMux by registerAPIKeysRoutes; served under BOTH
// /v1 and /api/v1):
//
//	POST   /api-keys       (agents.write — OWNER/ADMIN)  mint (value exactly once)
//	GET    /api-keys       (runs.execute — MEMBER+)      metadata list
//	DELETE /api-keys/{id}  (agents.write — OWNER/ADMIN)  revoke
//
// internal/apikeys has been service-complete since its introduction (SHA-256
// hash-at-rest, plaintext returned exactly once at creation, org-scoped
// store queries) and the auth middleware already VERIFIES these keys
// (auth.RequireAuthOrAPIKey); only the management routes were missing — the
// sole key so far was the dev-only auto-minted one in cmd/api/main.go (which
// is not manageable and stays untouched here).
//
// SECURITY: the plaintext key value exists in exactly one wire place — the
// response body of POST /api-keys, returned EXACTLY ONCE as the top-level
// "value" sibling of the metadata object (the POST /scim/tokens one-time
// credential shape). It is never echoed by list/revoke responses, never
// logged (handlers log nothing), and never written to audit metadata. At
// rest only the SHA-256 hash is persisted (store layer). List projections
// are metadata-only BY CONSTRUCTION: id, name, prefix (the "ak_…" display
// prefix kept for identification; the server only ever matches the hash of
// the FULL value, so the prefix authenticates nothing), created_by,
// created_at and the revoked flag — never the value, never the full hash.
//
// RBAC reuse (no new permission enum was introduced), mirroring the secrets
// track (cmd/api/secrets.go):
//   - writes (create/revoke) -> agents.write (matrix exactly OWNER/ADMIN)
//   - list (metadata only)   -> runs.execute (matrix exactly OWNER/ADMIN/
//     MEMBER — MEMBER+, so viewers cannot enumerate credentials)
//
// API keys authenticate as OWNER (auth.RequireAuthOrAPIKey), matching the
// platform-wide API-key model.
//
// Tenant guard: org scope comes exclusively from the auth claims —
// client-supplied organization ids are never trusted. Revocation verifies
// key ownership through the org-scoped ListKeysCtx before calling RevokeCtx:
// the durable store re-checks organization_id on the UPDATE itself, and the
// ownership pre-check keeps the same 404 semantics for the in-memory service
// whose legacy context-free revoke path predates tenant guards. Foreign and
// unknown ids surface as 404 with no existence leak across tenants.
// Revocation is IDEMPOTENT per service behavior: revoking an already-revoked
// key returns 200 (both the durable UPDATE, which does not filter on
// revoked_at, and the in-memory path simply re-confirm the revocation);
// only unknown/foreign ids are 404.

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
)

// registerAPIKeysRoutes mounts the api-keys management surface on apiMux.
func registerAPIKeysRoutes(apiMux *http.ServeMux, svc *apikeys.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	// auth wrap pattern from cmd/api/main.go: RequireAuthOrAPIKey outer,
	// RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("POST /api-keys", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(createAPIKeyHandler(svc, auditSvc))))
	apiMux.Handle("GET /api-keys", wrap(auth.PermissionRunsExecute, http.HandlerFunc(listAPIKeysHandler(svc))))
	apiMux.Handle("DELETE /api-keys/{id}", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(revokeAPIKeyHandler(svc, auditSvc))))
}

// writeJSONAPIKey serializes v with the given status (distinct name to avoid
// clashing with helpers in other handler files).
func writeJSONAPIKey(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAPIKeyError emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeAPIKeyError(w http.ResponseWriter, status int, code, message string) {
	writeJSONAPIKey(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readAPIKeyJSON decodes the request body into dst, writing a 400 envelope on
// malformed JSON. Returns false when the response is already written.
func readAPIKeyJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeAPIKeyError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// apiKeyClaims resolves the caller's identity from the auth context (never
// from client input); a missing organization claim is a wiring bug and 401s.
func apiKeyClaims(w http.ResponseWriter, r *http.Request) (auth.UserClaims, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeAPIKeyError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return auth.UserClaims{}, false
	}
	if strings.TrimSpace(claims.OrganizationID) == "" {
		writeAPIKeyError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing organization claim")
		return auth.UserClaims{}, false
	}
	return claims, true
}

// apiKeyJSON renders one API key METADATA projection. There is deliberately
// no `value` and no `hash` property: the plaintext leaves the API exactly
// once (the create response) and only the SHA-256 hash is ever persisted.
func apiKeyJSON(k *apikeys.APIKey) map[string]any {
	return map[string]any{
		"id":         k.ID,
		"name":       k.Name,
		"prefix":     k.Prefix,
		"created_by": k.UserID,
		"created_at": k.CreatedAt.UTC().Format(time.RFC3339),
		"revoked":    k.Revoked,
	}
}

// createAPIKeyHandler mints a new key for the caller's organization. The
// plaintext value is returned EXACTLY ONCE (top-level "value" sibling of the
// metadata object) and the minting writes an audit entry without key material.
func createAPIKeyHandler(svc *apikeys.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := apiKeyClaims(w, r)
		if !ok {
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if !readAPIKeyJSON(w, r, &req) {
			return
		}
		// Validation pre-check mirroring the service's own message: an empty
		// name is a client error (422 VALIDATION_ERROR), like the secrets
		// track; org/user ids come from the claims and cannot be empty here.
		if strings.TrimSpace(req.Name) == "" {
			writeAPIKeyError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "key name is required")
			return
		}
		// Tenant guard: the key is created with the caller's organization_id;
		// client-supplied org ids are ignored by design.
		key, err := svc.CreateCtx(r.Context(), claims.OrganizationID, claims.UserID, req.Name)
		if err != nil {
			writeAPIKeyError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert; no key material)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "api_key.created",
				claims.OrganizationID, "api-keys/"+key.ID, map[string]any{"name": req.Name})
		}
		writeJSONAPIKey(w, http.StatusCreated, map[string]any{
			"api_key": apiKeyJSON(key),
			"value":   key.Value,
		})
	}
}

// listAPIKeysHandler returns metadata ONLY for the caller's organization,
// newest first. The durable store already orders by created_at DESC; the
// in-memory projection is sorted here so both modes honor one contract
// (ties broken by id for determinism).
func listAPIKeysHandler(svc *apikeys.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := apiKeyClaims(w, r)
		if !ok {
			return
		}
		// Tenant guard: the listing filters on the caller's organization_id.
		list, err := svc.ListKeysCtx(r.Context(), claims.OrganizationID)
		if err != nil {
			writeAPIKeyError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		keys := make([]*apikeys.APIKey, len(list))
		copy(keys, list)
		sort.Slice(keys, func(i, j int) bool {
			if !keys[i].CreatedAt.Equal(keys[j].CreatedAt) {
				return keys[i].CreatedAt.After(keys[j].CreatedAt)
			}
			return keys[i].ID < keys[j].ID
		})
		items := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			items = append(items, apiKeyJSON(k))
		}
		writeJSONAPIKey(w, http.StatusOK, map[string]any{"api_keys": items})
	}
}

// revokeAPIKeyHandler revokes one key within the caller's organization.
// Unknown/foreign ids surface as 404 without an existence leak; revoking an
// already-revoked key stays 200 (idempotent per service semantics). The
// revoke writes an audit entry without key material.
func revokeAPIKeyHandler(svc *apikeys.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := apiKeyClaims(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeAPIKeyError(w, http.StatusNotFound, "API_KEY_NOT_FOUND", "api key not found")
			return
		}
		// Ownership pre-check (tenant guard): the durable store re-checks
		// organization_id on the UPDATE itself; the org-scoped listing keeps
		// the same 404 semantics for the legacy in-memory service whose
		// context-free revoke path predates tenant guards. Revoked keys stay
		// listed, so re-revocation resolves as 200, not 404.
		owned, err := svc.ListKeysCtx(r.Context(), claims.OrganizationID)
		if err != nil {
			writeAPIKeyError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		found := false
		for _, k := range owned {
			if k.ID == id {
				found = true
				break
			}
		}
		if !found {
			writeAPIKeyError(w, http.StatusNotFound, "API_KEY_NOT_FOUND", "api key not found")
			return
		}
		if err := svc.RevokeCtx(r.Context(), claims.OrganizationID, id); err != nil {
			if errors.Is(err, apikeys.ErrKeyNotFound) {
				writeAPIKeyError(w, http.StatusNotFound, "API_KEY_NOT_FOUND", "api key not found")
				return
			}
			writeAPIKeyError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert; no key material)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "api_key.revoked",
				claims.OrganizationID, "api-keys/"+id, nil)
		}
		writeJSONAPIKey(w, http.StatusOK, map[string]any{"revoked": true})
	}
}
