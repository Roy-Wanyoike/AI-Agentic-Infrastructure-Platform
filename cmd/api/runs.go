package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

var runsServiceVar *runs.Service

func createRunHandler(workQueue *queue.Queue, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			OrganizationID string `json:"organization_id"`
			AgentID        string `json:"agent_id"`
			Input          string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.OrganizationID == "" {
			claims, err := auth.ExtractClaims(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			req.OrganizationID = claims.OrganizationID
		}
		if !requireOrganizationAccess(w, r, req.OrganizationID) {
			return
		}
		if strings.TrimSpace(req.AgentID) == "" {
			http.Error(w, "agent id is required", http.StatusBadRequest)
			return
		}
		var runID string
		var rs *runs.Service
		if runsServiceVar != nil {
			rs = runsServiceVar
		}
		if rs != nil {
			// Tenant guard: the run row is created with the caller's organization_id.
			run, err := rs.CreateRunCtx(r.Context(), req.OrganizationID, req.AgentID, req.Input)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			runID = run.ID
		}
		if workQueue == nil {
			workQueue = queue.NewQueue()
		}
		payload := map[string]any{"organization_id": req.OrganizationID, "agent_id": req.AgentID, "input": req.Input}
		if runID != "" {
			payload["run_id"] = runID
		}
		task := workQueue.Enqueue("agent.run", payload)
		if task == nil {
			http.Error(w, "failed to enqueue run", http.StatusInternalServerError)
			return
		}
		if runID == "" {
			runID = task.ID
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			claims, claimsErr := auth.ExtractClaims(r.Context())
			if claimsErr == nil {
				_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "run.created", req.OrganizationID, "runs/"+runID, nil)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": runID, "status": "queued"})
	}
}

func getRunHandler(runsService *runs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		runID := trimRoutePrefix(r.URL.Path, "/runs/")
		if runID == "" || strings.Contains(runID, "/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if runsService == nil {
			http.Error(w, "runs service not available", http.StatusInternalServerError)
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: the lookup requires the run's organization_id to match
		// the caller's tenant; foreign runs surface as 404.
		run, err := runsService.GetRunCtx(r.Context(), orgID, runID)
		if err != nil {
			if errors.Is(err, runs.ErrRunNotFound) {
				http.Error(w, "run not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(run)
	}
}

func runStepsHandler(runsService *runs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		runID := strings.TrimSuffix(trimRoutePrefix(r.URL.Path, "/runs/"), "/steps")
		if runID == "" || strings.Contains(runID, "/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if runsService == nil {
			http.Error(w, "runs service not available", http.StatusInternalServerError)
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: steps are read via a join scoped to organization_id.
		steps, err := runsService.Steps(r.Context(), orgID, runID)
		if err != nil {
			if errors.Is(err, runs.ErrRunNotFound) {
				http.Error(w, "run not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": runID, "steps": steps})
	}
}

func listRunsHandler(runsService *runs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if runsService == nil {
			http.Error(w, "runs service not available", http.StatusInternalServerError)
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: the listing filters on organization_id.
		list, err := runsService.ListRunsCtx(r.Context(), orgID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": list})
	}
}

func queuePullHandler(q *queue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if q == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		task := q.Dequeue()
		if task == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(task)
	}
}
