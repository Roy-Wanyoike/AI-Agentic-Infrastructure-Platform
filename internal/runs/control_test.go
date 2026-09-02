package runs

import (
	"context"
	"errors"
	"testing"
)

func TestCancelRunIdempotent(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}

	cancelled, err := svc.CancelRun(ctx, "org-1", run.ID)
	if err != nil {
		t.Fatalf("CancelRun returned error: %v", err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("expected cancelled, got %q", cancelled.Status)
	}

	// Idempotent: cancelling again is a no-op, not an error.
	again, err := svc.CancelRun(ctx, "org-1", run.ID)
	if err != nil {
		t.Fatalf("idempotent CancelRun returned error: %v", err)
	}
	if again.Status != StatusCancelled {
		t.Fatalf("expected cancelled, got %q", again.Status)
	}

	stored, err := svc.GetRunCtx(ctx, "org-1", run.ID)
	if err != nil || stored.Status != StatusCancelled {
		t.Fatalf("cancelled status not persisted: %#v err=%v", stored, err)
	}
}

func TestCancelRunTerminalRejected(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	for _, status := range []RunStatus{StatusCompleted, StatusFailed, StatusTimeout} {
		run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
		if err != nil {
			t.Fatalf("CreateRunCtx returned error: %v", err)
		}
		if err := svc.UpdateStatusCtx(ctx, "org-1", run.ID, status, ""); err != nil {
			t.Fatalf("UpdateStatusCtx returned error: %v", err)
		}
		if _, err := svc.CancelRun(ctx, "org-1", run.ID); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("cancelling a %s run should be ErrInvalidTransition, got %v", status, err)
		}
	}
}

func TestPauseRunTransitions(t *testing.T) {
	svc := NewService()
	ctx := context.Background()

	// queued/pending/running/waiting_approval can all be paused.
	for _, seed := range []RunStatus{StatusQueued, StatusPending, StatusRunning, StatusWaitingApproval} {
		run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
		if err != nil {
			t.Fatalf("CreateRunCtx returned error: %v", err)
		}
		if err := svc.UpdateStatusCtx(ctx, "org-1", run.ID, seed, ""); err != nil {
			t.Fatalf("UpdateStatusCtx returned error: %v", err)
		}
		paused, err := svc.PauseRun(ctx, "org-1", run.ID)
		if err != nil {
			t.Fatalf("PauseRun from %s returned error: %v", seed, err)
		}
		if paused.Status != StatusPaused {
			t.Fatalf("expected paused, got %q", paused.Status)
		}
		// Idempotent.
		if _, err := svc.PauseRun(ctx, "org-1", run.ID); err != nil {
			t.Fatalf("idempotent PauseRun returned error: %v", err)
		}
	}

	// Terminal runs cannot be paused.
	done, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}
	if err := svc.UpdateStatusCtx(ctx, "org-1", done.ID, StatusCompleted, ""); err != nil {
		t.Fatalf("UpdateStatusCtx returned error: %v", err)
	}
	if _, err := svc.PauseRun(ctx, "org-1", done.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pausing a completed run should be ErrInvalidTransition, got %v", err)
	}
}

func TestResumeRunTransitions(t *testing.T) {
	svc := NewService()
	ctx := context.Background()

	run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}
	// Resume on a non-paused, non-pending run is invalid.
	if err := svc.UpdateStatusCtx(ctx, "org-1", run.ID, StatusRunning, ""); err != nil {
		t.Fatalf("UpdateStatusCtx returned error: %v", err)
	}
	if _, err := svc.ResumeRun(ctx, "org-1", run.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resuming a running run should be ErrInvalidTransition, got %v", err)
	}

	// paused -> pending.
	if _, err := svc.PauseRun(ctx, "org-1", run.ID); err != nil {
		t.Fatalf("PauseRun returned error: %v", err)
	}
	resumed, err := svc.ResumeRun(ctx, "org-1", run.ID)
	if err != nil {
		t.Fatalf("ResumeRun returned error: %v", err)
	}
	if resumed.Status != StatusPending {
		t.Fatalf("expected pending after resume, got %q", resumed.Status)
	}

	// Idempotent on already-pending runs.
	if _, err := svc.ResumeRun(ctx, "org-1", run.ID); err != nil {
		t.Fatalf("idempotent ResumeRun returned error: %v", err)
	}

	// Terminal runs cannot be resumed.
	if err := svc.UpdateStatusCtx(ctx, "org-1", run.ID, StatusCancelled, ""); err != nil {
		t.Fatalf("UpdateStatusCtx returned error: %v", err)
	}
	if _, err := svc.ResumeRun(ctx, "org-1", run.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resuming a cancelled run should be ErrInvalidTransition, got %v", err)
	}
}

func TestRunControlTenantGuard(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}
	if _, err := svc.CancelRun(ctx, "org-2", run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-tenant cancel should be ErrRunNotFound, got %v", err)
	}
	if _, err := svc.PauseRun(ctx, "org-2", run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-tenant pause should be ErrRunNotFound, got %v", err)
	}
	if _, err := svc.ResumeRun(ctx, "org-2", run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-tenant resume should be ErrRunNotFound, got %v", err)
	}
}

func TestIsTerminalStatus(t *testing.T) {
	terminal := []RunStatus{"COMPLETED", "completed", "FAILED", "failed", "CANCELLED", "cancelled", "timeout"}
	for _, s := range terminal {
		if !IsTerminalStatus(s) {
			t.Fatalf("%q should be terminal", s)
		}
	}
	nonTerminal := []RunStatus{"QUEUED", "pending", "RUNNING", "running", "paused", "waiting_approval"}
	for _, s := range nonTerminal {
		if IsTerminalStatus(s) {
			t.Fatalf("%q should not be terminal", s)
		}
	}
}
