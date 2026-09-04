package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/billing"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

var runsServiceVar *runs.Service

// billingServiceVar exposes the billing service to the create-run handler for
// quota enforcement (issue #47). It mirrors the runsServiceVar precedent: the
// app wiring (cmd/api/main.go newApp) assigns it after construction — see the
// reported wiring diff. When nil, enforcement degrades to an explicit 503
// BILLING_UNAVAILABLE whenever AGENTOS_BILLING_ENFORCEMENT is on, so a
// half-wired rollout fails loudly instead of silently bypassing the quota.
var billingServiceVar *billing.Service

// writeRunError emits the structured {"error":{"code","message"}} envelope
// used by the quota enforcement paths (issue #47). The legacy handlers below
// keep their historical http.Error plain-text responses; only the new
// contract-shaped denials use this envelope.
func writeRunError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// enforceQuota is the create-run quota gate (issue #47). It runs when
// AGENTOS_BILLING_ENFORCEMENT is on, BEFORE the run row is created or the
// agent.run task is enqueued. Documented decision matrix:
//
//   - enforcement OFF            -> allow (today's behavior; the display
//     endpoint GET /billing/subscription keeps reporting quota state)
//   - enforcement ON, billing not wired -> deny with 503 BILLING_UNAVAILABLE
//     (fail-closed: an operator asked for enforcement, a nil service is a
//     wiring bug that must surface, not silently pass)
//   - enforcement ON, no subscription   -> allow (no subscription = no plan =
//     no quota to exceed; metering starts with POST /billing/subscriptions)
//   - enforcement ON, quota check fails -> deny with 500 INTERNAL_ERROR
//     (billing propagates usage-source failures precisely because a silent
//     consumed=0 fallback would fake availability; see internal/billing)
//   - enforcement ON, exceeded && !unlimited -> 402 quota_exceeded, the denial
//     is audit-logged, and NO run row / queue task is created
func enforceQuota(w http.ResponseWriter, r *http.Request, orgID, agentID string, auditSvc *audit.Service) bool {
	if !billing.EnforcementFromEnv() {
		return true
	}
	if billingServiceVar == nil {
		writeRunError(w, http.StatusServiceUnavailable, "billing_unavailable", "billing service not available")
		return false
	}
	quota, err := billingServiceVar.CheckQuotaCtx(r.Context(), orgID)
	switch {
	case errors.Is(err, billing.ErrNoSubscription):
		return true // no plan, no quota to exceed (documented above)
	case err != nil:
		// Never fake quota availability when the metering source fails.
		writeRunError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return false
	}
	if quota.Unlimited || !quota.Exceeded {
		return true
	}
	message := billing.QuotaExceededMessage(quota)
	if auditSvc != nil {
		// Best-effort denial audit (same claims-scoped pattern as
		// run.created below). Resource is "runs/-": the denial happens
		// before any run row exists, so there is no run id to
		// reference; metadata carries the machine reason and the
		// quota numbers that triggered the decision.
		metadata := map[string]any{
			"reason":        billing.ReasonQuotaExceeded,
			"agent_id":      agentID,
			"included_runs": quota.IncludedRuns,
			"consumed_runs": quota.ConsumedRuns,
		}
		if claims, claimsErr := auth.ExtractClaims(r.Context()); claimsErr == nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "run.quota_denied", orgID, "runs/-", metadata)
		}
	}
	writeRunError(w, http.StatusPaymentRequired, billing.ReasonQuotaExceeded, message)
	return false
}

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
		// issue #47: quota gate BEFORE the run row is created and BEFORE the
		// task is enqueued — an over-quota denial leaves no trace except the
		// 402 response and its audit entry.
		if !enforceQuota(w, r, req.OrganizationID, req.AgentID, auditSvc) {
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
