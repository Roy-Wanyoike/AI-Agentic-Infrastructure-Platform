package main

// Worker-side budget enforcement (wave-8 issue C). The API already refuses to
// enqueue runs for budget-exceeded scopes when AGENTOS_BILLING_ENFORCEMENT is
// on; this pre-execution gate is the defense-in-depth layer for tasks that
// were enqueued before the flag flipped (or by paths outside the create-run
// handler, e.g. workflow node expansions). It mirrors cmd/worker/quota.go and
// runs ADDITIVELY AFTER the quota gate: a run must pass BOTH gates.
//
// Decision matrix (mirrors quotaEnforcer.allow, adapted to the worker's
// position downstream of enqueue):
//
//   - gate not wired (enforcement off / no durable billing state) -> allow
//   - no budget rows for the org / scope                          -> allow
//   - consumption check fails (usage-source/store error)          -> allow
//     (the run was already accepted at enqueue time; failing accepted work
//     on a transient metering error would destroy it — the API side, which
//     gates NEW work, is the fail-closed surface)
//   - decision denied                                             -> DENY:
//     audit "run.budget_denied" + run FAILED with reason budget_exceeded
//
// The enforcer is a plain struct (not a closure inside main) so the denial
// path is unit-testable — the same reason quotaEnforcer is one.

import (
	"context"
	"log/slog"

	"agentos/internal/audit"
	"agentos/internal/billing"
	"agentos/internal/runs"
)

// workflowCodeBudgetExceeded is the node error code recorded on workflow
// checkpoints whose agent run was denied by the budget gate (sibling of
// workflowCodeQuotaExceeded; recovery treats it like any other failed node).
const workflowCodeBudgetExceeded = "BUDGET_EXCEEDED"

// budgetEnforcer gates run execution on the organization's per-scope budgets.
type budgetEnforcer struct {
	budgets *billing.BudgetService
	runs    *runs.Service
	audit   *audit.Service
	log     *slog.Logger
}

// allow reports whether the run may execute. On denial it performs the two
// side effects (audit entry + failed run marking) so callers only have to
// finish their own bookkeeping (workflow checkpoints, API status mirror) and
// ack the task without retrying. projectedCostCents is 0: the worker knows
// the run COUNT it is about to execute (1) but has no per-run cost estimate
// at this point — the cost knob is enforced from the metered month-to-date
// consumption itself (a budget already blown by consumption denies with zero
// projection).
func (b *budgetEnforcer) allow(ctx context.Context, orgID, runID, agentID string) bool {
	if b == nil || b.budgets == nil {
		return true // enforcement not wired (flag off / no durable billing state)
	}
	decision, err := b.budgets.EvaluateBudget(ctx, orgID, agentID, 1, 0)
	if err != nil {
		// Metering failure: degrade to allow (already-accepted work; see the
		// matrix in the header).
		if b.log != nil {
			b.log.Warn("worker budget gate: check failed, allowing execution", "run_id", runID, "error", err.Error())
		}
		return true
	}
	if decision.Allowed {
		return true
	}
	// Positive evidence of an exceeded budget: deny.
	if b.audit != nil {
		_, _ = b.audit.LogCtx(ctx, "worker", "run.budget_denied", orgID, "runs/"+runID, map[string]any{
			"reason":    billing.ReasonBudgetExceeded,
			"agent_id":  agentID,
			"budget_id": decision.DeniedBudgetID,
			"scope":     decision.DeniedScope,
			"message":   billing.BudgetExceededMessage(decision),
		})
	}
	if b.runs != nil {
		// Existing failure semantics: UpdateStatus moves the run to FAILED;
		// the reason rides in the output slot so GET /runs/{id} and the
		// status.changed mirror surface WHY the run failed.
		_ = b.runs.UpdateStatus(runID, runs.StatusFailed, billing.ReasonBudgetExceeded)
	}
	if b.log != nil {
		b.log.Warn("worker budget gate: run denied, budget exceeded",
			"run_id", runID, "organization_id", orgID,
			"budget_id", decision.DeniedBudgetID, "scope", decision.DeniedScope)
	}
	return false
}
