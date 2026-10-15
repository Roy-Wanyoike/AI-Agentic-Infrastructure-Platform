package orchestration

// delegation_test.go — issue #108: delegation protocol semantics on the REAL
// InProcessCommBus (integration where it matters) with fake seams for the
// runs factory, executor and approval router. Deterministic throughout:
// delegation is synchronous and every message assertion is a non-blocking
// read of a buffered channel after the call returns.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fakes -----------------------------------------------------------------

type execCall struct {
	req ExecuteRequest
	ctx context.Context
}

type fakeExecutor struct {
	mu    sync.Mutex
	calls []execCall
	fn    func(ctx context.Context, req ExecuteRequest) (*ExecutionResult, error)
}

func (f *fakeExecutor) ExecuteRun(ctx context.Context, req ExecuteRequest) (*ExecutionResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, execCall{req: req, ctx: ctx})
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	return &ExecutionResult{RunID: req.RunID, Status: "COMPLETED", Output: "did it", Tokens: Usage{1, 2, 3}, Steps: 1}, nil
}

func (f *fakeExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type createCall struct {
	orgID, agentID, input string
	meta                  map[string]any
}

type finishCall struct {
	orgID, runID, status, output string
}

type parentStepCall struct {
	orgID, parentRunID string
	meta               map[string]any
}

type fakeRuns struct {
	mu          sync.Mutex
	created     []createCall
	finished    []finishCall
	parentSteps []parentStepCall
	createErr   error
	nextID      int
}

func (f *fakeRuns) CreateSubRun(ctx context.Context, orgID, agentID, input string, meta map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.nextID++
	id := "child-" + string(rune('0'+f.nextID))
	f.created = append(f.created, createCall{orgID: orgID, agentID: agentID, input: input, meta: meta})
	return id, nil
}

func (f *fakeRuns) FinishSubRun(ctx context.Context, orgID, runID, status, output string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, finishCall{orgID: orgID, runID: runID, status: status, output: output})
	return nil
}

func (f *fakeRuns) RecordParentDelegationStep(ctx context.Context, orgID, parentRunID string, meta map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parentSteps = append(f.parentSteps, parentStepCall{orgID: orgID, parentRunID: parentRunID, meta: meta})
	return nil
}

type fakeApprovals struct {
	mu       sync.Mutex
	requests []ApprovalInput
	id       string
	err      error
}

func (f *fakeApprovals) Request(ctx context.Context, orgID string, in ApprovalInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.requests = append(f.requests, in)
	if f.id == "" {
		f.id = "approval-1"
	}
	return f.id, nil
}

// --- harness ----------------------------------------------------------------

type delegationHarness struct {
	bus       *InProcessCommBus
	runs      *fakeRuns
	executor  *fakeExecutor
	approvals *fakeApprovals
	svc       *DelegationService
}

func newDelegationHarness(t *testing.T, busOpts *BusOptions, mutate func(*DelegationOptions)) *delegationHarness {
	t.Helper()
	bo := BusOptions{ChannelCapacity: 16, InboxRing: 16}
	if busOpts != nil {
		bo = *busOpts
	}
	h := &delegationHarness{
		bus:       NewInProcessCommBus(bo),
		runs:      &fakeRuns{},
		executor:  &fakeExecutor{},
		approvals: &fakeApprovals{},
	}
	opts := DelegationOptions{
		Bus:       h.bus,
		Runs:      h.runs,
		Executor:  h.executor,
		Approvals: h.approvals,
	}
	if mutate != nil {
		mutate(&opts)
	}
	h.svc = NewDelegationService(opts)
	return h
}

func inboxOf(t *testing.T, h *delegationHarness, org, agent string) <-chan AgentMessage {
	t.Helper()
	ch, err := h.bus.Receive(context.Background(), org, agent)
	if err != nil {
		t.Fatalf("Receive(%s, %s): %v", org, agent, err)
	}
	return ch
}

func mustBeMessage(t *testing.T, ch <-chan AgentMessage, wantType string) AgentMessage {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("inbox closed before a %s message arrived", wantType)
		}
		if got.MessageType != wantType {
			t.Fatalf("message type = %q, want %q", got.MessageType, wantType)
		}
		return got
	default:
		t.Fatalf("no %s message in the inbox", wantType)
		return AgentMessage{}
	}
}

// --- tests ------------------------------------------------------------------

// TestDelegateHappyPath: child run created with parent linkage in metadata,
// executed through the executor seam, delegate notification to the worker,
// response routed back to the delegator, child usage attributed to the
// parent run, ledger terminal state consistent.
func TestDelegateHappyPath(t *testing.T) {
	h := newDelegationHarness(t, nil, nil)
	chA := inboxOf(t, h, "org-a", "agent-a") // delegator
	chB := inboxOf(t, h, "org-a", "agent-b") // delegated-to worker

	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID:       "org-a",
		FromAgentID: "agent-a",
		ToAgentID:   "agent-b",
		Task:        "summarize the ledger",
		ParentRunID: "run-parent",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if rec.Status != DelegationStatusCompleted || rec.Result != "did it" {
		t.Fatalf("record terminal state wrong: %+v", rec)
	}
	if rec.ChildRunID == "" || rec.Depth != 1 || !rec.ResponseDelivered {
		t.Fatalf("record linkage wrong: child=%q depth=%d delivered=%v", rec.ChildRunID, rec.Depth, rec.ResponseDelivered)
	}
	if rec.Tokens != (Usage{1, 2, 3}) {
		t.Fatalf("token usage not propagated: %+v", rec.Tokens)
	}

	// Child run creation: agent B, task input, full linkage metadata.
	if len(h.runs.created) != 1 {
		t.Fatalf("created runs = %d, want 1", len(h.runs.created))
	}
	created := h.runs.created[0]
	if created.orgID != "org-a" || created.agentID != "agent-b" || created.input != "summarize the ledger" {
		t.Fatalf("child run fields wrong: %+v", created)
	}
	for _, k := range []string{"role", "parent_run_id", "delegation_id", "from_agent_id", "depth"} {
		if created.meta[k] == nil {
			t.Fatalf("child run metadata missing %q: %+v", k, created.meta)
		}
	}
	if created.meta["parent_run_id"] != "run-parent" || created.meta["from_agent_id"] != "agent-a" {
		t.Fatalf("child run linkage wrong: %+v", created.meta)
	}

	// Executor received the pre-created child run id + parent context.
	if h.executor.callCount() != 1 {
		t.Fatalf("executor calls = %d, want 1", h.executor.callCount())
	}
	call := h.executor.calls[0]
	if call.req.RunID != rec.ChildRunID || call.req.AgentID != "agent-b" || call.req.ParentRunID != "run-parent" || call.req.Depth != 1 {
		t.Fatalf("execute request wrong: %+v", call.req)
	}
	if _, ok := call.ctx.Deadline(); !ok {
		t.Fatal("execution context must carry the delegation deadline")
	}

	// Protocol messages: "delegate" notification to B, "response" to A.
	notify := mustBeMessage(t, chB, MessageTypeDelegate)
	if notify.FromAgentID != "agent-a" || notify.ToAgentID != "agent-b" || notify.RunID != rec.ChildRunID {
		t.Fatalf("delegate notification wrong: %+v", notify)
	}
	var notifyBody map[string]any
	if err := json.Unmarshal(notify.Payload, &notifyBody); err != nil {
		t.Fatalf("notify payload invalid: %v", err)
	}
	if notifyBody["child_run_id"] != rec.ChildRunID {
		t.Fatalf("notify child_run_id = %v", notifyBody["child_run_id"])
	}
	resp := mustBeMessage(t, chA, MessageTypeResponse)
	if resp.FromAgentID != "agent-b" || resp.ToAgentID != "agent-a" || resp.RunID != rec.ChildRunID {
		t.Fatalf("response routing wrong: %+v", resp)
	}
	var respBody map[string]any
	if err := json.Unmarshal(resp.Payload, &respBody); err != nil {
		t.Fatalf("response payload invalid: %v", err)
	}
	if respBody["status"] != DelegationStatusCompleted || respBody["output"] != "did it" {
		t.Fatalf("response body wrong: %v", respBody)
	}

	// Parent attribution: one delegation step on the parent run with usage.
	if len(h.runs.parentSteps) != 1 {
		t.Fatalf("parent steps = %d, want 1", len(h.runs.parentSteps))
	}
	step := h.runs.parentSteps[0]
	if step.orgID != "org-a" || step.parentRunID != "run-parent" || step.meta["status"] != "completed" {
		t.Fatalf("parent step wrong: %+v", step)
	}
	tokens, ok := step.meta["tokens"].(map[string]any)
	if !ok || tokens["total_tokens"] != 3 {
		t.Fatalf("parent step usage attribution wrong: %v", step.meta["tokens"])
	}
	if step.meta["child_run_id"] != rec.ChildRunID {
		t.Fatalf("parent step child_run_id = %v", step.meta["child_run_id"])
	}
}

// TestDelegateTwoPhaseMatchesDelegate: Prepare + Execute == Delegate.
func TestDelegateTwoPhaseMatchesDelegate(t *testing.T) {
	h := newDelegationHarness(t, nil, nil)
	chA := inboxOf(t, h, "org-a", "agent-a")

	rec, err := h.svc.Prepare(context.Background(), DelegationRequest{OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "t"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if rec.Status != DelegationStatusPending {
		t.Fatalf("prepared status = %s, want pending", rec.Status)
	}
	if h.executor.callCount() != 0 {
		t.Fatal("Prepare must not execute")
	}
	final, err := h.svc.Execute(context.Background(), rec)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.Status != DelegationStatusCompleted {
		t.Fatalf("final status = %s", final.Status)
	}
	if got, ok := <-chA; !ok || got.MessageType != MessageTypeResponse {
		t.Fatalf("response not routed in two-phase flow: %+v", got)
	}
	// A prepared record cannot be executed twice.
	if _, err := h.svc.Execute(context.Background(), rec); err == nil {
		t.Fatal("second Execute must fail")
	}
}

// TestDelegateDepthLimit: the recursion bomb guard (default depth 3).
func TestDelegateDepthLimit(t *testing.T) {
	h := newDelegationHarness(t, nil, nil) // MaxDepth default 3
	for _, tc := range []struct {
		depth   int
		wantErr bool
	}{
		{0, false}, // child depth 1
		{1, false}, // child depth 2
		{2, false}, // child depth 3
		{3, true},  // child depth 4 -> rejected
		{7, true},
	} {
		_, err := h.svc.Delegate(context.Background(), DelegationRequest{
			OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b",
			Task: "t", Depth: tc.depth,
		})
		if tc.wantErr && !errors.Is(err, ErrDepthExceeded) {
			t.Errorf("depth %d: err = %v, want ErrDepthExceeded", tc.depth, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("depth %d: unexpected err %v", tc.depth, err)
		}
	}
	if got := len(h.runs.created); got != 3 {
		t.Fatalf("created child runs = %d, want 3 (rejections create nothing)", got)
	}
}

// TestDelegateValidation: malformed request matrix.
func TestDelegateValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  DelegationRequest
	}{
		{"missing org", DelegationRequest{FromAgentID: "a", ToAgentID: "b", Task: "t"}},
		{"missing from", DelegationRequest{OrgID: "o", ToAgentID: "b", Task: "t"}},
		{"missing to", DelegationRequest{OrgID: "o", FromAgentID: "a", Task: "t"}},
		{"missing task", DelegationRequest{OrgID: "o", FromAgentID: "a", ToAgentID: "b"}},
		{"self delegation", DelegationRequest{OrgID: "o", FromAgentID: "a", ToAgentID: "a", Task: "t"}},
		{"negative depth", DelegationRequest{OrgID: "o", FromAgentID: "a", ToAgentID: "b", Task: "t", Depth: -1}},
	} {
		h := newDelegationHarness(t, nil, nil)
		_, err := h.svc.Delegate(context.Background(), tc.req)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", tc.name, err)
		}
		if h.executor.callCount() != 0 || len(h.runs.created) != 0 {
			t.Errorf("%s: rejected request must have no side effects", tc.name)
		}
	}
}

// TestDelegateApprovalGate: require_approval parks the delegation, files the
// approval, creates the linkable child run and NEVER executes.
func TestDelegateApprovalGate(t *testing.T) {
	h := newDelegationHarness(t, nil, nil)
	chA := inboxOf(t, h, "org-a", "agent-a")

	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b",
		Task: "deploy the canary", RequireApproval: true, Requester: "user-1",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if rec.Status != DelegationStatusAwaitingApproval || rec.ApprovalID != "approval-1" {
		t.Fatalf("record not parked at the gate: %+v", rec)
	}
	if h.executor.callCount() != 0 {
		t.Fatalf("approval-gated delegation must not execute (%d calls)", h.executor.callCount())
	}
	if len(h.runs.finished) != 0 {
		t.Fatalf("no terminal transitions expected: %+v", h.runs.finished)
	}
	if len(h.runs.created) != 1 {
		t.Fatalf("child run must exist for the approval linkage: %d", len(h.runs.created))
	}
	if h.runs.created[0].meta["status"] != DelegationStatusAwaitingApproval {
		t.Fatalf("child run metadata must record the parked state: %+v", h.runs.created[0].meta)
	}
	// Approval request content.
	if len(h.approvals.requests) != 1 {
		t.Fatalf("approval requests = %d, want 1", len(h.approvals.requests))
	}
	ap := h.approvals.requests[0]
	if ap.Action != ActionOrchestrationDelegate || ap.Requester != "user-1" || ap.Risk != "medium" {
		t.Fatalf("approval input wrong: %+v", ap)
	}
	if !strings.Contains(ap.Resource, rec.ID) {
		t.Fatalf("approval resource must reference the delegation: %q", ap.Resource)
	}
	// No protocol messages: nothing executed, nothing to respond with.
	select {
	case got := <-chA:
		t.Fatalf("no response expected while awaiting approval: %+v", got)
	default:
	}
}

// TestDelegateApprovalUnavailable: require_approval without a wired approval
// service fails CLOSED and creates no child run.
func TestDelegateApprovalUnavailable(t *testing.T) {
	h := newDelegationHarness(t, nil, func(o *DelegationOptions) { o.Approvals = nil })
	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b",
		Task: "t", RequireApproval: true,
	})
	if !errors.Is(err, ErrApprovalUnavailable) {
		t.Fatalf("err = %v, want ErrApprovalUnavailable", err)
	}
	if rec.Status != DelegationStatusFailed {
		t.Fatalf("record status = %s, want failed", rec.Status)
	}
	if len(h.runs.created) != 0 || h.executor.callCount() != 0 {
		t.Fatal("fail-closed approval gate must have no side effects")
	}
}

// TestDelegateApprovalRouterError: a failing approval router fails the
// delegation before the child run is created.
func TestDelegateApprovalRouterError(t *testing.T) {
	h := newDelegationHarness(t, nil, func(o *DelegationOptions) {
		o.Approvals = &fakeApprovals{err: errors.New("approval store down")}
	})
	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b",
		Task: "t", RequireApproval: true,
	})
	if err == nil || !strings.Contains(err.Error(), "approval routing failed") {
		t.Fatalf("err = %v, want approval routing failure", err)
	}
	if rec.Status != DelegationStatusFailed || len(h.runs.created) != 0 {
		t.Fatalf("failed routing must not create runs: %+v", rec)
	}
}

// TestDelegateExecutorError: an executor failure finalizes the child run
// FAILED, attributes the failure to the parent step, routes the failure
// response and surfaces the error.
func TestDelegateExecutorError(t *testing.T) {
	h := newDelegationHarness(t, nil, func(o *DelegationOptions) {
		o.Executor = &fakeExecutor{fn: func(ctx context.Context, req ExecuteRequest) (*ExecutionResult, error) {
			return nil, errors.New("model provider unreachable")
		}}
	})
	chA := inboxOf(t, h, "org-a", "agent-a")
	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b",
		Task: "t", ParentRunID: "run-parent",
	})
	if err == nil || !strings.Contains(err.Error(), "model provider unreachable") {
		t.Fatalf("err = %v, want the executor error", err)
	}
	if rec.Status != DelegationStatusFailed || rec.Error != "model provider unreachable" {
		t.Fatalf("record not failed: %+v", rec)
	}
	// Child run finalized FAILED by the orchestrator (executor contract).
	if len(h.runs.finished) != 1 || h.runs.finished[0].status != "FAILED" || h.runs.finished[0].runID != rec.ChildRunID {
		t.Fatalf("FinishSubRun calls wrong: %+v", h.runs.finished)
	}
	// Failure visible on the parent timeline.
	if len(h.runs.parentSteps) != 1 || h.runs.parentSteps[0].meta["status"] != "failed" {
		t.Fatalf("parent failure step missing: %+v", h.runs.parentSteps)
	}
	// Failure response routed back to the delegator.
	resp := mustBeMessage(t, chA, MessageTypeResponse)
	var body map[string]any
	if err := json.Unmarshal(resp.Payload, &body); err != nil {
		t.Fatalf("response payload invalid: %v", err)
	}
	if body["status"] != DelegationStatusFailed || body["error"] != "model provider unreachable" {
		t.Fatalf("failure response body wrong: %v", body)
	}
}

// TestDelegateFailedResult: a non-COMPLETED result is a failed delegation
// WITHOUT an extra FinishSubRun call (the executor owns terminal transitions
// on result paths, per its contract).
func TestDelegateFailedResult(t *testing.T) {
	h := newDelegationHarness(t, nil, func(o *DelegationOptions) {
		o.Executor = &fakeExecutor{fn: func(ctx context.Context, req ExecuteRequest) (*ExecutionResult, error) {
			return &ExecutionResult{RunID: req.RunID, Status: "FAILED", Error: "max steps exceeded"}, nil
		}}
	})
	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "t",
	})
	if err != nil {
		t.Fatalf("a failed run RESULT is not an executor error: %v", err)
	}
	if rec.Status != DelegationStatusFailed || rec.Error != "max steps exceeded" {
		t.Fatalf("record wrong: %+v", rec)
	}
	if len(h.runs.finished) != 0 {
		t.Fatalf("executor owns terminal transitions on result paths: %+v", h.runs.finished)
	}
}

// TestDelegateResponseDeliveryBestEffort: a saturated delegator inbox does
// NOT fail the delegation — the ledger records that delivery failed.
func TestDelegateResponseDeliveryBestEffort(t *testing.T) {
	h := newDelegationHarness(t, &BusOptions{ChannelCapacity: 1, InboxRing: 16}, nil)
	// Saturate the delegator's inbox before delegating.
	chA := inboxOf(t, h, "org-a", "agent-a")
	if err := h.bus.Send(context.Background(), AgentMessage{
		OrgID: "org-a", FromAgentID: "noise", ToAgentID: "agent-a", MessageType: MessageTypeRequest,
	}); err != nil {
		t.Fatalf("prefill: %v", err)
	}
	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "t",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if rec.Status != DelegationStatusCompleted {
		t.Fatalf("delegation must complete: %+v", rec)
	}
	if rec.ResponseDelivered {
		t.Fatal("response delivery must be recorded as failed")
	}
	// The channel still holds only the pre-fill noise.
	if got, ok := <-chA; !ok || got.FromAgentID != "noise" {
		t.Fatalf("inbox corrupted: %+v", got)
	}
}

// TestDelegateChildNotifyBestEffort: the delegated agent needs no inbox —
// the sub-run executes regardless.
func TestDelegateChildNotifyBestEffort(t *testing.T) {
	h := newDelegationHarness(t, nil, nil)
	inboxOf(t, h, "org-a", "agent-a") // only the delegator has an inbox
	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "t",
	})
	if err != nil || rec.Status != DelegationStatusCompleted {
		t.Fatalf("delegation without a worker inbox must complete: rec=%+v err=%v", rec, err)
	}
}

// TestDelegateChildRunCreationError: a failing run factory fails the
// delegation before any execution.
func TestDelegateChildRunCreationError(t *testing.T) {
	h := newDelegationHarness(t, nil, func(o *DelegationOptions) {
		o.Runs = &fakeRuns{createErr: errors.New("store down")}
	})
	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "t",
	})
	if err == nil || !strings.Contains(err.Error(), "child run creation failed") {
		t.Fatalf("err = %v, want child run creation failure", err)
	}
	if rec.Status != DelegationStatusFailed || h.executor.callCount() != 0 {
		t.Fatalf("no execution after a failed create: %+v", rec)
	}
}

// TestDelegateExecutionDeadline: the executor context carries the
// per-delegation timeout (default and explicit).
func TestDelegateExecutionDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{"default", 0},
		{"explicit", 2 * time.Second},
	} {
		var seen time.Duration
		h := newDelegationHarness(t, nil, func(o *DelegationOptions) {
			o.Timeout = tc.timeout
			o.Executor = &fakeExecutor{fn: func(ctx context.Context, req ExecuteRequest) (*ExecutionResult, error) {
				dl, ok := ctx.Deadline()
				if !ok {
					return nil, errors.New("no deadline")
				}
				seen = time.Until(dl)
				return &ExecutionResult{RunID: req.RunID, Status: "COMPLETED"}, nil
			}}
		})
		if _, err := h.svc.Delegate(context.Background(), DelegationRequest{
			OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "t",
		}); err != nil {
			t.Fatalf("%s: Delegate: %v", tc.name, err)
		}
		want := tc.timeout
		if want <= 0 {
			want = DefaultDelegationTimeout
		}
		if seen <= 0 || seen > want+time.Second {
			t.Fatalf("%s: deadline = %v, want within (%v, %v]", tc.name, seen, want, want+time.Second)
		}
	}
}

// TestDelegationLedger: org scoping, run_id chain filter, chronological
// order and the per-org bound.
func TestDelegationLedger(t *testing.T) {
	h := newDelegationHarness(t, nil, func(o *DelegationOptions) { o.MaxRecords = 3 })

	// Two chains in two orgs; chain A has two levels.
	for _, req := range []DelegationRequest{
		{OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "level1", ParentRunID: "run-1"},
		{OrgID: "org-a", FromAgentID: "agent-b", ToAgentID: "agent-c", Task: "level2", Depth: 1},
		{OrgID: "org-b", FromAgentID: "agent-z", ToAgentID: "agent-y", Task: "other-org"},
	} {
		if _, err := h.svc.Delegate(context.Background(), req); err != nil {
			t.Fatalf("Delegate(%s): %v", req.Task, err)
		}
	}

	all := h.svc.ListDelegations("org-a", "")
	if len(all) != 2 || all[0].Task != "level1" || all[1].Task != "level2" {
		t.Fatalf("org-a ledger = %+v, want [level1 level2] in chain order", tasksOf(all))
	}
	// Cross-tenant: org-b sees exactly its own record.
	if got := h.svc.ListDelegations("org-b", ""); len(got) != 1 || got[0].Task != "other-org" {
		t.Fatalf("org-b ledger leaked org-a records: %+v", got)
	}
	// run_id filter matches parent OR child.
	chain := h.svc.ListDelegations("org-a", "run-1")
	if len(chain) != 1 || chain[0].Task != "level1" {
		t.Fatalf("run-1 chain = %+v", tasksOf(chain))
	}
	childFiltered := h.svc.ListDelegations("org-a", all[0].ChildRunID)
	if len(childFiltered) != 1 || childFiltered[0].Task != "level1" {
		t.Fatalf("child-run filter = %+v", tasksOf(childFiltered))
	}
	if got := h.svc.ListDelegations("org-a", "run-unknown"); len(got) != 0 {
		t.Fatalf("unknown run filter must be empty, got %+v", tasksOf(got))
	}

	// Ledger bound: two more org-a delegations push past MaxRecords=3 and
	// evict the oldest records.
	for _, task := range []string{"level3", "level4"} {
		if _, err := h.svc.Delegate(context.Background(), DelegationRequest{
			OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: task,
		}); err != nil {
			t.Fatalf("Delegate(%s): %v", task, err)
		}
	}
	all = h.svc.ListDelegations("org-a", "")
	if len(all) != 3 || all[0].Task != "level2" || all[2].Task != "level4" {
		t.Fatalf("bounded ledger = %+v, want oldest (level1) evicted", tasksOf(all))
	}
}

func tasksOf(recs []*DelegationRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Task)
	}
	return out
}

// TestGetDelegation: typed not-found with a strict tenant guard.
func TestGetDelegation(t *testing.T) {
	h := newDelegationHarness(t, nil, nil)
	rec, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "t",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	got, err := h.svc.GetDelegation("org-a", rec.ID)
	if err != nil || got.ID != rec.ID {
		t.Fatalf("GetDelegation: %+v %v", got, err)
	}
	if _, err := h.svc.GetDelegation("org-b", rec.ID); !errors.Is(err, ErrDelegationNotFound) {
		t.Fatalf("cross-tenant read err = %v, want ErrDelegationNotFound", err)
	}
	if _, err := h.svc.GetDelegation("org-a", "nope"); !errors.Is(err, ErrDelegationNotFound) {
		t.Fatalf("unknown id err = %v, want ErrDelegationNotFound", err)
	}
}

// TestDelegationServiceSetExecutor: the wiring-layer seam attachment.
func TestDelegationServiceSetExecutor(t *testing.T) {
	h := newDelegationHarness(t, nil, func(o *DelegationOptions) { o.Executor = nil })
	if _, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "t",
	}); err == nil {
		t.Fatal("delegation without an executor must fail")
	}
	h.svc.SetExecutor(&fakeExecutor{})
	if _, err := h.svc.Delegate(context.Background(), DelegationRequest{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-b", Task: "t2",
	}); err != nil {
		t.Fatalf("Delegate after SetExecutor: %v", err)
	}
	// Nil-safe on a nil receiver.
	var nilSvc *DelegationService
	nilSvc.SetExecutor(nil)
}
