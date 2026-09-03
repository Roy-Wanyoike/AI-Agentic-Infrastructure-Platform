package workflows

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/approvals"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// ---------------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------------

func durableFixture(opts ...Option) (*Service, *runs.Service, *queue.Queue, *approvals.Service) {
	runsSvc := runs.NewService()
	q := queue.NewQueue()
	apSvc := approvals.NewService()
	svc := NewServiceWithOptions(nil, opts...)
	svc.SetEngine(Engine{Runs: runsSvc, Queue: q, Approvals: apSvc})
	apSvc.SetRunController(runsSvc)
	return svc, runsSvc, q, apSvc
}

func durableAgentDSL(ids ...string) DSL {
	nodes := make([]Node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, Node{ID: id, Type: NodeAgent, Name: "Agent " + id, Config: map[string]any{"agent_id": "agent-" + id}})
	}
	return DSL{Nodes: nodes}
}

// cacheWorkflowRun returns the mutable cached run (same-package test access).
func cacheWorkflowRun(svc *Service, id string) *WorkflowRun {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.workflowRuns[id]
}

// cacheNodeRun returns the mutable cached node run by checkpoint id.
func cacheNodeRun(svc *Service, id string) *NodeRun {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.nodeRunIndex[id]
}

func ageWorkflowRun(svc *Service, id string, age time.Duration) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	past := time.Now().UTC().Add(-age)
	if wr := svc.workflowRuns[id]; wr != nil {
		wr.HeartbeatAt = &past
		wr.UpdatedAt = past
	}
}

// ---------------------------------------------------------------------------
// Idempotent checkpoint state machine (in-memory mode).
// ---------------------------------------------------------------------------

func TestBeginNodeRunCreatesFirstAttempt(t *testing.T) {
	svc, _, q, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, err := svc.CreateWorkflow(ctx, "org-1", "Flow", "", durableAgentDSL("n1"))
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	result, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	before := q.Length()

	nr, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", result.RunIDs[0])
	if err != nil {
		t.Fatalf("BeginNodeRun: %v", err)
	}
	if nr.Status != RunStatusRunning || nr.Attempt != 1 {
		t.Fatalf("first claim should be attempt 1 running, got %#v", nr)
	}
	if nr.StartedAt == nil || nr.LockedAt == nil || nr.HeartbeatAt == nil {
		t.Fatalf("claim should stamp started/locked/heartbeat: %#v", nr)
	}

	// The claim heartbeats the parent workflow run too.
	if wr := cacheWorkflowRun(svc, result.WorkflowRunID); wr.HeartbeatAt == nil {
		t.Fatal("workflow run heartbeat should be stamped by the claim")
	}
	if q.Length() != before {
		t.Fatalf("claim must not enqueue anything (queue %d -> %d)", before, q.Length())
	}
}

func TestBeginNodeRunRefusesTerminalAttempt(t *testing.T) {
	svc, _, _, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Flow", "", durableAgentDSL("n1"))
	result, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	nr, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", result.RunIDs[0])
	if err != nil {
		t.Fatalf("BeginNodeRun: %v", err)
	}
	if err := svc.FinishNodeRun(ctx, "org-1", nr.ID, RunStatusCompleted, ""); err != nil {
		t.Fatalf("FinishNodeRun: %v", err)
	}

	// Replay after a terminal attempt: never re-executed.
	if _, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", result.RunIDs[0]); !errors.Is(err, ErrNodeRunTerminal) {
		t.Fatalf("replayed begin should be ErrNodeRunTerminal, got %v", err)
	}

	// Finishing an already-terminal checkpoint is an idempotent no-op.
	if err := svc.FinishNodeRun(ctx, "org-1", nr.ID, RunStatusFailed, "SECOND_ATTEMPT"); err != nil {
		t.Fatalf("idempotent finish should not error: %v", err)
	}
	if got := cacheNodeRun(svc, nr.ID); got.Status != RunStatusCompleted || got.ErrorCode != "" {
		t.Fatalf("terminal checkpoint must be immutable, got %#v", got)
	}

	// Non-terminal finish targets are rejected.
	if err := svc.FinishNodeRun(ctx, "org-1", nr.ID, RunStatusRunning, ""); !errors.Is(err, ErrInvalidNodeStatus) {
		t.Fatalf("non-terminal finish should be ErrInvalidNodeStatus, got %v", err)
	}
}

func TestBeginNodeRunRefusesFreshInFlightAndReclaimsStale(t *testing.T) {
	svc, _, _, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Flow", "", durableAgentDSL("n1"))
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")

	first, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", result.RunIDs[0])
	if err != nil {
		t.Fatalf("first BeginNodeRun: %v", err)
	}

	// Another worker replays the task while the lease is fresh: refused.
	if _, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", result.RunIDs[0]); !errors.Is(err, ErrNodeRunInFlight) {
		t.Fatalf("fresh in-flight begin should be ErrNodeRunInFlight, got %v", err)
	}

	// The worker dies; the lease goes stale: a replay claims in place with a
	// bumped attempt and keeps the original started_at.
	past := time.Now().UTC().Add(-2 * time.Hour)
	if nr := cacheNodeRun(svc, first.ID); nr != nil {
		nr.HeartbeatAt = &past
		nr.LockedAt = &past
	}
	second, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", result.RunIDs[0])
	if err != nil {
		t.Fatalf("stale BeginNodeRun: %v", err)
	}
	if second.ID != first.ID || second.Attempt != 2 {
		t.Fatalf("stale re-claim should reuse the row with attempt 2, got %#v", second)
	}
	if !second.StartedAt.Equal(*first.StartedAt) {
		t.Fatalf("started_at should survive a re-claim: %v vs %v", second.StartedAt, first.StartedAt)
	}
}

func TestBeginNodeRunRestartsOrphanedAttempt(t *testing.T) {
	svc, _, q, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Flow", "", durableAgentDSL("n1"))
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")
	ageWorkflowRun(svc, result.WorkflowRunID, 2*time.Hour)

	recovered, err := svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil {
		t.Fatalf("RecoverIncompleteWorkflowRuns: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered run, got %d", recovered)
	}

	// The orphaned placeholder restarts as a NEW attempt row (the executor's
	// never-claimed placeholder sits at attempt 0, so the restart is the first
	// real execution attempt) instead of being re-executed in place.
	nr, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", "")
	if err != nil {
		t.Fatalf("BeginNodeRun after orphaning: %v", err)
	}
	if nr.Attempt != 1 || nr.Status != RunStatusRunning {
		t.Fatalf("orphaned attempt should restart as attempt 1 running, got %#v", nr)
	}
	if nr.RunID != result.RunIDs[0] {
		t.Fatalf("restart should inherit the node's run linkage, got %q", nr.RunID)
	}

	// The recovery pass re-enqueued the node task through the queue.
	if q.Length() != 2 {
		t.Fatalf("expected 1 re-enqueued task on top of the original, got %d", q.Length())
	}

	// Convergence: the fresh attempt finishes -> run completes.
	if err := svc.FinishNodeRun(ctx, "org-1", nr.ID, RunStatusCompleted, ""); err != nil {
		t.Fatalf("FinishNodeRun: %v", err)
	}
	if wr := cacheWorkflowRun(svc, result.WorkflowRunID); wr.Status != RunStatusCompleted {
		t.Fatalf("run should converge to completed, got %q", wr.Status)
	}
}

func TestFinishNodeRunConvergesAndFailsRuns(t *testing.T) {
	svc, _, _, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Two Nodes", "", durableAgentDSL("n1", "n2"))
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")

	n1, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", result.RunIDs[0])
	if err != nil {
		t.Fatalf("BeginNodeRun n1: %v", err)
	}
	if err := svc.FinishNodeRun(ctx, "org-1", n1.ID, RunStatusFailed, "AGENT_TIMEOUT"); err != nil {
		t.Fatalf("FinishNodeRun n1: %v", err)
	}
	// Fail-fast: a genuine node failure fails the whole run immediately.
	if wr := cacheWorkflowRun(svc, result.WorkflowRunID); wr.Status != RunStatusFailed || wr.ErrorCode != "AGENT_TIMEOUT" {
		t.Fatalf("genuine failure should fail the run, got %#v", wr)
	}
	if wr := cacheWorkflowRun(svc, result.WorkflowRunID); wr.FinishedAt == nil {
		t.Fatal("terminal run should carry finished_at")
	}

	// A second healthy workflow completes only when every node converged.
	wf2, _ := svc.CreateWorkflow(ctx, "org-1", "Happy", "", durableAgentDSL("m1", "m2"))
	result2, _ := svc.ExecuteWorkflow(ctx, "org-1", wf2.ID, "hi", "user-1")
	m1, _ := svc.BeginNodeRun(ctx, "org-1", result2.WorkflowRunID, "m1", result2.RunIDs[0])
	if err := svc.FinishNodeRun(ctx, "org-1", m1.ID, RunStatusCompleted, ""); err != nil {
		t.Fatalf("FinishNodeRun m1: %v", err)
	}
	if wr := cacheWorkflowRun(svc, result2.WorkflowRunID); wr.Status != RunStatusPending {
		t.Fatalf("run must stay pending until every node converged, got %q", wr.Status)
	}
	m2, _ := svc.BeginNodeRun(ctx, "org-1", result2.WorkflowRunID, "m2", result2.RunIDs[1])
	if err := svc.FinishNodeRun(ctx, "org-1", m2.ID, RunStatusCompleted, ""); err != nil {
		t.Fatalf("FinishNodeRun m2: %v", err)
	}
	if wr := cacheWorkflowRun(svc, result2.WorkflowRunID); wr.Status != RunStatusCompleted {
		t.Fatalf("converged run should complete, got %q", wr.Status)
	}
}

func TestHeartbeatNodeRunExtendsLease(t *testing.T) {
	svc, _, _, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Flow", "", durableAgentDSL("n1"))
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")
	nr, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", result.RunIDs[0])
	if err != nil {
		t.Fatalf("BeginNodeRun: %v", err)
	}
	old := time.Now().UTC().Add(-30 * time.Minute)
	if cached := cacheNodeRun(svc, nr.ID); cached != nil {
		cached.HeartbeatAt = &old
	}

	if err := svc.HeartbeatNodeRun(ctx, "org-1", nr.ID); err != nil {
		t.Fatalf("HeartbeatNodeRun: %v", err)
	}
	if cached := cacheNodeRun(svc, nr.ID); cached.HeartbeatAt == nil || cached.HeartbeatAt.Before(old.Add(29*time.Minute)) {
		t.Fatalf("heartbeat should refresh the node lease, got %#v", cached.HeartbeatAt)
	}
	if wr := cacheWorkflowRun(svc, result.WorkflowRunID); wr.HeartbeatAt == nil || wr.HeartbeatAt.Before(old.Add(29*time.Minute)) {
		t.Fatalf("heartbeat should refresh the parent run, got %#v", wr.HeartbeatAt)
	}
}

func TestWorkflowRunDeadlineAndHeartbeatHelpers(t *testing.T) {
	svc, _, _, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Flow", "", durableAgentDSL("n1"))
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")

	deadline := time.Now().UTC().Add(15 * time.Minute)
	if err := svc.SetWorkflowRunDeadline(ctx, "org-1", result.WorkflowRunID, deadline); err != nil {
		t.Fatalf("SetWorkflowRunDeadline: %v", err)
	}
	if wr := cacheWorkflowRun(svc, result.WorkflowRunID); wr.DeadlineAt == nil || !wr.DeadlineAt.Equal(deadline) {
		t.Fatalf("deadline should be pinned, got %#v", wr.DeadlineAt)
	}

	if err := svc.TouchWorkflowRunHeartbeat(ctx, "org-1", result.WorkflowRunID); err != nil {
		t.Fatalf("TouchWorkflowRunHeartbeat: %v", err)
	}

	// Foreign tenant: not found.
	if err := svc.SetWorkflowRunDeadline(ctx, "org-2", result.WorkflowRunID, deadline); !errors.Is(err, ErrWorkflowRunNotFound) && svc.store == nil {
		if err == nil {
			t.Fatal("foreign tenant deadline should fail")
		}
	}
}

func TestExecuteWorkflowStampsHeartbeatAndDeadline(t *testing.T) {
	svc, _, _, _ := durableFixture(WithStaleAfter(time.Hour), WithDefaultRunDeadline(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Budgeted", "", durableAgentDSL("n1"))
	result, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	wr := cacheWorkflowRun(svc, result.WorkflowRunID)
	if wr.HeartbeatAt == nil {
		t.Fatal("new runs should be born with a heartbeat")
	}
	if wr.DeadlineAt == nil || wr.DeadlineAt.Sub(wr.CreatedAt) < 55*time.Minute {
		t.Fatalf("new runs should carry the default deadline, got %#v", wr.DeadlineAt)
	}

	// The watchdog only fires when the budget is exhausted.
	recovered, err := svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil || recovered != 0 {
		t.Fatalf("fresh budgeted run should not be recovered, got (%d, %v)", recovered, err)
	}
}

func TestStaleAfterFromEnv(t *testing.T) {
	t.Setenv(StaleAfterEnvVar, "90s")
	if got := StaleAfterFromEnv(); got != 90*time.Second {
		t.Fatalf("expected 90s, got %v", got)
	}
	t.Setenv(StaleAfterEnvVar, "not-a-duration")
	if got := StaleAfterFromEnv(); got != DefaultStaleAfter {
		t.Fatalf("invalid value should fall back to default, got %v", got)
	}
	t.Setenv(StaleAfterEnvVar, "0s")
	if got := StaleAfterFromEnv(); got != DefaultStaleAfter {
		t.Fatalf("non-positive value should fall back to default, got %v", got)
	}
	t.Setenv(StaleAfterEnvVar, "")
	if got := StaleAfterFromEnv(); got != DefaultStaleAfter {
		t.Fatalf("unset knob should fall back to default, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Recovery pass (in-memory mode).
// ---------------------------------------------------------------------------

func TestRecoverOrphansAndReenqueuesStaleRun(t *testing.T) {
	svc, _, q, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Stale Flow", "", durableAgentDSL("n1", "n2"))
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")
	ageWorkflowRun(svc, result.WorkflowRunID, 2*time.Hour)

	recovered, err := svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil {
		t.Fatalf("RecoverIncompleteWorkflowRuns: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered run, got %d", recovered)
	}

	wr := cacheWorkflowRun(svc, result.WorkflowRunID)
	if wr.Status != RunStatusRunning || wr.Attempt != 1 {
		t.Fatalf("stale run should be re-claimed (running, attempt 1), got %#v", wr)
	}
	if wr.LockedAt == nil || wr.HeartbeatAt == nil || wr.HeartbeatAt.Before(time.Now().UTC().Add(-time.Minute)) {
		t.Fatalf("claim should stamp a fresh lease, got %#v", wr)
	}

	// Every pending/running checkpoint is orphaned with NODE_ORPHANED...
	_, nodes, err := svc.GetWorkflowRun(ctx, "org-1", result.WorkflowRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	orphans := 0
	for _, nr := range nodes {
		if nr.Status == RunStatusFailed && nr.ErrorCode == ErrorCodeNodeOrphaned && nr.FinishedAt != nil {
			orphans++
		}
	}
	if orphans != 2 {
		t.Fatalf("expected 2 orphaned node runs, got %d (%#v)", orphans, nodes)
	}

	// ...and the pending work is re-enqueued through the queue interface.
	if q.Length() != 4 {
		t.Fatalf("expected 2 original + 2 re-enqueued tasks, got %d", q.Length())
	}
	for i := 0; i < 2; i++ {
		task := q.Dequeue()
		task = q.Dequeue() // drop the originals
		if task == nil || task.Payload["workflow_run_id"] != result.WorkflowRunID {
			t.Fatalf("re-enqueued task should carry the workflow run linkage: %#v", task)
		}
	}

	// A second pass finds nothing: the claim refreshed the heartbeat.
	recovered, err = svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil || recovered != 0 {
		t.Fatalf("second pass should be a no-op, got (%d, %v)", recovered, err)
	}
}

func TestRecoverTimesOutOverdueRun(t *testing.T) {
	svc, _, _, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Overdue", "", durableAgentDSL("n1"))
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")

	// The watchdog fires on deadline_at even with a fresh heartbeat.
	if err := svc.SetWorkflowRunDeadline(ctx, "org-1", result.WorkflowRunID, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("SetWorkflowRunDeadline: %v", err)
	}
	recovered, err := svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil {
		t.Fatalf("RecoverIncompleteWorkflowRuns: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 timed-out run, got %d", recovered)
	}

	wr := cacheWorkflowRun(svc, result.WorkflowRunID)
	if wr.Status != RunStatusTimeout || wr.ErrorCode != ErrorCodeWorkflowRunTimeout {
		t.Fatalf("overdue run should time out, got %#v", wr)
	}
	if wr.FinishedAt == nil {
		t.Fatal("timed-out run should carry finished_at")
	}
	_, nodes, _ := svc.GetWorkflowRun(ctx, "org-1", result.WorkflowRunID)
	for _, nr := range nodes {
		if nr.Status != RunStatusFailed || nr.ErrorCode != ErrorCodeWorkflowRunTimeout {
			t.Fatalf("in-flight node runs should be timed out too: %#v", nr)
		}
	}

	// Terminal runs are never touched again.
	recovered, err = svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil || recovered != 0 {
		t.Fatalf("terminal run must not be re-processed, got (%d, %v)", recovered, err)
	}
}

func TestRecoverLeavesRunsWithPendingApprovalAlone(t *testing.T) {
	svc, _, q, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Gated", "", DSL{
		Nodes: []Node{
			{ID: "n1", Type: NodeAgent, Config: map[string]any{"agent_id": "agent-1"}},
			{ID: "n2", Type: NodeApproval, Config: map[string]any{"action": "deploy", "risk": "high"}},
			{ID: "n3", Type: NodeAgent, Config: map[string]any{"agent_id": "agent-2"}},
		},
		Edges: []Edge{{From: "n1", To: "n2"}, {From: "n2", To: "n3"}},
	})
	result, err := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	if cacheWorkflowRun(svc, result.WorkflowRunID).Status != RunStatusWaitingApproval {
		t.Fatal("precondition: run should be waiting_approval")
	}
	ageWorkflowRun(svc, result.WorkflowRunID, 2*time.Hour)

	// A pending human gate keeps the run alive by design: no recovery.
	recovered, err := svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil {
		t.Fatalf("RecoverIncompleteWorkflowRuns: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("waiting_approval run with a pending gate must not be recovered, got %d", recovered)
	}
	if wr := cacheWorkflowRun(svc, result.WorkflowRunID); wr.Status != RunStatusWaitingApproval || wr.Attempt != 0 {
		t.Fatalf("gated run should be untouched, got %#v", wr)
	}
	if q.Length() != 2 {
		t.Fatalf("no re-enqueue expected, got %d tasks", q.Length())
	}
}

func TestRecoverResumesWaitingApprovalRunAfterDecision(t *testing.T) {
	svc, _, q, apSvc := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Gated", "", DSL{
		Nodes: []Node{
			{ID: "n1", Type: NodeAgent, Config: map[string]any{"agent_id": "agent-1"}},
			{ID: "n2", Type: NodeApproval, Config: map[string]any{"action": "deploy"}},
			{ID: "n3", Type: NodeAgent, Config: map[string]any{"agent_id": "agent-2"}},
		},
		Edges: []Edge{{From: "n1", To: "n2"}, {From: "n2", To: "n3"}},
	})
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")
	pending, _ := apSvc.List(ctx, "org-1", approvals.StatusPending)
	if _, err := apSvc.Decide(ctx, "org-1", pending[0].ID, approvals.StatusApproved, "ok", "user-2"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	// The decide flow resumes the linked run but leaves the workflow run in
	// waiting_approval (the gap recovery exists to close).
	if wr := cacheWorkflowRun(svc, result.WorkflowRunID); wr.Status != RunStatusWaitingApproval {
		t.Fatalf("precondition: workflow run should still be waiting_approval, got %q", wr.Status)
	}
	ageWorkflowRun(svc, result.WorkflowRunID, 2*time.Hour)

	recovered, err := svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil {
		t.Fatalf("RecoverIncompleteWorkflowRuns: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered run, got %d", recovered)
	}

	wr := cacheWorkflowRun(svc, result.WorkflowRunID)
	if wr.Status != RunStatusRunning || wr.Attempt != 1 {
		t.Fatalf("rescued gate run should be running attempt 1, got %#v", wr)
	}
	_, nodes, _ := svc.GetWorkflowRun(ctx, "org-1", result.WorkflowRunID)
	byNode := make(map[string]*NodeRun, len(nodes))
	for _, nr := range nodes {
		byNode[nr.NodeID] = nr
	}
	if byNode["n2"].Status != RunStatusCompleted {
		t.Fatalf("decided gate checkpoint should be completed by recovery, got %#v", byNode["n2"])
	}
	// The executable nodes re-enqueue for fresh attempts.
	if q.Length() != 4 {
		t.Fatalf("expected 2 original + 2 re-enqueued tasks, got %d", q.Length())
	}
}

func TestRecoverFinalizesRunWithGenuineNodeFailure(t *testing.T) {
	svc, _, q, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Failing", "", durableAgentDSL("n1", "n2"))
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")

	// Node n1 fails genuinely but the run-finalize write is lost (worker
	// crash): the recovery pass must fail the run without re-enqueueing.
	n1, err := svc.BeginNodeRun(ctx, "org-1", result.WorkflowRunID, "n1", result.RunIDs[0])
	if err != nil {
		t.Fatalf("BeginNodeRun: %v", err)
	}
	if err := svc.FinishNodeRun(ctx, "org-1", n1.ID, RunStatusFailed, "AGENT_TIMEOUT"); err != nil {
		t.Fatalf("FinishNodeRun: %v", err)
	}
	wr := cacheWorkflowRun(svc, result.WorkflowRunID)
	wr.Status = RunStatusRunning // simulate the lost finalize
	wr.ErrorCode = ""
	ageWorkflowRun(svc, result.WorkflowRunID, 2*time.Hour)

	recovered, err := svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil {
		t.Fatalf("RecoverIncompleteWorkflowRuns: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered run, got %d", recovered)
	}
	wr = cacheWorkflowRun(svc, result.WorkflowRunID)
	if wr.Status != RunStatusFailed || wr.ErrorCode != "AGENT_TIMEOUT" {
		t.Fatalf("recovery should fail the run with the node's error code, got %#v", wr)
	}
	if q.Length() != 2 {
		t.Fatalf("failed run must not re-enqueue work, got %d tasks", q.Length())
	}
}

func TestRecoverTenantScopeAndSweep(t *testing.T) {
	svc, _, _, _ := durableFixture(WithStaleAfter(time.Hour))
	ctx := context.Background()

	wf, _ := svc.CreateWorkflow(ctx, "org-1", "Mine", "", durableAgentDSL("n1"))
	result, _ := svc.ExecuteWorkflow(ctx, "org-1", wf.ID, "hi", "user-1")
	ageWorkflowRun(svc, result.WorkflowRunID, 2*time.Hour)

	// Another tenant's recovery pass must not touch org-1's run.
	recovered, err := svc.RecoverIncompleteWorkflowRuns(ctx, "org-2")
	if err != nil || recovered != 0 {
		t.Fatalf("cross-tenant recovery must be empty, got (%d, %v)", recovered, err)
	}
	if wr := cacheWorkflowRun(svc, result.WorkflowRunID); wr.Attempt != 0 {
		t.Fatalf("foreign recovery pass must not claim, got %#v", wr)
	}

	// The owner's pass recovers it...
	recovered, err = svc.RecoverIncompleteWorkflowRuns(ctx, "org-1")
	if err != nil || recovered != 1 {
		t.Fatalf("org pass should recover, got (%d, %v)", recovered, err)
	}

	// ...and the internal sweep ("" = every tenant, worker-only) works too.
	wf2, _ := svc.CreateWorkflow(ctx, "org-2", "Theirs", "", durableAgentDSL("m1"))
	result2, _ := svc.ExecuteWorkflow(ctx, "org-2", wf2.ID, "hi", "user-2")
	ageWorkflowRun(svc, result2.WorkflowRunID, 2*time.Hour)
	recovered, err = svc.RecoverIncompleteWorkflowRuns(ctx, "")
	if err != nil || recovered != 1 {
		t.Fatalf("sweep should recover org-2's run, got (%d, %v)", recovered, err)
	}
}

func TestRecoveryWorkerLoop(t *testing.T) {
	svc, _, _, _ := durableFixture(WithStaleAfter(time.Hour))
	worker := NewRecoveryWorker(svc, time.Hour)
	if worker.Interval() != time.Hour {
		t.Fatalf("interval should be kept, got %v", worker.Interval())
	}
	if got := NewRecoveryWorker(svc, 0).Interval(); got != DefaultRecoveryInterval {
		t.Fatalf("non-positive interval should fall back to default, got %v", got)
	}

	ctx := context.Background()
	n, err := worker.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("RunOnce on a healthy system should be (0, nil), got (%d, %v)", n, err)
	}

	// Run performs the startup pass and returns when the context is cancelled.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := worker.Run(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run should return the context error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Store-mode service behavior (sqlmock): the service drives the durable store
// for checkpointing and recovery; the in-memory maps stay a write-through
// cache.
// ---------------------------------------------------------------------------

func storeModeFixture(t *testing.T) (*Service, sqlmock.Sqlmock, *queue.Queue, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	svc := NewServiceWithStore(NewPostgresStore(db))
	q := queue.NewQueue()
	svc.SetEngine(Engine{Queue: q})
	return svc, mock, q, func() { _ = db.Close() }
}

func TestServiceStoreModeCheckpointRoundTrip(t *testing.T) {
	svc, mock, _, closeDB := storeModeFixture(t)
	defer closeDB()
	ctx := context.Background()

	// BeginNodeRun: no checkpoint yet -> insert attempt 1 + parent heartbeat.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectLatestNodeRun)).
		WithArgs("org-1", "wr-1", "n1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workflow_run_id", "node_id", "run_id", "status", "error", "attempt", "locked_at", "heartbeat_at", "error_code", "started_at", "finished_at", "created_at"}))
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertNodeRunCheckpoint)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(sqlTouchWorkflowRunHeartbeat)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	nr, err := svc.BeginNodeRun(ctx, "org-1", "wr-1", "n1", "run-1")
	if err != nil {
		t.Fatalf("BeginNodeRun: %v", err)
	}
	if nr.Attempt != 1 || nr.Status != RunStatusRunning {
		t.Fatalf("unexpected checkpoint: %#v", nr)
	}

	// Replay: the latest attempt is terminal -> refused without writes.
	terminal := durableNodeRunRow(&NodeRun{ID: nr.ID, WorkflowRunID: "wr-1", NodeID: "n1", RunID: "run-1", Status: RunStatusCompleted, Attempt: 1, CreatedAt: time.Now().UTC()})
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectLatestNodeRun)).
		WillReturnRows(terminal)
	if _, err := svc.BeginNodeRun(ctx, "org-1", "wr-1", "n1", "run-1"); !errors.Is(err, ErrNodeRunTerminal) {
		t.Fatalf("store-mode replay should be ErrNodeRunTerminal, got %v", err)
	}

	// FinishNodeRun: guarded terminal transition.
	mock.ExpectExec(regexp.QuoteMeta(sqlMarkNodeRunStatus)).
		WithArgs(nr.ID, "org-1", RunStatusCompleted, "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := svc.FinishNodeRun(ctx, "org-1", nr.ID, RunStatusCompleted, ""); err != nil {
		t.Fatalf("FinishNodeRun: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestServiceStoreModeRecoveryPass(t *testing.T) {
	svc, mock, q, closeDB := storeModeFixture(t)
	defer closeDB()
	ctx := context.Background()
	now := time.Now().UTC()

	// Cache seeds: the workflow definition lives in the write-through cache so
	// the pass only exercises the recovery SQL.
	svc.mu.Lock()
	svc.workflows["wf-1"] = &Workflow{ID: "wf-1", OrganizationID: "org-1", Name: "Flow", DSL: durableAgentDSL("n1")}
	svc.workflowRuns["wr-1"] = &WorkflowRun{ID: "wr-1", WorkflowID: "wf-1", OrganizationID: "org-1", Status: RunStatusRunning}
	svc.mu.Unlock()

	stale := &WorkflowRun{ID: "wr-1", WorkflowID: "wf-1", OrganizationID: "org-1", Status: RunStatusRunning, UpdatedAt: now.Add(-2 * time.Hour)}
	claimSQL, err := sqlClaimWorkflowRun([]string{RunStatusRunning})
	if err != nil {
		t.Fatalf("sqlClaimWorkflowRun: %v", err)
	}

	// 0. Watchdog candidates first (none: no deadlines pinned).
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectTimedOutWorkflowRuns)).
		WithArgs(sqlmock.AnyArg(), "", RecoveryBatchLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workflow_id", "organization_id", "input", "status", "created_by", "attempt", "locked_at", "heartbeat_at", "finished_at", "deadline_at", "error_code", "created_at", "updated_at"}))
	mock.ExpectCommit()
	// 1. Candidate listing (FOR UPDATE SKIP LOCKED inside a transaction).
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectStaleWorkflowRuns)).
		WithArgs(sqlmock.AnyArg(), "", RecoveryBatchLimit).
		WillReturnRows(durableWorkflowRunRow(stale))
	mock.ExpectCommit()
	// 2. Claim (attempt bump + fresh lease).
	mock.ExpectExec(regexp.QuoteMeta(claimSQL)).
		WithArgs("wr-1", "org-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 3. Orphan the pending/running checkpoints.
	mock.ExpectExec(regexp.QuoteMeta(sqlFailNonTerminalNodeRuns)).
		WithArgs("org-1", "wr-1", ErrorCodeNodeOrphaned, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 4. Timeline + definition reads (twice: recovery loop + convergence check).
	orpharedNode := &NodeRun{ID: "nr-1", WorkflowRunID: "wr-1", NodeID: "n1", RunID: "run-1", Status: RunStatusFailed, ErrorCode: ErrorCodeNodeOrphaned, Attempt: 1, CreatedAt: now}
	workflowRow := sqlmock.NewRows([]string{"id", "organization_id", "name", "description", "status", "current_version", "definition", "created_at", "updated_at"}).
		AddRow("wf-1", "org-1", "Flow", "", StatusDraft, 0, `{"nodes":[{"id":"n1","type":"agent","config":{"agent_id":"agent-1"}}],"edges":[]}`, now, now)
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(regexp.QuoteMeta(sqlSelectWorkflowRunScoped)).
			WithArgs("wr-1", "org-1").
			WillReturnRows(durableWorkflowRunRow(stale))
		mock.ExpectQuery(regexp.QuoteMeta(sqlSelectNodeRunsByWorkflowRun)).
			WillReturnRows(durableNodeRunRow(orpharedNode))
		mock.ExpectQuery(regexp.QuoteMeta(sqlSelectWorkflowScoped)).
			WillReturnRows(workflowRow)
	}

	recovered, err := svc.RecoverIncompleteWorkflowRuns(ctx, "")
	if err != nil {
		t.Fatalf("RecoverIncompleteWorkflowRuns: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered run, got %d", recovered)
	}
	if q.Length() != 1 {
		t.Fatalf("the orphaned node should be re-enqueued, got %d tasks", q.Length())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
