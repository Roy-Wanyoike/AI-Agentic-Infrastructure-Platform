package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

var runsServiceVar *runs.Service

func createRunHandler(workQueue *queue.Queue) http.HandlerFunc {
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
			run, err := rs.Create(req.OrganizationID, req.AgentID, req.Input)
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
		path := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
		if path == "" || strings.Contains(path, "/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if runsService == nil {
			http.Error(w, "runs service not available", http.StatusInternalServerError)
			return
		}
		run, ok := runsService.Get(path)
		if !ok {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		if !requireOrganizationAccess(w, r, run.OrganizationID) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(run)
	}
}
