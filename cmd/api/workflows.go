package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/workflows"
)

// registerWorkflowsRoutes mounts every wave-2 track 2-a endpoint on apiMux:
// workflows CRUD + publish + execute, workflow runs, approvals and run
// control (cancel/pause/resume). Routes use Go 1.22 method+path patterns, so
// the run-control routes (POST /runs/{id}/cancel|pause|resume) take
// precedence over the legacy /runs/ subtree handler registered in main.go.
//
// Every route is wrapped with RequireAuthOrAPIKey -> RequirePermission using
// the pinned 2-a permission constants; the tenant (organization_id) is always
// taken from the authenticated claims, never from client input.
//
// queueSvc is accepted for signature parity with the wiring contract (the
// execution engine already carries it); when runsSvc/queueSvc are present the
// engine and run controller are wired here so the orchestrator does not have to.
func registerWorkflowsRoutes(apiMux *http.ServeMux, wfSvc *workflows.Service, apSvc *approvals.Service, runsSvc *runs.Service, queueSvc *queue.Queue, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	_ = queueSvc // carried by workflows.Engine; kept in the signature per the wiring convention.
	if wfSvc != nil && runsSvc != nil && queueSvc != nil {
		wfSvc.SetEngine(workflows.Engine{Runs: runsSvc, Queue: queueSvc, Approvals: apSvc})
	}
	if apSvc != nil && runsSvc != nil {
		apSvc.SetRunController(runsSvc)
	}

	wf := func(perm auth.Permission, h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("GET /workflows", wf(auth.PermissionWorkflowsRead, listWorkflowsHandler(wfSvc)))
	apiMux.Handle("POST /workflows/create", wf(auth.PermissionWorkflowsWrite, createWorkflowHandler(wfSvc)))
	apiMux.Handle("GET /workflows/{id}", wf(auth.PermissionWorkflowsRead, getWorkflowHandler(wfSvc)))
	apiMux.Handle("POST /workflows/{id}/validate", wf(auth.PermissionWorkflowsRead, validateWorkflowHandler(wfSvc)))
	apiMux.Handle("POST /workflows/{id}/publish", wf(auth.PermissionWorkflowsWrite, publishWorkflowHandler(wfSvc)))
	apiMux.Handle("POST /workflows/{id}/execute", wf(auth.PermissionWorkflowsExecute, executeWorkflowHandler(wfSvc)))
	apiMux.Handle("GET /workflow-runs/{id}", wf(auth.PermissionWorkflowsRead, getWorkflowRunHandler(wfSvc)))

	apiMux.Handle("GET /approvals", wf(auth.PermissionApprovalsRead, listApprovalsHandler(apSvc)))
	apiMux.Handle("GET /approvals/{id}", wf(auth.PermissionApprovalsRead, getApprovalHandler(apSvc)))
	apiMux.Handle("POST /approvals/{id}/decide", wf(auth.PermissionApprovalsDecide, decideApprovalHandler(apSvc)))

	apiMux.Handle("POST /runs/{id}/cancel", wf(auth.PermissionRunsControl, runControlHandler(runsSvc, "cancel")))
	apiMux.Handle("POST /runs/{id}/pause", wf(auth.PermissionRunsControl, runControlHandler(runsSvc, "pause")))
	apiMux.Handle("POST /runs/{id}/resume", wf(auth.PermissionRunsControl, runControlHandler(runsSvc, "resume")))
}

// ---------------------------------------------------------------------------
// Local JSON helpers. Distinct names (…WF suffix) avoid redeclaration with the
// other cmd/api route files (policies.go, evaluations.go, ...).
// ---------------------------------------------------------------------------

func writeJSONWF(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type wfErrorBody struct {
	Error wfErrorDetail `json:"error"`
}

type wfErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeErrorWF(w http.ResponseWriter, status int, code, message string) {
	writeJSONWF(w, status, wfErrorBody{Error: wfErrorDetail{Code: code, Message: message}})
}

func readJSONWF(r *http.Request, dst any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}

// wfPathID extracts the {id} wildcard; requests that did not match a wildcard
// pattern (direct handler invocation) yield "" and fail the handler with 404.
func wfPathID(r *http.Request) string {
	return r.PathValue("id")
}

// wfClaims resolves the authenticated tenant; every handler scopes its service
// calls with this organization_id.
func wfClaims(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeErrorWF(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return "", "", false
	}
	return claims.OrganizationID, claims.UserID, true
}

// ---------------------------------------------------------------------------
// Response views (contract-pinned shapes; the engine structs carry a few more
// fields that are trimmed away here).
// ---------------------------------------------------------------------------

type wfSummaryView struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description,omitempty"`
	Status         string    `json:"status"`
	CurrentVersion int       `json:"current_version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func wfSummaryViewOf(wf *workflows.Workflow) wfSummaryView {
	if wf == nil {
		return wfSummaryView{}
	}
	return wfSummaryView{
		ID:             wf.ID,
		Name:           wf.Name,
		Description:    wf.Description,
		Status:         wf.Status,
		CurrentVersion: wf.CurrentVersion,
		CreatedAt:      wf.CreatedAt,
		UpdatedAt:      wf.UpdatedAt,
	}
}

type wfVersionView struct {
	Version   int           `json:"version"`
	Status    string        `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	DSLSnap   workflows.DSL `json:"dsl_snapshot"`
}

func wfVersionViewOf(v *workflows.Version) wfVersionView {
	if v == nil {
		return wfVersionView{}
	}
	return wfVersionView{
		Version:   v.Version,
		Status:    v.Status,
		CreatedAt: v.CreatedAt,
		DSLSnap:   v.DSL,
	}
}

type wfDetailView struct {
	wfSummaryView
	DSL      workflows.DSL   `json:"dsl"`
	Versions []wfVersionView `json:"versions,omitempty"`
}

// ---------------------------------------------------------------------------
// Workflow handlers
// ---------------------------------------------------------------------------

func listWorkflowsHandler(wfSvc *workflows.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		list, err := wfSvc.ListWorkflows(r.Context(), orgID)
		if err != nil {
			writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		items := make([]wfSummaryView, 0, len(list))
		for _, item := range list {
			items = append(items, wfSummaryViewOf(item))
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"workflows": items})
	}
}

func createWorkflowHandler(wfSvc *workflows.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name        string        `json:"name"`
			Description string        `json:"description"`
			DSL         workflows.DSL `json:"dsl"`
		}
		if err := readJSONWF(r, &req); err != nil {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		// Tenant guard: the workflow is created with the caller's organization_id
		// (any client-supplied organization_id is ignored).
		wf, err := wfSvc.CreateWorkflow(r.Context(), orgID, req.Name, req.Description, req.DSL)
		if err != nil {
			var verrs *workflows.ValidationErrors
			switch {
			case errors.As(err, &verrs):
				writeJSONWF(w, http.StatusUnprocessableEntity, map[string]any{"errors": verrs.Errors})
			case errors.Is(err, workflows.ErrWorkflowNameRequired):
				writeErrorWF(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			default:
				writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONWF(w, http.StatusCreated, map[string]any{
			"workflow": wfDetailView{wfSummaryViewOf(wf), wf.DSL, nil},
		})
	}
}

func getWorkflowHandler(wfSvc *workflows.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "workflow not found")
			return
		}
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		wf, err := wfSvc.GetWorkflow(r.Context(), orgID, id)
		if err != nil {
			writeWorkflowErrorWF(w, err)
			return
		}
		versions, err := wfSvc.GetVersions(r.Context(), orgID, id)
		if err != nil {
			writeWorkflowErrorWF(w, err)
			return
		}
		versionViews := make([]wfVersionView, 0, len(versions))
		for _, v := range versions {
			versionViews = append(versionViews, wfVersionViewOf(v))
		}
		writeJSONWF(w, http.StatusOK, map[string]any{
			"workflow": wfDetailView{wfSummaryViewOf(wf), wf.DSL, versionViews},
		})
	}
}

func validateWorkflowHandler(wfSvc *workflows.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "workflow not found")
			return
		}
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		verrs, err := wfSvc.ValidateWorkflow(r.Context(), orgID, id)
		if err != nil {
			writeWorkflowErrorWF(w, err)
			return
		}
		if len(verrs) > 0 {
			writeJSONWF(w, http.StatusUnprocessableEntity, map[string]any{"errors": verrs})
			return
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"valid": true})
	}
}

func publishWorkflowHandler(wfSvc *workflows.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "workflow not found")
			return
		}
		orgID, userID, ok := wfClaims(w, r)
		if !ok {
			return
		}
		wf, version, err := wfSvc.Publish(r.Context(), orgID, id, userID)
		if err != nil {
			writeWorkflowErrorWF(w, err)
			return
		}
		writeJSONWF(w, http.StatusOK, map[string]any{
			"workflow": wfSummaryViewOf(wf),
			"version":  wfVersionViewOf(version),
		})
	}
}

func executeWorkflowHandler(wfSvc *workflows.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input string `json:"input"`
		}
		if err := readJSONWF(r, &req); err != nil {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "workflow not found")
			return
		}
		orgID, userID, ok := wfClaims(w, r)
		if !ok {
			return
		}
		// Tenant guard: the run expansion only touches the caller's workflow,
		// runs and approvals (organization_id from the claims).
		result, err := wfSvc.ExecuteWorkflow(r.Context(), orgID, id, req.Input, userID)
		if err != nil {
			var verrs *workflows.ValidationErrors
			switch {
			case errors.As(err, &verrs):
				writeJSONWF(w, http.StatusUnprocessableEntity, map[string]any{"errors": verrs.Errors})
			case errors.Is(err, workflows.ErrWorkflowNotFound):
				writeErrorWF(w, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "workflow not found")
			case errors.Is(err, workflows.ErrEngineNotWired):
				writeErrorWF(w, http.StatusServiceUnavailable, "ENGINE_NOT_WIRED", "workflow execution engine is not wired")
			default:
				writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONWF(w, http.StatusOK, result)
	}
}

func getWorkflowRunHandler(wfSvc *workflows.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "WORKFLOW_RUN_NOT_FOUND", "workflow run not found")
			return
		}
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		wr, nodes, err := wfSvc.GetWorkflowRun(r.Context(), orgID, id)
		if err != nil {
			writeWorkflowErrorWF(w, err)
			return
		}
		nodeRuns := make([]*workflows.NodeRun, len(nodes))
		copy(nodeRuns, nodes)
		writeJSONWF(w, http.StatusOK, map[string]any{
			"id":          wr.ID,
			"workflow_id": wr.WorkflowID,
			"status":      wr.Status,
			"node_runs":   nodeRuns,
		})
	}
}

// writeWorkflowErrorWF maps workflow service errors onto the HTTP contract.
func writeWorkflowErrorWF(w http.ResponseWriter, err error) {
	var verrs *workflows.ValidationErrors
	switch {
	case errors.Is(err, workflows.ErrWorkflowNotFound):
		writeErrorWF(w, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "workflow not found")
	case errors.Is(err, workflows.ErrWorkflowRunNotFound):
		writeErrorWF(w, http.StatusNotFound, "WORKFLOW_RUN_NOT_FOUND", "workflow run not found")
	case errors.As(err, &verrs):
		writeJSONWF(w, http.StatusUnprocessableEntity, map[string]any{"errors": verrs.Errors})
	default:
		writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Approval handlers
// ---------------------------------------------------------------------------

func listApprovalsHandler(apSvc *approvals.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		list, err := apSvc.List(r.Context(), orgID, r.URL.Query().Get("status"))
		if err != nil {
			writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"approvals": list})
	}
}

func getApprovalHandler(apSvc *approvals.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "APPROVAL_NOT_FOUND", "approval not found")
			return
		}
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		approval, err := apSvc.Get(r.Context(), orgID, id)
		if err != nil {
			writeApprovalErrorWF(w, err)
			return
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"approval": approval})
	}
}

func decideApprovalHandler(apSvc *approvals.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if err := readJSONWF(r, &req); err != nil {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "APPROVAL_NOT_FOUND", "approval not found")
			return
		}
		orgID, userID, ok := wfClaims(w, r)
		if !ok {
			return
		}
		// The approver identity comes from the authenticated caller; approving
		// resumes the linked paused run inside the approvals service.
		approval, err := apSvc.Decide(r.Context(), orgID, id, req.Decision, req.Reason, userID)
		if err != nil {
			writeApprovalErrorWF(w, err)
			return
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"approval": approval})
	}
}

func writeApprovalErrorWF(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, approvals.ErrApprovalNotFound):
		writeErrorWF(w, http.StatusNotFound, "APPROVAL_NOT_FOUND", "approval not found")
	case errors.Is(err, approvals.ErrInvalidDecision):
		writeErrorWF(w, http.StatusUnprocessableEntity, "INVALID_DECISION", err.Error())
	case errors.Is(err, approvals.ErrAlreadyDecided):
		writeErrorWF(w, http.StatusConflict, "ALREADY_DECIDED", err.Error())
	default:
		writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Run control handlers (cancel / pause / resume)
// ---------------------------------------------------------------------------

func runControlHandler(runsSvc *runs.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "RUN_NOT_FOUND", "run not found")
			return
		}
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		if runsSvc == nil {
			writeErrorWF(w, http.StatusServiceUnavailable, "RUNS_SERVICE_UNAVAILABLE", "runs service is not available")
			return
		}
		var (
			run *runs.Run
			err error
		)
		switch action {
		case "cancel":
			run, err = runsSvc.CancelRun(r.Context(), orgID, id)
		case "pause":
			run, err = runsSvc.PauseRun(r.Context(), orgID, id)
		case "resume":
			run, err = runsSvc.ResumeRun(r.Context(), orgID, id)
		default:
			writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", "unknown run control action")
			return
		}
		if err != nil {
			switch {
			case errors.Is(err, runs.ErrRunNotFound):
				writeErrorWF(w, http.StatusNotFound, "RUN_NOT_FOUND", "run not found")
			case errors.Is(err, runs.ErrInvalidTransition):
				writeErrorWF(w, http.StatusConflict, "INVALID_STATE", err.Error())
			default:
				writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONWF(w, http.StatusOK, run)
	}
}
