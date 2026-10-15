package runs

// lifecycle_test.go — issue #115: the enforced run lifecycle state machine.
// Pins the authoritative transition table (valid + invalid moves), the typed
// *RunTransitionError, the service-level enforcement (TransitionRunStatus),
// the control-plane event emissions, the optimistic store guard
// (ErrStatusConflict) and the RunControlRegistry act-on-in-flight semantics.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"agentos/internal/events"
)

// wantTransitions is the issue #115 contract table (mirrors
// runStateTransitions; TestRunTransitionsTable pins the code against it so a
// typo cannot silently widen or close a legal move).
var wantTransitions = map[RunState][]RunState{
	StatePending:         {StateQueued, StateRunning, StatePaused, StateCancelling, StateCancelled},
	StateQueued:          {StateRunning, StatePaused, StateCancelling, StateCancelled},
	StateRunning:         {StatePaused, StateCancelling, StateCompleted, StateFailed},
	StatePaused:          {StateQueued, StatePending, StateCancelling, StateCancelled, StateCompleted, StateFailed},
	StateWaitingApproval: {StateQueued, StatePending, StatePaused, StateCancelling, StateCancelled},
	StateCancelling:      {StateCancelled, StateFailed},
	StateCompleted:       {},
	StateFailed:          {},
	StateCancelled:       {},
	StateTimeout:         {},
}

func TestRunTransitionsTable(t *testing.T) {
	states := AllRunStates()
	if len(states) != len(wantTransitions) {
		t.Fatalf("AllRunStates returned %d states, want %d", len(states), len(wantTransitions))
	}
	for _, from := range states {
		got := AllowedRunTransitions(from)
		want := wantTransitions[from]
		if len(got) != len(want) {
			t.Fatalf("%s: allowed = %v, want %v", from, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: allowed[%d] = %s, want %s (table order is contract)", from, i, got[i], want[i])
			}
		}
	}
	// Spot invalid moves in both directions (sampled — the table above is the
	// exhaustive pin; these guard against accidental superset behavior).
	invalid := []struct{ from, to RunState }{
		{StateCompleted, StateRunning}, {StateCompleted, StateCancelled},
		{StateCancelled, StateRunning}, {StateTimeout, StateQueued},
		{StateFailed, StatePaused}, {StateRunning, StatePending},
		{StateRunning, StateQueued}, {StatePaused, StateRunning},
		{StateCancelling, StateCompleted}, {StateCancelling, StatePaused},
		{StateWaitingApproval, StateCompleted}, {StatePending, StateFailed},
	}
	for _, tc := range invalid {
		if CanTransitionRunState(tc.from, tc.to) {
			t.Errorf("%s → %s must be illegal", tc.from, tc.to)
		}
	}
}

func TestCanTransitionRunStateUnknownStates(t *testing.T) {
	for _, from := range []RunState{"", "WEIRD", "completed "} {
		if CanTransitionRunState(from, StateRunning) {
			t.Errorf("unknown from %q must never transition", from)
		}
	}
	if CanTransitionRunState(StateRunning, "") || CanTransitionRunState(StateRunning, "nope") {
		t.Error("unknown targets must never transition")
	}
}

func TestParseRunStateNormalizes(t *testing.T) {
	cases := map[string]RunState{
		"queued":           StateQueued,
		"QUEUED":           StateQueued,
		" Running ":        StateRunning,
		"CANCELLING":       StateCancelling,
		"WAITING_APPROVAL": StateWaitingApproval,
		"waiting_approval": StateWaitingApproval,
		"timeout":          StateTimeout,
	}
	for raw, want := range cases {
		got, ok := ParseRunState(raw)
		if !ok || got != want {
			t.Errorf("ParseRunState(%q) = %q,%v want %q,true", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "   ", "finished", "COMPLEETED"} {
		if _, ok := ParseRunState(raw); ok {
			t.Errorf("ParseRunState(%q) must not parse", raw)
		}
	}
}

func TestRunTransitionErrorTyped(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx: %v", err)
	}
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StateCompleted, ""); err == nil {
		t.Fatalf("queued → completed must be rejected by the table, got success")
	}
	err = svc.TransitionRunStatus(ctx, "org-1", run.ID, StateCompleted, "")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition sentinel, got %v", err)
	}
	var typed *RunTransitionError
	if !errors.As(err, &typed) {
		t.Fatalf("want *RunTransitionError, got %T", err)
	}
	if typed.From != StateQueued || typed.To != StateCompleted {
		t.Fatalf("typed pair = %s → %s, want queued → completed", typed.From, typed.To)
	}
	if !strings.Contains(typed.Error(), "allowed: running, paused, cancelling, cancelled") {
		t.Errorf("rejections must name the allowed targets, got %q", typed.Error())
	}
	// A terminal rejection names the terminal state (allowed: none).
	_ = svc.TransitionRunStatus(ctx, "org-1", run.ID, StateCancelled, "")
	err = svc.TransitionRunStatus(ctx, "org-1", run.ID, StateRunning, "")
	if !errors.As(err, &typed) || !strings.Contains(typed.Error(), "allowed: none") {
		t.Errorf("terminal rejection must name no allowed targets, got %q", typed)
	}
}

func TestTransitionRunStatusLifecycleWalk(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx: %v", err)
	}
	// Full legal walk: queued → running → paused → queued → running →
	// cancelling → cancelled.
	walk := []RunState{StateRunning, StatePaused, StateQueued, StateRunning, StateCancelling, StateCancelled}
	for _, target := range walk {
		if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, target, ""); err != nil {
			t.Fatalf("%s → %s rejected: %v", run.Status, target, err)
		}
	}
	stored, err := svc.GetRunCtx(ctx, "org-1", run.ID)
	if err != nil || stored.Status != RunStatus(StateCancelled) {
		t.Fatalf("terminal state not persisted: %#v err=%v", stored, err)
	}
	// Self-transition is an idempotent no-op.
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StateCancelled, ""); err != nil {
		t.Errorf("self-transition must be an idempotent success, got %v", err)
	}
	// Unknown target fails closed without touching the row.
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, RunState("ascended"), ""); !errors.Is(err, ErrUnknownRunState) {
		t.Errorf("unknown target: want ErrUnknownRunState, got %v", err)
	}
	// Trusted internal path (empty orgID) resolves the tenant from the row.
	run2, _ := svc.CreateRunCtx(ctx, "org-2", "agent-1", "hi")
	if err := svc.TransitionRunStatus(ctx, "", run2.ID, StateRunning, ""); err != nil {
		t.Errorf("internal worker path rejected: %v", err)
	}
	// Blank id fails with the not-found sentinel.
	if err := svc.TransitionRunStatus(ctx, "org-1", "  ", StateRunning, ""); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("blank id: want ErrRunNotFound, got %v", err)
	}
}

func TestTransitionRunStatusEmitsControlEvents(t *testing.T) {
	svc := NewService()
	pub := &capturePublisher{}
	svc.SetEventPublisher(pub)
	ctx := context.Background()

	run, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StatePaused, ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StateQueued, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StateCancelling, ""); err != nil {
		t.Fatalf("cancelling: %v", err)
	}
	// cancelling → cancelled confirmation is deliberately silent: the
	// runtime's graceful unwind already published run.cancelled.
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StateCancelled, ""); err != nil {
		t.Fatalf("cancel confirm: %v", err)
	}
	want := []string{events.EventRunPaused, events.EventRunResumed}
	got := pub.types()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v (cancelling→cancelled must stay silent)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	// Cancel from a non-running state emits run.cancelled.
	run2, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err := svc.TransitionRunStatus(ctx, "org-1", run2.ID, StateCancelled, ""); err != nil {
		t.Fatalf("direct cancel: %v", err)
	}
	types := pub.types()
	if last := types[len(types)-1]; last != events.EventRunCancelled {
		t.Fatalf("last event = %s, want %s", last, events.EventRunCancelled)
	}
}

// guardStore wraps fakeStore with the issue #115 optimistic guard.
type guardStore struct {
	*fakeStore
	// guardHook decides the guard outcome (updated, err) for the next call.
	guardHook  func(expected, target RunStatus) (bool, error)
	guardCalls []struct{ expected, target RunStatus }
}

func (g *guardStore) UpdateRunStatusGuarded(_ context.Context, _, _ string, expected, target RunStatus, _ string) (bool, error) {
	g.guardCalls = append(g.guardCalls, struct{ expected, target RunStatus }{expected, target})
	if g.guardHook != nil {
		return g.guardHook(expected, target)
	}
	return true, nil
}

func TestTransitionRunStatusOptimisticGuard(t *testing.T) {
	fake := newFakeStore()
	gs := &guardStore{fakeStore: fake}
	svc := NewServiceWithStore(gs)
	ctx := context.Background()
	run, err := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx: %v", err)
	}

	// Guard accepts: the transition lands and the mirror refreshes.
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StateRunning, ""); err != nil {
		t.Fatalf("guarded transition rejected: %v", err)
	}
	if len(gs.guardCalls) != 1 {
		t.Fatalf("guard calls = %d, want 1", len(gs.guardCalls))
	}
	// The expected status must be the RAW stored spelling (legacy uppercase),
	// not the canonical lowercase contract form.
	if gs.guardCalls[0].expected != StatusQueued {
		t.Fatalf("guard expected = %q, want the raw stored %q", gs.guardCalls[0].expected, StatusQueued)
	}
	if got := gs.guardCalls[0].target; got != RunStatus(StateRunning) {
		t.Fatalf("guard target = %q, want running", got)
	}

	// Guard rejects (another writer moved the row first): the caller loses
	// with ErrStatusConflict — never a silent overwrite.
	gs.guardHook = func(RunStatus, RunStatus) (bool, error) { return false, nil }
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StateCompleted, ""); !errors.Is(err, ErrStatusConflict) {
		t.Fatalf("want ErrStatusConflict on lost race, got %v", err)
	}
	// Store error propagates verbatim.
	boom := errors.New("db down")
	gs.guardHook = func(RunStatus, RunStatus) (bool, error) { return false, boom }
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StateCompleted, ""); !errors.Is(err, boom) {
		t.Fatalf("store error must propagate, got %v", err)
	}
}

// fakeController is a RunController double recording the signals it received.
type fakeController struct {
	mu      sync.Mutex
	pauses  int
	cancels int
	err     error
}

func (f *fakeController) Pause(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauses++
	return f.err
}

func (f *fakeController) Cancel(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return f.err
}

func TestRunControlRegistryActOnInFlight(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	run, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StateRunning, ""); err != nil {
		t.Fatalf("to running: %v", err)
	}

	// Not registered: the control op transitions the durable status only.
	if _, err := svc.PauseRun(ctx, "org-1", run.ID); err != nil {
		t.Fatalf("pause without in-flight controller: %v", err)
	}
	stored, _ := svc.GetRunCtx(ctx, "org-1", run.ID)
	if stored.Status != RunStatus(StatePaused) {
		t.Fatalf("status = %s, want paused", stored.Status)
	}

	run2, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.TransitionRunStatus(ctx, "org-1", run2.ID, StateRunning, "")

	// Registered: the in-flight controller is signalled after the durable
	// transition landed (the controller is looked up BY RUN ID).
	ctrl := &fakeController{}
	unregister := svc.Controls().Register(run2.ID, ctrl)
	defer unregister()
	if _, ok := svc.Controls().Lookup(run2.ID); !ok {
		t.Fatal("controller must be registered")
	}
	if _, err := svc.PauseRun(ctx, "org-1", run2.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if ctrl.pauses != 1 || ctrl.cancels != 0 {
		t.Fatalf("pause signals: pauses=%d cancels=%d, want 1/0", ctrl.pauses, ctrl.cancels)
	}

	// Cancel of a RUNNING run takes the two-phase route: cancelling first,
	// then the controller is asked to unwind gracefully. The controller for
	// run3 is registered separately (the registry maps run ids → handles).
	run3, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.TransitionRunStatus(ctx, "org-1", run3.ID, StateRunning, "")
	svc.Controls().Register(run3.ID, ctrl)
	cancelled, err := svc.CancelRun(ctx, "org-1", run3.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != RunStatus(StateCancelling) {
		t.Fatalf("running cancel must land in cancelling, got %s", cancelled.Status)
	}
	if ctrl.cancels != 1 {
		t.Fatalf("cancel signals = %d, want 1", ctrl.cancels)
	}
	// Re-signalling an in-flight cancelling run is an idempotent no-op.
	if _, err := svc.CancelRun(ctx, "org-1", run3.ID); err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
	if ctrl.cancels != 2 {
		t.Fatalf("idempotent cancel must re-signal, cancels = %d", ctrl.cancels)
	}

	// Unregister removes the handle (the deferred unregister covers run.ID;
	// explicit removal for run2/run3 keeps the registry clean).
	svc.Controls().Unregister(run2.ID)
	svc.Controls().Unregister(run3.ID)
	if _, ok := svc.Controls().Lookup(run2.ID); ok {
		t.Fatal("unregister must remove the controller")
	}
	if _, ok := svc.Controls().Lookup(run3.ID); ok {
		t.Fatal("unregister must remove the controller (run3)")
	}

	// A failing controller never fails the (already persisted) transition.
	failing := &fakeController{err: errors.New("signal channel closed")}
	svc.Controls().Register(run3.ID, failing)
	defer svc.Controls().Unregister(run3.ID)
	run4, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.TransitionRunStatus(ctx, "org-1", run4.ID, StateRunning, "")
	if _, err := svc.PauseRun(ctx, "org-1", run4.ID); err != nil {
		t.Fatalf("controller failure must not fail the control op: %v", err)
	}
}

func TestRunControlRegistryNilSafety(t *testing.T) {
	var reg *RunControlRegistry
	reg.Unregister("x")
	if _, ok := reg.Lookup("x"); ok {
		t.Fatal("nil registry must lookup empty")
	}
	if un := reg.Register("x", &fakeController{}); un == nil {
		t.Fatal("nil registry Register must return a callable no-op")
	} else {
		un()
	}
	svc := NewService()
	if svc.Controls() == nil {
		t.Fatal("Controls() must lazily create the registry")
	}
	if svc.Controls() != svc.Controls() {
		t.Fatal("Controls() must return the same registry")
	}
}

func TestResumeRunQueuedEndpointSemantics(t *testing.T) {
	svc := NewService()
	ctx := context.Background()

	// paused → queued (the issue #115 resume endpoint semantic).
	run, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.TransitionRunStatus(ctx, "org-1", run.ID, StatePaused, "")
	resumed, err := svc.ResumeRunQueued(ctx, "org-1", run.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Status != RunStatus(StateQueued) {
		t.Fatalf("resume must re-enqueue (queued), got %s", resumed.Status)
	}
	// Idempotent for already-queued runs.
	if _, err := svc.ResumeRunQueued(ctx, "org-1", run.ID); err != nil {
		t.Fatalf("idempotent resume: %v", err)
	}
	// A running run is not resumable.
	_ = svc.TransitionRunStatus(ctx, "org-1", run.ID, StateRunning, "")
	if _, err := svc.ResumeRunQueued(ctx, "org-1", run.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resume of running run: want ErrInvalidTransition, got %v", err)
	}

	// waiting_approval must NOT bypass the approval gate.
	gated, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.UpdateStatusCtx(ctx, "org-1", gated.ID, StatusWaitingApproval, "")
	if _, err := svc.ResumeRunQueued(ctx, "org-1", gated.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("waiting_approval resume must be rejected, got %v", err)
	}
	// ...but the approvals flow (ResumeRun) lifts the gate back to pending.
	approved, err := svc.ResumeRun(ctx, "org-1", gated.ID)
	if err != nil {
		t.Fatalf("approval resume: %v", err)
	}
	if approved.Status != RunStatus(StatePending) {
		t.Fatalf("approval resume must land pending, got %s", approved.Status)
	}
	// Terminal runs are never resumable.
	_ = svc.TransitionRunStatus(ctx, "org-1", run.ID, StateCompleted, "")
	if _, err := svc.ResumeRunQueued(ctx, "org-1", run.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal resume: want ErrInvalidTransition, got %v", err)
	}
}

func TestPauseRunRejectsTerminalAndUnknown(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	for _, status := range []RunState{StateCompleted, StateFailed, StateCancelled, StateTimeout} {
		run, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
		// Force the terminal state through the legacy write (setup only).
		if err := svc.UpdateStatusCtx(ctx, "org-1", run.ID, RunStatus(status), ""); err != nil {
			t.Fatalf("setup %s: %v", status, err)
		}
		if _, err := svc.PauseRun(ctx, "org-1", run.ID); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("pause of %s run: want ErrInvalidTransition, got %v", status, err)
		}
	}
	// Unparseable stored status fails closed with a typed rejection whose
	// From carries the mangled value.
	run, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	svc.mu.Lock()
	svc.runs[run.ID].Status = "MANGLED"
	svc.mu.Unlock()
	_, err := svc.PauseRun(ctx, "org-1", run.ID)
	var typed *RunTransitionError
	if !errors.As(err, &typed) || typed.From != RunState("MANGLED") {
		t.Errorf("mangled status must fail closed with the typed rejection, got %v", err)
	}
}

// capturePublisher records every published event (issue #115 control-plane
// emissions + run.recovered).
type capturePublisher struct {
	mu        sync.Mutex
	published []events.Event
}

func (p *capturePublisher) Publish(_ context.Context, ev events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, ev)
	return nil
}

func (p *capturePublisher) types() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.published))
	for _, ev := range p.published {
		out = append(out, ev.Type)
	}
	return out
}

func (p *capturePublisher) payloads() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.published))
	for _, ev := range p.published {
		out = append(out, ev.Payload)
	}
	return out
}

func TestControlEventsCarryTenantAndPayload(t *testing.T) {
	svc := NewService()
	pub := &capturePublisher{}
	svc.SetEventPublisher(pub)
	ctx := context.Background()
	run, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	if err := svc.TransitionRunStatus(ctx, "org-1", run.ID, StatePaused, ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if len(pub.published) != 1 {
		t.Fatalf("published %d events, want 1", len(pub.published))
	}
	ev := pub.published[0]
	if ev.Type != events.EventRunPaused || ev.TenantID != "org-1" {
		t.Fatalf("event = type %s tenant %s, want run.paused/org-1", ev.Type, ev.TenantID)
	}
	if ev.Resource.ID != run.ID || ev.Resource.Type != "run" {
		t.Fatalf("resource = %+v, want run/%s", ev.Resource, run.ID)
	}
	if ev.Payload["from"] != string(StateQueued) || ev.Payload["to"] != string(StatePaused) {
		t.Fatalf("payload = %v, want from/to pair", ev.Payload)
	}
	// A nil publisher or blank tenant never fails the transition.
	svc.SetEventPublisher(nil)
	run2, _ := svc.CreateRunCtx(ctx, "org-2", "agent-1", "hi")
	if err := svc.TransitionRunStatus(ctx, "org-2", run2.ID, StatePaused, ""); err != nil {
		t.Fatalf("nil publisher must not fail the transition: %v", err)
	}
	if err := svc.TransitionRunStatus(ctx, "", run.ID, StateFailed, "reason"); err != nil {
		t.Fatalf("empty-org transition: %v", err)
	}
	if got := len(pub.published); got != 1 {
		t.Errorf("nil publisher must add no events, got %d (want the 1 earlier run.paused)", got)
	}
}
