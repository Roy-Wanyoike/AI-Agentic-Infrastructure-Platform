package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/evaluations"
)

// Evaluations HTTP surface (wave 2, track 2-d).
//
// Contract endpoints (mounted on apiMux, served under BOTH /v1 and /api/v1):
//
//	GET  /eval-datasets                -> {"datasets": [...]}          (evaluations.read)
//	POST /eval-datasets/create         -> {"dataset": {...}}           (evaluations.write)
//	GET  /eval-datasets/{id}           -> {"dataset": {..., cases}}    (evaluations.read)
//	POST /eval-datasets/{id}/run       -> {"eval_run_id","status"}     (evaluations.write)
//	GET  /eval-runs/{id}               -> {"id",...,"results","summary"} (evaluations.read)
//	POST /eval-runs/compare            -> {"baseline","candidate","regressions","improvements"} (evaluations.write)
//
// JSON/error helpers use distinct eval-prefixed names to avoid clashing with
// the shared handlers.go helpers.

// registerEvaluationsRoutes mounts all evaluation dataset/run routes on apiMux.
func registerEvaluationsRoutes(apiMux *http.ServeMux, evalSvc *evaluations.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	authWrap := func(perm auth.Permission, next http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(next))
	}
	apiMux.Handle("/eval-datasets", authWrap(auth.PermissionEvalsRead, http.HandlerFunc(listEvalDatasetsHandler(evalSvc))))
	apiMux.Handle("/eval-datasets/create", authWrap(auth.PermissionEvalsWrite, http.HandlerFunc(createEvalDatasetHandler(evalSvc))))
	apiMux.Handle("/eval-datasets/", authWrap(auth.PermissionEvalsRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// POST /eval-datasets/{id}/run executes the dataset; everything else
		// under the subtree is the tenant-scoped detail read.
		rest := trimRoutePrefix(r.URL.Path, "/eval-datasets/")
		if strings.HasSuffix(rest, "/run") {
			auth.RequirePermission(authSvc, auth.PermissionEvalsWrite)(http.HandlerFunc(runEvalDatasetHandler(evalSvc))).ServeHTTP(w, r)
			return
		}
		getEvalDatasetHandler(evalSvc).ServeHTTP(w, r)
	})))
	apiMux.Handle("/eval-runs/compare", authWrap(auth.PermissionEvalsWrite, http.HandlerFunc(compareEvalRunsHandler(evalSvc))))
	apiMux.Handle("/eval-runs/", authWrap(auth.PermissionEvalsRead, http.HandlerFunc(getEvalRunHandler(evalSvc))))
}

func writeEvalJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// readEvalJSON decodes the request body; on failure it writes the error
// response itself and returns false so handlers can stop.
func readEvalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeEvalError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

func writeEvalError(w http.ResponseWriter, status int, code, message string) {
	writeEvalJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func evalRFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// writeEvalServiceError maps typed evaluations service errors to HTTP codes.
func writeEvalServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, evaluations.ErrDatasetNotFound),
		errors.Is(err, evaluations.ErrRunNotFound),
		errors.Is(err, evaluations.ErrAgentNotFound):
		writeEvalError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, evaluations.ErrRunnerNotConfigured):
		writeEvalError(w, http.StatusServiceUnavailable, "RUNNER_UNAVAILABLE", err.Error())
	default:
		writeEvalError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
}

// evalDatasetSummaryPayload renders the list-endpoint dataset shape:
// {"id","name","description","case_count","created_at"}.
func evalDatasetSummaryPayload(d *evaluations.Dataset) map[string]any {
	return map[string]any{
		"id":          d.ID,
		"name":        d.Name,
		"description": d.Description,
		"case_count":  d.CaseCount,
		"created_at":  evalRFC3339(d.CreatedAt),
	}
}

// evalDatasetPayload renders the detail/create dataset shape: the summary
// fields plus the full ordered case list.
func evalDatasetPayload(d *evaluations.Dataset) map[string]any {
	payload := evalDatasetSummaryPayload(d)
	cases := d.Cases
	if cases == nil {
		cases = []evaluations.Case{}
	}
	payload["cases"] = cases
	return payload
}

// evalRunPayload renders the contract run shape:
// {"id","dataset_id","agent_id","status","results":[...],"summary":{...}}.
func evalRunPayload(run *evaluations.EvalRun) map[string]any {
	results := run.Results
	if results == nil {
		results = []evaluations.Result{}
	}
	payload := map[string]any{
		"id":         run.ID,
		"dataset_id": run.DatasetID,
		"agent_id":   run.AgentID,
		"status":     run.Status,
		"results":    results,
	}
	if run.Summary != nil {
		payload["summary"] = run.Summary
	} else {
		payload["summary"] = map[string]any{
			"pass_rate": 0, "avg_latency_ms": 0, "total_cost_cents": 0, "by_scorer": map[string]any{},
		}
	}
	return payload
}

func listEvalDatasetsHandler(evalSvc *evaluations.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if evalSvc == nil {
			writeEvalError(w, http.StatusServiceUnavailable, "EVALUATIONS_UNAVAILABLE", "evaluations service not available")
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: the listing filters on organization_id.
		datasets, err := evalSvc.ListDatasets(r.Context(), orgID)
		if err != nil {
			writeEvalError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		items := make([]map[string]any, 0, len(datasets))
		for _, d := range datasets {
			items = append(items, evalDatasetSummaryPayload(d))
		}
		writeEvalJSON(w, http.StatusOK, map[string]any{"datasets": items})
	}
}

func createEvalDatasetHandler(evalSvc *evaluations.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if evalSvc == nil {
			writeEvalError(w, http.StatusServiceUnavailable, "EVALUATIONS_UNAVAILABLE", "evaluations service not available")
			return
		}
		var req struct {
			OrganizationID string             `json:"organization_id"`
			Name           string             `json:"name"`
			Description    string             `json:"description"`
			Cases          []evaluations.Case `json:"cases"`
		}
		if !readEvalJSON(w, r, &req) {
			return
		}
		// Never trust a client-supplied organization_id; when present it is
		// only honored after the tenant-access check passes.
		orgID, ok := claimsOrganizationID(w, r, req.OrganizationID)
		if !ok {
			return
		}
		dataset, err := evalSvc.CreateDataset(r.Context(), orgID, req.Name, req.Description, req.Cases)
		if err != nil {
			writeEvalError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		writeEvalJSON(w, http.StatusCreated, map[string]any{"dataset": evalDatasetPayload(dataset)})
	}
}

func getEvalDatasetHandler(evalSvc *evaluations.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if evalSvc == nil {
			writeEvalError(w, http.StatusServiceUnavailable, "EVALUATIONS_UNAVAILABLE", "evaluations service not available")
			return
		}
		datasetID := trimRoutePrefix(r.URL.Path, "/eval-datasets/")
		if datasetID == "" || strings.Contains(datasetID, "/") {
			writeEvalError(w, http.StatusNotFound, "NOT_FOUND", "eval dataset not found")
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: foreign datasets surface as 404.
		dataset, err := evalSvc.GetDataset(r.Context(), orgID, datasetID)
		if err != nil {
			writeEvalServiceError(w, err)
			return
		}
		writeEvalJSON(w, http.StatusOK, map[string]any{"dataset": evalDatasetPayload(dataset)})
	}
}

// runEvalDatasetHandler executes every case of the dataset synchronously
// (bounded: max 50 cases, 30s per case) and returns the completed run id.
func runEvalDatasetHandler(evalSvc *evaluations.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if evalSvc == nil {
			writeEvalError(w, http.StatusServiceUnavailable, "EVALUATIONS_UNAVAILABLE", "evaluations service not available")
			return
		}
		datasetID := strings.TrimSuffix(trimRoutePrefix(r.URL.Path, "/eval-datasets/"), "/run")
		if datasetID == "" || strings.Contains(datasetID, "/") {
			writeEvalError(w, http.StatusNotFound, "NOT_FOUND", "eval dataset not found")
			return
		}
		var req struct {
			OrganizationID string `json:"organization_id"`
			AgentID        string `json:"agent_id"`
		}
		if !readEvalJSON(w, r, &req) {
			return
		}
		orgID, ok := claimsOrganizationID(w, r, req.OrganizationID)
		if !ok {
			return
		}
		if strings.TrimSpace(req.AgentID) == "" {
			writeEvalError(w, http.StatusBadRequest, "VALIDATION_ERROR", "agent_id is required")
			return
		}
		run, err := evalSvc.RunDataset(r.Context(), orgID, datasetID, req.AgentID)
		if err != nil {
			writeEvalServiceError(w, err)
			return
		}
		writeEvalJSON(w, http.StatusOK, map[string]any{"eval_run_id": run.ID, "status": run.Status})
	}
}

func getEvalRunHandler(evalSvc *evaluations.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if evalSvc == nil {
			writeEvalError(w, http.StatusServiceUnavailable, "EVALUATIONS_UNAVAILABLE", "evaluations service not available")
			return
		}
		runID := trimRoutePrefix(r.URL.Path, "/eval-runs/")
		if runID == "" || strings.Contains(runID, "/") {
			writeEvalError(w, http.StatusNotFound, "NOT_FOUND", "eval run not found")
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: foreign runs surface as 404.
		run, err := evalSvc.GetRun(r.Context(), orgID, runID)
		if err != nil {
			writeEvalServiceError(w, err)
			return
		}
		writeEvalJSON(w, http.StatusOK, evalRunPayload(run))
	}
}

func compareEvalRunsHandler(evalSvc *evaluations.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if evalSvc == nil {
			writeEvalError(w, http.StatusServiceUnavailable, "EVALUATIONS_UNAVAILABLE", "evaluations service not available")
			return
		}
		var req struct {
			BaselineRunID  string `json:"baseline_run_id"`
			CandidateRunID string `json:"candidate_run_id"`
		}
		if !readEvalJSON(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.BaselineRunID) == "" || strings.TrimSpace(req.CandidateRunID) == "" {
			writeEvalError(w, http.StatusBadRequest, "VALIDATION_ERROR", "baseline_run_id and candidate_run_id are required")
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: both runs are resolved within the caller's organization.
		comparison, err := evalSvc.CompareRuns(r.Context(), orgID, req.BaselineRunID, req.CandidateRunID)
		if err != nil {
			writeEvalServiceError(w, err)
			return
		}
		writeEvalJSON(w, http.StatusOK, comparison)
	}
}
