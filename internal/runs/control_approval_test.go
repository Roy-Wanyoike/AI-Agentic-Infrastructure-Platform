package runs

// Issue #75: the approval-decision resume path. Policy-gated runs park in
// waiting_approval until the linked approval is decided; approvals.Service
// then calls ResumeRun through its RunController, so resuming from
// waiting_approval must land the run back in pending (the state the queue
// consumer re-enqueues).

import (
	"context"
	"errors"
	"testing"
)

func TestResumeRunFromWaitingApproval(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}
	if err := svc.UpdateStatusCtx(ctx, "org-1", run.ID, StatusWaitingApproval, ""); err != nil {
		t.Fatalf("UpdateStatusCtx returned error: %v", err)
	}

	resumed, err := svc.ResumeRun(ctx, "org-1", run.ID)
	if err != nil {
		t.Fatalf("ResumeRun from waiting_approval returned error: %v", err)
	}
	if resumed.Status != StatusPending {
		t.Fatalf("expected pending after approval resume, got %q", resumed.Status)
	}
	stored, err := svc.GetRunCtx(ctx, "org-1", run.ID)
	if err != nil || stored.Status != StatusPending {
		t.Fatalf("resumed status not persisted: %#v err=%v", stored, err)
	}

	// Idempotent: resuming an already-pending run is a no-op.
	if _, err := svc.ResumeRun(ctx, "org-1", run.ID); err != nil {
		t.Fatalf("idempotent ResumeRun returned error: %v", err)
	}
}

func TestResumeRunFromWaitingApprovalTenantGuard(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}
	if err := svc.UpdateStatusCtx(ctx, "org-1", run.ID, StatusWaitingApproval, ""); err != nil {
		t.Fatalf("UpdateStatusCtx returned error: %v", err)
	}
	if _, err := svc.ResumeRun(ctx, "org-2", run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-tenant resume = %v, want ErrRunNotFound", err)
	}
	// The foreign resume must not have touched the run.
	stored, err := svc.GetRunCtx(ctx, "org-1", run.ID)
	if err != nil || stored.Status != StatusWaitingApproval {
		t.Fatalf("run mutated by cross-tenant resume: %#v err=%v", stored, err)
	}
}

func TestResumeRunStillRejectsRunning(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}
	if err := svc.UpdateStatusCtx(ctx, "org-1", run.ID, StatusRunning, ""); err != nil {
		t.Fatalf("UpdateStatusCtx returned error: %v", err)
	}
	if _, err := svc.ResumeRun(ctx, "org-1", run.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resuming a running run = %v, want ErrInvalidTransition", err)
	}
}
