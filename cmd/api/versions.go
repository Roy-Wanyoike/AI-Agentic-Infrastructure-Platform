package main

// Track 2-b (agent versions + deployments) HTTP handlers.
//
// Endpoints (registered on apiMux by registerVersionsRoutes /
// registerDeploymentsRoutes; served under BOTH /v1 and /api/v1):
//
//	GET    /agents/{id}/versions                   -> agents.read
//	POST   /agents/{id}/versions/create            -> agents.write
//	POST   /agents/{id}/versions/{version}/publish -> agents.write
//	POST   /agents/{id}/rollback                   -> agents.write
//	GET    /deployments?agent_id=                  -> deployments.read
//	POST   /deployments/create                     -> deployments.write
//	GET    /deployments/{id}                       -> deployments.read
//	POST   /deployments/{id}/promote               -> deployments.deploy
//	POST   /deployments/{id}/rollback              -> deployments.deploy
//
// The tenant is taken from the auth claims only; client-supplied organization
// ids are never trusted. Error bodies use the shared
// {"error":{"code","message"}} envelope via the local writeErrorVD helper.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/deployments"
)

// writeJSONVD renders a JSON response with the given status code (local helper,
// distinct name avoids collisions with other tracks' helpers in package main).
func writeJSONVD(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeErrorVD renders the shared structured error envelope.
func writeErrorVD(w http.ResponseWriter, status int, code, message string) {
	writeJSONVD(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// readJSONVD decodes a JSON request body, writing 400 on malformed input.
func readJSONVD(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeErrorVD(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// claimsOrgIDVD resolves the caller's tenant from the auth context.
func claimsOrgIDVD(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeErrorVD(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return "", false
	}
	if strings.TrimSpace(claims.OrganizationID) == "" {
		writeErrorVD(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing organization claim")
		return "", false
	}
	return claims.OrganizationID, true
}

// agentVersionView is the wire shape pinned by the wave-2 contract:
// {"version","snapshot","published_at","published_by","status"}. Snapshot is
// emitted as an embedded JSON object (it IS the agent config document).
type agentVersionView struct {
	Version     int             `json:"version"`
	Snapshot    json.RawMessage `json:"snapshot"`
	PublishedAt *time.Time      `json:"published_at"`
	PublishedBy string          `json:"published_by"`
	Status      string          `json:"status"`
}

func newAgentVersionView(version *agents.ConfigVersion) agentVersionView {
	snapshot := json.RawMessage(version.Snapshot)
	if len(snapshot) == 0 || !json.Valid(snapshot) {
		snapshot = json.RawMessage("{}")
	}
	return agentVersionView{
		Version:     version.Version,
		Snapshot:    snapshot,
		PublishedAt: version.PublishedAt,
		PublishedBy: version.PublishedBy,
		Status:      version.Status,
	}
}

// writeAgentVersionsError maps agents service errors onto the contract's error
// envelope; returns true when the error was handled.
func writeAgentVersionsError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, agents.ErrAgentNotFound):
		writeErrorVD(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
	case errors.Is(err, agents.ErrVersionNotFound):
		writeErrorVD(w, http.StatusNotFound, "VERSION_NOT_FOUND", "agent version not found")
	case errors.Is(err, agents.ErrVersionArchived):
		writeErrorVD(w, http.StatusConflict, "VERSION_ARCHIVED", "archived versions are revived through rollback only")
	default:
		return false
	}
	return true
}

// listAgentVersionsHandler serves GET /agents/{id}/versions.
func listAgentVersionsHandler(versionsSvc *agents.VersionsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if strings.TrimSpace(agentID) == "" {
			writeErrorVD(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		// Tenant guard: the service scopes the lookup by organization_id.
		list, err := versionsSvc.ListVersionsCtx(r.Context(), orgID, agentID)
		if err != nil {
			if !writeAgentVersionsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		views := make([]agentVersionView, 0, len(list))
		for _, version := range list {
			views = append(views, newAgentVersionView(version))
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"versions": views})
	}
}

// createAgentVersionHandler serves POST /agents/{id}/versions/create: snapshots
// the agent's current configuration into a new draft version.
func createAgentVersionHandler(versionsSvc *agents.VersionsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if strings.TrimSpace(agentID) == "" {
			writeErrorVD(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		claims, _ := auth.ExtractClaims(r.Context())
		// Tenant guard: the snapshot is created within the caller's tenant.
		version, err := versionsSvc.CreateVersionCtx(r.Context(), orgID, agentID, claims.UserID)
		if err != nil {
			if !writeAgentVersionsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusCreated, map[string]any{"version": version.Version})
	}
}

// publishAgentVersionHandler serves POST /agents/{id}/versions/{version}/publish:
// marks a draft version immutable (published).
func publishAgentVersionHandler(versionsSvc *agents.VersionsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		version, err := strconv.Atoi(r.PathValue("version"))
		if strings.TrimSpace(agentID) == "" || err != nil || version <= 0 {
			writeErrorVD(w, http.StatusBadRequest, "INVALID_REQUEST", "version must be a positive integer")
			return
		}
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		claims, _ := auth.ExtractClaims(r.Context())
		published, err := versionsSvc.PublishVersionCtx(r.Context(), orgID, agentID, version, claims.UserID)
		if err != nil {
			if !writeAgentVersionsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"version": published.Version})
	}
}

// rollbackAgentHandler serves POST /agents/{id}/rollback with body
// {"target_version": 2}: re-points the agent to the target version and restores
// its live configuration from the target snapshot.
func rollbackAgentHandler(versionsSvc *agents.VersionsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if strings.TrimSpace(agentID) == "" {
			writeErrorVD(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		var req struct {
			TargetVersion int `json:"target_version"`
		}
		if !readJSONVD(w, r, &req) {
			return
		}
		if req.TargetVersion <= 0 {
			writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "target_version must be a positive integer")
			return
		}
		claims, _ := auth.ExtractClaims(r.Context())
		target, err := versionsSvc.RollbackVersionCtx(r.Context(), orgID, agentID, req.TargetVersion, claims.UserID)
		if err != nil {
			if !writeAgentVersionsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"current_version": target.Version})
	}
}

// deploymentView is the wire shape pinned by the wave-2 contract:
// {"id","agent_id","version","environment","status","health","created_at",
// "updated_at"} (created_by/superseded_at stay server-side; health carries the
// contract's error_rate/last_check_at plus omitempty failure markers).
type deploymentView struct {
	ID          string              `json:"id"`
	AgentID     string              `json:"agent_id"`
	Version     int                 `json:"version"`
	Environment string              `json:"environment"`
	Status      string              `json:"status"`
	Health      *deployments.Health `json:"health"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

func newDeploymentView(deployment *deployments.Deployment) deploymentView {
	return deploymentView{
		ID:          deployment.ID,
		AgentID:     deployment.AgentID,
		Version:     deployment.Version,
		Environment: deployment.Environment,
		Status:      deployment.Status,
		Health:      deployment.Health,
		CreatedAt:   deployment.CreatedAt,
		UpdatedAt:   deployment.UpdatedAt,
	}
}

// writeDeploymentsError maps deployments service errors onto the contract's
// error envelope; returns true when the error was handled.
func writeDeploymentsError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, deployments.ErrDeploymentNotFound):
		writeErrorVD(w, http.StatusNotFound, "DEPLOYMENT_NOT_FOUND", "deployment not found")
	case errors.Is(err, deployments.ErrInvalidTransition):
		writeErrorVD(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, deployments.ErrNoPreviousHealthy):
		writeErrorVD(w, http.StatusConflict, "NO_PREVIOUS_HEALTHY", "no previous healthy deployment to roll back to")
	case errors.Is(err, deployments.ErrVersionNotDeployable):
		writeErrorVD(w, http.StatusUnprocessableEntity, "VERSION_NOT_PUBLISHED", "agent version must exist and be published to create a deployment")
	case errors.Is(err, deployments.ErrInvalidEnvironment):
		writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "environment must be one of development|staging|production")
	default:
		return false
	}
	return true
}

// listDeploymentsHandler serves GET /deployments?agent_id= (agent_id optional).
func listDeploymentsHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		// Tenant guard: the listing filters on organization_id (agent_id optional).
		list, err := depSvc.ListDeploymentsCtx(r.Context(), orgID, r.URL.Query().Get("agent_id"))
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		views := make([]deploymentView, 0, len(list))
		for _, deployment := range list {
			views = append(views, newDeploymentView(deployment))
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"deployments": views})
	}
}

// createDeploymentHandler serves POST /deployments/create with body
// {"agent_id","version","environment"}: validates the target version exists and
// is published, then creates a deployment in status requested.
func createDeploymentHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		var req struct {
			AgentID     string `json:"agent_id"`
			Version     int    `json:"version"`
			Environment string `json:"environment"`
		}
		if !readJSONVD(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.AgentID) == "" {
			writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "agent_id is required")
			return
		}
		if req.Version <= 0 {
			writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "version must be a positive integer")
			return
		}
		if strings.TrimSpace(req.Environment) == "" {
			writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "environment is required")
			return
		}
		claims, _ := auth.ExtractClaims(r.Context())
		// Tenant guard: the deployment row is created with the caller's org.
		deployment, err := depSvc.CreateDeploymentCtx(r.Context(), orgID, req.AgentID, req.Version, req.Environment, claims.UserID)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusCreated, map[string]any{"deployment": newDeploymentView(deployment)})
	}
}

// getDeploymentHandler serves GET /deployments/{id}.
func getDeploymentHandler(depSvc *deployments.Service) http.HandlerFunc {
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
		// Tenant guard: foreign-tenant rows surface as 404.
		deployment, err := depSvc.GetDeploymentCtx(r.Context(), orgID, id)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"deployment": newDeploymentView(deployment)})
	}
}

// promoteDeploymentHandler serves POST /deployments/{id}/promote: advances the
// lifecycle one step (requested -> validated -> deploying -> healthy).
func promoteDeploymentHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		deployment, err := depSvc.PromoteDeploymentCtx(r.Context(), orgID, id)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"deployment": newDeploymentView(deployment)})
	}
}

// rollbackDeploymentHandler serves POST /deployments/{id}/rollback: re-points
// the environment to the previous healthy deployment's version.
func rollbackDeploymentHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		claims, _ := auth.ExtractClaims(r.Context())
		deployment, version, err := depSvc.RollbackDeploymentCtx(r.Context(), orgID, id, claims.UserID)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{
			"deployment":             newDeploymentView(deployment),
			"rolled_back_to_version": version,
		})
	}
}

// registerVersionsRoutes mounts the agent config-version routes on apiMux.
// Versions are agent configuration, so they reuse the existing agents.* grants
// (the wave-2 contract pins no dedicated versions permission).
func registerVersionsRoutes(apiMux *http.ServeMux, versionsSvc *agents.VersionsService, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	wrap := func(perm auth.Permission, h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	apiMux.Handle("GET /agents/{id}/versions", wrap(auth.PermissionAgentsRead, listAgentVersionsHandler(versionsSvc)))
	apiMux.Handle("POST /agents/{id}/versions/create", wrap(auth.PermissionAgentsWrite, createAgentVersionHandler(versionsSvc)))
	apiMux.Handle("POST /agents/{id}/versions/{version}/publish", wrap(auth.PermissionAgentsWrite, publishAgentVersionHandler(versionsSvc)))
	apiMux.Handle("POST /agents/{id}/rollback", wrap(auth.PermissionAgentsWrite, rollbackAgentHandler(versionsSvc)))
}

// registerDeploymentsRoutes mounts the deployment lifecycle routes on apiMux.
// Permission mapping (contract RBAC table):
//   - deployments.read   -> GET /deployments, GET /deployments/{id} (all roles)
//   - deployments.write  -> POST /deployments/create (OWNER/ADMIN/MEMBER: request a deployment)
//   - deployments.deploy -> promote + rollback (OWNER/ADMIN: change what serves traffic)
func registerDeploymentsRoutes(apiMux *http.ServeMux, depSvc *deployments.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	wrap := func(perm auth.Permission, h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	apiMux.Handle("GET /deployments", wrap(auth.PermissionDeploymentsRead, listDeploymentsHandler(depSvc)))
	apiMux.Handle("POST /deployments/create", wrap(auth.PermissionDeploymentsWrite, createDeploymentHandler(depSvc)))
	apiMux.Handle("GET /deployments/{id}", wrap(auth.PermissionDeploymentsRead, getDeploymentHandler(depSvc)))
	apiMux.Handle("POST /deployments/{id}/promote", wrap(auth.PermissionDeploymentsDeploy, promoteDeploymentHandler(depSvc)))
	apiMux.Handle("POST /deployments/{id}/rollback", wrap(auth.PermissionDeploymentsDeploy, rollbackDeploymentHandler(depSvc)))
}
