package main

import (
	"net/http"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/workflows"
)

// registerWorkflowRunNodeRoutes mounts the wave-3 track 3-c checkpointed
// node-timeline endpoint:
//
//	GET /workflow-runs/{id}/nodes   (workflows:read)
//
// The route wraps the same RequireAuthOrAPIKey -> RequirePermission chain as
// the wave-2 workflow routes in workflows.go; the tenant is always taken from
// the authenticated claims. Registration line for main.go (see
// docs/wiring/durable-workflows.md):
//
//	registerWorkflowRunNodeRoutes(apiMux, a.wfSvc, a.authSvc, a.apiKeysSvc)
func registerWorkflowRunNodeRoutes(apiMux *http.ServeMux, wfSvc *workflows.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	// Same middleware chain as the wave-2 workflow routes in workflows.go.
	nodeTimeline := func(h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionWorkflowsRead)(h))
	}
	apiMux.Handle("GET /workflow-runs/{id}/nodes", nodeTimeline(workflowRunNodesHandler(wfSvc)))
}

// workflowRunNodeView is the contract-pinned wire shape of one checkpointed
// node execution attempt (snake_case; null when a timestamp/error code is not
// set). Deliberately distinct from workflows.NodeRun: the engine struct keeps
// internal bookkeeping fields (run_id, error, locked_at, heartbeat_at) that
// are trimmed away here.
//
// Status values follow the existing workflow status vocabulary (pending,
// running, waiting_approval, completed, failed, cancelled, timeout) so the
// timeline aligns with GET /workflow-runs/{id} and the machine error codes
// (NODE_ORPHANED, WORKFLOW_RUN_TIMEOUT, ...).
type workflowRunNodeView struct {
	ID         string     `json:"id"`
	NodeID     string     `json:"node_id"`
	Status     string     `json:"status"`
	Attempt    int        `json:"attempt"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	ErrorCode  *string    `json:"error_code"`
}

func workflowRunNodeViewOf(nr *workflows.NodeRun) workflowRunNodeView {
	if nr == nil {
		return workflowRunNodeView{}
	}
	var errorCode *string
	if nr.ErrorCode != "" {
		code := nr.ErrorCode
		errorCode = &code
	}
	return workflowRunNodeView{
		ID:         nr.ID,
		NodeID:     nr.NodeID,
		Status:     nr.Status,
		Attempt:    nr.Attempt,
		StartedAt:  nr.StartedAt,
		FinishedAt: nr.FinishedAt,
		ErrorCode:  errorCode,
	}
}

// workflowRunNodesHandler returns the checkpointed node timeline of one
// workflow run: one entry per (node, attempt), oldest first. Every row is the
// durable checkpoint written synchronously by the executor/worker, so the
// endpoint shows the exact state a recovery pass would act on.
func workflowRunNodesHandler(wfSvc *workflows.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if wfSvc == nil {
			writeErrorWF(w, http.StatusServiceUnavailable, "WORKFLOWS_UNAVAILABLE", "workflows service is not available")
			return
		}
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "WORKFLOW_RUN_NOT_FOUND", "workflow run not found")
			return
		}
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		// Tenant guard: the run and its node runs are scoped to the caller's
		// organization_id; foreign runs surface as 404.
		_, nodes, err := wfSvc.GetWorkflowRun(r.Context(), orgID, id)
		if err != nil {
			writeWorkflowErrorWF(w, err)
			return
		}
		views := make([]workflowRunNodeView, 0, len(nodes))
		for _, nr := range nodes {
			views = append(views, workflowRunNodeViewOf(nr))
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"nodes": views})
	}
}
