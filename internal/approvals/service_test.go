package approvals

import (
	"context"
	"errors"
	"testing"

	"agentos/internal/runs"
)

// fakeRunController records ResumeRun calls instead of touching the runs
// service, letting the tests assert exactly when a decision resumes a run.
type fakeRunController struct {
	resumed []string
	err     error
}

func (f *fakeRunController) ResumeRun(_ context.Context, _ string, runID string) (*runs.Run, error) {
	f.resumed = append(f.resumed, runID)
	return &runs.Run{ID: runID}, f.err
}

func TestApprovalsRequestDefaultsAndListing(t *testing.T) {
	svc := NewService()
	ctx := context.Background()

	a, err := svc.Request(ctx, "org-1", RequestInput{RunID: "run-1", Action: "deploy", Risk: RiskHigh, Requester: "user-1"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if a.Status != StatusPending || a.Risk != RiskHigh || a.ID == "" {
		t.Fatalf("unexpected approval: %#v", a)
	}

	// Missing risk defaults to medium; other fields are carried through.
	b, err := svc.Request(ctx, "org-1", RequestInput{WorkflowRunID: "wr-1", Reason: "gate"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if b.Risk != RiskMedium {
		t.Fatalf("missing risk should default to medium, got %q", b.Risk)
	}

	all, err := svc.List(ctx, "org-1", "")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 approvals, got %d", len(all))
	}
	pending, err := svc.List(ctx, "org-1", StatusPending)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending approvals, got %d", len(pending))
	}
	foreign, err := svc.List(ctx, "org-2", "")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(foreign) != 0 {
		t.Fatalf("cross-tenant list should be empty, got %d", len(foreign))
	}
	if _, err := svc.List(ctx, "", ""); err == nil {
		t.Fatal("List without organization id should fail")
	}
	if _, err := svc.Request(ctx, "", RequestInput{}); err == nil {
		t.Fatal("Request without organization id should fail")
	}
}

func TestApprovalsGetTenantGuard(t *testing.T) {
	svc := NewService()
	ctx := context.Background()

	a, err := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if _, err := svc.Get(ctx, "org-1", a.ID); err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if _, err := svc.Get(ctx, "org-2", a.ID); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("cross-tenant Get should be ErrApprovalNotFound, got %v", err)
	}
	if _, err := svc.Get(ctx, "org-1", ""); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("empty id should be ErrApprovalNotFound, got %v", err)
	}
}

func TestApprovalsDecideLifecycle(t *testing.T) {
	svc := NewService()
	controller := &fakeRunController{}
	svc.SetRunController(controller)
	ctx := context.Background()

	approved, err := svc.Request(ctx, "org-1", RequestInput{RunID: "run-1", Action: "deploy"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	rejected, err := svc.Request(ctx, "org-1", RequestInput{RunID: "run-2", Action: "deploy"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	cancelled, err := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}

	// Invalid decisions are rejected before touching the record.
	for _, decision := range []string{"", "maybe", "APPROVE"} {
		if _, err := svc.Decide(ctx, "org-1", approved.ID, decision, "", "user-9"); !errors.Is(err, ErrInvalidDecision) {
			t.Fatalf("decision %q should be ErrInvalidDecision, got %v", decision, err)
		}
	}

	// Approve: approver + decided_at recorded, linked paused run resumed.
	decided, err := svc.Decide(ctx, "org-1", approved.ID, "APPROVED", "ship it", "user-9")
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if decided.Status != StatusApproved || decided.Approver != "user-9" || decided.DecisionReason != "ship it" {
		t.Fatalf("unexpected decided approval: %#v", decided)
	}
	if decided.DecidedAt == nil {
		t.Fatal("decided_at should be set")
	}
	if len(controller.resumed) != 1 || controller.resumed[0] != "run-1" {
		t.Fatalf("approved approval should resume run-1, resumed=%v", controller.resumed)
	}

	// Second decision on the same approval -> conflict.
	if _, err := svc.Decide(ctx, "org-1", approved.ID, StatusRejected, "", "user-9"); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("double decide should be ErrAlreadyDecided, got %v", err)
	}

	// Reject: no resume call happens.
	if _, err := svc.Decide(ctx, "org-1", rejected.ID, StatusRejected, "nope", "user-9"); err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if len(controller.resumed) != 1 {
		t.Fatalf("rejected decision must not resume a run, resumed=%v", controller.resumed)
	}

	// Cancel a pending approval.
	if _, err := svc.Cancel(ctx, "org-1", cancelled.ID); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	got, err := svc.Get(ctx, "org-1", cancelled.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.Status != StatusCancelled || got.DecidedAt == nil {
		t.Fatalf("cancelled approval mismatch: %#v", got)
	}
	if _, err := svc.Cancel(ctx, "org-1", cancelled.ID); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("double cancel should be ErrAlreadyDecided, got %v", err)
	}

	// Status filter reflects the lifecycle.
	if list, err := svc.List(ctx, "org-1", StatusApproved); err != nil || len(list) != 1 {
		t.Fatalf("expected exactly 1 approved approval, got %d err=%v", len(list), err)
	}
	if list, err := svc.List(ctx, "org-1", StatusPending); err != nil || len(list) != 0 {
		t.Fatalf("expected no pending approvals left, got %d err=%v", len(list), err)
	}

	// Cross-tenant and unknown ids.
	if _, err := svc.Decide(ctx, "org-2", rejected.ID, StatusApproved, "", "user-9"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("cross-tenant decide should be ErrApprovalNotFound, got %v", err)
	}
	if _, err := svc.Decide(ctx, "org-1", "missing", StatusApproved, "", "user-9"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("unknown approval decide should be ErrApprovalNotFound, got %v", err)
	}
}

func TestApprovalsDecideWithoutRunControllerOrRunID(t *testing.T) {
	// No controller wired at all: the decision still lands.
	svc := NewService()
	ctx := context.Background()
	a, err := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "ok", "user-9"); err != nil {
		t.Fatalf("Decide without controller returned error: %v", err)
	}

	// Controller wired but approval carries no run id: no resume attempted.
	controller := &fakeRunController{}
	svc2 := NewService()
	svc2.SetRunController(controller)
	b, err := svc2.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if _, err := svc2.Decide(ctx, "org-1", b.ID, StatusApproved, "ok", "user-9"); err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if len(controller.resumed) != 0 {
		t.Fatalf("approval without run_id must not resume anything, resumed=%v", controller.resumed)
	}
}
