package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/billing"
	"agentos/internal/policies"
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

// policyEnforcementVar carries the governance enforcement seam (issue #75) to
// the create-run handler, mirroring the runsServiceVar/billingServiceVar
// wiring precedent: newApp assigns it after construction and tests wire it
// directly. When nil (or its enforcer disabled), run creation keeps the
// legacy always-allow behavior — no policy evaluation, no policy steps.
var policyEnforcementVar *runPolicyEnforcement

// Reason codes answered by the run-creation enforcement gate. They match the
// step-error codes the runtime tool seam records so one vocabulary spans run
// creation, steps and audit.
const (
	reasonPolicyDenied      = "policy_denied"
	reasonPolicyUnavailable = "policy_unavailable"
)

// runPolicyEnforcement bundles the policy service (shared with the
// governance CRUD routes so POST /policies/create is immediately visible to
// enforcement), the run-creation decision source and the approvals service
// that receives require_approval pauses.
type runPolicyEnforcement struct {
	Policies  *policies.Service
	Enforcer  *policies.Enforcer
	Approvals *approvals.Service
}

// newRunPolicyEnforcement builds the seam in the established dual-mode
// fashion: the policies service picks its store from db (Postgres when
// non-nil, in-memory otherwise).
func newRunPolicyEnforcement(db *sql.DB, apSvc *approvals.Service) *runPolicyEnforcement {
	polSvc := newPoliciesService(db)
	return &runPolicyEnforcement{
		Policies:  polSvc,
		Enforcer:  policies.NewEnforcer(polSvc),
		Approvals: apSvc,
	}
}

// sharedPoliciesService returns the policies service shared between the
// governance CRUD routes and the enforcement seam. routes() uses it so both
// surfaces see ONE tenant's policies in every mode (two in-memory services
// would silently disagree in zero-infrastructure deployments). When the
// enforcement wiring is absent (tests that construct routes() without opting
// in) it falls back to a fresh dual-mode service, preserving legacy behavior.
func (a *app) sharedPoliciesService() *policies.Service {
	if policyEnforcementVar != nil && policyEnforcementVar.Policies != nil {
		return policyEnforcementVar.Policies
	}
	return newPoliciesService(a.db)
}

// runPolicyVerdict is the outcome of the create-run policy gate.
type runPolicyVerdict struct {
	decision policies.Decision
	// enforced reports whether an active enforcement seam evaluated the
	// request (false = disabled/unwired: legacy behavior, no policy traces).
	enforced bool
	// approvalID is the approval request filed for a require_approval pause
	// (empty otherwise); it is echoed in the 202 response.
	approvalID string
}

// enforceRunPolicy is the create-run governance gate (issue #75). It runs
// AFTER RBAC (the route middleware) and BEFORE the quota gate and any run
// row exists, so a refusal leaves no run behind. Decision matrix:
//
//   - seam unwired / flag off -> allow (legacy behavior; enforced=false)
//   - evaluation fails        -> 503 policy_unavailable (fail closed: an
//     operator asked for enforcement, a failing policy source must surface,
//     never silently bypass) + audit entry
//   - decision deny           -> 403 policy_denied with the decision reason
//   - audit entry; no run row, no queue task
//   - decision allow with
//     require_approval         -> caller parks the run in WAITING_APPROVAL
//     and files an approval request (verified here: the approvals service
//     must be wired, else 503 fail closed)
//   - decision allow           -> run proceeds; the verdict is recorded on
//     the run's timeline (policy step) so the dashboard shows WHY
func enforceRunPolicy(w http.ResponseWriter, r *http.Request, orgID, agentID, environment string, estimatedCostCents int64, auditSvc *audit.Service) (runPolicyVerdict, bool) {
	verdict := runPolicyVerdict{decision: policies.Decision{Decision: policies.EffectAllow, Reason: "policy enforcement disabled"}}
	seam := policyEnforcementVar
	if seam == nil || !seam.Enforcer.Enabled() {
		return verdict, true
	}
	verdict.enforced = true
	decision, err := seam.Enforcer.AuthorizeRunCreation(r.Context(), orgID, agentID, environment, estimatedCostCents)
	if err != nil {
		// Fail closed with a distinct code so operators can tell a policy
		// denial from a broken decision source.
		if auditSvc != nil {
			if claims, claimsErr := auth.ExtractClaims(r.Context()); claimsErr == nil {
				_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "run.policy_unavailable", orgID, "runs/-", map[string]any{
					"reason":   err.Error(),
					"agent_id": agentID,
				})
			}
		}
		writeRunError(w, http.StatusServiceUnavailable, reasonPolicyUnavailable, err.Error())
		return verdict, false
	}
	verdict.decision = decision
	if !decision.Allowed() {
		if auditSvc != nil {
			if claims, claimsErr := auth.ExtractClaims(r.Context()); claimsErr == nil {
				_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "run.policy_denied", orgID, "runs/-", map[string]any{
					"reason":      decision.Reason,
					"policy_id":   decision.MatchedPolicyID,
					"decision":    decision.Decision,
					"agent_id":    agentID,
					"environment": environment,
				})
			}
		}
		writeRunError(w, http.StatusForbidden, reasonPolicyDenied, policies.DenyMessage("run", decision))
		return verdict, false
	}
	if decision.RequireApproval && (seam.Approvals == nil) {
		// Approval routing was requested by policy but the approvals flow is
		// not wired: failing open would execute an action an operator
		// explicitly gated.
		writeRunError(w, http.StatusServiceUnavailable, reasonPolicyUnavailable, "require_approval policy matched but the approvals service is not available")
		return verdict, false
	}
	return verdict, true
}

// recordPolicyEvaluationStep appends the policy verdict to the run timeline
// (run_steps row, step_type "policy") so GET /runs/{id}/steps and the
// dashboard show WHY a run was allowed, denied or parked for approval.
// Best-effort: observability must never block run acceptance.
func recordPolicyEvaluationStep(ctx context.Context, orgID, runID string, verdict runPolicyVerdict, agentID, environment string, estimatedCostCents int64) {
	if !verdict.enforced || runsServiceVar == nil {
		return
	}
	now := time.Now().UTC()
	step := &runs.Step{
		StepType: "policy",
		Status:   "succeeded",
		InputMeta: map[string]any{
			"action":               policies.ActionRunExecute,
			"agent_id":             agentID,
			"environment":          environment,
			"estimated_cost_cents": estimatedCostCents,
		},
		OutputMeta: map[string]any{
			"policy_id":        verdict.decision.MatchedPolicyID,
			"decision":         verdict.decision.Decision,
			"reason":           verdict.decision.Reason,
			"require_approval": verdict.decision.RequireApproval,
		},
		StartedAt:   now,
		CompletedAt: now,
	}
	if err := runsServiceVar.RecordStep(ctx, orgID, runID, step); err != nil {
		slog.Warn("policy evaluation step not recorded", "run_id", runID, "error", err.Error())
	}
}

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
			// issue #75: optional governance context. Environment
			// feeds the policies' environments condition (default
			// "production"); EstimatedCostCents feeds the
			// max_cost_cents budget-guard condition.
			Environment        string `json:"environment,omitempty"`
			EstimatedCostCents int64  `json:"estimated_cost_cents,omitempty"`
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
		// issue #75: governance gate BEFORE the quota gate and before
		// any run row exists — policy refusals (deny / failing policy
		// source) leave no run behind, only the response and their
		// audit entry. The verdict rides along so the accepted-run
		// paths below can record it on the timeline and route
		// require_approval decisions into the approvals flow.
		verdict, ok := enforceRunPolicy(w, r, req.OrganizationID, req.AgentID, req.Environment, req.EstimatedCostCents, auditSvc)
		if !ok {
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

		// issue #75: require_approval decisions park the run in
		// WAITING_APPROVAL and file an approval request INSTEAD of
		// enqueueing execution. An operator approval resumes the run
		// (approvalRunController) and re-enqueues it.
		if verdict.decision.RequireApproval && verdict.enforced {
			if runID == "" {
				// No runs service wired: the pause flow needs a
				// run row to gate on — fail loudly rather than
				// executing an approval-gated run.
				http.Error(w, "require_approval policy matched but the runs service is not available", http.StatusServiceUnavailable)
				return
			}
			approvalID, err := parkRunForApproval(r.Context(), req.OrganizationID, runID, verdict, req.AgentID, req.Environment, req.EstimatedCostCents, auditSvc)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			verdict.approvalID = approvalID
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			resp := map[string]any{
				"run_id": runID,
				"status": string(runs.StatusWaitingApproval),
				"policy": map[string]any{
					"policy_id":        verdict.decision.MatchedPolicyID,
					"decision":         verdict.decision.Decision,
					"reason":           verdict.decision.Reason,
					"require_approval": true,
				},
			}
			if verdict.approvalID != "" {
				resp["approval_id"] = verdict.approvalID
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// issue #75: record the verdict on the run timeline so the
		// dashboard/audit can show why the run was accepted.
		recordPolicyEvaluationStep(r.Context(), req.OrganizationID, runID, verdict, req.AgentID, req.Environment, req.EstimatedCostCents)

		if workQueue == nil {
			workQueue = queue.NewQueue()
		}
		payload := map[string]any{"organization_id": req.OrganizationID, "agent_id": req.AgentID, "input": req.Input}
		if runID != "" {
			payload["run_id"] = runID
		}
		if env := strings.TrimSpace(req.Environment); env != "" {
			// issue #75: the worker stamps the tool-policy scope
			// from this field; blank keeps the platform default.
			payload["environment"] = env
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

// parkRunForApproval routes an approval-gated run into the existing approvals
// flow (issue #75): the run is transitioned QUEUED -> WAITING_APPROVAL, a
// pending approval linked to the run is filed for the operators, the policy
// verdict lands on the run timeline, and the decision is audited. Execution
// is NOT enqueued; the approval decision resumes the run through the
// approvalRunController wired into the approvals service. It returns the
// filed approval id (empty when the approvals service is unwired).
func parkRunForApproval(ctx context.Context, orgID, runID string, verdict runPolicyVerdict, agentID, environment string, estimatedCostCents int64, auditSvc *audit.Service) (string, error) {
	decision := verdict.decision
	if runsServiceVar != nil {
		if err := runsServiceVar.UpdateStatusCtx(ctx, orgID, runID, runs.StatusWaitingApproval, ""); err != nil {
			return "", err
		}
	}
	recordPolicyEvaluationStep(ctx, orgID, runID, runPolicyVerdict{decision: decision, enforced: verdict.enforced}, agentID, environment, estimatedCostCents)

	claims, claimsErr := auth.ExtractClaims(ctx)
	var requester string
	if claimsErr == nil {
		requester = claims.UserID
	}
	var approvalID string
	if seam := policyEnforcementVar; seam != nil && seam.Approvals != nil {
		approval, err := seam.Approvals.Request(ctx, orgID, approvals.RequestInput{
			RunID:     runID,
			Resource:  "runs/" + runID,
			Action:    policies.ActionRunExecute,
			Reason:    decision.Reason,
			Risk:      approvals.RiskMedium,
			Requester: requester,
		})
		if err != nil {
			return "", err
		}
		approvalID = approval.ID
	}
	if auditSvc != nil && claimsErr == nil {
		_, _ = auditSvc.LogCtx(ctx, claims.UserID, "run.approval_required", orgID, "runs/"+runID, map[string]any{
			"reason":      decision.Reason,
			"policy_id":   decision.MatchedPolicyID,
			"approval_id": approvalID,
			"agent_id":    agentID,
		})
	}
	return approvalID, nil
}

// approvalRunController completes the approvals flow (issue #75): when an
// approval linked to a run is decided, the run is resumed through
// runs.ResumeRun (paused or waiting_approval -> pending) and, when the
// resume actually moved it out of a gate, the agent.run task is re-enqueued
// so execution continues. Without the re-enqueue the run would sit in
// pending forever — the queue is the only execution trigger. Idempotence:
// approvals can only be decided once, and already-pending/running runs are
// never re-enqueued.
type approvalRunController struct {
	runsSvc  *runs.Service
	queueSvc *queue.Queue
}

// newApprovalRunController builds the RunController the approvals service
// uses to resume gated runs (wired in newApp).
func newApprovalRunController(runsSvc *runs.Service, queueSvc *queue.Queue) *approvalRunController {
	return &approvalRunController{runsSvc: runsSvc, queueSvc: queueSvc}
}

// ResumeRun implements approvals.RunController.
func (c *approvalRunController) ResumeRun(ctx context.Context, orgID, runID string) (*runs.Run, error) {
	if c == nil || c.runsSvc == nil {
		return nil, errors.New("runs service is not available")
	}
	before, err := c.runsSvc.GetRunCtx(ctx, orgID, runID)
	if err != nil {
		return nil, err
	}
	// Capture the pre-resume status BEFORE resuming: the in-memory runs
	// service hands out the live run pointer, so once ResumeRun lands the
	// shared record already reports pending and a post-resume read would
	// wrongly conclude nothing was gated (issue #75 approval resume).
	wasGated := before.Status == runs.StatusPaused || before.Status == runs.StatusWaitingApproval
	run, err := c.runsSvc.ResumeRun(ctx, orgID, runID)
	if err != nil {
		return nil, err
	}
	if !wasGated {
		// Nothing was gated (legacy paused-run approvals that already
		// resumed, races, replays): no re-enqueue.
		return run, nil
	}
	if c.queueSvc != nil {
		payload := map[string]any{
			"organization_id":  run.OrganizationID,
			"agent_id":         run.AgentID,
			"input":            run.Input,
			"run_id":           run.ID,
			"approval_resumed": true,
		}
		c.queueSvc.Enqueue("agent.run", payload)
	}
	return run, nil
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
