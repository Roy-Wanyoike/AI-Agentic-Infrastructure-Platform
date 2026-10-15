package runs

import (
	"context"
	"errors"
	"time"
)

// Run control statuses introduced by wave-2 track 2-a. The API contract pins
// the lowercase spelling; the legacy worker statuses (QUEUED/RUNNING/...)
// from service.go remain valid inputs and are normalized below.
//
// Issue #115: these constants are now the storage/wire spellings of the
// lifecycle states defined in lifecycle.go (StatePending, StatePaused, ...)
// and every control op below routes through the ENFORCED transition machine
// (TransitionRunStatus): illegal moves surface as *RunTransitionError
// wrapping the ErrInvalidTransition sentinel, racing writers lose with
// ErrStatusConflict against the store's optimistic guard.
const (
	StatusPending         RunStatus = "pending"
	StatusPaused          RunStatus = "paused"
	StatusWaitingApproval RunStatus = "waiting_approval"
	StatusCancelled       RunStatus = "cancelled"
	StatusTimeout         RunStatus = "timeout"
)

// ErrInvalidTransition is returned when a control-plane transition is not
// allowed from the run's current status (e.g. pausing a completed run).
// Typed rejections carry the offending pair: errors.As to
// *RunTransitionError (issue #115) for from/to; errors.Is keeps working.
var ErrInvalidTransition = errors.New("invalid run status transition")

// IsTerminalStatus reports whether the given status is final: a terminal run
// can no longer be paused, resumed or cancelled. Parsing is case-insensitive
// (ParseRunState); unknown values are never terminal (fail-closed callers
// decide how to treat them).
func IsTerminalStatus(s RunStatus) bool {
	state, ok := ParseRunState(string(s))
	if !ok {
		return false
	}
	return IsTerminalRunState(state)
}

// currentRunState parses the run's stored status onto a lifecycle state
// (fail-closed: the caller decides how to treat unknown values).
func currentRunState(run *Run) (RunState, bool) {
	if run == nil {
		return "", false
	}
	return ParseRunState(string(run.Status))
}

// CancelRun transitions a run to cancelled (idempotent). Issue #115
// semantics: a run that is currently RUNNING (in-flight, possibly in another
// process) first moves to cancelling — the graceful-unwind phase — and the
// executor confirms cancelling → cancelled after the current step completed
// and the partial results were recorded (issue #107 machinery, triggered
// through the RunControlRegistry when the executor lives in this process).
// Cancelling an already-cancelled (or already-cancelling) run is a no-op;
// cancelling a completed/failed/timed-out run is rejected with a
// *RunTransitionError. When the run belongs to a workflow node, the cancel
// cascades to the owning workflow run (best-effort). The tenant guard comes
// from orgID.
func (s *Service) CancelRun(ctx context.Context, orgID, id string) (*Run, error) {
	run, err := s.GetRunCtx(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	current, ok := currentRunState(run)
	if !ok {
		return nil, &RunTransitionError{From: RunState(run.Status), To: StateCancelled}
	}
	if current == StateCancelled || current == StateCancelling {
		// Idempotent: re-signalling an in-flight executor is harmless (the
		// runtime graceful-unwind path is idempotent); nothing left to do.
		s.signalInFlight(ctx, id, RunController.Cancel)
		return run, nil
	}
	if IsTerminalRunState(current) {
		return nil, &RunTransitionError{From: current, To: StateCancelled}
	}
	// In-flight (running) cancels always take the two-phase route so the
	// partial results of the graceful unwind land before the terminal write;
	// every other state cancels directly (nothing is executing).
	target := StateCancelled
	if current == StateRunning {
		target = StateCancelling
	}
	if err := s.TransitionRunStatus(ctx, orgID, id, target, ""); err != nil {
		return nil, err
	}
	if target == StateCancelling {
		s.signalInFlight(ctx, id, RunController.Cancel)
	} else {
		// Best-effort workflow cascade after the terminal transition landed.
		_ = s.cascadeRunCancelled(ctx, run.OrganizationID, id)
	}
	run.Status = RunStatus(target)
	run.UpdatedAt = time.Now().UTC()
	return run, nil
}

// PauseRun transitions a queued/pending/running/waiting_approval run to
// paused (idempotent). Issue #115 semantics: the durable transition lands
// first (the optimistic store guard rejects racing terminal writes), then an
// in-flight executor registered in the RunControlRegistry is signalled to
// halt at its current step boundary — the current step completes, partial
// results are preserved (issue #107 machinery) and the run stays paused until
// an explicit resume. When the run is not executing in this process the
// signal is a no-op and the executor observes the paused status through its
// own seam (control poller / outcome handler). When the run belongs to a
// workflow node, the pause cascades to the owning workflow run
// (best-effort).
func (s *Service) PauseRun(ctx context.Context, orgID, id string) (*Run, error) {
	run, err := s.GetRunCtx(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	current, ok := currentRunState(run)
	if !ok {
		return nil, &RunTransitionError{From: RunState(run.Status), To: StatePaused}
	}
	if current == StatePaused {
		return run, nil
	}
	if IsTerminalRunState(current) {
		return nil, &RunTransitionError{From: current, To: StatePaused}
	}
	if err := s.TransitionRunStatus(ctx, orgID, id, StatePaused, ""); err != nil {
		return nil, err
	}
	s.signalInFlight(ctx, id, RunController.Pause)
	_ = s.cascadeRunPaused(ctx, run.OrganizationID, id)
	run.Status = RunStatus(StatePaused)
	run.UpdatedAt = time.Now().UTC()
	return run, nil
}

// ResumeRun transitions a paused or waiting_approval run back to pending so
// it can be re-enqueued by the caller (idempotent for already-pending runs).
// Waiting-approval runs are resumed through the approval decision: the
// approvals service decides the linked approval and its RunController calls
// this method (issue #75 completes that flow — policy-gated runs parked in
// waiting_approval return to pending when the operator approves).
//
// Issue #115: the public resume ENDPOINT uses ResumeRunQueued instead —
// resuming an operator-paused run re-enters the queue as queued (the row
// reflects the enqueued state) and never bypasses a waiting_approval gate.
func (s *Service) ResumeRun(ctx context.Context, orgID, id string) (*Run, error) {
	run, err := s.GetRunCtx(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	current, ok := currentRunState(run)
	if !ok {
		return nil, &RunTransitionError{From: RunState(run.Status), To: StatePending}
	}
	if current == StatePending {
		return run, nil
	}
	if current != StatePaused && current != StateWaitingApproval {
		return nil, &RunTransitionError{From: current, To: StatePending}
	}
	if err := s.TransitionRunStatus(ctx, orgID, id, StatePending, ""); err != nil {
		return nil, err
	}
	run.Status = RunStatus(StatePending)
	run.UpdatedAt = time.Now().UTC()
	return run, nil
}

// ResumeRunQueued is the issue #115 resume endpoint semantic: a paused run
// returns to queued (the durable row reflects the enqueued state; the caller
// re-enqueues the agent.run task right after — the queue is the only
// execution trigger). Idempotent for already-queued runs. waiting_approval
// runs are deliberately NOT resumable through this path: bypassing the
// approval gate would defeat the governance flow — only an approval decision
// (ResumeRun via the approvals RunController) may lift that gate. Resuming
// any other state is rejected with a *RunTransitionError.
func (s *Service) ResumeRunQueued(ctx context.Context, orgID, id string) (*Run, error) {
	run, err := s.GetRunCtx(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	current, ok := currentRunState(run)
	if !ok {
		return nil, &RunTransitionError{From: RunState(run.Status), To: StateQueued}
	}
	if current == StateQueued {
		return run, nil
	}
	if current != StatePaused {
		return nil, &RunTransitionError{From: current, To: StateQueued}
	}
	if err := s.TransitionRunStatus(ctx, orgID, id, StateQueued, ""); err != nil {
		return nil, err
	}
	run.Status = RunStatus(StateQueued)
	run.UpdatedAt = time.Now().UTC()
	return run, nil
}
