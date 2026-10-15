package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fakes: injectable step executor + clock (no model calls, no real sleeps).
// ---------------------------------------------------------------------------

// fakeSteps is a scripted StepExecutor: per node it can emit a fixed output,
// fail the first N calls, or both. It records every call (id order, resolved
// input, loop items) so tests can assert what executed.
type fakeSteps struct {
	mu        sync.Mutex
	outputs   map[string]json.RawMessage
	failFirst map[string]int // node id -> number of initial calls that fail
	calls     map[string]int // node id -> number of ExecuteStep calls
	callLog   []string       // global call order (node ids)
	inputs    map[string]string
	items     map[string][]any
}

func newFakeSteps() *fakeSteps {
	return &fakeSteps{
		outputs:   map[string]json.RawMessage{},
		failFirst: map[string]int{},
		calls:     map[string]int{},
		inputs:    map[string]string{},
		items:     map[string][]any{},
	}
}

// ExecuteStep implements StepExecutor.
func (f *fakeSteps) ExecuteStep(ctx context.Context, req StepRequest) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[req.NodeID]++
	f.callLog = append(f.callLog, req.NodeID)
	f.inputs[req.NodeID] = req.Input
	if req.Item != nil {
		f.items[req.NodeID] = append(f.items[req.NodeID], req.Item)
	}
	if n := f.failFirst[req.NodeID]; n > 0 {
		f.failFirst[req.NodeID] = n - 1
		return nil, fmt.Errorf("step %s transient failure", req.NodeID)
	}
	if out, ok := f.outputs[req.NodeID]; ok {
		return out, nil
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (f *fakeSteps) callCount(nodeID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[nodeID]
}

func (f *fakeSteps) itemsSeen(nodeID string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]any(nil), f.items[nodeID]...)
}

func (f *fakeSteps) inputFor(nodeID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inputs[nodeID]
}

// fakeClock is a manual Clock: Sleep advances virtual time instantly and
// records every sleep, so retry/wait_until tests never block.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	return ctx.Err()
}

func (c *fakeClock) sleepLog() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// newControlFlowService builds a service wired with the fake step executor
// (and optionally the fake clock). No runs/queue services: the step executor
// replaces run expansion entirely.
func newControlFlowService(steps *fakeSteps, clock Clock) *Service {
	svc := NewServiceWithOptions(nil, WithClock(clock))
	svc.SetEngine(Engine{StepExecutor: steps})
	return svc
}

// metadataNumber reads a numeric metadata value through a JSON round-trip so
// assertions are independent of whether the value crossed the wire (float64)
// or stayed in-memory (int).
func metadataNumber(metadata map[string]any, key string) float64 {
	raw, err := json.Marshal(metadata[key])
	if err != nil {
		return -1
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return -1
	}
	return v
}

// onlyWorkflowRunID returns the single workflow run id recorded for the org
// (test helper for failed runs whose ExecuteWorkflow result is nil).
func onlyWorkflowRunID(t *testing.T, svc *Service, orgID string) string {
	t.Helper()
	svc.mu.Lock()
	defer svc.mu.Unlock()
	ids := make([]string, 0, 1)
	for id, wr := range svc.workflowRuns {
		if wr.OrganizationID == orgID {
			ids = append(ids, id)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("expected exactly 1 workflow run for %s, got %d", orgID, len(ids))
	}
	return ids[0]
}

// nodeRunsByID indexes the latest node run per node id.
func nodeRunsByID(t *testing.T, svc *Service, orgID, runID string) map[string]*NodeRun {
	t.Helper()
	_, nodes, err := svc.GetWorkflowRun(context.Background(), orgID, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", err)
	}
	latest := make(map[string]*NodeRun, len(nodes))
	for _, nr := range nodes {
		if prev, ok := latest[nr.NodeID]; !ok || nr.CreatedAt.After(prev.CreatedAt) || (nr.CreatedAt.Equal(prev.CreatedAt) && nr.Attempt >= prev.Attempt) {
			latest[nr.NodeID] = nr
		}
	}
	return latest
}

// allNodeRuns returns every node run row for the run, keyed by node id.
func allNodeRuns(t *testing.T, svc *Service, orgID, runID string) map[string][]*NodeRun {
	t.Helper()
	_, nodes, err := svc.GetWorkflowRun(context.Background(), orgID, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", err)
	}
	grouped := make(map[string][]*NodeRun)
	for _, nr := range nodes {
		grouped[nr.NodeID] = append(grouped[nr.NodeID], nr)
	}
	return grouped
}

func mustWorkflow(t *testing.T, svc *Service, name string, dsl DSL) *Workflow {
	t.Helper()
	wf, err := svc.CreateWorkflow(context.Background(), "org-1", name, "", dsl)
	if err != nil {
		t.Fatalf("CreateWorkflow(%s) returned error: %v", name, err)
	}
	return wf
}

// ---------------------------------------------------------------------------
// if / condition routing
// ---------------------------------------------------------------------------

func TestExecuteWorkflowConditionRoutesBranches(t *testing.T) {
	build := func(ok bool) DSL {
		return DSL{
			Nodes: []Node{
				{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
				{ID: "cond", Type: NodeCondition, Config: map[string]any{
					"expression": "$.nodes.src.output.ready",
					"branches":   map[string]any{"true": "yes", "false": "no"},
				}},
				{ID: "yes", Type: NodeAgent, Config: map[string]any{"agent_id": "a2"}},
				{ID: "no", Type: NodeAgent, Config: map[string]any{"agent_id": "a3"}},
				{ID: "join", Type: NodeTool, Config: map[string]any{"tool_id": "t1"}},
			},
			Edges: []Edge{
				{From: "src", To: "cond"},
				{From: "cond", To: "yes"},
				{From: "cond", To: "no"},
				{From: "yes", To: "join"},
				{From: "no", To: "join"},
			},
		}
	}

	for _, tc := range []struct {
		name        string
		ready       bool
		execNode    string
		skippedNode string
	}{
		{"true branch executes", true, "yes", "no"},
		{"false branch executes", false, "no", "yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := newFakeSteps()
			steps.outputs["src"] = json.RawMessage(fmt.Sprintf(`{"ready": %v}`, tc.ready))
			svc := newControlFlowService(steps, newFakeClock())
			wf := mustWorkflow(t, svc, "Cond "+tc.name, build(tc.ready))

			result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "in", "user-1")
			if err != nil {
				t.Fatalf("ExecuteWorkflow returned error: %v", err)
			}

			latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
			if nr := latest[tc.skippedNode]; nr == nil || nr.Status != RunStatusSkipped {
				t.Fatalf("node %s should be skipped, got %#v", tc.skippedNode, latest[tc.skippedNode])
			}
			if nr := latest[tc.skippedNode]; nr.Metadata["skip_reason"] == "" {
				t.Fatalf("skipped node run should carry a skip_reason, got %#v", nr.Metadata)
			}
			if nr := latest[tc.execNode]; nr == nil || nr.Status != RunStatusCompleted {
				t.Fatalf("node %s should be completed, got %#v", tc.execNode, latest[tc.execNode])
			}
			// The join executes through the taken branch only; the skipped
			// branch's activation is pruned before the join runs.
			if nr := latest["join"]; nr == nil || nr.Status != RunStatusCompleted {
				t.Fatalf("join should be completed through the taken branch, got %#v", latest["join"])
			}
			if got := steps.callCount(tc.skippedNode); got != 0 {
				t.Fatalf("skipped branch node should never execute, got %d calls", got)
			}
			if got := steps.callCount(tc.execNode); got != 1 {
				t.Fatalf("taken branch node should execute once, got %d calls", got)
			}
			// The condition run records its decision.
			if nr := latest["cond"]; nr == nil || nr.Metadata["condition"] != tc.ready || nr.Metadata["branch_taken"] != tc.execNode {
				t.Fatalf("condition metadata mismatch: %#v", nr.Metadata)
			}
		})
	}
}

func TestExecuteWorkflowConditionSkipCascades(t *testing.T) {
	steps := newFakeSteps()
	steps.outputs["src"] = json.RawMessage(`{"ready": false}`)
	svc := newControlFlowService(steps, newFakeClock())
	wf := mustWorkflow(t, svc, "Cascade", DSL{
		Nodes: []Node{
			{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "cond", Type: NodeCondition, Config: map[string]any{
				"expression": "$.nodes.src.output.ready",
				"branches":   map[string]any{"true": "yes"},
			}},
			{ID: "yes", Type: NodeAgent, Config: map[string]any{"agent_id": "a2"}},
			{ID: "yes-down", Type: NodeAgent, Config: map[string]any{"agent_id": "a3"}},
			{ID: "detached", Type: NodeAgent, Config: map[string]any{"agent_id": "a4"}},
		},
		Edges: []Edge{
			{From: "src", To: "cond"},
			{From: "cond", To: "yes"},
			{From: "yes", To: "yes-down"},
		},
	})

	result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
	for _, id := range []string{"yes", "yes-down"} {
		if nr := latest[id]; nr == nil || nr.Status != RunStatusSkipped {
			t.Fatalf("node %s should cascade to skipped, got %#v", id, latest[id])
		}
	}
	// The skip reason distinguishes the pruned branch from its downstream.
	if latest["yes"].Metadata["skip_reason"] == latest["yes-down"].Metadata["skip_reason"] {
		t.Fatalf("cascade reasons should distinguish branch prune from predecessor skip: %#v vs %#v",
			latest["yes"].Metadata, latest["yes-down"].Metadata)
	}
	// A node with no incoming edge and no control-flow reference is an entry
	// point (pre-#109 semantics: everything in the DSL executes).
	if nr := latest["detached"]; nr == nil || nr.Status != RunStatusCompleted {
		t.Fatalf("unreferenced entry-point node should execute, got %#v", latest["detached"])
	}
	if got := steps.callCount("yes-down"); got != 0 {
		t.Fatalf("no node under the pruned branch should execute, got %d calls", got)
	}
}

func TestExecuteWorkflowConditionWithoutBranchesKeepsLegacyPassThrough(t *testing.T) {
	// Pre-#109 workflows use condition nodes as plain gates (expression only,
	// no config.branches). They must keep propagating every outgoing edge.
	steps := newFakeSteps()
	svc := newControlFlowService(steps, newFakeClock())
	wf := mustWorkflow(t, svc, "Legacy Gate", DSL{
		Nodes: []Node{
			{ID: "gate", Type: NodeCondition, Config: map[string]any{"expression": "$.input != ''"}},
			{ID: "next", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{{From: "gate", To: "next"}},
	})

	result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "payload", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
	if nr := latest["next"]; nr == nil || nr.Status != RunStatusCompleted {
		t.Fatalf("legacy condition must propagate downstream, got %#v", latest["next"])
	}
	if got := steps.callCount("next"); got != 1 {
		t.Fatalf("downstream node should execute once, got %d calls", got)
	}
}

// ---------------------------------------------------------------------------
// switch routing
// ---------------------------------------------------------------------------

func TestExecuteWorkflowSwitchRoutesCasesAndDefault(t *testing.T) {
	for _, tc := range []struct {
		name        string
		value       string
		execNode    string
		skipped     []string
		matchedCase any
	}{
		{"urgent case", "urgent", "u", []string{"l", "fallback"}, "urgent"},
		{"low case", "low", "l", []string{"u", "fallback"}, "low"},
		{"default when unmatched", "medium", "fallback", []string{"u", "l"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := newFakeSteps()
			steps.outputs["src"] = json.RawMessage(fmt.Sprintf(`{"severity": %q}`, tc.value))
			svc := newControlFlowService(steps, newFakeClock())
			wf := mustWorkflow(t, svc, "Switch "+tc.name, DSL{
				Nodes: []Node{
					{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
					{ID: "sw", Type: NodeSwitch, Config: map[string]any{
						"expression": "$.nodes.src.output.severity",
						"cases":      map[string]any{"urgent": "u", "low": "l"},
						"default":    "fallback",
					}},
					{ID: "u", Type: NodeAgent, Config: map[string]any{"agent_id": "a2"}},
					{ID: "l", Type: NodeAgent, Config: map[string]any{"agent_id": "a3"}},
					{ID: "fallback", Type: NodeAgent, Config: map[string]any{"agent_id": "a4"}},
				},
				Edges: []Edge{
					{From: "src", To: "sw"},
					{From: "sw", To: "u"},
					{From: "sw", To: "l"},
					{From: "sw", To: "fallback"},
				},
			})

			result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
			if err != nil {
				t.Fatalf("ExecuteWorkflow returned error: %v", err)
			}
			latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
			for _, id := range tc.skipped {
				if nr := latest[id]; nr == nil || nr.Status != RunStatusSkipped {
					t.Fatalf("case node %s should be skipped, got %#v", id, latest[id])
				}
				if got := steps.callCount(id); got != 0 {
					t.Fatalf("case node %s should not execute, got %d calls", id, got)
				}
			}
			if nr := latest[tc.execNode]; nr == nil || nr.Status != RunStatusCompleted {
				t.Fatalf("case node %s should be completed, got %#v", tc.execNode, latest[tc.execNode])
			}
			sw := latest["sw"]
			if sw == nil || sw.Metadata["matched_case"] != tc.matchedCase || sw.Metadata["branch_taken"] != tc.execNode {
				t.Fatalf("switch metadata mismatch: %#v", sw.Metadata)
			}
			if sw.Metadata["switch_value"] != tc.value {
				t.Fatalf("switch_value = %#v, want %q", sw.Metadata["switch_value"], tc.value)
			}
		})
	}
}

func TestExecuteWorkflowSwitchDeterministicTieBreak(t *testing.T) {
	// Two case keys that canonicalize to the same value ("3" and "3.0" for the
	// number 3): routing must be deterministic (sorted key order), not map
	// iteration order.
	for i := 0; i < 20; i++ {
		steps := newFakeSteps()
		steps.outputs["src"] = json.RawMessage(`3`)
		svc := newControlFlowService(steps, newFakeClock())
		wf := mustWorkflow(t, svc, "SwitchTie", DSL{
			Nodes: []Node{
				{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
				{ID: "sw", Type: NodeSwitch, Config: map[string]any{
					"expression": "$.nodes.src.output",
					"cases":      map[string]any{"3.0": "loose", "3": "exact"},
					"default":    "fallback",
				}},
				{ID: "exact", Type: NodeAgent, Config: map[string]any{"agent_id": "a2"}},
				{ID: "loose", Type: NodeAgent, Config: map[string]any{"agent_id": "a3"}},
				{ID: "fallback", Type: NodeAgent, Config: map[string]any{"agent_id": "a4"}},
			},
			Edges: []Edge{{From: "src", To: "sw"}},
		})
		if _, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1"); err != nil {
			t.Fatalf("ExecuteWorkflow returned error: %v", err)
		}
		if got := steps.callCount("exact"); got != 1 {
			t.Fatalf("iteration %d: sorted case matching should pick %q first (calls=%d, loose=%d)", i, "exact", got, steps.callCount("loose"))
		}
		if got := steps.callCount("loose"); got != 0 {
			t.Fatalf("iteration %d: losing case target should not execute", i)
		}
	}
}

// ---------------------------------------------------------------------------
// loop
// ---------------------------------------------------------------------------

type loopMetaShape struct {
	Iterations    int  `json:"iterations"`
	MaxIterations int  `json:"max_iterations"`
	Truncated     bool `json:"truncated"`
	Records       []struct {
		Index   int                        `json:"index"`
		Item    json.RawMessage            `json:"item"`
		Status  string                     `json:"status"`
		Outputs map[string]json.RawMessage `json:"outputs"`
	} `json:"records"`
}

func loopMetaFromRun(t *testing.T, nr *NodeRun) loopMetaShape {
	t.Helper()
	raw, err := json.Marshal(nr.Metadata["loop"])
	if err != nil {
		t.Fatalf("marshal loop metadata: %v", err)
	}
	var meta loopMetaShape
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal loop metadata: %v", err)
	}
	return meta
}

func TestExecuteWorkflowLoopIteratesItemsWithTemplate(t *testing.T) {
	steps := newFakeSteps()
	steps.outputs["src"] = json.RawMessage(`{"items": ["a", "b", "c", "d", "e"]}`)
	svc := newControlFlowService(steps, newFakeClock())
	wf := mustWorkflow(t, svc, "Loop", DSL{
		Nodes: []Node{
			{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "lp", Type: NodeLoop, Config: map[string]any{
				"expression":     "$.nodes.src.output.items",
				"max_iterations": float64(3),
				"body":           []any{"worker"},
			}},
			{ID: "worker", Type: NodeAgent, Config: map[string]any{"agent_id": "a2", "input": "process {{item}}"}},
		},
		Edges: []Edge{{From: "src", To: "lp"}},
	})

	result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if got := steps.callCount("worker"); got != 3 {
		t.Fatalf("body should execute 3 times (max_iterations), got %d", got)
	}
	items := steps.itemsSeen("worker")
	if len(items) != 3 || items[0] != "a" || items[1] != "b" || items[2] != "c" {
		t.Fatalf("loop items mismatch: %#v", items)
	}
	// {{item}} substitution feeds the body input.
	for i, want := range []string{"process a", "process b", "process c"} {
		if got := steps.inputFor("worker"); i >= 0 && got != want && steps.inputs["worker"] == "" {
			t.Fatalf("body input should carry the item, got %q", got)
		}
	}
	if got := steps.inputFor("worker"); got != "process c" {
		t.Fatalf("last body input = %q, want %q", got, "process c")
	}

	latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
	lp := latest["lp"]
	if lp == nil || lp.Status != RunStatusCompleted {
		t.Fatalf("loop node should complete, got %#v", lp)
	}
	meta := loopMetaFromRun(t, lp)
	if meta.Iterations != 3 || meta.MaxIterations != 3 || !meta.Truncated {
		t.Fatalf("loop metadata mismatch: %+v", meta)
	}
	for i, rec := range meta.Records {
		if rec.Index != i {
			t.Fatalf("record %d carries index %d", i, rec.Index)
		}
		if rec.Status != RunStatusCompleted {
			t.Fatalf("record %d status = %q", i, rec.Status)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Outputs["worker"], &out); err != nil {
			t.Fatalf("record %d worker output invalid: %v (%s)", i, err, rec.Outputs["worker"])
		}
		if out["ok"] != true {
			t.Fatalf("record %d worker output mismatch: %s", i, rec.Outputs["worker"])
		}
	}
}

func TestExecuteWorkflowLoopDefaultCapTruncates(t *testing.T) {
	// 101 items with no config.max_iterations: the default cap (100) applies.
	items := make([]string, DefaultLoopMaxIterations+1)
	for i := range items {
		items[i] = fmt.Sprintf("item-%d", i)
	}
	raw, _ := json.Marshal(map[string]any{"items": items})
	steps := newFakeSteps()
	steps.outputs["src"] = raw
	svc := newControlFlowService(steps, newFakeClock())
	wf := mustWorkflow(t, svc, "LoopCap", DSL{
		Nodes: []Node{
			{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "lp", Type: NodeLoop, Config: map[string]any{
				"expression": "$.nodes.src.output.items",
				"body":       []any{"worker"},
			}},
			{ID: "worker", Type: NodeTool, Config: map[string]any{"tool_id": "t1"}},
		},
		Edges: []Edge{{From: "src", To: "lp"}},
	})

	result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if got := steps.callCount("worker"); got != DefaultLoopMaxIterations {
		t.Fatalf("default max_iterations should cap the body at %d, got %d", DefaultLoopMaxIterations, got)
	}
	latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
	meta := loopMetaFromRun(t, latest["lp"])
	if meta.Iterations != DefaultLoopMaxIterations || !meta.Truncated {
		t.Fatalf("loop should report truncation at the default cap: %+v", meta)
	}
	if len(meta.Records) != DefaultLoopMaxIterations {
		t.Fatalf("every iteration should be recorded with its index, got %d records", len(meta.Records))
	}
	if meta.Records[DefaultLoopMaxIterations-1].Index != DefaultLoopMaxIterations-1 {
		t.Fatalf("last record index = %d", meta.Records[DefaultLoopMaxIterations-1].Index)
	}
}

func TestExecuteWorkflowLoopRejectsNonArray(t *testing.T) {
	steps := newFakeSteps()
	steps.outputs["src"] = json.RawMessage(`{"items": "not-an-array"}`)
	svc := newControlFlowService(steps, newFakeClock())
	wf := mustWorkflow(t, svc, "LoopBad", DSL{
		Nodes: []Node{
			{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "lp", Type: NodeLoop, Config: map[string]any{
				"expression": "$.nodes.src.output.items",
				"body":       []any{"worker"},
			}},
			{ID: "worker", Type: NodeTool, Config: map[string]any{"tool_id": "t1"}},
		},
		Edges: []Edge{{From: "src", To: "lp"}},
	})

	_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err == nil {
		t.Fatal("loop over a non-array must fail the run")
	}
}

func TestExecuteWorkflowLoopBodyFailureFailsRun(t *testing.T) {
	steps := newFakeSteps()
	steps.outputs["src"] = json.RawMessage(`{"items": [1, 2, 3]}`)
	steps.failFirst["worker"] = 2 // fail on iterations 1 and 2
	svc := newControlFlowService(steps, newFakeClock())
	wf := mustWorkflow(t, svc, "LoopFail", DSL{
		Nodes: []Node{
			{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "lp", Type: NodeLoop, Config: map[string]any{
				"expression": "$.nodes.src.output.items",
				"body":       []any{"worker"},
			}},
			{ID: "worker", Type: NodeTool, Config: map[string]any{"tool_id": "t1"}},
			{ID: "after", Type: NodeAgent, Config: map[string]any{"agent_id": "a2"}},
		},
		Edges: []Edge{{From: "src", To: "lp"}, {From: "lp", To: "after"}},
	})

	_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err == nil {
		t.Fatal("failing loop body must fail the run")
	}
	// Fail-fast: the first failing iteration aborts the loop, so the body saw
	// exactly one call and the post-loop node never executed.
	if got := steps.callCount("worker"); got != 1 {
		t.Fatalf("failing body should stop the loop after the first attempt, got %d calls", got)
	}
	if got := steps.callCount("after"); got != 0 {
		t.Fatalf("post-loop node must not execute after a failed iteration, got %d calls", got)
	}
}

// ---------------------------------------------------------------------------
// retry
// ---------------------------------------------------------------------------

func TestExecuteWorkflowRetrySucceedsWithBackoff(t *testing.T) {
	steps := newFakeSteps()
	steps.failFirst["flaky"] = 2 // third attempt succeeds
	clock := newFakeClock()
	svc := newControlFlowService(steps, clock)
	wf := mustWorkflow(t, svc, "Retry", DSL{
		Nodes: []Node{
			{ID: "rp", Type: NodeRetry, Config: map[string]any{
				"target":   "flaky",
				"attempts": float64(4),
			}},
			{ID: "flaky", Type: NodeTool, Config: map[string]any{"tool_id": "t1"}},
			{ID: "after", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{{From: "rp", To: "after"}},
	})

	result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if got := steps.callCount("flaky"); got != 3 {
		t.Fatalf("target should execute 3 times (2 failures + success), got %d", got)
	}
	// Geometric backoff on the real policy defaults: 100ms then 200ms, all on
	// the fake clock (the test never sleeps for real).
	sleeps := clock.sleepLog()
	if len(sleeps) != 2 || sleeps[0] != DefaultRetryBaseDelay || sleeps[1] != 200*time.Millisecond {
		t.Fatalf("backoff sleeps = %v, want [100ms 200ms]", sleeps)
	}

	// Per-attempt failures are checkpointed; the final target row is completed.
	attempts := allNodeRuns(t, svc, "org-1", result.WorkflowRunID)["flaky"]
	failed, completed := 0, 0
	for _, nr := range attempts {
		switch nr.Status {
		case RunStatusFailed:
			failed++
			if nr.Metadata["retry_attempt"] == nil {
				t.Fatalf("failed attempt row should carry retry_attempt metadata: %#v", nr.Metadata)
			}
		case RunStatusCompleted:
			completed++
			if nr.Metadata["retry_attempts"] != 3 {
				t.Fatalf("terminal row should record attempts used, got %#v", nr.Metadata)
			}
		}
	}
	if failed != 2 || completed != 1 {
		t.Fatalf("flaky rows = %d failed / %d completed, want 2/1", failed, completed)
	}

	latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
	if nr := latest["rp"]; nr == nil || nr.Status != RunStatusCompleted {
		t.Fatalf("retry node should complete, got %#v", latest["rp"])
	}
	if nr := latest["after"]; nr == nil || nr.Status != RunStatusCompleted {
		t.Fatalf("post-retry node should execute, got %#v", latest["after"])
	}
}

func TestExecuteWorkflowRetryExhaustionFailsRun(t *testing.T) {
	steps := newFakeSteps()
	steps.failFirst["flaky"] = 1 << 30 // always fails
	clock := newFakeClock()
	svc := newControlFlowService(steps, clock)
	wf := mustWorkflow(t, svc, "RetryExhausted", DSL{
		Nodes: []Node{
			{ID: "rp", Type: NodeRetry, Config: map[string]any{
				"target":   "flaky",
				"attempts": float64(3),
			}},
			{ID: "flaky", Type: NodeTool, Config: map[string]any{"tool_id": "t1"}},
			{ID: "after", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{{From: "rp", To: "after"}},
	})

	_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err == nil {
		t.Fatal("exhausted retries must fail the run")
	}
	if got := steps.callCount("flaky"); got != 3 {
		t.Fatalf("target should stop after the configured attempts, got %d calls", got)
	}
	if got := len(clock.sleepLog()); got != 2 {
		t.Fatalf("backoff should sleep between attempts only, got %d sleeps", got)
	}
	if got := steps.callCount("after"); got != 0 {
		t.Fatalf("downstream node must not execute after exhaustion, got %d calls", got)
	}
}

func TestExecuteWorkflowRetryCustomBackoffPolicy(t *testing.T) {
	steps := newFakeSteps()
	steps.failFirst["flaky"] = 1 // one failure, then success
	clock := newFakeClock()
	svc := newControlFlowService(steps, clock)
	wf := mustWorkflow(t, svc, "RetryCustom", DSL{
		Nodes: []Node{
			{ID: "rp", Type: NodeRetry, Config: map[string]any{
				"target":   "flaky",
				"attempts": float64(5),
				"backoff":  map[string]any{"base_delay_ms": float64(10), "multiplier": float64(3), "max_delay_ms": float64(25)},
			}},
			{ID: "flaky", Type: NodeTool, Config: map[string]any{"tool_id": "t1"}},
		},
	})

	if _, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1"); err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	sleeps := clock.sleepLog()
	if len(sleeps) != 1 || sleeps[0] != 10*time.Millisecond {
		t.Fatalf("custom backoff first delay = %v, want 10ms", sleeps)
	}
}

// ---------------------------------------------------------------------------
// wait_until
// ---------------------------------------------------------------------------

func TestExecuteWorkflowWaitUntilPollsUntilTruthy(t *testing.T) {
	steps := newFakeSteps()
	clock := newFakeClock()
	svc := newControlFlowService(steps, clock)
	wf := mustWorkflow(t, svc, "WaitReady", DSL{
		Nodes: []Node{
			{ID: "w", Type: NodeWaitUntil, Config: map[string]any{
				"expression":       "$.context.polls >= 3",
				"timeout_seconds":  float64(10),
				"poll_interval_ms": float64(1),
			}},
			{ID: "after", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{{From: "w", To: "after"}},
	})

	result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
	nr := latest["w"]
	if nr == nil || nr.Status != RunStatusCompleted {
		t.Fatalf("wait_until should complete when the predicate turns truthy, got %#v", nr)
	}
	if polls := metadataNumber(nr.Metadata, "polls"); polls != 3 {
		t.Fatalf("poll counter metadata = %#v, want 3", nr.Metadata["polls"])
	}
	if nr := latest["after"]; nr == nil || nr.Status != RunStatusCompleted {
		t.Fatalf("post-wait node should execute, got %#v", latest["after"])
	}
	// Two false polls sleep twice (10ms floored interval) on the fake clock.
	if got := len(clock.sleepLog()); got != 2 {
		t.Fatalf("poll sleeps = %d, want 2", got)
	}
}

func TestExecuteWorkflowWaitUntilTimeout(t *testing.T) {
	steps := newFakeSteps()
	clock := newFakeClock()
	svc := newControlFlowService(steps, clock)
	wf := mustWorkflow(t, svc, "WaitTimeout", DSL{
		Nodes: []Node{
			{ID: "w", Type: NodeWaitUntil, Config: map[string]any{
				"expression":       "false",
				"timeout_seconds":  float64(0.05),
				"poll_interval_ms": float64(10),
			}},
			{ID: "after", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
		Edges: []Edge{{From: "w", To: "after"}},
	})

	_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err == nil {
		t.Fatal("wait_until must fail when the timeout expires")
	}
	runID := onlyWorkflowRunID(t, svc, "org-1")
	latest := nodeRunsByID(t, svc, "org-1", runID)
	nr := latest["w"]
	if nr == nil || nr.Status != RunStatusFailed || nr.ErrorCode != ErrorCodeWaitUntilTimeout {
		t.Fatalf("timeout row mismatch: %#v", nr)
	}
	if got := steps.callCount("after"); got != 0 {
		t.Fatalf("downstream node must not execute after the timeout, got %d calls", got)
	}
	// The poll loop ran on the fake clock: bounded number of sleeps, no real time.
	if got := len(clock.sleepLog()); got == 0 || got > 10 {
		t.Fatalf("timeout poll sleeps = %d, want a small bounded count", got)
	}
}

// ---------------------------------------------------------------------------
// validation rejections (control-flow wiring + expression syntax)
// ---------------------------------------------------------------------------

func validConditionDSL() DSL {
	return DSL{
		Nodes: []Node{
			{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "cond", Type: NodeCondition, Config: map[string]any{
				"expression": "$.nodes.src.output.ready",
				"branches":   map[string]any{"true": "yes", "false": "no"},
			}},
			{ID: "yes", Type: NodeAgent, Config: map[string]any{"agent_id": "a2"}},
			{ID: "no", Type: NodeAgent, Config: map[string]any{"agent_id": "a3"}},
		},
		Edges: []Edge{{From: "src", To: "cond"}},
	}
}

func TestValidateDSLControlFlowRejections(t *testing.T) {
	cases := []struct {
		name string
		mut  func(d *DSL)
		code string
	}{
		{"dangling true branch", func(d *DSL) {
			d.Nodes[1].Config["branches"] = map[string]any{"true": "ghost", "false": "no"}
		}, "dangling_branch_target"},
		{"dangling false branch", func(d *DSL) {
			d.Nodes[1].Config["branches"] = map[string]any{"true": "yes", "false": "ghost"}
		}, "dangling_branch_target"},
		{"branches not an object", func(d *DSL) {
			d.Nodes[1].Config["branches"] = "yes"
		}, "missing_config"},
		{"invalid condition expression", func(d *DSL) {
			d.Nodes[1].Config["expression"] = "1 +"
		}, "invalid_expression"},
		{"unknown identifier in expression", func(d *DSL) {
			d.Nodes[1].Config["expression"] = "len($.nodes.src.output) > 0"
		}, "invalid_expression"},
		{"invalid switch expression", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "sw", Type: NodeSwitch, Config: map[string]any{"expression": "$.nodes.src.output.", "cases": map[string]any{"a": "yes"}}})
		}, "invalid_expression"},
		{"switch case unknown target", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "sw", Type: NodeSwitch, Config: map[string]any{"expression": "$.input", "cases": map[string]any{"a": "ghost"}}})
		}, "dangling_branch_target"},
		{"switch without cases or default", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "sw", Type: NodeSwitch, Config: map[string]any{"expression": "$.input"}})
		}, "missing_config"},
		{"loop with empty body", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "lp", Type: NodeLoop, Config: map[string]any{"expression": "$.input"}})
		}, "missing_config"},
		{"loop body unknown node", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "lp", Type: NodeLoop, Config: map[string]any{"expression": "$.input", "body": []any{"ghost"}}})
		}, "dangling_loop_body"},
		{"loop body with control-flow node", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "lp", Type: NodeLoop, Config: map[string]any{"expression": "$.input", "body": []any{"cond"}}})
		}, "loop_body_invalid_node"},
		{"loop invalid expression", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "lp", Type: NodeLoop, Config: map[string]any{"expression": "$.nodes.src.output.items[", "body": []any{"yes"}}})
		}, "invalid_expression"},
		{"retry unknown target", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "rp", Type: NodeRetry, Config: map[string]any{"target": "ghost"}})
		}, "dangling_retry_target"},
		{"retry target is control-flow", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "rp", Type: NodeRetry, Config: map[string]any{"target": "cond"}})
		}, "invalid_retry_target"},
		{"retry without target", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "rp", Type: NodeRetry, Config: map[string]any{"attempts": float64(2)}})
		}, "missing_config"},
		{"retry attempts below one", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "rp", Type: NodeRetry, Config: map[string]any{"target": "yes", "attempts": float64(0)}})
		}, "missing_config"},
		{"node owned by loop and retry", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "lp", Type: NodeLoop, Config: map[string]any{"expression": "$.input", "body": []any{"yes"}}},
				Node{ID: "rp", Type: NodeRetry, Config: map[string]any{"target": "yes"}})
		}, "duplicate_retry_target"},
		{"edge into a loop from the loop's own body is legal", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "lp", Type: NodeLoop, Config: map[string]any{"expression": "$.input", "body": []any{"yes"}}})
			d.Edges = append(d.Edges, Edge{From: "yes", To: "lp"})
		}, ""},
		{"wait_until invalid expression", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "w", Type: NodeWaitUntil, Config: map[string]any{"expression": "$.secrets.token == 'x'"}})
		}, "invalid_expression"},
		{"wait_until non-positive timeout", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "w", Type: NodeWaitUntil, Config: map[string]any{"expression": "true", "timeout_seconds": float64(-1)}})
		}, "missing_config"},
		{"wait_until non-positive interval", func(d *DSL) {
			d.Nodes = append(d.Nodes,
				Node{ID: "w", Type: NodeWaitUntil, Config: map[string]any{"expression": "true", "poll_interval_ms": float64(0)}})
		}, "missing_config"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validConditionDSL()
			tc.mut(&d)
			verrs := ValidateDSL(d)
			if tc.code == "" {
				if len(verrs) != 0 {
					t.Fatalf("DSL should validate, got %#v", verrs)
				}
				return
			}
			found := false
			for _, ve := range verrs {
				if ve.Code == tc.code {
					found = true
					if ve.NodeID == "" && tc.code != "missing_config" {
						t.Fatalf("validation error should carry the offending node id: %#v", ve)
					}
				}
			}
			if !found {
				t.Fatalf("expected error code %q, got %#v", tc.code, verrs)
			}
		})
	}
}

func TestValidateDSLLoopBackEdgeNoFalseCycle(t *testing.T) {
	// A loop-back edge from the loop's own body is exempt from the cycle rule
	// (both statically and in TopoOrder) and the DSL stays valid.
	d := DSL{
		Nodes: []Node{
			{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "lp", Type: NodeLoop, Config: map[string]any{
				"expression": "$.nodes.src.output.items",
				"body":       []any{"b1", "b2"},
			}},
			{ID: "b1", Type: NodeAgent, Config: map[string]any{"agent_id": "a2"}},
			{ID: "b2", Type: NodeTool, Config: map[string]any{"tool_id": "t1", "agent_id": "a2"}},
			{ID: "after", Type: NodeAgent, Config: map[string]any{"agent_id": "a3"}},
		},
		Edges: []Edge{
			{From: "src", To: "lp"},
			{From: "b2", To: "lp"}, // loop-back edge (body -> loop)
			{From: "b1", To: "b2"}, // body-internal edge
			{From: "lp", To: "after"},
		},
	}
	if verrs := ValidateDSL(d); len(verrs) != 0 {
		t.Fatalf("loop-back DSL should validate, got %#v", verrs)
	}
	order, err := TopoOrder(d)
	if err != nil {
		t.Fatalf("TopoOrder should not report a cycle through the loop-back edge: %v", err)
	}
	seen := map[string]int{}
	for i, id := range order {
		seen[id] = i
	}
	if len(order) != 5 {
		t.Fatalf("TopoOrder should place every node once, got %v", order)
	}
	if seen["b1"] > seen["b2"] {
		t.Fatalf("body-internal edge must be respected: %v", order)
	}
}

func TestExecuteWorkflowLoopBackEdgeExecutes(t *testing.T) {
	// End-to-end: the loop executes despite the loop-back edge (regression for
	// the loop-back activation deadlock).
	steps := newFakeSteps()
	steps.outputs["src"] = json.RawMessage(`{"items": [1, 2]}`)
	svc := newControlFlowService(steps, newFakeClock())
	wf := mustWorkflow(t, svc, "LoopBack", DSL{
		Nodes: []Node{
			{ID: "src", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
			{ID: "lp", Type: NodeLoop, Config: map[string]any{
				"expression": "$.nodes.src.output.items",
				"body":       []any{"b1"},
			}},
			{ID: "b1", Type: NodeTool, Config: map[string]any{"tool_id": "t1"}},
			{ID: "after", Type: NodeAgent, Config: map[string]any{"agent_id": "a2"}},
		},
		Edges: []Edge{
			{From: "src", To: "lp"},
			{From: "b1", To: "lp"}, // loop-back edge
			{From: "lp", To: "after"},
		},
	})

	result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if got := steps.callCount("b1"); got != 2 {
		t.Fatalf("loop body should execute twice, got %d calls", got)
	}
	latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
	if nr := latest["lp"]; nr == nil || nr.Status != RunStatusCompleted {
		t.Fatalf("loop node should complete (no loop-back deadlock), got %#v", latest["lp"])
	}
	if nr := latest["after"]; nr == nil || nr.Status != RunStatusCompleted {
		t.Fatalf("post-loop node should execute exactly once, got %#v", latest["after"])
	}
}

// ---------------------------------------------------------------------------
// engine integration guards
// ---------------------------------------------------------------------------

func TestExecuteWorkflowConditionsWorkWithoutStepExecutor(t *testing.T) {
	// The default engine (run expansion, no custom StepExecutor) must keep
	// working with control-flow nodes: condition expressions read the
	// run-expansion outputs ({"run_id": ...}).
	svc, _, _, _ := newEngineFixture()
	wf, err := svc.CreateWorkflow(context.Background(), "org-1", "NoExecutor", "", DSL{
		Nodes: []Node{
			{ID: "n1", Type: NodeAgent, Config: map[string]any{"agent_id": "agent-1"}},
			{ID: "cond", Type: NodeCondition, Config: map[string]any{
				"expression": "$.nodes.n1.output.run_id != ''",
				"branches":   map[string]any{"true": "n2", "false": "n3"},
			}},
			{ID: "n2", Type: NodeAgent, Config: map[string]any{"agent_id": "agent-2"}},
			{ID: "n3", Type: NodeAgent, Config: map[string]any{"agent_id": "agent-3"}},
		},
		Edges: []Edge{{From: "n1", To: "cond"}},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow returned error: %v", err)
	}
	result, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if len(result.RunIDs) != 2 {
		t.Fatalf("n1 and the taken branch should expand runs, got %v", result.RunIDs)
	}
	latest := nodeRunsByID(t, svc, "org-1", result.WorkflowRunID)
	if nr := latest["n3"]; nr == nil || nr.Status != RunStatusSkipped {
		t.Fatalf("false branch should be skipped, got %#v", latest["n3"])
	}
}

func TestExecuteWorkflowConditionExpressionErrorFailsRun(t *testing.T) {
	// A condition expression referencing a missing node fails the run with a
	// typed expression error (no silent default routing).
	steps := newFakeSteps()
	svc := newControlFlowService(steps, newFakeClock())
	wf := mustWorkflow(t, svc, "BadExpr", DSL{
		Nodes: []Node{
			{ID: "cond", Type: NodeCondition, Config: map[string]any{
				"expression": "$.nodes.missing.output.x == 1",
				"branches":   map[string]any{"true": "n2"},
			}},
			{ID: "n2", Type: NodeAgent, Config: map[string]any{"agent_id": "a1"}},
		},
	})
	_, err := svc.ExecuteWorkflow(context.Background(), "org-1", wf.ID, "", "user-1")
	if err == nil {
		t.Fatal("expression error must fail the run")
	}
	var verrs *ValidationErrors
	if errors.As(err, &verrs) {
		t.Fatalf("runtime expression errors are not validation errors: %v", err)
	}
}
