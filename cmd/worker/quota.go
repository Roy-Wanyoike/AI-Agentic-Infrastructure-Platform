package main

// Worker-side quota enforcement (issue #47). The API already refuses to
// enqueue runs for over-quota organizations when AGENTOS_BILLING_ENFORCEMENT
// is on; this pre-execution gate is the defense-in-depth layer for tasks that
// were enqueued before the flag flipped (or by paths outside the create-run
// handler, e.g. workflow node expansions). It consults billing BEFORE the
// runner touches the task and, on a positive evidence of an exceeded monthly
// run budget, marks the run failed with the machine reason "quota_exceeded",
// writes an audit entry, and lets the queue loop continue with other tasks.
//
// Decision matrix (mirrors cmd/api/runs.go enforceQuota, adapted to the
// worker's position downstream of enqueue):
//
//   - gate not wired (enforcement off / no durable billing state) -> allow
//   - no subscription for the org                                 -> allow
//   - quota check fails (usage-source/store error)                -> allow
//     (the run was already accepted at enqueue time; failing accepted work
//     on a transient metering error would destroy it — the API side, which
//     gates NEW work, is the fail-closed surface)
//   - exceeded && !unlimited                                      -> DENY:
//     audit "run.quota_denied" + run FAILED with reason quota_exceeded
//
// The enforcer is a plain struct (not a closure inside main) so the denial
// path is unit-testable — cmd/worker previously had no tests at all.

import (
	"context"
	"errors"
	"log/slog"

	"agentos/internal/audit"
	"agentos/internal/billing"
	"agentos/internal/runs"
)

// workflowCodeQuotaExceeded is the node error code recorded on workflow
// checkpoints whose agent run was denied by the quota gate. The field is
// free-form (existing codes: NODE_FAILED / NODE_ORPHANED /
// WORKFLOW_RUN_TIMEOUT); recovery treats it like any other failed node.
const workflowCodeQuotaExceeded = "QUOTA_EXCEEDED"

// quotaEnforcer gates run execution on the organization's billing quota.
type quotaEnforcer struct {
	billing *billing.Service
	runs    *runs.Service
	audit   *audit.Service
	log     *slog.Logger
}

// allow reports whether the run may execute. On denial it performs the two
// side effects (audit entry + failed run marking) so callers only have to
// finish their own bookkeeping (workflow checkpoints, API status mirror) and
// ack the task without retrying.
func (q *quotaEnforcer) allow(ctx context.Context, orgID, runID, agentID string) bool {
	if q == nil || q.billing == nil {
		return true // enforcement not wired (flag off / no durable billing state)
	}
	quota, err := q.billing.CheckQuotaCtx(ctx, orgID)
	switch {
	case err == nil:
		// fall through to the exceeded decision below.
	case errors.Is(err, billing.ErrNoSubscription):
		// No plan -> no quota to exceed (the API gate allows these too).
		return true
	default:
		// Metering failure: degrade to allow (the run was already accepted
		// at enqueue time; failing accepted work on a transient metering
		// error would destroy it — see the matrix in the header).
		if q.log != nil {
			q.log.Warn("worker quota gate: check failed, allowing execution", "run_id", runID, "error", err.Error())
		}
		return true
	}
	if quota.Unlimited || !quota.Exceeded {
		return true
	}
	// Positive evidence of an exceeded budget: deny.
	if q.audit != nil {
		_, _ = q.audit.LogCtx(ctx, "worker", "run.quota_denied", orgID, "runs/"+runID, map[string]any{
			"reason":        billing.ReasonQuotaExceeded,
			"agent_id":      agentID,
			"included_runs": quota.IncludedRuns,
			"consumed_runs": quota.ConsumedRuns,
		})
	}
	if q.runs != nil {
		// Existing failure semantics: UpdateStatus moves the run to FAILED;
		// the reason rides in the output slot so GET /runs/{id} and the
		// status.changed mirror surface WHY the run failed.
		_ = q.runs.UpdateStatus(runID, runs.StatusFailed, billing.ReasonQuotaExceeded)
	}
	if q.log != nil {
		q.log.Warn("worker quota gate: run denied, quota exceeded",
			"run_id", runID, "organization_id", orgID,
			"included_runs", quota.IncludedRuns, "consumed_runs", quota.ConsumedRuns)
	}
	return false
}
