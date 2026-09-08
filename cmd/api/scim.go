package main

// Issue #29 (wave 7-b) HTTP handlers — SCIM 2.0 half.
//
// Endpoints (registered on apiMux by registerScimRoutes; served under BOTH
// /v1 and /api/v1):
//
//      POST /scim/tokens              -> mint a SCIM bearer credential
//                                        (session/API-key auth + organization.manage
//                                        = OWNER only); plaintext shown ONCE
//      DELETE /scim/tokens/{id}       -> revoke one credential (session/API-key
//                                        auth + users.manage = OWNER/ADMIN,
//                                        issue #79); 204, org-scoped, audited
//      GET   /scim/v2/Users?filter=   -> SCIM 2.0 ListResponse
//                                        (filter=userName eq "..." supported)
//      POST  /scim/v2/Users           -> JIT-provision one identity (201)
//      GET   /scim/v2/Users/{id}      -> point read (404 without existence leak)
//      PUT   /scim/v2/Users/{id}      -> full replace (userName immutable)
//      PATCH /scim/v2/Users/{id}      -> replace active (deprovisioning)
//
// The four /scim/v2/Users endpoints are guarded by scim.RequireSCIMToken —
// they accept ONLY a dedicated scim_ bearer credential (hashed at rest like
// api_keys), never session tokens or API keys, so directory automation
// cannot be confused with user login. The tenant comes from the token and
// is re-enforced at the identity store; SCIM responses use the standard
// application/scim+json envelopes (urn:ietf:params:scim:...).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/scim"
)

// scimStatus maps typed service errors onto the SCIM error contract
// (RFC 7644 section 3.12 statuses carried by scim.WriteError).
func scimStatus(err error) (int, string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, scim.ErrInvalidFilter),
		errors.Is(err, scim.ErrInvalidUserName),
		errors.Is(err, scim.ErrInvalidPatch),
		errors.Is(err, scim.ErrUserNameImmutable):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, scim.ErrDuplicateUser):
		return http.StatusConflict, err.Error()
	case errors.Is(err, scim.ErrUserNotFound):
		return http.StatusNotFound, err.Error()
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// writeScimJSON emits a success envelope with SCIM media type.
func writeScimJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeScimServiceError maps a service error onto the SCIM error envelope.
func writeScimServiceError(w http.ResponseWriter, err error) {
	status, message := scimStatus(err)
	scim.WriteError(w, status, message)
}

// scimOrg retrieves the tenant injected by scim.RequireSCIMToken. Reaching a
// handler without the middleware is a wiring bug, hence the 500.
func scimOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	org, err := scim.OrgFromContext(r.Context())
	if err != nil || strings.TrimSpace(org) == "" {
		scim.WriteError(w, http.StatusInternalServerError, "missing scim tenant context")
		return "", false
	}
	return org, true
}

// createSCIMTokenHandler serves POST /scim/tokens. Auth is the platform
// session/API-key surface and the permission is organization.manage, whose
// role matrix is EXACTLY OWNER — directory credentials are owner-level
// secrets. The plaintext scim_ secret is returned exactly once; only its
// SHA-256 hex hash is persisted (api_keys.key_hash pattern).
func createSCIMTokenHandler(svc *scim.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeSsoError(w, http.StatusServiceUnavailable, "SCIM_UNAVAILABLE", "scim service not available")
			return
		}
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil || strings.TrimSpace(claims.OrganizationID) == "" {
			writeSsoError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing organization claim")
			return
		}
		token, secret, err := svc.CreateToken(r.Context(), claims.OrganizationID, claims.UserID)
		if err != nil {
			writeSsoError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "scim token creation failed")
			return
		}
		// The token_hash is deliberately NOT echoed — it is neither a secret
		// nor something any client needs.
		writeSsoJSON(w, http.StatusCreated, map[string]any{
			"token": map[string]any{
				"id":              token.ID,
				"organization_id": token.OrgID,
				"created_by":      token.CreatedBy,
				"created_at":      token.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			},
			"secret": secret,
		})
	}
}

// revokeSCIMTokenHandler serves DELETE /scim/tokens/{id} (issue #79). The
// tenant comes exclusively from the auth claims; the store re-checks
// organization_id on the revoke itself, so unknown and foreign ids collapse
// into one 404 with no existence leak across tenants.
//
// Permission: users.manage (matrix exactly OWNER/ADMIN, per issue #79).
// Minting stays organization.manage (OWNER only) — creating directory
// credentials is owner-level — while retiring an existing one follows the
// api-keys revoke precedent (agents.write, OWNER/ADMIN): credential
// lifecycle management, not tenant administration.
//
// Semantics: success is 204 No Content and the revoked secret authenticates
// as nothing at all (bearer plane unchanged — SCIM tokens remain distinct
// from session tokens and API keys). Idempotency mirrors the existing
// scim.Service.RevokeToken behavior, which is reused verbatim: the
// in-memory store re-confirms an already-revoked id as 204, while the
// Postgres store's org-guarded UPDATE only matches live rows (revoked_at IS
// NULL), so a repeated DELETE of an already-revoked id 404s there — revoked
// and never-existed are deliberately indistinguishable, mirroring the
// bearer plane where a revoked credential is simply invalid. Neither path
// can ever un-revoke. The audit row carries no credential material (neither
// the plaintext secret nor the stored hash).
func revokeSCIMTokenHandler(svc *scim.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeSsoError(w, http.StatusServiceUnavailable, "SCIM_UNAVAILABLE", "scim service not available")
			return
		}
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil || strings.TrimSpace(claims.OrganizationID) == "" {
			writeSsoError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing organization claim")
			return
		}
		id := r.PathValue("id")
		if strings.TrimSpace(id) == "" {
			writeSsoError(w, http.StatusNotFound, "SCIM_TOKEN_NOT_FOUND", "scim token not found")
			return
		}
		if err := svc.RevokeToken(r.Context(), claims.OrganizationID, id); err != nil {
			if errors.Is(err, scim.ErrTokenNotFound) {
				writeSsoError(w, http.StatusNotFound, "SCIM_TOKEN_NOT_FOUND", "scim token not found")
				return
			}
			writeSsoError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "scim token revocation failed")
			return
		}
		if auditSvc != nil {
			// Best-effort audit trail entry (tenant-scoped insert; no
			// credential material). Mirrors the api_key.revoked pattern.
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "scim_token.revoked",
				claims.OrganizationID, "scim/tokens/"+id, nil)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// listSCIMUsersHandler serves GET /scim/v2/Users?filter=userName eq "...".
func listSCIMUsersHandler(svc *scim.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org, ok := scimOrg(w, r)
		if !ok {
			return
		}
		list, err := svc.ListUsers(r.Context(), org, r.URL.Query().Get("filter"))
		if err != nil {
			writeScimServiceError(w, err)
			return
		}
		writeScimJSON(w, http.StatusOK, list)
	}
}

// createSCIMUserHandler serves POST /scim/v2/Users.
func createSCIMUserHandler(svc *scim.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org, ok := scimOrg(w, r)
		if !ok {
			return
		}
		var req scim.UserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			scim.WriteError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		resource, err := svc.CreateUser(r.Context(), org, req)
		if err != nil {
			writeScimServiceError(w, err)
			return
		}
		w.Header().Set("Location", resource.Meta.Location)
		writeScimJSON(w, http.StatusCreated, resource)
	}
}

// getSCIMUserHandler serves GET /scim/v2/Users/{id}.
func getSCIMUserHandler(svc *scim.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org, ok := scimOrg(w, r)
		if !ok {
			return
		}
		resource, err := svc.GetUser(r.Context(), org, r.PathValue("id"))
		if err != nil {
			writeScimServiceError(w, err)
			return
		}
		writeScimJSON(w, http.StatusOK, resource)
	}
}

// replaceSCIMUserHandler serves PUT /scim/v2/Users/{id} (full replace;
// userName is immutable because it IS the login credential).
func replaceSCIMUserHandler(svc *scim.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org, ok := scimOrg(w, r)
		if !ok {
			return
		}
		var req scim.UserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			scim.WriteError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		resource, err := svc.ReplaceUser(r.Context(), org, r.PathValue("id"), req)
		if err != nil {
			writeScimServiceError(w, err)
			return
		}
		writeScimJSON(w, http.StatusOK, resource)
	}
}

// patchSCIMUserHandler serves PATCH /scim/v2/Users/{id}: replace `active`.
// Disabling here blocks password login through the shared auth lifecycle
// check (auth.ErrAccountDisabled).
func patchSCIMUserHandler(svc *scim.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org, ok := scimOrg(w, r)
		if !ok {
			return
		}
		var req scim.PatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			scim.WriteError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		resource, err := svc.PatchUser(r.Context(), org, r.PathValue("id"), req)
		if err != nil {
			writeScimServiceError(w, err)
			return
		}
		writeScimJSON(w, http.StatusOK, resource)
	}
}

// registerScimRoutes mounts the token-minting route (platform OWNER surface),
// the token-revocation route (issue #79, OWNER/ADMIN) and the SCIM 2.0
// protocol endpoints (dedicated bearer-token surface).
//
// auditSvc is optional (variadic) so the legacy 4-argument call sites (e.g.
// the issue #29 tests in scim_test.go) keep compiling; cmd/api/main.go passes
// a.auditSvc, so the production DELETE route writes its scim_token.revoked
// row (issue #79). Without it the route still mounts and only skips the
// audit entry.
func registerScimRoutes(apiMux *http.ServeMux, svc *scim.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc ...*audit.Service) {
	if apiMux == nil {
		return
	}
	var audits *audit.Service
	if len(auditSvc) > 0 {
		audits = auditSvc[0]
	}
	apiMux.Handle("POST /scim/tokens", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionOrgManage)(http.HandlerFunc(createSCIMTokenHandler(svc)))))
	apiMux.Handle("DELETE /scim/tokens/{id}", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionUsersManage)(http.HandlerFunc(revokeSCIMTokenHandler(svc, audits)))))

	guard := scim.RequireSCIMToken(svc)
	apiMux.Handle("GET /scim/v2/Users", guard(http.HandlerFunc(listSCIMUsersHandler(svc))))
	apiMux.Handle("POST /scim/v2/Users", guard(http.HandlerFunc(createSCIMUserHandler(svc))))
	apiMux.Handle("GET /scim/v2/Users/{id}", guard(http.HandlerFunc(getSCIMUserHandler(svc))))
	apiMux.Handle("PUT /scim/v2/Users/{id}", guard(http.HandlerFunc(replaceSCIMUserHandler(svc))))
	apiMux.Handle("PATCH /scim/v2/Users/{id}", guard(http.HandlerFunc(patchSCIMUserHandler(svc))))
}
