package main

// Issue #29 (wave 7-b) HTTP handlers — SCIM 2.0 half.
//
// Endpoints (registered on apiMux by registerScimRoutes; served under BOTH
// /v1 and /api/v1):
//
//	POST /scim/tokens              -> mint a SCIM bearer credential
//	                                  (session/API-key auth + organization.manage
//	                                  = OWNER only); plaintext shown ONCE
//	GET   /scim/v2/Users?filter=   -> SCIM 2.0 ListResponse
//	                                  (filter=userName eq "..." supported)
//	POST  /scim/v2/Users           -> JIT-provision one identity (201)
//	GET   /scim/v2/Users/{id}      -> point read (404 without existence leak)
//	PUT   /scim/v2/Users/{id}      -> full replace (userName immutable)
//	PATCH /scim/v2/Users/{id}      -> replace active (deprovisioning)
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

// registerScimRoutes mounts the token-minting route (platform OWNER surface)
// and the SCIM 2.0 protocol endpoints (dedicated bearer-token surface).
func registerScimRoutes(apiMux *http.ServeMux, svc *scim.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	apiMux.Handle("POST /scim/tokens", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionOrgManage)(http.HandlerFunc(createSCIMTokenHandler(svc)))))

	guard := scim.RequireSCIMToken(svc)
	apiMux.Handle("GET /scim/v2/Users", guard(http.HandlerFunc(listSCIMUsersHandler(svc))))
	apiMux.Handle("POST /scim/v2/Users", guard(http.HandlerFunc(createSCIMUserHandler(svc))))
	apiMux.Handle("GET /scim/v2/Users/{id}", guard(http.HandlerFunc(getSCIMUserHandler(svc))))
	apiMux.Handle("PUT /scim/v2/Users/{id}", guard(http.HandlerFunc(replaceSCIMUserHandler(svc))))
	apiMux.Handle("PATCH /scim/v2/Users/{id}", guard(http.HandlerFunc(patchSCIMUserHandler(svc))))
}
