package workflows

import (
	"context"
	"errors"
	"testing"

	"agentos/internal/approvals"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

func newEngineFixture() (*Service, *runs.Service, *queue.Queue, *approvals.Service) {
	runsSvc := runs.NewService()
	q := queue.NewQueue()
	apSvc := approvals.NewService()
	svc := NewService()
	svc.SetEngine(Engine{Runs: runsSvc, Queue: q, Approvals: apSvc})
	apSvc.SetRunController(runsSvc)
	return svc, runsSvc, q, apSvc
}

func TestExecuteWorkflowExpandsDAGInTopoOrder(t *testing.T) {
	svc, runsSvc, q, apSvc := newEngineFixture()
	ctx := context.Background()

	wf, err := svc.CreateWorkflow(ctx, "org-1", "Deploy Flow", "", DSL{
		Nodes: []Node{
			{ID: "n1", Type: NodeAgent, Name: "Planner", Config: map[string]any{"agent_id": "agent-1", "input": "{{input}}"}},
			{ID: "n2", Type: NodeApproval, Name: "Gate", Config: map[string]any{"action": "deploy", "risk": "high"}},
			{ID: "n3", Type: NodeAgent, Name: "Executor", Config: map[string]any{"agent_id": "agent-2"}},
		},
		Edges: []Edge{{From: "n1", To: "n2"}, {From: "n2", To: "n3"}},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow returned error: %v", err)
	}

	result, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hello world", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if result.WorkflowRunID == "" || result.Status != RunStatusPending {
		t.Fatalf("unexpected execution result: %#v", result)
	}
	if len(result.RunIDs) != 2 {
		t.Fatalf("expected 2 agent runs, got %v", result.RunIDs)
	}
	if q.Length() != 2 {
		t.Fatalf("expected 2 queued tasks, got %d", q.Length())
	}

	// Workflow run + node runs are recorded and queryable.
	wr, nodes, err := svc.GetWorkflowRun(ctx, "org-1", result.WorkflowRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", err)
	}
	if wr.Status != RunStatusWaitingApproval {
		t.Fatalf("workflow run with an approval gate should be waiting_approval, got %q", wr.Status)
	}
	if len(nodes) != 3 {
		t.Fatalf("expected 3 node runs, got %d", len(nodes))
	}
	byNode := make(map[string]*NodeRun, len(nodes))
	for _, nr := range nodes {
		byNode[nr.NodeID] = nr
	}
	if nr := byNode["n2"]; nr == nil || nr.Status != RunStatusWaitingApproval || nr.RunID != result.RunIDs[0] {
		t.Fatalf("approval node run should link the paused run: %#v", byNode["n2"])
	}
	if nr := byNode["n3"]; nr == nil || nr.RunID != result.RunIDs[1] {
		t.Fatalf("downstream agent node run mismatch: %#v", byNode["n3"])
	}

	// The run before the approval gate is paused until the approval is decided.
	before, err := runsSvc.GetRunCtx(ctx, "org-1", result.RunIDs[0])
	if err != nil {
		t.Fatalf("GetRunCtx returned error: %v", err)
	}
	if before.Status != runs.StatusPaused {
		t.Fatalf("expected paused run before the gate, got %q", before.Status)
	}
	after, err := runsSvc.GetRunCtx(ctx, "org-1", result.RunIDs[1])
	if err != nil {
		t.Fatalf("GetRunCtx returned error: %v", err)
	}
	if after.Status != runs.StatusQueued {
		t.Fatalf("expected queued run after the gate, got %q", after.Status)
	}

	// One pending approval links workflow run and paused run.
	pending, err := apSvc.List(ctx, "org-1", approvals.StatusPending)
	if err != nil {
		t.Fatalf("approvals List returned error: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending approval, got %d", len(pending))
	}
	if pending[0].RunID != result.RunIDs[0] || pending[0].WorkflowRunID != result.WorkflowRunID {
		t.Fatalf("approval linkage mismatch: %#v", pending[0])
	}

	// Approving resumes the paused run (contract: decide -> resume).
	if _, err := apSvc.Decide(ctx, "org-1", pending[0].ID, approvals.StatusApproved, "ok", "user-2"); err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	resumed, err := runsSvc.GetRunCtx(ctx, "org-1", result.RunIDs[0])
	if err != nil {
		t.Fatalf("GetRunCtx returned error: %v", err)
	}
	if resumed.Status != runs.StatusPending {
		t.Fatalf("approved approval should resume the run to pending, got %q", resumed.Status)
	}
}

func TestExecuteWorkflowTaskPayloads(t *testing.T) {
	svc, _, q, _ := newEngineFixture()
	ctx := context.Background()

	wf, err := svc.CreateWorkflow(ctx, "org-1", "Tool Flow", "", DSL{
		Nodes: []Node{
			{ID: "n1", Type: NodeAgent, Config: map[string]any{"agent_id": "agent-1"}},
			{ID: "n2", Type: NodeTool, Config: map[string]any{"tool_id": "tool-1", "input": "fixed"}},
		},
		Edges: []Edge{{From: "n1", To: "n2"}},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow returned error: %v", err)
	}
	result, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hello", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if len(result.RunIDs) != 2 {
		t.Fatalf("tool node without agent_id should ride the first agent node's run, got %v", result.RunIDs)
	}

	seen := make(map[string]map[string]any)
	for i := 0; i < 2; i++ {
		task := q.Dequeue()
		if task == nil {
			t.Fatalf("expected task #%d on the queue", i+1)
		}
		seen[task.Type] = task.Payload
	}
	if seen["agent.run"] == nil || seen["tool.run"] == nil {
		t.Fatalf("expected one agent.run and one tool.run task, got %v", seen)
	}
	if seen["agent.run"]["workflow_run_id"] != result.WorkflowRunID || seen["tool.run"]["workflow_run_id"] != result.WorkflowRunID {
		t.Fatalf("task payloads should carry the workflow run linkage: %#v", seen)
	}
	if seen["agent.run"]["run_id"] != result.RunIDs[0] || seen["tool.run"]["run_id"] != result.RunIDs[1] {
		t.Fatalf("task payloads should carry their own run ids: %#v vs %v", seen, result.RunIDs)
	}
}

func TestExecuteWorkflowToolOnlyWorkflow(t *testing.T) {
	svc, _, q, _ := newEngineFixture()
	ctx := context.Background()

	wf, err := svc.CreateWorkflow(ctx, "org-1", "Bare Tool", "", DSL{
		Nodes: []Node{{ID: "t1", Type: NodeTool, Config: map[string]any{"tool_id": "tool-1"}}},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow returned error: %v", err)
	}
	result, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hello", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if len(result.RunIDs) != 0 {
		t.Fatalf("tool-only workflow has no agent to carry runs, got %v", result.RunIDs)
	}
	if q.Length() != 1 {
		t.Fatalf("expected the bare tool task to be enqueued, got %d", q.Length())
	}
	_, nodes, err := svc.GetWorkflowRun(ctx, "org-1", result.WorkflowRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", err)
	}
	if len(nodes) != 1 || nodes[0].RunID != "" || nodes[0].Status != RunStatusPending {
		t.Fatalf("unexpected node run: %#v", nodes)
	}
}

func TestExecuteWorkflowStructuralNodesRecordCompletedNodeRuns(t *testing.T) {
	svc, _, _, _ := newEngineFixture()
	ctx := context.Background()

	wf, err := svc.CreateWorkflow(ctx, "org-1", "Structural", "", DSL{
		Nodes: []Node{
			{ID: "n1", Type: NodeParallel},
			{ID: "n2", Type: NodeDelay, Config: map[string]any{"seconds": 1}},
		},
		Edges: []Edge{{From: "n1", To: "n2"}},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow returned error: %v", err)
	}
	result, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if len(result.RunIDs) != 0 {
		t.Fatalf("structural nodes create no agent runs, got %v", result.RunIDs)
	}
	_, nodes, err := svc.GetWorkflowRun(ctx, "org-1", result.WorkflowRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun returned error: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 completed node runs, got %d", len(nodes))
	}
	for _, nr := range nodes {
		if nr.Status != RunStatusCompleted || nr.FinishedAt == nil {
			t.Fatalf("structural node run should be completed with finished_at: %#v", nr)
		}
	}
}

func TestExecuteWorkflowErrors(t *testing.T) {
	ctx := context.Background()

	// Engine not wired.
	unwired := NewService()
	if _, err := unwired.ExecuteWorkflow(ctx, "org-1", "any", "", "user-1"); !errors.Is(err, ErrEngineNotWired) {
		t.Fatalf("expected ErrEngineNotWired, got %v", err)
	}

	// Foreign workflow -> not found.
	svc, _, _, _ := newEngineFixture()
	if _, err := svc.ExecuteWorkflow(ctx, "org-1", "missing", "", "user-1"); !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("expected ErrWorkflowNotFound, got %v", err)
	}

	// Invalid DSL at execute time -> ValidationErrors. Simulates DSL drift
	// after creation (e.g. a legacy import): the cached *Workflow is mutated
	// so GetWorkflow serves the now-invalid definition.
	wf, err := svc.CreateWorkflow(ctx, "org-1", "Becomes Invalid", "", DSL{
		Nodes: []Node{validAgentNode("n1", "agent-1")},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow returned error: %v", err)
	}
	wf.DSL = DSL{Nodes: []Node{{ID: "n1", Type: NodeAgent}}}
	if _, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "", "user-1"); err == nil {
		t.Fatal("expected validation errors for the invalid DSL")
	} else {
		var verrs *ValidationErrors
		if !errors.As(err, &verrs) {
			t.Fatalf("expected *ValidationErrors, got %T", err)
		}
	}

	// Cross-tenant execute -> not found.
	tenantWf, err := svc.CreateWorkflow(ctx, "org-1", "Private", "", DSL{
		Nodes: []Node{validAgentNode("n1", "agent-1")},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow returned error: %v", err)
	}
	if _, err := svc.ExecuteWorkflow(ctx, "org-2", tenantWf.ID, "", "user-1"); !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("cross-tenant execute should be ErrWorkflowNotFound, got %v", err)
	}
}

func TestGetWorkflowRunTenantGuard(t *testing.T) {
	svc, _, _, _ := newEngineFixture()
	ctx := context.Background()

	wf, err := svc.CreateWorkflow(ctx, "org-1", "Guarded", "", DSL{
		Nodes: []Node{validAgentNode("n1", "agent-1")},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow returned error: %v", err)
	}
	result, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "x", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow returned error: %v", err)
	}
	if _, _, err := svc.GetWorkflowRun(ctx, "org-2", result.WorkflowRunID); !errors.Is(err, ErrWorkflowRunNotFound) {
		t.Fatalf("cross-tenant workflow run read should be ErrWorkflowRunNotFound, got %v", err)
	}
}
