package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Core RunParallelBranches tests: barrier-synchronized fakes, no sleeps.
// ---------------------------------------------------------------------------

// barrierBody returns a branch Execute func that counts itself into the
// active set, fires once when the active count reaches target (closing
// release via once), waits for release (or ctx), then emits out (or err).
func barrierBody(active *atomic.Int64, target int64, release chan struct{}, once *sync.Once, out json.RawMessage, err error) func(ctx context.Context) (json.RawMessage, error) {
	return func(ctx context.Context) (json.RawMessage, error) {
		n := active.Add(1)
		if n >= target {
			once.Do(func() { close(release) })
		}
		defer active.Add(-1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return out, err
	}
}

// hangBody returns a branch Execute that blocks until its context is
// cancelled and never completes on its own — the deterministic loser of
// fail_fast/any/race decisions (no sleeps, the context is the timer).
func hangBody() func(ctx context.Context) (json.RawMessage, error) {
	return func(ctx context.Context) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// runWithWatchdog executes RunParallelBranches on a goroutine and fails the
// test when the branches deadlock (i.e. execution was not concurrent) instead
// of hanging until the go test timeout.
func runWithWatchdog(t *testing.T, branches []ParallelBranch, policy ParallelPolicy, limits ParallelLimits) ParallelReport {
	t.Helper()
	done := make(chan ParallelReport, 1)
	go func() { done <- RunParallelBranches(context.Background(), branches, policy, limits) }()
	select {
	case report := <-done:
		return report
	case <-time.After(5 * time.Second):
		t.Fatal("parallel branches deadlocked — execution is not concurrent")
		return ParallelReport{}
	}
}

func TestRunParallelBranchesAllStrategyMergesOutputs(t *testing.T) {
	branches := []ParallelBranch{
		{ID: "b1", Execute: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{"v":1}`), nil }},
		{ID: "b2", Execute: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{"v":2}`), nil }},
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggAll, FailFast: true}, ParallelLimits{})
	if report.Status != RunStatusCompleted {
		t.Fatalf("status = %q err %q, want completed", report.Status, report.Error)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(report.Output, &merged); err != nil {
		t.Fatalf("aggregate output should be an object: %v", err)
	}
	if string(merged["b1"]) != `{"v":1}` || string(merged["b2"]) != `{"v":2}` {
		t.Fatalf("aggregate output = %s", report.Output)
	}
	if report.Succeeded != 2 || report.Failed != 0 || report.BranchCount != 2 {
		t.Fatalf("counts = %+v", report)
	}
	// Deterministic result ordering: outcomes follow branch declaration order.
	if report.Outcomes[0].BranchID != "b1" || report.Outcomes[1].BranchID != "b2" {
		t.Fatalf("outcomes not in declaration order: %+v", report.Outcomes)
	}
}

func TestRunParallelBranchesAllFailFastCancelsRemaining(t *testing.T) {
	branches := []ParallelBranch{
		{ID: "boom", Execute: func(context.Context) (json.RawMessage, error) { return nil, fmt.Errorf("kaboom") }},
		{ID: "slow", Execute: hangBody()}, // blocked until ctx cancel
		{ID: "mid", Execute: hangBody()},
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggAll, FailFast: true}, ParallelLimits{})
	if report.Status != RunStatusFailed || !strings.Contains(report.Error, "kaboom") {
		t.Fatalf("status = %q err = %q, want failed with kaboom", report.Status, report.Error)
	}
	if report.Failed != 1 {
		t.Fatalf("failed = %d, want 1", report.Failed)
	}
	if report.Cancelled+report.TimedOut != 2 {
		t.Fatalf("losers should be cancelled/timed out, got cancelled=%d timeout=%d", report.Cancelled, report.TimedOut)
	}
	for _, out := range report.Outcomes {
		if out.BranchID == "boom" {
			continue
		}
		if out.Status != ParallelBranchCancelled && out.Status != ParallelBranchTimeout {
			t.Fatalf("branch %s should observe the fail-fast cancellation, got %q", out.BranchID, out.Status)
		}
	}
}

func TestRunParallelBranchesAllCollectAllWaitsForEveryBranch(t *testing.T) {
	branches := []ParallelBranch{
		{ID: "ok1", Execute: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`"a"`), nil }},
		{ID: "bad", Execute: func(context.Context) (json.RawMessage, error) { return nil, fmt.Errorf("nope") }},
		{ID: "ok2", Execute: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`"c"`), nil }},
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggAll, FailFast: false}, ParallelLimits{})
	if report.Status != RunStatusFailed || !strings.Contains(report.Error, "nope") {
		t.Fatalf("status = %q err %q, want failed", report.Status, report.Error)
	}
	if report.Mode != ParallelModeCollectAll {
		t.Fatalf("mode = %q, want collect_all", report.Mode)
	}
	if report.Cancelled != 0 {
		t.Fatalf("collect_all must not cancel losers, got %d cancelled", report.Cancelled)
	}
	if report.Succeeded != 2 || report.Failed != 1 {
		t.Fatalf("counts = %+v", report)
	}
}

func TestRunParallelBranchesAnyFirstSuccessWins(t *testing.T) {
	// failfast completes first (deterministically: winner waits for it).
	failDone := make(chan struct{})
	branches := []ParallelBranch{
		{ID: "failfast", Execute: func(context.Context) (json.RawMessage, error) { close(failDone); return nil, fmt.Errorf("transient") }},
		{ID: "winner", Execute: func(ctx context.Context) (json.RawMessage, error) {
			select {
			case <-failDone:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return json.RawMessage(`"won"`), nil
		}},
		{ID: "loser", Execute: hangBody()}, // blocked until the any-decision cancels it
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggAny, FailFast: true}, ParallelLimits{})
	if report.Status != RunStatusCompleted {
		t.Fatalf("status = %q err %q, want completed (a failure must not fail `any`)", report.Status, report.Error)
	}
	if report.WinningBranch != "winner" || string(report.Output) != `"won"` {
		t.Fatalf("winner = %q output = %s, want winner/won", report.WinningBranch, report.Output)
	}
	if report.Failed != 1 {
		t.Fatalf("failed = %d, want 1 (failfast)", report.Failed)
	}
}

func TestRunParallelBranchesAnyFailsWhenEveryBranchFails(t *testing.T) {
	branches := []ParallelBranch{
		{ID: "a", Execute: func(context.Context) (json.RawMessage, error) { return nil, fmt.Errorf("err-a") }},
		{ID: "b", Execute: func(context.Context) (json.RawMessage, error) { return nil, fmt.Errorf("err-b") }},
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggAny, FailFast: true}, ParallelLimits{})
	if report.Status != RunStatusFailed || !strings.Contains(report.Error, "err-a") || !strings.Contains(report.Error, "err-b") {
		t.Fatalf("status = %q err = %q, want failed with both errors", report.Status, report.Error)
	}
}

func TestRunParallelBranchesRaceFirstCompletionWins(t *testing.T) {
	branches := []ParallelBranch{
		{ID: "fast", Execute: func(context.Context) (json.RawMessage, error) {
			return nil, fmt.Errorf("fast-failure")
		}},
		{ID: "gated", Execute: hangBody()}, // never becomes ready on its own
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggRace, FailFast: false}, ParallelLimits{})
	// Race takes the first completion regardless of status: the failing fast
	// branch wins and the aggregate fails (documented race semantics).
	if report.Status != RunStatusFailed || !strings.Contains(report.Error, "fast-failure") {
		t.Fatalf("status = %q err = %q, want failed via race winner", report.Status, report.Error)
	}
	if report.WinningBranch != "fast" {
		t.Fatalf("winner = %q, want fast", report.WinningBranch)
	}
	if got := report.Outcomes[1]; got.Status == ParallelBranchCompleted {
		t.Fatalf("gated branch should not have completed after the race decision, got %+v", got)
	}
}

func TestRunParallelBranchesMajorityPluralityAndTieBreak(t *testing.T) {
	// Clear plurality: 3x v1 beat 1x v2; the failed branch does not vote.
	mk := func(v string) func(context.Context) (json.RawMessage, error) {
		return func(context.Context) (json.RawMessage, error) { return json.RawMessage(v), nil }
	}
	report := runWithWatchdog(t, []ParallelBranch{
		{ID: "m1", Execute: mk(`"v1"`)},
		{ID: "m2", Execute: mk(`"v1"`)},
		{ID: "m3", Execute: mk(`"v2"`)},
		{ID: "m4", Execute: mk(`"v1"`)},
		{ID: "m5", Execute: func(context.Context) (json.RawMessage, error) { return nil, fmt.Errorf("no vote") }},
	}, ParallelPolicy{Strategy: ParallelAggMajority, FailFast: false}, ParallelLimits{})
	if report.Status != RunStatusCompleted || string(report.Output) != `"v1"` || report.WinningBranch != "m1" {
		t.Fatalf("majority = %q winner %q status %q, want v1/m1/completed", report.Output, report.WinningBranch, report.Status)
	}

	// Tie 2-2: lexicographically smallest serialized output wins ("a" beats "b").
	report = runWithWatchdog(t, []ParallelBranch{
		{ID: "t1", Execute: mk(`"b"`)},
		{ID: "t2", Execute: mk(`"a"`)},
		{ID: "t3", Execute: mk(`"b"`)},
		{ID: "t4", Execute: mk(`"a"`)},
		{ID: "t5", Execute: mk(`"c"`)},
	}, ParallelPolicy{Strategy: ParallelAggMajority, FailFast: false}, ParallelLimits{})
	if report.Status != RunStatusCompleted || string(report.Output) != `"a"` {
		t.Fatalf("tie-break = %q, want \"a\" (lexicographically smallest)", report.Output)
	}
}

func TestRunParallelBranchesMajorityFailsWithoutSuccesses(t *testing.T) {
	branches := []ParallelBranch{
		{ID: "a", Execute: func(context.Context) (json.RawMessage, error) { return nil, fmt.Errorf("x") }},
		{ID: "b", Execute: func(context.Context) (json.RawMessage, error) { return nil, fmt.Errorf("y") }},
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggMajority, FailFast: false}, ParallelLimits{})
	if report.Status != RunStatusFailed {
		t.Fatalf("majority with zero successes = %q, want failed", report.Status)
	}
}

func TestRunParallelBranchesRunsFiveBranchesConcurrently(t *testing.T) {
	// 5 branches must ALL be in flight simultaneously: the fifth entrant
	// opens the barrier; a sequential executor deadlocks on it and the
	// watchdog fails the test.
	var active atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	body := func(ctx context.Context) (json.RawMessage, error) {
		n := active.Add(1)
		if n == 5 {
			once.Do(func() { close(release) })
		}
		defer active.Add(-1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return json.RawMessage(fmt.Sprintf(`{"entrant":%d}`, n)), nil
	}
	branches := make([]ParallelBranch, 0, 5)
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		branches = append(branches, ParallelBranch{ID: id, Execute: body})
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggAll, FailFast: true}, ParallelLimits{})
	if report.Status != RunStatusCompleted {
		t.Fatalf("status = %q err %q", report.Status, report.Error)
	}
	if report.MaxInFlight != 5 {
		t.Fatalf("max in flight = %d, want 5 (branches must run concurrently)", report.MaxInFlight)
	}
}

func TestRunParallelBranchesInFlightNeverExceedsCap(t *testing.T) {
	// Per-workflow override caps the wave at 2: branches stream in batches,
	// the high-water mark of the in-flight counter must be exactly 2.
	var active atomic.Int64
	var maxSeen atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	body := func(ctx context.Context) (json.RawMessage, error) {
		n := active.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		if n == 2 {
			once.Do(func() { close(release) })
		}
		defer active.Add(-1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return json.RawMessage(`{}`), nil
	}
	branches := make([]ParallelBranch, 0, 5)
	for _, id := range []string{"c1", "c2", "c3", "c4", "c5"} {
		branches = append(branches, ParallelBranch{ID: id, Execute: body})
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggAll, FailFast: true}, ParallelLimits{Global: 8, PerWorkflow: 2})
	if report.Status != RunStatusCompleted {
		t.Fatalf("status = %q err %q", report.Status, report.Error)
	}
	if report.Effective != 2 {
		t.Fatalf("effective limit = %d, want 2", report.Effective)
	}
	if report.MaxInFlight != 2 {
		t.Fatalf("max in flight = %d, want exactly 2 (cap enforced AND reached)", report.MaxInFlight)
	}
	if got := maxSeen.Load(); got != 2 {
		t.Fatalf("observed in-flight high-water mark = %d, want 2 (never above the cap, concurrency proven)", got)
	}
}

func TestRunParallelBranchesPerBranchTimeout(t *testing.T) {
	branches := []ParallelBranch{
		{ID: "quick", Execute: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`"fast"`), nil }},
		{ID: "stuck", Execute: func(ctx context.Context) (json.RawMessage, error) {
			<-ctx.Done() // never becomes ready on its own
			return nil, ctx.Err()
		}},
	}
	report := runWithWatchdog(t, branches, ParallelPolicy{Strategy: ParallelAggAll, FailFast: true, BranchTimeout: 20 * time.Millisecond}, ParallelLimits{})
	if report.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed (timeout is a failure for all)", report.Status)
	}
	var stuck ParallelBranchOutcome
	for _, out := range report.Outcomes {
		if out.BranchID == "stuck" {
			stuck = out
		}
	}
	if stuck.Status != ParallelBranchTimeout {
		t.Fatalf("stuck branch status = %q, want timeout", stuck.Status)
	}
}

func TestRunParallelBranchesOuterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the run starts
	branches := []ParallelBranch{
		{ID: "a", Execute: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }},
		{ID: "b", Execute: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }},
	}
	report := RunParallelBranches(ctx, branches, ParallelPolicy{Strategy: ParallelAggAll, FailFast: true}, ParallelLimits{})
	if report.Status != RunStatusFailed {
		t.Fatalf("status = %q, want failed under cancelled outer context", report.Status)
	}
	if report.Cancelled+report.Failed != 2 {
		t.Fatalf("branches should not have executed: cancelled=%d failed=%d", report.Cancelled, report.Failed)
	}
}

func TestRunParallelBranchesEmptyBranchSet(t *testing.T) {
	report := RunParallelBranches(context.Background(), nil, ParallelPolicy{Strategy: ParallelAggAll, FailFast: true}, ParallelLimits{})
	if report.Status != RunStatusCompleted || report.BranchCount != 0 || report.Output == nil {
		t.Fatalf("empty wave should complete trivially, got %+v", report)
	}
}

func TestEffectiveParallelLimits(t *testing.T) {
	cases := []struct {
		name    string
		limits  ParallelLimits
		wantEff int
	}{
		{"global only", ParallelLimits{Global: 8}, 8},
		{"per-workflow tightens", ParallelLimits{Global: 8, PerWorkflow: 3}, 3},
		{"org quota tightens", ParallelLimits{Global: 8, OrgQuota: 2}, 2},
		{"all compose to the min", ParallelLimits{Global: 8, PerWorkflow: 5, OrgQuota: 2}, 2},
		{"unset global defaults", ParallelLimits{}, DefaultWorkflowParallelism},
		{"zero per-workflow ignored", ParallelLimits{Global: 4, PerWorkflow: 0}, 4},
		{"negative org quota ignored", ParallelLimits{Global: 4, OrgQuota: -1}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.limits.Effective(); got != tc.wantEff {
				t.Fatalf("Effective() = %d, want %d", got, tc.wantEff)
			}
		})
	}
}

func TestDefaultMaxWorkflowParallelismEnv(t *testing.T) {
	t.Setenv(EnvMaxWorkflowParallelism, "3")
	if got := DefaultMaxWorkflowParallelism(); got != 3 {
		t.Fatalf("env override = %d, want 3", got)
	}
	t.Setenv(EnvMaxWorkflowParallelism, "bogus")
	if got := DefaultMaxWorkflowParallelism(); got != DefaultWorkflowParallelism {
		t.Fatalf("invalid env should fall back to the default, got %d", got)
	}
	t.Setenv(EnvMaxWorkflowParallelism, "0")
	if got := DefaultMaxWorkflowParallelism(); got != DefaultWorkflowParallelism {
		t.Fatalf("non-positive env should fall back to the default, got %d", got)
	}
}

func TestParallelBranchOutcomeExecutionShare(t *testing.T) {
	out := ParallelBranchOutcome{Duration: 100 * time.Millisecond, Wait: 25 * time.Millisecond}
	if got := out.ExecutionShare(); got < 0.749 || got > 0.751 {
		t.Fatalf("ExecutionShare = %v, want 0.75", got)
	}
	if got := (ParallelBranchOutcome{}).ExecutionShare(); got != 0 {
		t.Fatalf("zero-duration share = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Executor integration tests: parallel nodes inside the DAG scheduler.
// ---------------------------------------------------------------------------

// gateSteps is a StepExecutor whose steps block on a gate channel after
// registering in the active set, so tests can prove wave concurrency with
// exact synchronization (no sleeps). Failing steps bypass the gate and return
// immediately, so a failing branch deterministically completes before its
// gated peers.
type gateSteps struct {
	mu      sync.Mutex
	active  int
	maxSeen int
	gate    chan struct{}
	outputs map[string]json.RawMessage
	fail    map[string]bool
}

func newGateSteps() *gateSteps {
	return &gateSteps{gate: make(chan struct{}), outputs: map[string]json.RawMessage{}, fail: map[string]bool{}}
}

// ExecuteStep implements StepExecutor.
func (g *gateSteps) ExecuteStep(ctx context.Context, req StepRequest) (json.RawMessage, error) {
	g.mu.Lock()
	g.active++
	if g.active > g.maxSeen {
		g.maxSeen = g.active
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active--
		g.mu.Unlock()
	}()
	if g.fail[req.NodeID] {
		return nil, fmt.Errorf("step %s failed", req.NodeID)
	}
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if out, ok := g.outputs[req.NodeID]; ok {
		return out, nil
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (g *gateSteps) maxConcurrent() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxSeen
}

// waitAndOpen spins until the gate proof point (active >= n) is reached and
// opens the gate; a sequential executor never reaches n and the 5s deadline
// fails the test instead of hanging.
func (g *gateSteps) waitAndOpen(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		active := g.active
		g.mu.Unlock()
		if active >= n {
			close(g.gate)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gate proof point %d never reached (active=%d) — execution is not concurrent", n, active)
		}
		runtime.Gosched()
	}
}

// gatedEngineFixture wires a workflow service with the gate step executor
// (and optionally a per-org quota hook).
func gatedEngineFixture(steps *gateSteps, quota OrgParallelQuota) *Service {
	svc := NewServiceWithOptions(nil, WithClock(newFakeClock()))
	svc.SetEngine(Engine{StepExecutor: steps, OrgQuota: quota})
	return svc
}

// failedRunNodeRuns digs the node runs of a failed execution out of the
// service's in-memory caches (same-package white-box helper): ExecuteWorkflow
// returns a nil result on aggregate failure, so there is no run id to query.
func failedRunNodeRuns(t *testing.T, svc *Service) (string, []*NodeRun) {
	t.Helper()
	svc.mu.Lock()
	defer svc.mu.Unlock()
	for wrID, runs := range svc.nodeRuns {
		if len(runs) > 0 {
			return wrID, runs
		}
	}
	t.Fatal("no node runs recorded")
	return "", nil
}

func failedRunStatus(t *testing.T, svc *Service, wrID string) string {
	t.Helper()
	svc.mu.Lock()
	defer svc.mu.Unlock()
	wr, ok := svc.workflowRuns[wrID]
	if !ok {
		t.Fatalf("workflow run %s not recorded", wrID)
	}
	return wr.Status
}

func nodeRunByNode(t *testing.T, nodeRuns []*NodeRun, nodeID string) *NodeRun {
	t.Helper()
	var found *NodeRun
	for _, nr := range nodeRuns {
		if nr.NodeID == nodeID {
			if found != nil {
				t.Fatalf("node %s has more than one run in this assertion", nodeID)
			}
			found = nr
		}
	}
	if found == nil {
		t.Fatalf("no node run recorded for %s", nodeID)
	}
	return found
}

func TestExecuteWorkflowParallelFanOutRunsConcurrently(t *testing.T) {
	steps := newGateSteps()
	svc := gatedEngineFixture(steps, nil)
	wf := mustWorkflow(t, svc, "FanOut", DSL{
		Nodes: []Node{
			{ID: "par", Type: NodeParallel},
			{ID: "b1", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "b2", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "b3", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "b4", Type: NodeTool, Config: map[string]any{"tool_id": "t1", "agent_id": "a1"}},
			{ID: "b5", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{
			{From: "par", To: "b1", Condition: EdgeAlways},
			{From: "par", To: "b2", Condition: EdgeAlways},
			{From: "par", To: "b3", Condition: EdgeAlways},
			{From: "par", To: "b4", Condition: EdgeAlways},
			{From: "par", To: "b5", Condition: EdgeAlways},
		},
	})
	type execOut struct {
		result *ExecutionResult
		err    error
	}
	done := make(chan execOut, 1)
	go func() {
		result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
		done <- execOut{result, err}
	}()
	steps.waitAndOpen(t, 5) // only reachable when all 5 branches run concurrently
	out := <-done
	if out.err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", out.err)
	}
	if got := steps.maxConcurrent(); got != 5 {
		t.Fatalf("max concurrent steps = %d, want 5", got)
	}
	_, nodeRuns, err := svc.GetWorkflowRun(context.Background(), "org-1", out.result.WorkflowRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", err)
	}
	// Branch completion metadata: per-branch duration/wait/utilization.
	for _, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		nr := nodeRunByNode(t, nodeRuns, id)
		if nr.Status != RunStatusCompleted {
			t.Fatalf("branch %s status = %q, want completed", id, nr.Status)
		}
		if nr.Metadata["parallel_node"] != "par" {
			t.Fatalf("branch %s metadata missing parallel_node: %v", id, nr.Metadata)
		}
		for _, key := range []string{"duration_ms", "wait_ms", "utilization"} {
			if _, ok := nr.Metadata[key]; !ok {
				t.Fatalf("branch %s metadata missing %q: %v", id, key, nr.Metadata)
			}
		}
	}
	// Parallel node aggregate metadata: deterministic branch order + limits.
	par := nodeRunByNode(t, nodeRuns, "par")
	agg, ok := par.Metadata["parallel"].(map[string]any)
	if !ok {
		t.Fatalf("parallel node metadata missing aggregate: %v", par.Metadata)
	}
	if agg["strategy"] != string(ParallelAggAll) || agg["mode"] != ParallelModeFailFast {
		t.Fatalf("aggregate = %v", agg)
	}
	if got := metaNumber(t, agg, "branches"); got != 5 || metaNumber(t, agg, "succeeded") != 5 {
		t.Fatalf("aggregate counts = %v", agg)
	}
	if got := metaNumber(t, agg, "max_in_flight"); got != 5 {
		t.Fatalf("aggregate max_in_flight = %v, want 5", agg["max_in_flight"])
	}
	var order []string
	switch v := agg["branch_order"].(type) {
	case []string: // in-memory metadata keeps Go slices verbatim
		order = v
	case []any: // JSON round-tripped metadata
		for _, item := range v {
			if s, ok := item.(string); ok {
				order = append(order, s)
			}
		}
	default:
		order = nil
	}
	if len(order) != 5 || order[0] != "b1" || order[4] != "b5" {
		t.Fatalf("branch_order = %v, want deterministic b1..b5", agg["branch_order"])
	}
	limits, ok := agg["limits"].(map[string]any)
	if !ok || metaNumber(t, limits, "global") != float64(DefaultWorkflowParallelism) {
		t.Fatalf("aggregate limits = %v, want the global default", agg["limits"])
	}
}

func TestExecuteWorkflowParallelGroupExcludesDependentChildren(t *testing.T) {
	steps := newGateSteps()
	svc := gatedEngineFixture(steps, nil)
	// b3 depends on b1 (edge b1->b3): the wave is {b1, b2}; b3 activates
	// through normal scheduling after b1 completes.
	wf := mustWorkflow(t, svc, "Deps", DSL{
		Nodes: []Node{
			{ID: "par", Type: NodeParallel},
			{ID: "b1", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "b2", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "b3", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{
			{From: "par", To: "b1", Condition: EdgeAlways},
			{From: "par", To: "b2", Condition: EdgeAlways},
			{From: "par", To: "b3", Condition: EdgeAlways},
			{From: "b1", To: "b3", Condition: EdgeAlways},
		},
	})
	done := make(chan error, 1)
	go func() {
		_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
		done <- err
	}()
	steps.waitAndOpen(t, 2) // exactly the two independent wave members
	if err := <-done; err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if got := steps.maxConcurrent(); got != 2 {
		t.Fatalf("max concurrent steps = %d, want 2 (b3 must wait for b1)", got)
	}
}

func TestExecuteWorkflowParallelAllFailureFailsRunAndCancels(t *testing.T) {
	steps := newGateSteps()
	steps.fail["bad"] = true // fails immediately, without the gate
	svc := gatedEngineFixture(steps, nil)
	wf := mustWorkflow(t, svc, "AllFail", DSL{
		Nodes: []Node{
			{ID: "par", Type: NodeParallel},
			{ID: "bad", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "slow", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "after-slow", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{
			{From: "par", To: "bad", Condition: EdgeAlways},
			{From: "par", To: "slow", Condition: EdgeAlways},
			{From: "slow", To: "after-slow", Condition: EdgeAlways},
		},
	})
	_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err == nil || !strings.Contains(err.Error(), "parallel node par") {
		t.Fatalf("ExecuteWorkflow error = %v, want parallel aggregate failure", err)
	}
	// "slow" blocks on the never-opened gate, so the fail-fast cancellation
	// is the only way it terminates: cancelled (not completed) — and its
	// downstream node is skipped through the prune cascade.
	wrID, nodeRuns := failedRunNodeRuns(t, svc)
	if got := failedRunStatus(t, svc, wrID); got != RunStatusFailed {
		t.Fatalf("workflow run status = %q, want failed", got)
	}
	if got := nodeRunByNode(t, nodeRuns, "bad").Status; got != RunStatusFailed {
		t.Fatalf("bad branch status = %q, want failed", got)
	}
	slow := nodeRunByNode(t, nodeRuns, "slow")
	if slow.Status != RunStatusCancelled && slow.Status != RunStatusTimeout {
		t.Fatalf("slow branch status = %q, want cancelled by fail_fast", slow.Status)
	}
	if got := nodeRunByNode(t, nodeRuns, "after-slow").Status; got != RunStatusSkipped {
		t.Fatalf("after-slow status = %q, want skipped after branch cancellation", got)
	}
}

func TestExecuteWorkflowParallelCollectAllRecordsEveryBranch(t *testing.T) {
	steps := newGateSteps()
	steps.fail["bad"] = true
	svc := gatedEngineFixture(steps, nil)
	wf := mustWorkflow(t, svc, "CollectAll", DSL{
		Nodes: []Node{
			{ID: "par", Type: NodeParallel, Config: map[string]any{"fail_fast": false}},
			{ID: "ok", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "bad", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{
			{From: "par", To: "ok", Condition: EdgeAlways},
			{From: "par", To: "bad", Condition: EdgeAlways},
		},
	})
	done := make(chan error, 1)
	go func() {
		_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
		done <- err
	}()
	steps.waitAndOpen(t, 1) // "ok" holds the gate until it may complete
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "all strategy failed") {
		t.Fatalf("collect_all with a failed branch should fail the aggregate, got %v", err)
	}
	_, nodeRuns := failedRunNodeRuns(t, svc)
	if got := nodeRunByNode(t, nodeRuns, "ok").Status; got != RunStatusCompleted {
		t.Fatalf("ok branch status = %q, want completed (collect_all never cancels)", got)
	}
	if got := nodeRunByNode(t, nodeRuns, "bad").Status; got != RunStatusFailed {
		t.Fatalf("bad branch status = %q, want failed", got)
	}
	par := nodeRunByNode(t, nodeRuns, "par")
	if par.Status != RunStatusFailed {
		t.Fatalf("parallel node status = %q, want failed (all strategy with a failed branch)", par.Status)
	}
}

func TestExecuteWorkflowParallelAnyStrategyRoutesWinner(t *testing.T) {
	steps := newGateSteps()
	steps.outputs["win"] = json.RawMessage(`{"picked":true}`)
	steps.fail["flaky"] = true
	svc := gatedEngineFixture(steps, nil)
	wf := mustWorkflow(t, svc, "AnyWins", DSL{
		Nodes: []Node{
			{ID: "par", Type: NodeParallel, Config: map[string]any{"strategy": "any"}},
			{ID: "flaky", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "win", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{
			{From: "par", To: "flaky", Condition: EdgeAlways},
			{From: "par", To: "win", Condition: EdgeAlways},
		},
	})
	done := make(chan error, 1)
	go func() {
		_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
		done <- err
	}()
	steps.waitAndOpen(t, 1)
	if err := <-done; err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	_, nodeRuns, gerr := svc.GetWorkflowRun(context.Background(), "org-1", lastRunID(t, svc))
	if gerr != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", gerr)
	}
	win := nodeRunByNode(t, nodeRuns, "win")
	if win.Status != RunStatusCompleted {
		t.Fatalf("winner status = %q, want completed", win.Status)
	}
	par := nodeRunByNode(t, nodeRuns, "par")
	agg := par.Metadata["parallel"].(map[string]any)
	if agg["winner"] != "win" || agg["strategy"] != string(ParallelAggAny) {
		t.Fatalf("aggregate = %v, want any/winner", agg)
	}
}

func lastRunID(t *testing.T, svc *Service) string {
	t.Helper()
	svc.mu.Lock()
	defer svc.mu.Unlock()
	for wrID := range svc.workflowRuns {
		return wrID
	}
	t.Fatal("no workflow run recorded")
	return ""
}

// metaNumber reads a numeric metadata value, normalizing the Go types that
// appear in in-memory metadata (int/int64) vs JSON round-trips (float64).
func metaNumber(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("metadata missing %q: %v", key, m)
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	default:
		t.Fatalf("metadata %q = %#v, want a number", key, v)
		return 0
	}
}

func TestExecuteWorkflowParallelMajorityStrategyAggregates(t *testing.T) {
	steps := newGateSteps()
	steps.outputs["v1"] = json.RawMessage(`"consensus"`)
	steps.outputs["v1b"] = json.RawMessage(`"consensus"`)
	steps.outputs["v2"] = json.RawMessage(`"outlier"`)
	svc := gatedEngineFixture(steps, nil)
	wf := mustWorkflow(t, svc, "Vote", DSL{
		Nodes: []Node{
			{ID: "par", Type: NodeParallel, Config: map[string]any{"strategy": "majority", "fail_fast": false}},
			{ID: "v1", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "v1b", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "v2", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{
			{From: "par", To: "v1", Condition: EdgeAlways},
			{From: "par", To: "v1b", Condition: EdgeAlways},
			{From: "par", To: "v2", Condition: EdgeAlways},
		},
	})
	done := make(chan error, 1)
	go func() {
		_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
		done <- err
	}()
	steps.waitAndOpen(t, 3)
	if err := <-done; err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	_, nodeRuns, gerr := svc.GetWorkflowRun(context.Background(), "org-1", lastRunID(t, svc))
	if gerr != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", gerr)
	}
	par := nodeRunByNode(t, nodeRuns, "par")
	if par.Status != RunStatusCompleted || !jsonEqual(json.RawMessage(par.Output), json.RawMessage(`"consensus"`)) {
		t.Fatalf("majority output = %s (status %q), want consensus", par.Output, par.Status)
	}
	agg := par.Metadata["parallel"].(map[string]any)
	if agg["winner"] != "v1" && agg["winner"] != "v1b" {
		t.Fatalf("majority winner = %v, want a consensus branch", agg["winner"])
	}
}

func TestExecuteWorkflowParallelPerBranchTimeoutFailsBranch(t *testing.T) {
	steps := newGateSteps()
	svc := gatedEngineFixture(steps, nil)
	wf := mustWorkflow(t, svc, "BranchTimeout", DSL{
		Nodes: []Node{
			{ID: "par", Type: NodeParallel, Config: map[string]any{"branch_timeout_seconds": 0.02}},
			{ID: "stuck", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{{From: "par", To: "stuck", Condition: EdgeAlways}},
	})
	// "stuck" blocks on the never-opened gate; its per-branch context times
	// out after 20ms — no sleeps, the branch context is the timer.
	_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err == nil || !strings.Contains(err.Error(), "parallel node par") {
		t.Fatalf("branch timeout should fail the aggregate, got %v", err)
	}
	_, nodeRuns := failedRunNodeRuns(t, svc)
	stuck := nodeRunByNode(t, nodeRuns, "stuck")
	if stuck.Status != RunStatusTimeout || stuck.ErrorCode != "PARALLEL_BRANCH_TIMEOUT" {
		t.Fatalf("stuck branch = %q/%q, want timeout/PARALLEL_BRANCH_TIMEOUT", stuck.Status, stuck.ErrorCode)
	}
}

func TestExecuteWorkflowParallelPerWorkflowConcurrencyOverride(t *testing.T) {
	steps := newGateSteps()
	svc := gatedEngineFixture(steps, nil)
	wf := mustWorkflow(t, svc, "Capped", DSL{
		Nodes: []Node{
			{ID: "par", Type: NodeParallel, Config: map[string]any{"max_concurrency": 2}},
			{ID: "b1", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "b2", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "b3", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "b4", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{
			{From: "par", To: "b1", Condition: EdgeAlways},
			{From: "par", To: "b2", Condition: EdgeAlways},
			{From: "par", To: "b3", Condition: EdgeAlways},
			{From: "par", To: "b4", Condition: EdgeAlways},
		},
	})
	done := make(chan error, 1)
	go func() {
		_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
		done <- err
	}()
	steps.waitAndOpen(t, 2) // the cap is 2: exactly two branches run together
	if err := <-done; err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if got := steps.maxConcurrent(); got != 2 {
		t.Fatalf("max concurrent steps = %d, want 2 (per-workflow override enforced)", got)
	}
	_, nodeRuns, gerr := svc.GetWorkflowRun(context.Background(), "org-1", lastRunID(t, svc))
	if gerr != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", gerr)
	}
	par := nodeRunByNode(t, nodeRuns, "par")
	limits := par.Metadata["parallel"].(map[string]any)["limits"].(map[string]any)
	if metaNumber(t, limits, "per_workflow") != 2 || metaNumber(t, limits, "effective") != 2 {
		t.Fatalf("limits = %v, want per_workflow=2 effective=2", limits)
	}
	if got := metaNumber(t, par.Metadata["parallel"].(map[string]any), "max_in_flight"); got != 2 {
		t.Fatalf("max_in_flight = %v, want 2", got)
	}
}

// fixedQuota is a per-org quota hook returning a constant cap.
type fixedQuota struct{ n int }

// MaxWorkflowParallelism implements OrgParallelQuota.
func (f fixedQuota) MaxWorkflowParallelism(context.Context, string) int { return f.n }

func TestExecuteWorkflowParallelOrgQuotaHook(t *testing.T) {
	steps := newGateSteps()
	svc := gatedEngineFixture(steps, fixedQuota{n: 1})
	wf := mustWorkflow(t, svc, "Quota", DSL{
		Nodes: []Node{
			{ID: "par", Type: NodeParallel},
			{ID: "b1", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "b2", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{
			{From: "par", To: "b1", Condition: EdgeAlways},
			{From: "par", To: "b2", Condition: EdgeAlways},
		},
	})
	done := make(chan error, 1)
	go func() {
		_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
		done <- err
	}()
	steps.waitAndOpen(t, 1) // quota 1: a single branch runs at a time
	if err := <-done; err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if got := steps.maxConcurrent(); got != 1 {
		t.Fatalf("max concurrent steps = %d, want 1 (org quota enforced)", got)
	}
	_, nodeRuns, gerr := svc.GetWorkflowRun(context.Background(), "org-1", lastRunID(t, svc))
	if gerr != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", gerr)
	}
	par := nodeRunByNode(t, nodeRuns, "par")
	limits := par.Metadata["parallel"].(map[string]any)["limits"].(map[string]any)
	if metaNumber(t, limits, "org_quota") != 1 || metaNumber(t, limits, "effective") != 1 {
		t.Fatalf("limits = %v, want org_quota=1 effective=1", limits)
	}
}

func TestValidateDSLParallelNodeConfig(t *testing.T) {
	valid := DSL{Nodes: []Node{
		{ID: "par", Type: NodeParallel, Config: map[string]any{"strategy": "majority", "max_concurrency": 4, "branch_timeout_seconds": 30}},
		{ID: "b", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
	}, Edges: []Edge{{From: "par", To: "b", Condition: EdgeAlways}}}
	if errs := ValidateDSL(valid); errs != nil {
		t.Fatalf("valid parallel config rejected: %v", errs)
	}

	invalid := DSL{Nodes: []Node{
		{ID: "par", Type: NodeParallel, Config: map[string]any{"strategy": "fastest", "max_concurrency": 0, "branch_timeout_seconds": -1}},
		{ID: "b", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
	}, Edges: []Edge{{From: "par", To: "b", Condition: EdgeAlways}}}
	errs := ValidateDSL(invalid)
	joined := make([]string, 0, len(errs))
	for _, e := range errs {
		joined = append(joined, e.Code)
	}
	joinedText := strings.Join(joined, ",")
	for _, want := range []string{"invalid_strategy", "missing_config"} {
		if !strings.Contains(joinedText, want) {
			t.Fatalf("errors should contain %q, got %s", want, joinedText)
		}
	}
	if len(errs) != 3 {
		t.Fatalf("want 3 validation errors (strategy + max_concurrency + timeout), got %d: %v", len(errs), errs)
	}
}

func TestParallelNodeConfigFromNode(t *testing.T) {
	cfg := ParallelNodeConfigFromNode(Node{ID: "p", Type: NodeParallel, Config: map[string]any{
		"strategy":               "RACE",
		"max_concurrency":        float64(4),
		"fail_fast":              false,
		"branch_timeout_seconds": float64(2.5),
	}})
	if cfg.Strategy != ParallelAggRace || cfg.MaxConcurrency != 4 || cfg.FailFast || cfg.BranchTimeout != 2500*time.Millisecond {
		t.Fatalf("ParallelNodeConfigFromNode = %#v", cfg)
	}
	def := ParallelNodeConfigFromNode(Node{ID: "p", Type: NodeParallel, Config: map[string]any{"strategy": "bogus"}})
	if def.Strategy != ParallelAggAll || !def.FailFast || def.MaxConcurrency != 0 || def.BranchTimeout != 0 {
		t.Fatalf("defaults = %#v (unknown strategy falls back to all)", def)
	}
}

// TestRunStatusForBranchMapping keeps the executor's status stamping honest.
func TestRunStatusForBranchMapping(t *testing.T) {
	if runStatusForBranch(ParallelBranchCancelled) != RunStatusCancelled ||
		runStatusForBranch(ParallelBranchTimeout) != RunStatusTimeout ||
		runStatusForBranch(ParallelBranchFailed) != RunStatusFailed ||
		runStatusForBranch(ParallelBranchCompleted) != RunStatusCompleted {
		t.Fatal("branch status mapping broken")
	}
	if branchErrorCode(ParallelBranchTimeout) != "PARALLEL_BRANCH_TIMEOUT" ||
		branchErrorCode(ParallelBranchCancelled) != "PARALLEL_BRANCH_CANCELLED" {
		t.Fatal("branch error code mapping broken")
	}
	if got := classifyBranchError(context.DeadlineExceeded); got != ParallelBranchTimeout {
		t.Fatalf("classifyBranchError(deadline) = %q", got)
	}
	if got := classifyBranchError(errors.New("boom")); got != ParallelBranchFailed {
		t.Fatalf("classifyBranchError(plain) = %q", got)
	}
}
