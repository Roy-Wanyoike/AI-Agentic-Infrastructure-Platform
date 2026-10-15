package approvals

// oversight_test.go — issue #137 enterprise oversight v2 coverage:
// delegation lifecycle + lazy expiry return, escalation SLA sweep (boundary:
// exactly at SLA = escalated) + lazy evaluation, dual control (two-person
// rule) for approve AND reject with the self-approval block, and the
// byte-deterministic evidence export (determinism, window filtering,
// no-mutation guarantee, org isolation). The existing wave-2 tests stay
// untouched and green.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/audit"
)

// recordingAuditor captures the best-effort audit rows the service writes
// through the Auditor seam.
type recordingAuditor struct {
	mu      sync.Mutex
	actions []string
	entries []*audit.Entry
}

func (r *recordingAuditor) LogCtx(_ context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := &audit.Entry{Actor: actor, Action: action, OrganizationID: organizationID, Resource: resource, Metadata: metadata}
	r.actions = append(r.actions, action)
	r.entries = append(r.entries, e)
	return e, nil
}

func (r *recordingAuditor) count(action string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.actions {
		if a == action {
			n++
		}
	}
	return n
}

func newOversightService(t *testing.T) (*Service, *recordingAuditor, *fakeRunController) {
	t.Helper()
	svc := NewService()
	aud := &recordingAuditor{}
	svc.SetAuditor(aud)
	controller := &fakeRunController{}
	svc.SetRunController(controller)
	return svc, aud, controller
}

func eventTypes(t *testing.T, svc *Service, orgID, approvalID string) []string {
	t.Helper()
	events, err := svc.Events(context.Background(), orgID, approvalID)
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	types := make([]string, 0, len(events))
	for _, e := range events {
		types = append(types, e.Type)
	}
	return types
}

// ---------------------------------------------------------------------------
// Request event + dual flag
// ---------------------------------------------------------------------------

func TestOversightRequestRecordsRequestedEvent(t *testing.T) {
	svc, aud, _ := newOversightService(t)
	ctx := context.Background()

	a, err := svc.Request(ctx, "org-1", RequestInput{Action: "deploy", Requester: "user-1", RequireDual: true, Risk: RiskHigh})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if !a.RequireDual {
		t.Fatal("RequireDual should round-trip onto the approval")
	}
	types := eventTypes(t, svc, "org-1", a.ID)
	if len(types) != 1 || types[0] != EventRequested {
		t.Fatalf("expected exactly one requested event, got %v", types)
	}
	if n := aud.count("approval.delegated"); n != 0 {
		t.Fatalf("plain request must not audit delegation, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Delegation
// ---------------------------------------------------------------------------

func TestDelegateLifecycleAndChain(t *testing.T) {
	svc, aud, _ := newOversightService(t)
	ctx := context.Background()

	a, err := svc.Request(ctx, "org-1", RequestInput{Action: "deploy", Requester: "user-1"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	approval, delegation, err := svc.Delegate(ctx, "org-1", a.ID, "user-2", "please handle", &expires, "user-1")
	if err != nil {
		t.Fatalf("Delegate returned error: %v", err)
	}
	if approval.DelegatedTo != "user-2" || approval.DelegatedBy != "user-1" || approval.DelegationNote != "please handle" {
		t.Fatalf("unexpected delegation stamp: %#v", approval)
	}
	if delegation.ToUser != "user-2" || delegation.FromUser != "user-1" || delegation.ExpiresAt == nil {
		t.Fatalf("unexpected chain entry: %#v", delegation)
	}
	chain, err := svc.Delegations(ctx, "org-1", a.ID)
	if err != nil || len(chain) != 1 || chain[0].ID != delegation.ID {
		t.Fatalf("chain mismatch: %v %v", chain, err)
	}
	if types := eventTypes(t, svc, "org-1", a.ID); len(types) != 2 || types[1] != EventDelegated {
		t.Fatalf("expected [requested delegated], got %v", types)
	}
	if n := aud.count("approval.delegated"); n != 1 {
		t.Fatalf("approval.delegated audit rows = %d, want 1", n)
	}

	// A delegated (still pending) approval stays decidable by anyone.
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "ok", "user-2"); err != nil {
		t.Fatalf("Decide on delegated approval returned error: %v", err)
	}
	// Delegating a decided approval is refused.
	if _, _, err := svc.Delegate(ctx, "org-1", a.ID, "user-3", "", nil, "user-1"); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("delegate on decided approval should be ErrAlreadyDecided, got %v", err)
	}
}

func TestDelegateValidationAndTenantGuard(t *testing.T) {
	svc, _, _ := newOversightService(t)
	ctx := context.Background()
	a, err := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if _, _, err := svc.Delegate(ctx, "org-1", a.ID, "  ", "", nil, "user-1"); !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("empty to_user should be ErrInvalidDelegation, got %v", err)
	}
	if _, _, err := svc.Delegate(ctx, "org-1", "missing", "user-2", "", nil, "user-1"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("unknown approval should be ErrApprovalNotFound, got %v", err)
	}
	// Org isolation: a foreign tenant cannot even see the approval to delegate it.
	if _, _, err := svc.Delegate(ctx, "org-2", a.ID, "user-2", "", nil, "user-9"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("cross-tenant delegate should be ErrApprovalNotFound, got %v", err)
	}
	if _, _, err := svc.Delegate(ctx, "", a.ID, "user-2", "", nil, "user-1"); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("missing org should be ErrOrgRequired, got %v", err)
	}
}

func TestDelegateExpiryLazyReturnOnGet(t *testing.T) {
	svc, aud, _ := newOversightService(t)
	ctx := context.Background()
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	past := time.Now().UTC().Add(-time.Minute)
	if _, _, err := svc.Delegate(ctx, "org-1", a.ID, "user-2", "", &past, "user-1"); err != nil {
		t.Fatalf("Delegate returned error: %v", err)
	}
	// No timers: nothing happens before the read.
	if a.DelegatedTo != "user-2" {
		t.Fatal("delegation should still be assigned before any read")
	}
	got, err := svc.Get(ctx, "org-1", a.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.DelegatedTo != "" || got.DelegatedBy != "" || got.DelegationExpiresAt != nil {
		t.Fatalf("expired delegation should lazily return to the queue, got %#v", got)
	}
	if got.Status != StatusPending {
		t.Fatalf("returned approval must stay pending, got %q", got.Status)
	}
	if types := eventTypes(t, svc, "org-1", a.ID); types[len(types)-1] != EventDelegationExpired {
		t.Fatalf("expected a delegation_expired event, got %v", types)
	}
	if n := aud.count("approval.delegation_expired"); n != 1 {
		t.Fatalf("approval.delegation_expired audit rows = %d, want 1", n)
	}
	// Idempotent: a second read adds no second expiry event.
	if _, err := svc.Get(ctx, "org-1", a.ID); err != nil {
		t.Fatalf("second Get returned error: %v", err)
	}
	if n := aud.count("approval.delegation_expired"); n != 1 {
		t.Fatalf("lazy expiry must be idempotent, audit rows = %d", n)
	}
	// Back in the queue: it can be delegated again.
	if _, _, err := svc.Delegate(ctx, "org-1", a.ID, "user-3", "requeue", nil, "user-1"); err != nil {
		t.Fatalf("re-delegation after expiry returned error: %v", err)
	}
}

func TestDelegateExpiryLazyReturnOnList(t *testing.T) {
	svc, _, _ := newOversightService(t)
	ctx := context.Background()
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	past := time.Now().UTC().Add(-time.Minute)
	if _, _, err := svc.Delegate(ctx, "org-1", a.ID, "user-2", "", &past, "user-1"); err != nil {
		t.Fatalf("Delegate returned error: %v", err)
	}
	list, err := svc.List(ctx, "org-1", StatusPending)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 1 || list[0].DelegatedTo != "" {
		t.Fatalf("List should lazily return the expired delegation, got %#v", list)
	}
}

// ---------------------------------------------------------------------------
// Dual control (two-person rule)
// ---------------------------------------------------------------------------

func TestDualControlFirstDecisionParks(t *testing.T) {
	svc, aud, controller := newOversightService(t)
	ctx := context.Background()
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy", RunID: "run-1", Requester: "user-1", RequireDual: true})

	// The requester can never be an approver on a dual-controlled approval.
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "", "user-1"); !errors.Is(err, ErrDualSelfApproval) {
		t.Fatalf("requester self-approval should be ErrDualSelfApproval, got %v", err)
	}
	if len(controller.resumed) != 0 {
		t.Fatalf("blocked decision must not resume a run, resumed=%v", controller.resumed)
	}

	decided, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "lgtm", "user-2")
	if err != nil {
		t.Fatalf("first decision returned error: %v", err)
	}
	if decided.Status != StatusPartiallyApproved || decided.FirstApprover != "user-2" || decided.FirstDecision != StatusApproved || decided.FirstDecidedAt == nil {
		t.Fatalf("unexpected first decision: %#v", decided)
	}
	if decided.DecidedAt != nil {
		t.Fatal("final decided_at must stay unset while partially_approved")
	}
	if len(controller.resumed) != 0 {
		t.Fatalf("first decision must not resume the run, resumed=%v", controller.resumed)
	}
	if types := eventTypes(t, svc, "org-1", a.ID); len(types) != 2 || types[1] != EventFirstDecision {
		t.Fatalf("expected [requested first_decision], got %v", types)
	}
	if n := aud.count("approval.first_decision"); n != 1 {
		t.Fatalf("approval.first_decision audit rows = %d, want 1", n)
	}
	// The parked approval is filterable by its new status marker.
	if list, err := svc.List(ctx, "org-1", StatusPartiallyApproved); err != nil || len(list) != 1 {
		t.Fatalf("expected 1 partially_approved approval, got %d err=%v", len(list), err)
	}
}

func TestDualControlCompletionApprove(t *testing.T) {
	svc, aud, controller := newOversightService(t)
	ctx := context.Background()
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy", RunID: "run-1", Requester: "user-1", RequireDual: true})
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "lgtm", "user-2"); err != nil {
		t.Fatalf("first decision returned error: %v", err)
	}
	// Same approver again -> machine-readable self-approval conflict.
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "", "user-2"); !errors.Is(err, ErrDualSelfApproval) {
		t.Fatalf("same-approver completion should be ErrDualSelfApproval, got %v", err)
	}
	completed, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "second look ok", "user-3")
	if err != nil {
		t.Fatalf("second decision returned error: %v", err)
	}
	if completed.Status != StatusApproved || completed.Approver != "user-3" || completed.DecidedAt == nil {
		t.Fatalf("unexpected completion: %#v", completed)
	}
	if completed.FirstApprover != "user-2" {
		t.Fatalf("the pair must stay visible on the final record: %#v", completed)
	}
	if len(controller.resumed) != 1 || controller.resumed[0] != "run-1" {
		t.Fatalf("completed approval should resume run-1 once, resumed=%v", controller.resumed)
	}
	if n := aud.count("approval.completed_dual"); n != 1 {
		t.Fatalf("approval.completed_dual audit rows = %d, want 1", n)
	}
	// Third decision attempts are refused.
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusRejected, "", "user-4"); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("post-completion decide should be ErrAlreadyDecided, got %v", err)
	}
}

func TestDualControlCompletionReject(t *testing.T) {
	svc, _, controller := newOversightService(t)
	ctx := context.Background()
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy", RunID: "run-1", RequireDual: true})
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "lgtm", "user-2"); err != nil {
		t.Fatalf("first decision returned error: %v", err)
	}
	// The second decision sets the final outcome — here a rejection.
	completed, err := svc.Decide(ctx, "org-1", a.ID, StatusRejected, "veto", "user-3")
	if err != nil {
		t.Fatalf("second decision returned error: %v", err)
	}
	if completed.Status != StatusRejected || completed.Approver != "user-3" {
		t.Fatalf("unexpected dual rejection: %#v", completed)
	}
	if len(controller.resumed) != 0 {
		t.Fatalf("dual rejection must not resume a run, resumed=%v", controller.resumed)
	}
}

func TestDualControlRequesterCannotCompleteEither(t *testing.T) {
	svc, _, _ := newOversightService(t)
	ctx := context.Background()
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy", Requester: "user-1", RequireDual: true})
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "", "user-2"); err != nil {
		t.Fatalf("first decision returned error: %v", err)
	}
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "", "user-1"); !errors.Is(err, ErrDualSelfApproval) {
		t.Fatalf("requester completion should be ErrDualSelfApproval, got %v", err)
	}
}

func TestNonDualDecideBehaviorUnchanged(t *testing.T) {
	svc, aud, controller := newOversightService(t)
	ctx := context.Background()
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy", RunID: "run-1"})
	decided, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "ok", "user-9")
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if decided.Status != StatusApproved || decided.FirstApprover != "" || decided.FirstDecision != "" {
		t.Fatalf("non-dual decide must not carry dual fields: %#v", decided)
	}
	if len(controller.resumed) != 1 {
		t.Fatalf("non-dual approval should resume its run, resumed=%v", controller.resumed)
	}
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusRejected, "", "user-8"); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("double decide should stay ErrAlreadyDecided, got %v", err)
	}
	// Plain decisions land in the event log as decided (dual:false) and, per
	// the issue, carry NO dedicated audit action.
	types := eventTypes(t, svc, "org-1", a.ID)
	if len(types) != 2 || types[1] != EventDecided {
		t.Fatalf("expected [requested decided], got %v", types)
	}
	if n := aud.count("approval.completed_dual") + aud.count("approval.first_decision"); n != 0 {
		t.Fatalf("non-dual decide must not write dual audit rows, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Escalation SLA
// ---------------------------------------------------------------------------

func TestEscalationPolicyValidationAndDefaults(t *testing.T) {
	svc, _, _ := newOversightService(t)
	ctx := context.Background()

	// Unset policy resolves to the disabled default.
	policy, err := svc.GetEscalationPolicy(ctx, "org-1")
	if err != nil {
		t.Fatalf("GetEscalationPolicy returned error: %v", err)
	}
	if policy.TargetStatus != StatusPending || policy.SLAMinutes != 0 {
		t.Fatalf("unexpected default policy: %#v", policy)
	}
	if _, err := svc.SetEscalationPolicy(ctx, "org-1", "approved", 30, "admin"); !errors.Is(err, ErrInvalidEscalationPolicy) {
		t.Fatalf("terminal target status should be invalid, got %v", err)
	}
	if _, err := svc.SetEscalationPolicy(ctx, "org-1", StatusPending, -1, "admin"); !errors.Is(err, ErrInvalidEscalationPolicy) {
		t.Fatalf("negative SLA should be invalid, got %v", err)
	}
	policy, err = svc.SetEscalationPolicy(ctx, "org-1", "", 45, "admin-1")
	if err != nil {
		t.Fatalf("SetEscalationPolicy returned error: %v", err)
	}
	if policy.TargetStatus != StatusPending || policy.SLAMinutes != 45 || policy.UpdatedBy != "admin-1" {
		t.Fatalf("unexpected stored policy: %#v", policy)
	}
}

func TestEscalateDueSweepBoundary(t *testing.T) {
	svc, aud, _ := newOversightService(t)
	ctx := context.Background()
	if _, err := svc.SetEscalationPolicy(ctx, "org-1", StatusPending, 30, "admin"); err != nil {
		t.Fatalf("SetEscalationPolicy returned error: %v", err)
	}
	fresh, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	due, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})

	// Below the SLA: nothing escalates.
	if escalated, err := svc.EscalateDue(ctx, "org-1", "admin"); err != nil || len(escalated) != 0 {
		t.Fatalf("fresh approvals must not escalate, got %d err=%v", len(escalated), err)
	}

	// Boundary: exactly at SLA = escalated (deadline inclusive).
	due.CreatedAt = time.Now().UTC().Add(-30 * time.Minute)
	escalated, err := svc.EscalateDue(ctx, "org-1", "admin")
	if err != nil {
		t.Fatalf("EscalateDue returned error: %v", err)
	}
	if len(escalated) != 1 || escalated[0].ID != due.ID {
		t.Fatalf("expected exactly the due approval escalated, got %d", len(escalated))
	}
	esc := escalated[0]
	if esc.EscalatedAt == nil || esc.EscalationReason != EscalationReasonSLABreach || esc.EscalationDeadline == nil {
		t.Fatalf("unexpected escalation stamp: %#v", esc)
	}
	if !esc.EscalationDeadline.Equal(esc.CreatedAt.Add(30 * time.Minute)) {
		t.Fatalf("deadline should be created_at + SLA, got %v", esc.EscalationDeadline)
	}
	if esc.Status != StatusPending {
		t.Fatalf("escalated approval must stay decidable (pending), got %q", esc.Status)
	}
	if types := eventTypes(t, svc, "org-1", due.ID); types[len(types)-1] != EventEscalated {
		t.Fatalf("expected an escalated event, got %v", types)
	}
	if n := aud.count("approval.escalated"); n != 1 {
		t.Fatalf("approval.escalated audit rows = %d, want 1", n)
	}

	// Idempotent: the second sweep reports nothing new.
	if escalated, err := svc.EscalateDue(ctx, "org-1", "admin"); err != nil || len(escalated) != 0 {
		t.Fatalf("second sweep must be a no-op, got %d err=%v", len(escalated), err)
	}
	// The fresh approval was never touched.
	freshGot, _ := svc.Get(ctx, "org-1", fresh.ID)
	if freshGot.EscalatedAt != nil {
		t.Fatalf("fresh approval must stay unescalated: %#v", freshGot)
	}
}

func TestEscalateDueDisabledOrWithoutPolicy(t *testing.T) {
	svc, _, _ := newOversightService(t)
	ctx := context.Background()
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	a.CreatedAt = time.Now().UTC().Add(-10 * time.Hour)
	if escalated, err := svc.EscalateDue(ctx, "org-1", "admin"); err != nil || len(escalated) != 0 {
		t.Fatalf("no policy -> no escalation, got %d err=%v", len(escalated), err)
	}
	if _, err := svc.SetEscalationPolicy(ctx, "org-1", StatusPending, 0, "admin"); err != nil {
		t.Fatalf("SetEscalationPolicy(0) returned error: %v", err)
	}
	if escalated, err := svc.EscalateDue(ctx, "org-1", "admin"); err != nil || len(escalated) != 0 {
		t.Fatalf("sla_minutes=0 disables the sweep, got %d err=%v", len(escalated), err)
	}
	// Reads stay lazy-free too.
	if got, _ := svc.Get(ctx, "org-1", a.ID); got.EscalatedAt != nil {
		t.Fatalf("disabled policy must not stamp escalations: %#v", got)
	}
}

func TestEscalationLazyOnGetAndList(t *testing.T) {
	svc, aud, _ := newOversightService(t)
	ctx := context.Background()
	if _, err := svc.SetEscalationPolicy(ctx, "org-1", StatusPending, 60, "admin"); err != nil {
		t.Fatalf("SetEscalationPolicy returned error: %v", err)
	}
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	b, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy"})
	a.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)

	got, err := svc.Get(ctx, "org-1", a.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.EscalatedAt == nil || got.EscalationReason != EscalationReasonSLABreach {
		t.Fatalf("Get should lazily stamp the SLA breach: %#v", got)
	}
	list, err := svc.List(ctx, "org-1", StatusPending)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	stamped := 0
	for _, item := range list {
		if item.EscalatedAt != nil {
			stamped++
		}
	}
	if stamped != 1 {
		t.Fatalf("List should lazily stamp exactly the breached approval, stamped=%d", stamped)
	}
	// Lazy escalation is IDEMPOTENT: Get stamps `a` (and audits) exactly
	// once; the later List observes a.EscalatedAt != nil and must not
	// duplicate the event or the audit row. `b` is fresh (SLA not reached).
	if n := aud.count("approval.escalated"); n != 1 {
		t.Fatalf("lazy escalation audit rows = %d, want 1 (idempotent)", n)
	}
	_ = b
}

func TestEscalationOrgIsolation(t *testing.T) {
	svc, _, _ := newOversightService(t)
	ctx := context.Background()
	if _, err := svc.SetEscalationPolicy(ctx, "org-1", StatusPending, 30, "admin"); err != nil {
		t.Fatalf("SetEscalationPolicy returned error: %v", err)
	}
	foreign, _ := svc.Request(ctx, "org-2", RequestInput{Action: "deploy"})
	foreign.CreatedAt = time.Now().UTC().Add(-24 * time.Hour)
	// The org-1 policy never reaches org-2 rows (sweep AND lazy read).
	if escalated, err := svc.EscalateDue(ctx, "org-2", "admin"); err != nil || len(escalated) != 0 {
		t.Fatalf("foreign org sweep must be empty, got %d err=%v", len(escalated), err)
	}
	got, _ := svc.Get(ctx, "org-2", foreign.ID)
	if got.EscalatedAt != nil {
		t.Fatalf("foreign org read must not escalate: %#v", got)
	}
}

func TestEscalationTargetsPartiallyApproved(t *testing.T) {
	svc, _, controller := newOversightService(t)
	ctx := context.Background()
	if _, err := svc.SetEscalationPolicy(ctx, "org-1", StatusPartiallyApproved, 15, "admin"); err != nil {
		t.Fatalf("SetEscalationPolicy returned error: %v", err)
	}
	a, _ := svc.Request(ctx, "org-1", RequestInput{Action: "deploy", RunID: "run-1", RequireDual: true})
	if _, err := svc.Decide(ctx, "org-1", a.ID, StatusApproved, "lgtm", "user-2"); err != nil {
		t.Fatalf("first decision returned error: %v", err)
	}
	a.CreatedAt = time.Now().UTC().Add(-20 * time.Minute) // backdate past the SLA
	escalated, err := svc.EscalateDue(ctx, orgID1, "admin")
	if err != nil {
		t.Fatalf("EscalateDue returned error: %v", err)
	}
	if len(escalated) != 1 || escalated[0].Status != StatusPartiallyApproved {
		t.Fatalf("expected the partially_approved approval escalated, got %#v", escalated)
	}
	if len(controller.resumed) != 0 {
		t.Fatalf("escalation must not resume runs, resumed=%v", controller.resumed)
	}
}

// ---------------------------------------------------------------------------
// Evidence export
// ---------------------------------------------------------------------------

// buildOversightFixture creates a deterministic-ish activity trail: a plain
// request+decide, a dual-controlled pair, a delegation and one SLA escalation.
func buildOversightFixture(t *testing.T, svc *Service) (plain, dual *Approval) {
	t.Helper()
	ctx := context.Background()
	var err error
	plain, err = svc.Request(ctx, orgID1, RequestInput{Action: "deploy", Requester: "user-1"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if _, err := svc.Decide(ctx, orgID1, plain.ID, StatusApproved, "ok", "user-9"); err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	dual, err = svc.Request(ctx, orgID1, RequestInput{Action: "deploy", Requester: "user-1", RequireDual: true})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if _, _, err := svc.Delegate(ctx, orgID1, dual.ID, "user-2", "please review", nil, "user-1"); err != nil {
		t.Fatalf("Delegate returned error: %v", err)
	}
	if _, err := svc.Decide(ctx, orgID1, dual.ID, StatusApproved, "first", "user-2"); err != nil {
		t.Fatalf("first decision returned error: %v", err)
	}
	if _, err := svc.Decide(ctx, orgID1, dual.ID, StatusApproved, "second", "user-3"); err != nil {
		t.Fatalf("second decision returned error: %v", err)
	}
	if _, err := svc.SetEscalationPolicy(ctx, orgID1, StatusPending, 30, "admin"); err != nil {
		t.Fatalf("SetEscalationPolicy returned error: %v", err)
	}
	esc, err := svc.Request(ctx, orgID1, RequestInput{Action: "rollback", Requester: "user-4"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	esc.CreatedAt = time.Now().UTC().Add(-45 * time.Minute)
	if escalated, err := svc.EscalateDue(ctx, orgID1, "admin"); err != nil || len(escalated) != 1 {
		t.Fatalf("fixture escalation failed: %d err=%v", len(escalated), err)
	}
	return plain, dual
}

const orgID1 = "org-1"

func TestEvidenceExportDeterminism(t *testing.T) {
	svc, _, _ := newOversightService(t)
	buildOversightFixture(t, svc)

	first, err := svc.ExportEvidenceJSONL(context.Background(), orgID1, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ExportEvidenceJSONL returned error: %v", err)
	}
	second, err := svc.ExportEvidenceJSONL(context.Background(), orgID1, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("second ExportEvidenceJSONL returned error: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two exports over unchanged data must be byte-identical")
	}
	// 3 requested + 1 decided (plain) + 1 delegated + 1 first_decision +
	// 1 decided (dual pair) + 1 escalated = 8 events + head line.
	lines := bytes.Split(bytes.TrimRight(first, "\n"), []byte("\n"))
	if len(lines) != 9 {
		t.Fatalf("expected 8 event lines + head, got %d", len(lines))
	}
	var head struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &head); err != nil {
		t.Fatalf("head line malformed: %v", err)
	}
	if head.Type != "evidence_head" || head.Count != 8 {
		t.Fatalf("unexpected evidence head: %+v", head)
	}
	// The dual pair is fully represented: first_decision + decided(dual).
	if !strings.Contains(string(first), `"type":"first_decision"`) || !strings.Contains(string(first), `"dual":true`) {
		t.Fatalf("dual-control pair missing from the export:\n%s", first)
	}
}

func TestEvidenceExportWindowFiltering(t *testing.T) {
	svc, _, _ := newOversightService(t)
	plain, _ := buildOversightFixture(t, svc)

	events, err := svc.Events(context.Background(), orgID1, "")
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(events) < 3 {
		t.Fatalf("fixture too small: %d events", len(events))
	}
	mid := events[2].CreatedAt

	// Events strictly before the midpoint only.
	before, err := svc.ExportEvidenceJSONL(context.Background(), orgID1, time.Time{}, mid.Add(-time.Nanosecond))
	if err != nil {
		t.Fatalf("windowed export returned error: %v", err)
	}
	var headBefore struct {
		Count int `json:"count"`
	}
	headLine := bytes.Split(bytes.TrimRight(before, "\n"), []byte("\n"))
	if err := json.Unmarshal(headLine[len(headLine)-1], &headBefore); err != nil {
		t.Fatalf("head malformed: %v", err)
	}
	if headBefore.Count != 2 {
		t.Fatalf("expected 2 events before the midpoint, got %d", headBefore.Count)
	}
	// Window from the plain approval's request onward excludes its own
	// requested event but keeps everything after.
	after, err := svc.ExportEvidenceJSONL(context.Background(), orgID1, plain.CreatedAt.Add(time.Nanosecond), time.Time{})
	if err != nil {
		t.Fatalf("open-start export returned error: %v", err)
	}
	if !bytes.Contains(after, []byte(`"type":"escalated"`)) {
		t.Fatalf("open-start export should keep later events:\n%s", after)
	}
	// Inverted window and blank org are refused.
	if _, err := svc.ExportEvidenceJSONL(context.Background(), orgID1, mid, mid.Add(-time.Hour)); !errors.Is(err, ErrInvalidEvidenceRange) {
		t.Fatalf("inverted window should be ErrInvalidEvidenceRange, got %v", err)
	}
	if _, err := svc.ExportEvidenceJSONL(context.Background(), "  ", time.Time{}, time.Time{}); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("blank org should be ErrOrgRequired, got %v", err)
	}
}

func TestEvidenceExportDoesNotMutateState(t *testing.T) {
	svc, _, _ := newOversightService(t)
	ctx := context.Background()
	// A delegated approval with an ALREADY expired delegation that has never
	// been read since, and a breached pending approval under an active policy.
	a, _ := svc.Request(ctx, orgID1, RequestInput{Action: "deploy"})
	past := time.Now().UTC().Add(-time.Hour)
	if _, _, err := svc.Delegate(ctx, orgID1, a.ID, "user-2", "", &past, "user-1"); err != nil {
		t.Fatalf("Delegate returned error: %v", err)
	}
	if _, err := svc.SetEscalationPolicy(ctx, orgID1, StatusPending, 30, "admin"); err != nil {
		t.Fatalf("SetEscalationPolicy returned error: %v", err)
	}
	b, _ := svc.Request(ctx, orgID1, RequestInput{Action: "deploy"})
	b.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)

	before, err := svc.ExportEvidenceJSONL(ctx, orgID1, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("export returned error: %v", err)
	}
	after, err := svc.ExportEvidenceJSONL(ctx, orgID1, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("second export returned error: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("export must not mutate the event log it is exporting")
	}
	if bytes.Contains(after, []byte(`"type":"delegation_expired"`)) {
		t.Fatal("export ran the lazy delegation expiry — it must be a pure read")
	}
	if bytes.Contains(after, []byte(`"type":"escalated"`)) {
		t.Fatal("export ran the lazy SLA evaluation — it must be a pure read")
	}
	// The underlying approvals are untouched too (lazy rules still pending):
	// read the RAW in-memory state, bypassing the lazy-evaluating Get/List
	// paths (Get itself applies the lazy rules by design, so it cannot be
	// used to observe the pre-read state).
	svc.mu.Lock()
	rawA, rawB := svc.approvals[a.ID], svc.approvals[b.ID]
	svc.mu.Unlock()
	if rawA == nil || rawA.DelegatedTo != "user-2" || rawA.DelegationExpiresAt == nil {
		t.Fatalf("export must not return the delegation to the queue: %#v", rawA)
	}
	if rawB == nil || rawB.EscalatedAt != nil {
		t.Fatalf("export must not stamp the SLA breach: %#v", rawB)
	}
	// A REAL read applies the lazy rules (sanity: they were never consumed
	// by the export — exactly once semantics hold).
	got, _ := svc.Get(ctx, orgID1, a.ID)
	if got.DelegatedTo != "" {
		t.Fatalf("lazy delegation expiry must still work after the export: %#v", got)
	}
	gotB, _ := svc.Get(ctx, orgID1, b.ID)
	if gotB.EscalatedAt == nil {
		t.Fatal("sanity: the lazy rules must still work on a real read after the export")
	}
}

func TestEvidenceEventsOrgScoped(t *testing.T) {
	svc, _, _ := newOversightService(t)
	ctx := context.Background()
	a, _ := svc.Request(ctx, orgID1, RequestInput{Action: "deploy"})
	b, _ := svc.Request(ctx, "org-2", RequestInput{Action: "deploy"})
	_ = b

	foreign, err := svc.ExportEvidenceJSONL(ctx, "org-2", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("foreign export returned error: %v", err)
	}
	if bytes.Contains(foreign, []byte(a.ID)) {
		t.Fatal("org-2 export must not contain org-1 events")
	}
	var head struct {
		Count int `json:"count"`
	}
	lines := bytes.Split(bytes.TrimRight(foreign, "\n"), []byte("\n"))
	if err := json.Unmarshal(lines[len(lines)-1], &head); err != nil {
		t.Fatalf("head malformed: %v", err)
	}
	if head.Count != 1 { // only org-2's own requested event
		t.Fatalf("expected exactly 1 org-2 event, got %d", head.Count)
	}
	events, err := svc.Events(ctx, orgID1, a.ID)
	if err != nil || len(events) != 1 {
		t.Fatalf("per-approval event filter mismatch: %d err=%v", len(events), err)
	}
}

// ---------------------------------------------------------------------------
// Seam safety
// ---------------------------------------------------------------------------

func TestOversightNilAuditorAndSeams(t *testing.T) {
	svc := NewService()
	svc.SetAuditor(nil) // must not panic
	ctx := context.Background()
	a, err := svc.Request(ctx, orgID1, RequestInput{Action: "deploy"})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if _, _, err := svc.Delegate(ctx, orgID1, a.ID, "user-2", "", nil, "user-1"); err != nil {
		t.Fatalf("Delegate without auditor returned error: %v", err)
	}
	if _, err := svc.EscalateDue(ctx, orgID1, "admin"); err != nil {
		t.Fatalf("EscalateDue without policy returned error: %v", err)
	}
}
