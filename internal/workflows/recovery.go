package workflows

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"agentos/internal/approvals"
)

// ---------------------------------------------------------------------------
// Recovery + watchdog (wave-3 track 3-c).
//
// RecoverIncompleteWorkflowRuns is the single recovery pass:
//
//  1. Watchdog: non-terminal runs past deadline_at are timed out
//     (status timeout, error code WORKFLOW_RUN_TIMEOUT) together with their
//     in-flight node runs.
//  2. Stale sweep: non-terminal runs whose heartbeat (or updated_at for
//     legacy rows) is older than the staleness threshold are claimed
//     (attempt++, fresh lease), their pending/running node runs are marked
//     failed with NODE_ORPHANED, and the next pending work is re-enqueued
//     through the existing queue interface (agents/tool nodes get queue
//     tasks; structural nodes complete synchronously; approval gates are
//     re-materialized when no pending approval exists).
//  3. Runs whose node timeline has fully converged are finalized
//     (completed, or failed when a node genuinely failed).
//
// Concurrency: the Postgres store selects candidates with
// SELECT ... FOR UPDATE SKIP LOCKED and every mutation is a guarded
// conditional UPDATE, so the pass is safe to run from several processes; the
// claim step is the serialization point (only one pass can claim a run per
// staleness window).
//
// Execution semantics: re-delivery is at-least-once. A recovered node starts
// a NEW attempt row; BeginNodeRun refuses to re-execute terminal attempts, so
// replays never duplicate finished work.
// ---------------------------------------------------------------------------

// RecoverIncompleteWorkflowRuns recovers orphaned workflow runs of one tenant
// and returns how many runs were acted upon (timed out, re-kicked or
// finalized). An empty orgID sweeps every tenant — an internal-worker-only
// mode used by RecoveryWorker (never exposed via HTTP; documented deviation
// from the tenant-scoped store-query rule).
func (s *Service) RecoverIncompleteWorkflowRuns(ctx context.Context, orgID string) (int, error) {
	if s == nil {
		return 0, errors.New("workflows: recovery requires a service")
	}
	orgID = strings.TrimSpace(orgID)
	now := time.Now().UTC()
	cutoff := now.Add(-s.staleAfterValue())
	recovered := 0

	// 1. Watchdog: runs past their deadline time out before the stale sweep
	// can re-kick them (timeout wins for over-deadline runs).
	timedOut, err := s.timedOutRunCandidates(ctx, orgID, now)
	if err != nil {
		return recovered, err
	}
	for _, wr := range timedOut {
		ok, terr := s.timeoutRun(ctx, wr, now)
		if terr != nil {
			return recovered, terr
		}
		if ok {
			recovered++
		}
	}

	// 2. Stale sweep: claim + orphan + re-enqueue + finalize.
	stale, err := s.staleRunCandidates(ctx, orgID, cutoff)
	if err != nil {
		return recovered, err
	}
	for _, wr := range stale {
		ok, rerr := s.recoverWorkflowRun(ctx, wr, now)
		if rerr != nil {
			return recovered, rerr
		}
		if ok {
			recovered++
		}
	}
	return recovered, nil
}

// timedOutRunCandidates lists watchdog candidates.
func (s *Service) timedOutRunCandidates(ctx context.Context, orgID string, now time.Time) ([]*WorkflowRun, error) {
	if s.store != nil {
		return s.store.TimedOutWorkflowRuns(ctx, orgID, now, RecoveryBatchLimit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*WorkflowRun, 0)
	for _, wr := range s.workflowRuns {
		if orgID != "" && wr.OrganizationID != orgID {
			continue
		}
		if isTerminalRunStatus(wr.Status) {
			continue
		}
		if wr.DeadlineAt != nil && wr.DeadlineAt.Before(now) {
			out = append(out, wr)
		}
	}
	sortWorkflowRuns(out)
	return out, nil
}

// staleRunCandidates lists stale-sweep candidates (non-terminal, heartbeat
// older than cutoff; COALESCE(heartbeat_at, updated_at) semantics).
func (s *Service) staleRunCandidates(ctx context.Context, orgID string, cutoff time.Time) ([]*WorkflowRun, error) {
	if s.store != nil {
		return s.store.StaleWorkflowRuns(ctx, orgID, cutoff, RecoveryBatchLimit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*WorkflowRun, 0)
	for _, wr := range s.workflowRuns {
		if orgID != "" && wr.OrganizationID != orgID {
			continue
		}
		if isTerminalRunStatus(wr.Status) {
			continue
		}
		ref := wr.HeartbeatAt
		if ref == nil {
			ref = &wr.UpdatedAt
		}
		if ref.Before(cutoff) {
			out = append(out, wr)
		}
	}
	sortWorkflowRuns(out)
	return out, nil
}

// sortWorkflowRuns orders candidates oldest-first for deterministic passes.
func sortWorkflowRuns(runs []*WorkflowRun) {
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].UpdatedAt.Equal(runs[j].UpdatedAt) {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].UpdatedAt.Before(runs[j].UpdatedAt)
	})
}

// timeoutRun executes the watchdog for one run: terminal timeout transition
// (guarded) + orphan its in-flight node runs with WORKFLOW_RUN_TIMEOUT.
func (s *Service) timeoutRun(ctx context.Context, wr *WorkflowRun, now time.Time) (bool, error) {
	if wr == nil {
		return false, nil
	}
	ok, err := s.timeoutRunRow(ctx, wr.OrganizationID, wr.ID, ErrorCodeWorkflowRunTimeout, now)
	if err != nil || !ok {
		return false, err
	}
	if _, err := s.failNonTerminalNodeRuns(ctx, wr.OrganizationID, wr.ID, ErrorCodeWorkflowRunTimeout, now); err != nil {
		return true, err
	}
	return true, nil
}

// timeoutRunRow transitions one non-terminal run to the terminal timeout
// status with the given machine error code.
func (s *Service) timeoutRunRow(ctx context.Context, orgID, workflowRunID, errorCode string, at time.Time) (bool, error) {
	if s.store != nil {
		return s.store.TimeoutWorkflowRun(ctx, orgID, workflowRunID, errorCode, at)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wr, ok := s.workflowRuns[workflowRunID]
	if !ok || wr.OrganizationID != orgID || isTerminalRunStatus(wr.Status) {
		return false, nil
	}
	wr.Status = RunStatusTimeout
	wr.ErrorCode = errorCode
	wr.FinishedAt = &at
	wr.UpdatedAt = at
	return true, nil
}

// failNonTerminalNodeRuns marks every pending/running checkpoint of one run
// failed with the given machine code (orphan pass).
func (s *Service) failNonTerminalNodeRuns(ctx context.Context, orgID, workflowRunID, errorCode string, at time.Time) (int64, error) {
	if s.store != nil {
		return s.store.FailNonTerminalNodeRuns(ctx, orgID, workflowRunID, errorCode, at)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, nr := range s.nodeRuns[workflowRunID] {
		if nr.Status != RunStatusPending && nr.Status != RunStatusRunning {
			continue
		}
		nr.Status = RunStatusFailed
		nr.ErrorCode = errorCode
		nr.FinishedAt = &at
		n++
	}
	return n, nil
}

// claimRun claims one run for recovery from the given statuses (the store
// UPDATE is the atomic fence; the in-memory path mirrors it).
func (s *Service) claimRun(ctx context.Context, orgID, workflowRunID string, fromStatuses []string, at time.Time) (bool, error) {
	if s.store != nil {
		return s.store.ClaimWorkflowRun(ctx, orgID, workflowRunID, fromStatuses, at)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wr, ok := s.workflowRuns[workflowRunID]
	if !ok || wr.OrganizationID != orgID {
		return false, nil
	}
	allowed := false
	for _, st := range fromStatuses {
		if wr.Status == st {
			allowed = true
			break
		}
	}
	if !allowed || isTerminalRunStatus(wr.Status) {
		return false, nil
	}
	// Recovered work is live again: any claimed non-terminal run (pending
	// placeholders and rescued gates included) moves back to running.
	wr.Status = RunStatusRunning
	wr.Attempt++
	atCopy := at
	wr.LockedAt = &atCopy
	wr.HeartbeatAt = &atCopy
	wr.UpdatedAt = at
	return true, nil
}

// recoverWorkflowRun runs the stale-sweep state machine for one candidate.
func (s *Service) recoverWorkflowRun(ctx context.Context, wr *WorkflowRun, now time.Time) (bool, error) {
	if wr == nil {
		return false, nil
	}
	orgID := wr.OrganizationID

	// waiting_approval runs are alive by design while a human gate is
	// pending; they are only recoverable once no pending approval remains
	// (e.g. the decide flow resumed the linked run but the workflow run was
	// never flipped back).
	if wr.Status == RunStatusWaitingApproval {
		engine := s.engineValue()
		if engine.Approvals == nil {
			return false, nil // cannot verify gate state: leave the run alone
		}
		if s.hasPendingApproval(ctx, orgID, wr.ID) {
			return false, nil
		}
	}

	claimed, err := s.claimRun(ctx, orgID, wr.ID, []string{wr.Status}, now)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil // lost the race or the run went terminal meanwhile
	}

	// Orphan every pending/running node checkpoint: their workers are gone.
	if _, err := s.failNonTerminalNodeRuns(ctx, orgID, wr.ID, ErrorCodeNodeOrphaned, now); err != nil {
		return true, err
	}

	_, nodes, err := s.GetWorkflowRun(ctx, orgID, wr.ID)
	if err != nil {
		return true, nil // claimed but unreadable: retry next pass
	}
	wf, err := s.GetWorkflow(ctx, orgID, wr.WorkflowID)
	if err != nil {
		// The definition is gone: the run can never make progress again.
		if _, ferr := s.finalizeRun(ctx, orgID, wr.ID, RunStatusFailed, ErrorCodeNodeFailed, now); ferr != nil {
			return true, ferr
		}
		return true, nil
	}
	order, err := TopoOrder(wf.DSL)
	if err != nil {
		// DSL drift (cycle): finalize failed rather than retry forever.
		if _, ferr := s.finalizeRun(ctx, orgID, wr.ID, RunStatusFailed, ErrorCodeNodeFailed, now); ferr != nil {
			return true, ferr
		}
		return true, nil
	}
	nodesByID := nodeMap(wf.DSL)
	latest := latestNodeRunByNode(nodes)

	// A genuine (non-orphan) failure fails the whole run — mirrors the
	// executor's fail-fast semantics.
	for _, nodeID := range order {
		if nr := latest[nodeID]; nr != nil && nr.Status == RunStatusFailed && nr.ErrorCode != ErrorCodeNodeOrphaned {
			code := nr.ErrorCode
			if code == "" {
				code = ErrorCodeNodeFailed
			}
			if _, ferr := s.finalizeRun(ctx, orgID, wr.ID, RunStatusFailed, code, now); ferr != nil {
				return true, ferr
			}
			return true, nil
		}
	}

	// Walk the DAG: complete structural nodes, re-materialize approval gates
	// and re-enqueue the pending executable nodes (the static expansion fans
	// out to every non-terminal node, not only the first).
	lastRunID := ""
	for _, nodeID := range order {
		node := nodesByID[nodeID]
		nr := latest[nodeID]
		switch node.Type {
		case NodeAgent, NodeTool:
			if nr != nil && nr.RunID != "" {
				lastRunID = nr.RunID
			}
			if nr != nil && isTerminalNodeStatus(nr.Status) && !(nr.Status == RunStatusFailed && nr.ErrorCode == ErrorCodeNodeOrphaned) {
				continue // done (or genuinely failed, handled above)
			}
			if runID := s.reenqueueNode(ctx, wf, wr, node, nr, now); runID != "" {
				lastRunID = runID
			}
		case NodeApproval:
			if nr != nil && nr.Status == RunStatusWaitingApproval {
				// Gate in flight: decide-flow bookkeeping may have left the
				// checkpoint waiting after the decision was applied.
				if s.hasPendingApproval(ctx, orgID, wr.ID) {
					continue
				}
				if _, err := s.markNodeRunRow(ctx, orgID, nr.ID, RunStatusCompleted, "", now); err != nil {
					return true, err
				}
				if s.store != nil {
					s.mirrorNodeRunStatus(orgID, nr.ID, RunStatusCompleted, "", now)
				}
				latest[nodeID] = nil // treat as done below
				continue
			}
			if nr != nil && isTerminalNodeStatus(nr.Status) && !(nr.Status == RunStatusFailed && nr.ErrorCode == ErrorCodeNodeOrphaned) {
				continue
			}
			s.ensureApproval(ctx, wf, wr, node, lastRunID, now)
			if err := s.insertGateCheckpoint(ctx, orgID, wr.ID, nodeID, nr, now); err != nil {
				return true, err
			}
			// The run pauses on the (re-materialized) human gate.
			if uerr := s.UpdateWorkflowRunStatus(ctx, orgID, wr.ID, RunStatusWaitingApproval); uerr != nil {
				return true, uerr
			}
		default:
			// Structural nodes are pure bookkeeping: converge them now.
			if nr != nil && isTerminalNodeStatus(nr.Status) && !(nr.Status == RunStatusFailed && nr.ErrorCode == ErrorCodeNodeOrphaned) {
				continue
			}
			if err := s.completeStructuralCheckpoint(ctx, orgID, wr.ID, nodeID, nr, now); err != nil {
				return true, err
			}
		}
	}

	// Converge the run when nothing is left to execute.
	s.finalizeIfConverged(ctx, orgID, wr.ID, now)
	return true, nil
}

// reenqueueNode re-enqueues one pending agent/tool node through the existing
// queue interface: the previous attempt's run id is reused when present,
// otherwise a fresh agent run is created (engine.Runs). Returns the run id
// the node now rides ("" when the node could not be re-enqueued).
func (s *Service) reenqueueNode(ctx context.Context, wf *Workflow, wr *WorkflowRun, node Node, latest *NodeRun, now time.Time) string {
	engine := s.engineValue()
	if engine.Queue == nil {
		return ""
	}
	runID := ""
	if latest != nil {
		runID = latest.RunID
	}
	runInput := resolveTemplate(configString(node.Config, "input"), wr.Input)
	agentID := configString(node.Config, "agent_id")
	if agentID == "" && node.Type == NodeTool {
		// runs.agent_id carries an FK: the first agent node carries tool runs
		// (same fallback as the executor).
		agentID = firstAgentNodeID(wf.DSL)
	}
	if runID == "" && agentID != "" {
		if engine.Runs == nil {
			return ""
		}
		run, err := engine.Runs.CreateRunCtx(ctx, wr.OrganizationID, agentID, runInput)
		if err != nil {
			return "" // retried on the next pass (the claim keeps the lease)
		}
		runID = run.ID
	}
	enqueue(engine.Queue, taskTypeFor(node), wr.OrganizationID, node.Config, wr.ID, node.ID, runID, runInput)
	return runID
}

// ensureApproval re-materializes the approval gate of a node when the run has
// no pending approval left (a crash between checkpoint and request, or an
// already-decided gate that left no record).
func (s *Service) ensureApproval(ctx context.Context, wf *Workflow, wr *WorkflowRun, node Node, lastRunID string, now time.Time) {
	engine := s.engineValue()
	if engine.Approvals == nil {
		return
	}
	if s.hasPendingApproval(ctx, wr.OrganizationID, wr.ID) {
		return
	}
	action := configString(node.Config, "action")
	if action == "" {
		action = "workflow.continue"
	}
	reason := configString(node.Config, "reason")
	if reason == "" {
		reason = "approval gate: " + nodeDisplayName(node)
	}
	_, _ = engine.Approvals.Request(ctx, wr.OrganizationID, approvals.RequestInput{
		RunID:         lastRunID,
		WorkflowRunID: wr.ID,
		Resource:      wf.ID,
		Action:        action,
		Reason:        reason,
		Risk:          configString(node.Config, "risk"),
	})
}

// hasPendingApproval reports whether the workflow run still has a pending
// human gate. When the approvals service cannot be consulted the gate is
// assumed pending (fail safe: recovery never bypasses a human decision).
func (s *Service) hasPendingApproval(ctx context.Context, orgID, workflowRunID string) bool {
	engine := s.engineValue()
	if engine.Approvals == nil {
		return true
	}
	list, err := engine.Approvals.List(ctx, orgID, approvals.StatusPending)
	if err != nil {
		return true
	}
	for _, a := range list {
		if a != nil && a.WorkflowRunID == workflowRunID {
			return true
		}
	}
	return false
}

// insertGateCheckpoint writes the waiting_approval checkpoint of a
// (re-)materialized approval gate as a fresh attempt row.
func (s *Service) insertGateCheckpoint(ctx context.Context, orgID, workflowRunID, nodeID string, latest *NodeRun, at time.Time) error {
	attempt := 1
	if latest != nil {
		attempt = latest.Attempt + 1
	}
	nr := &NodeRun{
		ID:             uuid.NewString(),
		WorkflowRunID:  workflowRunID,
		NodeID:         nodeID,
		Status:         RunStatusWaitingApproval,
		Attempt:        attempt,
		StartedAt:      &at,
		CreatedAt:      at,
		OrganizationID: orgID,
	}
	if latest != nil {
		nr.RunID = latest.RunID
	}
	_, err := s.insertNodeRun(ctx, orgID, nr)
	return err
}

// completeStructuralCheckpoint converges one structural node: a fresh
// completed attempt row when the previous attempt was orphaned, an in-place
// completion when it was still pending/running, or the first checkpoint row.
func (s *Service) completeStructuralCheckpoint(ctx context.Context, orgID, workflowRunID, nodeID string, latest *NodeRun, at time.Time) error {
	if latest != nil && !isTerminalNodeStatus(latest.Status) {
		// pending/running placeholder: complete in place.
		if _, err := s.markNodeRunRow(ctx, orgID, latest.ID, RunStatusCompleted, "", at); err != nil {
			return err
		}
		if s.store != nil {
			s.mirrorNodeRunStatus(orgID, latest.ID, RunStatusCompleted, "", at)
		}
		return nil
	}
	attempt := 1
	if latest != nil { // orphaned failed row: keep it, start a fresh attempt
		attempt = latest.Attempt + 1
	}
	nr := &NodeRun{
		ID:             uuid.NewString(),
		WorkflowRunID:  workflowRunID,
		NodeID:         nodeID,
		Status:         RunStatusCompleted,
		Attempt:        attempt,
		StartedAt:      &at,
		FinishedAt:     &at,
		CreatedAt:      at,
		OrganizationID: orgID,
	}
	if latest != nil {
		nr.RunID = latest.RunID
	}
	_, err := s.insertNodeRun(ctx, orgID, nr)
	return err
}

// engineValue snapshots the execution engine.
func (s *Service) engineValue() Engine {
	if s == nil {
		return Engine{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

// ---------------------------------------------------------------------------
// RecoveryWorker: the cmd/worker recovery loop (startup pass + ticker).
// ---------------------------------------------------------------------------

// DefaultRecoveryInterval is the default cadence of the recovery loop.
const DefaultRecoveryInterval = time.Minute

// RecoveryWorker drives RecoverIncompleteWorkflowRuns on a fixed interval
// (plus one startup pass). It is safe to run in several processes: the
// store-level claim makes passes mutually exclusive per run.
type RecoveryWorker struct {
	svc      *Service
	interval time.Duration
}

// NewRecoveryWorker builds a recovery loop; a non-positive interval falls
// back to DefaultRecoveryInterval.
func NewRecoveryWorker(svc *Service, interval time.Duration) *RecoveryWorker {
	if interval <= 0 {
		interval = DefaultRecoveryInterval
	}
	return &RecoveryWorker{svc: svc, interval: interval}
}

// Interval returns the configured tick cadence.
func (w *RecoveryWorker) Interval() time.Duration {
	if w == nil {
		return DefaultRecoveryInterval
	}
	return w.interval
}

// RunOnce performs a single recovery sweep over every tenant.
func (w *RecoveryWorker) RunOnce(ctx context.Context) (int, error) {
	if w == nil || w.svc == nil {
		return 0, errors.New("workflows: recovery worker has no service")
	}
	return w.svc.RecoverIncompleteWorkflowRuns(ctx, "")
}

// Run blocks until ctx is cancelled: startup pass, then one sweep per tick.
// Passes are best-effort (a failing sweep is retried on the next tick);
// Run returns the context error.
func (w *RecoveryWorker) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_, _ = w.RunOnce(ctx) // startup pass
	ticker := time.NewTicker(w.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, _ = w.RunOnce(ctx)
		}
	}
}
