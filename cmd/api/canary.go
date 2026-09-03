package main

// Canary deployments HTTP handlers (issue #13). These routes change WHAT
// SERVES TRAFFIC for an agent+environment, so every one of them requires the
// strictest existing deployment permission (deployments.deploy -> OWNER/ADMIN;
// no new permission enum was introduced - see internal/auth/
// permissions_deployments.go).
//
// Endpoints (registered on apiMux by registerCanaryRoutes, called from
// registerDeploymentsRoutes; served under BOTH /v1 and /api/v1):
//
//      POST /deployments/{id}/canary          -> deployments.deploy
//      POST /deployments/{id}/canary/promote  -> deployments.deploy
//      POST /deployments/{id}/canary/abort    -> deployments.deploy
//
// The tenant is taken from the auth claims only; error bodies use the shared
// {"error":{"code","message"}} envelope via writeErrorVD. Semantics:
//
//      canary         attach/replace the canary version and/or move the split
//                     point (omitted fields keep their current value)
//      canary/promote canary becomes stable (version swap, canary cleared)
//      canary/abort   clear the canary, keep the stable version

import (
	"net/http"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/deployments"
)

// setDeploymentCanaryHandler serves POST /deployments/{id}/canary with body
// {"canary_version": int (optional), "canary_weight": int (optional)}. At
// least one field must be provided. canary_version attaches/replaces the
// canary (must be an EXISTING version of the SAME agent - any publication
// status, because only one version can be published at a time and a canary
// by definition runs NEXT to the stable version - differing from the stable
// version); canary_weight moves the 0-100 split point (requires an attached
// canary; out-of-range values are rejected, not clamped).
func setDeploymentCanaryHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if strings.TrimSpace(id) == "" {
			writeErrorVD(w, http.StatusNotFound, "DEPLOYMENT_NOT_FOUND", "deployment not found")
			return
		}
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		var req struct {
			CanaryVersion *int `json:"canary_version"`
			CanaryWeight  *int `json:"canary_weight"`
		}
		if !readJSONVD(w, r, &req) {
			return
		}
		if req.CanaryVersion == nil && req.CanaryWeight == nil {
			writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "canary_version or canary_weight is required")
			return
		}
		// Tenant guard: the service scopes every lookup by organization_id.
		deployment, err := depSvc.GetDeploymentCtx(r.Context(), orgID, id)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		if req.CanaryVersion != nil {
			deployment, err = depSvc.SetCanaryVersionCtx(r.Context(), orgID, id, *req.CanaryVersion)
			if err != nil {
				if !writeDeploymentsError(w, err) {
					writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
		}
		if req.CanaryWeight != nil {
			deployment, err = depSvc.SetCanaryWeightCtx(r.Context(), orgID, id, *req.CanaryWeight)
			if err != nil {
				if !writeDeploymentsError(w, err) {
					writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"deployment": newDeploymentView(deployment)})
	}
}

// promoteCanaryHandler serves POST /deployments/{id}/canary/promote: the
// canary becomes the stable version (deployment.Version swaps to the canary
// version and the canary config is cleared; the row stays healthy).
func promoteCanaryHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		deployment, err := depSvc.PromoteCanaryCtx(r.Context(), orgID, id)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"deployment": newDeploymentView(deployment)})
	}
}

// abortCanaryHandler serves POST /deployments/{id}/canary/abort: clears the
// canary config and keeps the stable version serving 100% of traffic.
func abortCanaryHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		deployment, err := depSvc.AbortCanaryCtx(r.Context(), orgID, id)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"deployment": newDeploymentView(deployment)})
	}
}

// registerCanaryRoutes mounts the canary traffic-split routes on apiMux.
// Permission mapping: deployments.deploy (OWNER/ADMIN) - these operations
// change what serves traffic, exactly like promote/rollback. Called from
// registerDeploymentsRoutes so the whole deployment surface stays registered
// in one place.
func registerCanaryRoutes(apiMux *http.ServeMux, depSvc *deployments.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	wrap := func(perm auth.Permission, h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	// ServeMux precedence: the /promote and /abort patterns are more
	// specific than /canary and win under real wiring.
	apiMux.Handle("POST /deployments/{id}/canary", wrap(auth.PermissionDeploymentsDeploy, setDeploymentCanaryHandler(depSvc)))
	apiMux.Handle("POST /deployments/{id}/canary/promote", wrap(auth.PermissionDeploymentsDeploy, promoteCanaryHandler(depSvc)))
	apiMux.Handle("POST /deployments/{id}/canary/abort", wrap(auth.PermissionDeploymentsDeploy, abortCanaryHandler(depSvc)))
}
