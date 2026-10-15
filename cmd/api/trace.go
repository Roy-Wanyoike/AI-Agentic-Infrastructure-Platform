package main

// trace.go — issue #114: run trace id exposure.
//
// GET /runs/{id}/trace returns the W3C trace id of the distributed trace that
// carries the run's execution spans (run → agent → step → tool/model →
// provider HTTP), so operators can jump from the run timeline into the tracing
// backend (Tempo/Jaeger/...). The id is served from the in-process
// observability run-trace registry, populated wherever the run's root span is
// created (API create-run → "run.enqueue"; worker execution → "run.execute",
// which joins the same trace through the queue task's traceparent).
//
// Deliberately minimal + additive (documented choice for issue #114):
//   - the runs.Run struct and the run_steps schema are NOT extended; the
//     registry is the source of truth. It is in-memory and bounded (8192
//     entries, FIFO): entries exist only for runs whose trace started in this
//     API process and survive no restart. A miss (tracing disabled, evicted,
//     created elsewhere) answers 404 trace_not_available — no existence
//     leak, no error contract change for the run resource itself.
//   - tenant safety: the run row is resolved through the caller's
//     organization guard first; foreign runs 404 before any trace lookup.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/observability"
	"agentos/internal/runs"
)

// registerRunTraceRoutes mounts the issue #114 run trace exposure endpoint.
// The "GET /runs/{id}/trace" pattern is strictly more specific than the
// existing "/runs/" subtree registration, so ServeMux dispatches trace
// requests here without touching the run detail/events/steps handlers.
func registerRunTraceRoutes(apiMux *http.ServeMux, runsSvc *runs.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	apiMux.Handle("GET /runs/{id}/trace", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionRunsRead)(http.HandlerFunc(runTraceHandler(runsSvc)))))
}

// runTraceHandler serves {run_id, trace_id} for one tenant-visible run. 404
// (trace_not_available) when the run is unknown/foreign OR no trace was
// recorded for it in this process (tracing disabled, registry miss).
func runTraceHandler(runsSvc *runs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if runsSvc == nil {
			http.Error(w, "runs service not available", http.StatusInternalServerError)
			return
		}
		runID := r.PathValue("id")
		if strings.TrimSpace(runID) == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: resolve the run within the caller's organization
		// first — foreign runs must be indistinguishable from trace misses.
		if _, err := runsSvc.GetRunCtx(r.Context(), orgID, runID); err != nil {
			if errors.Is(err, runs.ErrRunNotFound) {
				writeTraceError(w, http.StatusNotFound, "trace_not_available", "run not found or no trace recorded")
				return
			}
			writeTraceError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		traceID, ok := observability.LookupRunTraceID(runID)
		if !ok {
			writeTraceError(w, http.StatusNotFound, "trace_not_available",
				"no trace recorded for this run (tracing may be disabled, or the run was started in another process; set AGENTOS_TRACING_ENABLED=1)")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"run_id":   runID,
			"trace_id": traceID,
		})
	}
}

// writeTraceError emits the structured {"error":{"code","message"}} envelope.
func writeTraceError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
