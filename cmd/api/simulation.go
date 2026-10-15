package main

// simulation.go — issue #133 (dual-platform simulation engine) HTTP wiring:
// cross-platform agent behavior modeling on top of internal/simulation.
//
// Endpoints (registered on apiMux by registerSimulationRoutes; served under
// BOTH /v1 and /api/v1 like every versioned surface):
//
//	POST /simulations        create + run ASYNC -> 202 with the queued record
//	GET  /simulations        list the org's simulations (newest first)
//	GET  /simulations/{id}   fetch one record (config + status + result)
//
// Design (mirrors cmd/api/synth.go / orchestration.go):
//   - RBAC uses the issue #133 grants (internal/auth/permissions_simulation.go):
//     reads are simulation.read (all roles — transcripts and emergence
//     metrics are observability surfaces); writes are simulation.write
//     (OWNER/ADMIN/MEMBER — bounded compute: max 500 rounds × 64 agents).
//   - Tenant scope comes EXCLUSIVELY from the auth claims: the request body
//     cannot name an organization (any client-supplied org_id is force-
//     stamped over by the service), so a caller can never create into, or
//     read across, a foreign tenant (cross-org reads 404 one layer down).
//   - POST is ASYNC by contract (issue #133): the request path validates
//     (fail fast 422) and returns the queued record immediately; execution
//     happens on a detached goroutine (context.WithoutCancel) and its
//     terminal status lands on the record (GET /simulations/{id}).
//   - Errors use the house envelope {"error":{"code","message"}}; unknown
//     and foreign ids are the same 404 (no existence leak).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/simulation"
)

// registerSimulationRoutes mounts the simulation surface.
func registerSimulationRoutes(apiMux *http.ServeMux, svc *simulation.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go / synth.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	write := func(h http.HandlerFunc) http.Handler { return wrap(auth.PermissionSimulationWrite, h) }
	read := func(h http.HandlerFunc) http.Handler { return wrap(auth.PermissionSimulationRead, h) }

	apiMux.Handle("POST /simulations", write(simulationCreateHandler(svc)))
	apiMux.Handle("GET /simulations", read(simulationListHandler(svc)))
	apiMux.Handle("GET /simulations/{id}", read(simulationGetHandler(svc)))
}

// --- house envelope helpers (distinct names; same shape as synth.go) -------

func writeJSONSimulation(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErrorSimulation(w http.ResponseWriter, status int, code, message string) {
	writeJSONSimulation(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// decodeSimulationBody decodes the JSON request body bounded to 1 MiB.
// Returns false when the response was already written.
func decodeSimulationBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	defer r.Body.Close() //nolint:errcheck // read path; nothing actionable
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErrorSimulation(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")
		return false
	}
	if len(raw) == 0 {
		writeErrorSimulation(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "request body is required")
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeErrorSimulation(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "invalid JSON body")
		return false
	}
	return true
}

// simulationClaims extracts the caller claims or writes the 401/404 shape,
// returning ok=false when the response was already written.
func simulationClaims(w http.ResponseWriter, r *http.Request) (auth.UserClaims, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeErrorSimulation(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return auth.UserClaims{}, false
	}
	if claims.OrganizationID == "" {
		writeErrorSimulation(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
		return auth.UserClaims{}, false
	}
	return claims, true
}

// mapSimulationError renders the service error vocabulary onto the house
// envelope. Cross-org/unknown ids are the same 404 (no existence leak).
func mapSimulationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, simulation.ErrNotFound):
		writeErrorSimulation(w, http.StatusNotFound, "NOT_FOUND", "simulation not found")
	case errors.Is(err, simulation.ErrInvalid):
		writeErrorSimulation(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	default:
		writeErrorSimulation(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// simulationCreateHandler: POST /simulations — validate + queue + run async.
func simulationCreateHandler(svc *simulation.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := simulationClaims(w, r)
		if !ok {
			return
		}
		var cfg simulation.SimConfig
		if !decodeSimulationBody(w, r, &cfg) {
			return
		}
		sim, err := svc.Create(r.Context(), claims.OrganizationID, cfg)
		if err != nil {
			mapSimulationError(w, err)
			return
		}
		// 202 Accepted: execution is in progress on a detached goroutine.
		writeJSONSimulation(w, http.StatusAccepted, map[string]any{"simulation": sim})
	}
}

// simulationListHandler: GET /simulations — org-scoped, newest first.
func simulationListHandler(svc *simulation.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := simulationClaims(w, r)
		if !ok {
			return
		}
		sims, err := svc.List(claims.OrganizationID)
		if err != nil {
			mapSimulationError(w, err)
			return
		}
		if sims == nil {
			sims = []*simulation.Simulation{}
		}
		writeJSONSimulation(w, http.StatusOK, map[string]any{"simulations": sims, "count": len(sims)})
	}
}

// simulationGetHandler: GET /simulations/{id} — config + status + result.
func simulationGetHandler(svc *simulation.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := simulationClaims(w, r)
		if !ok {
			return
		}
		sim, err := svc.Get(claims.OrganizationID, r.PathValue("id"))
		if err != nil {
			mapSimulationError(w, err)
			return
		}
		writeJSONSimulation(w, http.StatusOK, map[string]any{"simulation": sim})
	}
}
