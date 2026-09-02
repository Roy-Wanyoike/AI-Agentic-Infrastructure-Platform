package runs

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Run control statuses introduced by wave-2 track 2-a. The API contract pins
// the lowercase spelling; the legacy worker statuses (QUEUED/RUNNING/...)
// from service.go remain valid inputs and are normalized below.
const (
	StatusPending         RunStatus = "pending"
	StatusPaused          RunStatus = "paused"
	StatusWaitingApproval RunStatus = "waiting_approval"
	StatusCancelled       RunStatus = "cancelled"
	StatusTimeout         RunStatus = "timeout"
)

// ErrInvalidTransition is returned when a control-plane transition is not
// allowed from the run's current status (e.g. pausing a completed run).
var ErrInvalidTransition = errors.New("invalid run status transition")

// normalizeRunStatus maps every accepted status spelling (the legacy uppercase
// worker statuses and the lowercase contract set) onto the lowercase
// contract-canonical value used by the control-plane transitions.
func normalizeRunStatus(s RunStatus) RunStatus {
	switch RunStatus(strings.ToLower(strings.TrimSpace(string(s)))) {
	case "queued", "pending":
		return StatusPending
	case "running":
		return RunStatus("running")
	case "paused":
		return StatusPaused
	case "waiting_approval":
		return StatusWaitingApproval
	case "completed":
		return RunStatus("completed")
	case "failed":
		return RunStatus("failed")
	case "cancelled":
		return StatusCancelled
	case "timeout":
		return StatusTimeout
	}
	return s
}

// IsTerminalStatus reports whether the given status is final: a terminal run
// can no longer be paused, resumed or cancelled.
func IsTerminalStatus(s RunStatus) bool {
	switch normalizeRunStatus(s) {
	case RunStatus("completed"), RunStatus("failed"), StatusCancelled, StatusTimeout:
		return true
	}
	return false
}

// CancelRun transitions a run to cancelled (idempotent). Cancelling an already
// cancelled run is a no-op; cancelling a completed/failed/timed-out run is
// rejected with ErrInvalidTransition. The tenant guard comes from orgID.
func (s *Service) CancelRun(ctx context.Context, orgID, id string) (*Run, error) {
	run, err := s.GetRunCtx(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	current := normalizeRunStatus(run.Status)
	if current == StatusCancelled {
		return run, nil
	}
	if IsTerminalStatus(current) {
		return nil, ErrInvalidTransition
	}
	if err := s.UpdateStatusCtx(ctx, orgID, id, StatusCancelled, ""); err != nil {
		return nil, err
	}
	run.Status = StatusCancelled
	run.UpdatedAt = time.Now().UTC()
	return run, nil
}

// PauseRun transitions a queued/pending/running/waiting_approval run to
// paused (idempotent). Workers observe the paused status and stop processing.
func (s *Service) PauseRun(ctx context.Context, orgID, id string) (*Run, error) {
	run, err := s.GetRunCtx(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	current := normalizeRunStatus(run.Status)
	if current == StatusPaused {
		return run, nil
	}
	if IsTerminalStatus(current) {
		return nil, ErrInvalidTransition
	}
	if err := s.UpdateStatusCtx(ctx, orgID, id, StatusPaused, ""); err != nil {
		return nil, err
	}
	run.Status = StatusPaused
	run.UpdatedAt = time.Now().UTC()
	return run, nil
}

// ResumeRun transitions a paused run back to pending so it can be re-enqueued
// by the caller (idempotent for already-pending runs). Runs that are waiting
// on an approval must be resumed through the approval decision instead.
func (s *Service) ResumeRun(ctx context.Context, orgID, id string) (*Run, error) {
	run, err := s.GetRunCtx(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	current := normalizeRunStatus(run.Status)
	if current == StatusPending {
		return run, nil
	}
	if current != StatusPaused {
		return nil, ErrInvalidTransition
	}
	if err := s.UpdateStatusCtx(ctx, orgID, id, StatusPending, ""); err != nil {
		return nil, err
	}
	run.Status = StatusPending
	run.UpdatedAt = time.Now().UTC()
	return run, nil
}
