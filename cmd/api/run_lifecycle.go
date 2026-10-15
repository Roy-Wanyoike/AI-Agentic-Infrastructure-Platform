package main

// run_lifecycle.go — issue #115: the run pause/resume/cancel HTTP surface.
//
// The three control endpoints move a run through the ENFORCED lifecycle
// state machine (internal/runs/lifecycle.go):
//
//      POST /runs/{id}/pause   → paused   (in-flight executors halt at the step boundary)
//      POST /runs/{id}/resume  → queued   (the agent.run task is re-enqueued: the queue
//                                          is the only execution trigger)
//      POST /runs/{id}/cancel  → cancelled | cancelling (two-phase for in-flight runs)
//
// Auth chain (real middleware, house pattern cmd/api/residency_test.go):
// RequireAuthOrAPIKey → RequirePermission(runs.control) — the write-tier runs
// permission granted to OWNER/ADMIN/MEMBER (internal/auth/permissions_workflows.go;
// no new permission enum was introduced). The tenant comes from the
// authenticated claims, never from client input.
//
// Error matrix: 401 (no/invalid credentials, middleware), 403 (permission,
// middleware), 404 (unknown or foreign run — no existence leak), 409
// (illegal transition: typed *RunTransitionError / lost optimistic race),
// 503 (runs service unwired). Responses reuse the workflows track's
// structured envelope {"error":{"code","message"}}.
//
// Routes use Go 1.22 method+path patterns, so they take precedence over the
// legacy /runs/ subtree handler registered in main.go. The routes moved here
// from registerWorkflowsRoutes (wave-2 track 2-a) and gained the issue #115
// semantics: resume re-enters the QUEUE (paused → queued) instead of parking
// the run in pending, and the optimistic-store race maps to 409.

import (
	"errors"
	"net/http"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// registerRunLifecycleRoutes mounts the three control endpoints on apiMux.
// queueSvc is optional (nil = resume still transitions, but the re-enqueue
// degrades to a no-op — documented in runResumeHandler).
func registerRunLifecycleRoutes(apiMux *http.ServeMux, runsSvc *runs.Service, queueSvc *queue.Queue, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	wrap := func(h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionRunsControl)(h))
	}
	apiMux.Handle("POST /runs/{id}/pause", wrap(runControlHandler(runsSvc, queueSvc, "pause")))
	apiMux.Handle("POST /runs/{id}/resume", wrap(runControlHandler(runsSvc, queueSvc, "resume")))
	apiMux.Handle("POST /runs/{id}/cancel", wrap(runControlHandler(runsSvc, queueSvc, "cancel")))
}

// runControlHandler dispatches one control op. Shared body so the 404/409/503
// mapping and the tenant resolution stay identical across pause/resume/cancel.
func runControlHandler(runsSvc *runs.Service, queueSvc *queue.Queue, action string) http.HandlerFunc {
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
		run, err := executeRunControl(r, runsSvc, queueSvc, orgID, id, action)
		if err != nil {
			writeRunControlError(w, err)
			return
		}
		writeJSONWF(w, http.StatusOK, run)
	}
}

// executeRunControl performs the transition for one action. The resume path
// re-enqueues the agent.run task when the resume actually moved a paused run
// back into the queue (same contract as the approvals RunController in
// cmd/api/runs.go: the queue is the only execution trigger — without the
// re-enqueue the run would sit in queued forever).
func executeRunControl(r *http.Request, runsSvc *runs.Service, queueSvc *queue.Queue, orgID, id, action string) (*runs.Run, error) {
	switch action {
	case "pause":
		return runsSvc.PauseRun(r.Context(), orgID, id)
	case "cancel":
		return runsSvc.CancelRun(r.Context(), orgID, id)
	case "resume":
		// Capture the pre-resume status BEFORE resuming: the in-memory runs
		// service hands out the live run pointer, so once the transition
		// lands the shared record already reports queued and a post-resume
		// read would wrongly conclude nothing changed.
		before, err := runsSvc.GetRunCtx(r.Context(), orgID, id)
		if err != nil {
			return nil, err
		}
		wasPaused := before.Status == runs.StatusPaused || string(before.Status) == string(runs.StatePaused)
		run, err := runsSvc.ResumeRunQueued(r.Context(), orgID, id)
		if err != nil {
			return nil, err
		}
		if wasPaused && queueSvc != nil {
			queueSvc.Enqueue("agent.run", map[string]any{
				"organization_id": run.OrganizationID,
				"agent_id":        run.AgentID,
				"input":           run.Input,
				"run_id":          run.ID,
				"resumed":         true,
			})
		}
		return run, nil
	default: // unreachable: the routes pin the action set
		return nil, errors.New("unknown run control action")
	}
}

// writeRunControlError maps the control-op failures onto the contract matrix.
// ErrStatusConflict (lost optimistic race — another writer transitioned the
// run first) is a 409 like an illegal transition: the caller asked the run to
// move from a state it just left.
func writeRunControlError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, runs.ErrRunNotFound):
		writeErrorWF(w, http.StatusNotFound, "RUN_NOT_FOUND", "run not found")
	case errors.Is(err, runs.ErrStatusConflict):
		writeErrorWF(w, http.StatusConflict, "STATUS_CONFLICT", err.Error())
	case errors.Is(err, runs.ErrInvalidTransition):
		writeErrorWF(w, http.StatusConflict, "INVALID_STATE", err.Error())
	default:
		writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
}
