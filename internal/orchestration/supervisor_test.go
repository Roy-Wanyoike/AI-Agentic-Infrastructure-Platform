package orchestration

// supervisor_test.go — issue #108: supervisor routing strategies, retry
// semantics, concurrency cap and the planner adapter. Deterministic: workers
// are gated with channels (no sleeps for correctness); the only wall-clock
// probe is the bounded negative check on the concurrency cap (a false-pass
// direction only — it can never flake a correct implementation red).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fake delegator ---------------------------------------------------------

// fakeDelegator records every delegation request. With gate=true each
// Delegate call blocks on a per-call release channel (for in-flight /
// concurrency tests); with defaultFn set the outcome is scripted instead.
// started (when non-nil) is signaled after each request is recorded so tests
// can synchronize on "the task reached a worker" without sleeping.
type fakeDelegator struct {
	mu        sync.Mutex
	requests  []DelegationRequest
	gates     []chan struct{}
	gate      bool
	started   chan struct{}
	defaultFn func(req DelegationRequest) (string, *DelegationRecord, error)
	err       error
}

func (f *fakeDelegator) Delegate(ctx context.Context, req DelegationRequest) (*DelegationRecord, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	var release chan struct{}
	if f.gate {
		release = make(chan struct{})
		f.gates = append(f.gates, release)
	}
	f.mu.Unlock()
	if f.started != nil {
		f.started <- struct{}{} // buffered: never blocks the delegator
	}
	if release != nil {
		<-release // blocked in-flight until the test releases
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.defaultFn != nil {
		id, rec, err := f.defaultFn(req)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			rec = &DelegationRecord{ID: id, Status: DelegationStatusCompleted, Result: "from " + req.ToAgentID, ChildRunID: "run-" + id}
		}
		return rec, nil
	}
	return &DelegationRecord{
		ID:         "del-" + req.ToAgentID,
		ToAgentID:  req.ToAgentID,
		ChildRunID: "run-" + req.ToAgentID,
		Status:     DelegationStatusCompleted,
		Result:     "from " + req.ToAgentID,
	}, nil
}

func (f *fakeDelegator) requestTargets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.requests))
	for _, r := range f.requests {
		out = append(out, r.ToAgentID)
	}
	return out
}

// gateCount reports how many delegations are (or were) gated.
func (f *fakeDelegator) gateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gates)
}

// waitForDelegatorRequests blocks until the delegator has recorded n NEW
// requests since the last call (channel synchronization, never a sleep).
func waitForDelegatorRequests(t *testing.T, d *fakeDelegator, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-d.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for delegation request %d/%d", i+1, n)
		}
	}
}

func taskFor(org string) SupervisorTask {
	return SupervisorTask{OrgID: org, Task: "do the thing", FromAgentID: "agent-req", ParentRunID: "run-p"}
}

func mustSupervisor(t *testing.T, d Delegator, cfg SupervisorConfig) *Supervisor {
	t.Helper()
	s, err := NewSupervisor(cfg, d)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	return s
}

// --- routing strategies -----------------------------------------------------

// TestRoundRobinCyclesWorkers: the cursor rotates across RunTask calls and
// wraps around.
func TestRoundRobinCyclesWorkers(t *testing.T) {
	d := &fakeDelegator{}
	s := mustSupervisor(t, d, SupervisorConfig{
		Workers:  []WorkerAgent{{AgentID: "w1"}, {AgentID: "w2"}, {AgentID: "w3"}},
		Strategy: RouteRoundRobin,
	})
	for i := 0; i < 4; i++ {
		if _, err := s.RunTask(context.Background(), taskFor("org-a")); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	got := d.requestTargets()
	want := []string{"w1", "w2", "w3", "w1"}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("attempt %d routed to %s, want %s (all: %v)", i, got[i], id, got)
		}
	}
}

// TestLeastLoadedPicksIdleWorker: with worker w1 blocked in-flight, the next
// task routes to the idle worker w2 (ties keep configuration order).
func TestLeastLoadedPicksIdleWorker(t *testing.T) {
	d := &fakeDelegator{gate: true, started: make(chan struct{}, 8)}
	s := mustSupervisor(t, d, SupervisorConfig{
		Workers:  []WorkerAgent{{AgentID: "w1"}, {AgentID: "w2"}},
		Strategy: RouteLeastLoaded,
	})
	done1 := make(chan error, 1)
	go func() {
		_, err := s.RunTask(context.Background(), taskFor("org-a"))
		done1 <- err
	}()
	waitForDelegatorRequests(t, d, 1) // w1 is now blocked in-flight
	if got := d.requestTargets(); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("first task routed to %v, want [w1]", got)
	}
	// While w1 is blocked in-flight, a second task must pick the idle w2.
	done2 := make(chan error, 1)
	go func() {
		_, err := s.RunTask(context.Background(), taskFor("org-a"))
		done2 <- err
	}()
	waitForDelegatorRequests(t, d, 1) // one NEW request (task 2 on w2)
	if got := d.requestTargets(); len(got) != 2 || got[1] != "w2" {
		t.Fatalf("second task routed to %v, want [w1 w2] (idle w2)", got)
	}
	// Release both and reap.
	d.mu.Lock()
	close(d.gates[0])
	close(d.gates[1])
	d.mu.Unlock()
	if err := <-done1; err != nil {
		t.Fatalf("task1: %v", err)
	}
	if err := <-done2; err != nil {
		t.Fatalf("task2: %v", err)
	}
}

// TestCapabilityMatch: only capable workers are candidates; no match is a
// typed error with no delegation; an empty requirement falls back to
// configuration order.
func TestCapabilityMatch(t *testing.T) {
	d := &fakeDelegator{}
	s := mustSupervisor(t, d, SupervisorConfig{
		Workers: []WorkerAgent{
			{AgentID: "w-ui", Capabilities: []string{"ui"}},
			{AgentID: "w-sql", Capabilities: []string{"sql", "etl"}},
			{AgentID: "w-etl", Capabilities: []string{"etl"}},
		},
		Strategy: RouteCapabilityMatch,
	})
	res, err := s.RunTask(context.Background(), SupervisorTask{
		OrgID: "org-a", Task: "migrate", RequiredCapability: "sql",
	})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.WorkerAgentID != "w-sql" {
		t.Fatalf("worker = %s, want w-sql", res.WorkerAgentID)
	}
	_, err = s.RunTask(context.Background(), SupervisorTask{
		OrgID: "org-a", Task: "paint", RequiredCapability: "art",
	})
	if !errors.Is(err, ErrNoCapableWorker) {
		t.Fatalf("err = %v, want ErrNoCapableWorker", err)
	}
	if got := len(d.requests); got != 1 {
		t.Fatalf("failed capability match delegated %d times, want 1 total", got)
	}
	res, err = s.RunTask(context.Background(), SupervisorTask{OrgID: "org-a", Task: "anything"})
	if err != nil {
		t.Fatalf("RunTask(no requirement): %v", err)
	}
	if res.WorkerAgentID != "w-ui" {
		t.Fatalf("no-requirement routing = %s, want config order first (w-ui)", res.WorkerAgentID)
	}
}

// --- failure handling -------------------------------------------------------

// TestRetryNextWorkerThenSuccess: first worker fails, the retry lands on the
// next candidate and the attempt trace is aggregated.
func TestRetryNextWorkerThenSuccess(t *testing.T) {
	d := &fakeDelegator{defaultFn: func(req DelegationRequest) (string, *DelegationRecord, error) {
		if req.ToAgentID == "w1" {
			return "", nil, errors.New("worker crashed")
		}
		return "del-ok", nil, nil
	}}
	s := mustSupervisor(t, d, SupervisorConfig{
		Workers:    []WorkerAgent{{AgentID: "w1"}, {AgentID: "w2"}},
		Strategy:   RouteRoundRobin,
		MaxRetries: 1,
	})
	res, err := s.RunTask(context.Background(), taskFor("org-a"))
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.WorkerAgentID != "w2" || res.DelegationID != "del-ok" {
		t.Fatalf("result wrong: %+v", res)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].WorkerAgentID != "w1" || !strings.Contains(res.Attempts[0].Error, "worker crashed") {
		t.Fatalf("attempt trace wrong: %+v", res.Attempts)
	}
	if got := d.requestTargets(); len(got) != 2 || got[0] != "w1" || got[1] != "w2" {
		t.Fatalf("routing sequence = %v", got)
	}
}

// TestRetryFailedResultCountsAsFailure: a FAILED delegation result (the
// executor ran but the run failed) is retried on the next worker too.
func TestRetryFailedResultCountsAsFailure(t *testing.T) {
	d := &fakeDelegator{defaultFn: func(req DelegationRequest) (string, *DelegationRecord, error) {
		if req.ToAgentID == "w1" {
			return "", &DelegationRecord{ID: "del-f1", Status: DelegationStatusFailed, Error: "max steps"}, nil
		}
		return "del-ok", nil, nil
	}}
	s := mustSupervisor(t, d, SupervisorConfig{
		Workers:    []WorkerAgent{{AgentID: "w1"}, {AgentID: "w2"}},
		MaxRetries: 1,
	})
	res, err := s.RunTask(context.Background(), taskFor("org-a"))
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.WorkerAgentID != "w2" {
		t.Fatalf("retry landed on %s", res.WorkerAgentID)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].DelegationID != "del-f1" || res.Attempts[0].Error != "max steps" {
		t.Fatalf("attempt trace wrong: %+v", res.Attempts)
	}
}

// TestAllWorkersFailed: the typed error carries the full attempt trace and
// matches ErrAllWorkersFailed via errors.Is / errors.As.
func TestAllWorkersFailed(t *testing.T) {
	d := &fakeDelegator{err: errors.New("boom")}
	s := mustSupervisor(t, d, SupervisorConfig{
		Workers:    []WorkerAgent{{AgentID: "w1"}, {AgentID: "w2"}, {AgentID: "w3"}},
		MaxRetries: 5, // capped by the candidate list length
	})
	res, err := s.RunTask(context.Background(), SupervisorTask{OrgID: "org-a", Task: "t", TaskRef: "task-1"})
	if res != nil {
		t.Fatalf("result must be nil on total failure: %+v", res)
	}
	if !errors.Is(err, ErrAllWorkersFailed) {
		t.Fatalf("err = %v, want ErrAllWorkersFailed", err)
	}
	var supErr *SupervisorError
	if !errors.As(err, &supErr) {
		t.Fatalf("err is not *SupervisorError: %T", err)
	}
	if len(supErr.Attempts) != 3 {
		t.Fatalf("attempts = %d, want 3 (retries capped by worker count)", len(supErr.Attempts))
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "task-1") {
		t.Fatalf("error message must carry the trace: %v", err)
	}
	if got := len(d.requests); got != 3 {
		t.Fatalf("delegation attempts = %d, want 3", got)
	}
}

// --- concurrency ------------------------------------------------------------

// TestMaxConcurrentDelegations: the in-flight cap makes the second task wait
// for the released slot.
func TestMaxConcurrentDelegations(t *testing.T) {
	d := &fakeDelegator{gate: true, started: make(chan struct{}, 8)}
	s := mustSupervisor(t, d, SupervisorConfig{
		Workers:       []WorkerAgent{{AgentID: "w1"}},
		MaxConcurrent: 1,
	})
	done1 := make(chan error, 1)
	go func() {
		_, err := s.RunTask(context.Background(), taskFor("org-a"))
		done1 <- err
	}()
	waitForDelegatorRequests(t, d, 1) // task 1 holds the cap inside the worker
	if got := d.requestTargets(); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("first task routed to %v, want [w1]", got)
	}
	// Second task must be BLOCKED on the semaphore — never reaching a worker.
	done2 := make(chan error, 1)
	go func() {
		_, err := s.RunTask(context.Background(), taskFor("org-a"))
		done2 <- err
	}()
	deadline := time.After(250 * time.Millisecond)
	for {
		select {
		case err := <-done2:
			t.Fatalf("second task finished while the cap was held (err=%v) — cap not enforced", err)
		case <-deadline:
			// Negative check window elapsed; a correct implementation stays
			// gated here (false-pass direction only, never flaky red).
			if got := d.gateCount(); got != 1 {
				t.Fatalf("second task entered a worker despite the cap (gates=%d)", got)
			}
			goto released
		}
	}
released:
	// Release task 1 -> the waiting task 2 acquires the freed slot and reaches
	// the worker deterministically (signaled via the started channel).
	d.mu.Lock()
	close(d.gates[0])
	d.mu.Unlock()
	waitForDelegatorRequests(t, d, 1) // task 2 acquires the freed slot
	d.mu.Lock()
	close(d.gates[1]) // unblock task 2's delegation
	d.mu.Unlock()
	if err := <-done1; err != nil {
		t.Fatalf("task1: %v", err)
	}
	if err := <-done2; err != nil {
		t.Fatalf("task2: %v", err)
	}
}

// TestAwaitingApprovalPassthrough: a parked delegation is surfaced, not
// retried against other workers.
func TestAwaitingApprovalPassthrough(t *testing.T) {
	d := &fakeDelegator{defaultFn: func(req DelegationRequest) (string, *DelegationRecord, error) {
		return "", &DelegationRecord{ID: "del-park", Status: DelegationStatusAwaitingApproval, ChildRunID: "run-park"}, nil
	}}
	s := mustSupervisor(t, d, SupervisorConfig{
		Workers:    []WorkerAgent{{AgentID: "w1"}, {AgentID: "w2"}},
		MaxRetries: 3,
	})
	res, err := s.RunTask(context.Background(), taskFor("org-a"))
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.Status != DelegationStatusAwaitingApproval || res.DelegationID != "del-park" {
		t.Fatalf("parked result wrong: %+v", res)
	}
	if got := len(d.requests); got != 1 {
		t.Fatalf("parked delegation must not be retried (%d attempts)", got)
	}
}

// --- validation & wiring ----------------------------------------------------

// TestSupervisorValidation: construction and task validation matrix.
func TestSupervisorValidation(t *testing.T) {
	if _, err := NewSupervisor(SupervisorConfig{Strategy: "chaos"}, &fakeDelegator{}); !errors.Is(err, ErrInvalidStrategy) {
		t.Fatalf("bad strategy err = %v, want ErrInvalidStrategy", err)
	}
	if _, err := NewSupervisor(SupervisorConfig{}, nil); err == nil {
		t.Fatal("nil delegator must fail construction")
	}
	// Empty strategy defaults to round_robin.
	s, err := NewSupervisor(SupervisorConfig{Workers: []WorkerAgent{{AgentID: "w1"}}}, &fakeDelegator{})
	if err != nil {
		t.Fatalf("default strategy: %v", err)
	}
	if _, err := s.RunTask(context.Background(), SupervisorTask{OrgID: "org-a"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing task text err = %v, want ErrInvalidRequest", err)
	}
	// Empty worker list surfaces at run time.
	s2, err := NewSupervisor(SupervisorConfig{}, &fakeDelegator{})
	if err != nil {
		t.Fatalf("empty workers construction: %v", err)
	}
	if _, err := s2.RunTask(context.Background(), taskFor("org-a")); !errors.Is(err, ErrNoWorkers) {
		t.Fatalf("empty workers err = %v, want ErrNoWorkers", err)
	}
}

// TestSupervisorRequestCarriesTenantAndDepth: delegation requests inherit
// the task's org/parent/depth/response routing.
func TestSupervisorRequestCarriesTenantAndDepth(t *testing.T) {
	d := &fakeDelegator{}
	s := mustSupervisor(t, d, SupervisorConfig{Workers: []WorkerAgent{{AgentID: "w1"}}})
	if _, err := s.RunTask(context.Background(), SupervisorTask{
		OrgID: "org-x", Task: "t", FromAgentID: "agent-req", ParentRunID: "run-p", Depth: 1, TaskRef: "ref-1",
	}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	req := d.requests[0]
	if req.OrgID != "org-x" || req.FromAgentID != "agent-req" || req.ToAgentID != "w1" ||
		req.ParentRunID != "run-p" || req.Depth != 1 {
		t.Fatalf("delegation request wrong: %+v", req)
	}
}

// TestSupervisorTasksFromPlanAndRunPlan: the planner -> executor bridge.
func TestSupervisorTasksFromPlanAndRunPlan(t *testing.T) {
	plan := &Plan{
		Tasks: []Task{
			{ID: "t1", Phase: "auth", Description: "build auth"},
			{ID: "t2", Phase: "runtime", Description: "build runtime"},
		},
	}
	tasks := SupervisorTasksFromPlan(plan)
	if len(tasks) != 2 || tasks[0].TaskRef != "t1" || tasks[0].Task != "build auth" {
		t.Fatalf("plan adaptation wrong: %+v", tasks)
	}
	if got := SupervisorTasksFromPlan(nil); got != nil {
		t.Fatalf("nil plan must adapt to nil, got %+v", got)
	}

	d := &fakeDelegator{}
	s := mustSupervisor(t, d, SupervisorConfig{Workers: []WorkerAgent{{AgentID: "w1"}, {AgentID: "w2"}}})
	results, err := s.RunPlan(context.Background(), "org-a", plan, "agent-req")
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if len(results) != 2 || results[0].Output != "from w1" || results[1].Output != "from w2" {
		t.Fatalf("plan results wrong: %+v", results)
	}
	if got := d.requestTargets(); len(got) != 2 || got[0] != "w1" || got[1] != "w2" {
		t.Fatalf("plan routing = %v", got)
	}
	// Fail-fast: the second task failing aborts the plan with the typed error.
	d2 := &fakeDelegator{defaultFn: func(req DelegationRequest) (string, *DelegationRecord, error) {
		if req.Task == "build runtime" {
			return "", nil, errors.New("runtime build failed")
		}
		return "del-ok", nil, nil
	}}
	s2 := mustSupervisor(t, d2, SupervisorConfig{Workers: []WorkerAgent{{AgentID: "w1"}}})
	if _, err := s2.RunPlan(context.Background(), "org-a", plan, ""); !errors.Is(err, ErrAllWorkersFailed) {
		t.Fatalf("failing plan err = %v, want ErrAllWorkersFailed", err)
	}
	// A plan without tasks is a typed invalid request.
	if _, err := s2.RunPlan(context.Background(), "org-a", &Plan{}, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty plan err = %v, want ErrInvalidRequest", err)
	}
}
