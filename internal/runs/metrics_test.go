package runs

import (
	"context"
	"errors"
	"testing"

	"agentos/internal/observability"
)

// runsCount reads the agentos_runs_total counter from the registry snapshot
// (0 when the family has never been incremented).
func runsCount(t *testing.T, m *observability.Metrics) int64 {
	t.Helper()
	counts, _ := m.Snapshot()
	return counts[observability.MetricRunsTotal]
}

// TestIncRunsTerminalTransitions asserts agentos_runs_total is incremented at
// the point of truth (UpdateStatusCtx) exactly once per run reaching a
// terminal status, and never for non-terminal transitions (issue #12).
func TestIncRunsTerminalTransitions(t *testing.T) {
	m := observability.NewMetrics()
	svc := NewService()
	svc.SetMetrics(m)

	if got := runsCount(t, m); got != 0 {
		t.Fatalf("fresh registry should report 0 runs, got %d", got)
	}

	run, err := svc.Create("org-1", "agent-1", "hello")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Non-terminal transitions must not count.
	if err := svc.UpdateStatus(run.ID, StatusRunning, ""); err != nil {
		t.Fatalf("running transition failed: %v", err)
	}
	if err := svc.UpdateStatus(run.ID, StatusPaused, ""); err != nil {
		t.Fatalf("paused transition failed: %v", err)
	}
	if err := svc.UpdateStatus(run.ID, StatusRunning, ""); err != nil {
		t.Fatalf("resume transition failed: %v", err)
	}
	if got := runsCount(t, m); got != 0 {
		t.Fatalf("non-terminal transitions counted: got %d, want 0", got)
	}

	// First terminal transition counts exactly once.
	if err := svc.UpdateStatus(run.ID, StatusCompleted, "done"); err != nil {
		t.Fatalf("completed transition failed: %v", err)
	}
	if got := runsCount(t, m); got != 1 {
		t.Fatalf("after completion got %d runs, want 1", got)
	}

	// A retry/replay re-writing the same terminal status must not double count.
	if err := svc.UpdateStatus(run.ID, StatusCompleted, "done"); err != nil {
		t.Fatalf("replayed completed transition failed: %v", err)
	}
	if got := runsCount(t, m); got != 1 {
		t.Fatalf("replayed terminal transition double counted: got %d, want 1", got)
	}

	// A second run failing counts as its own terminal transition.
	run2, err := svc.Create("org-1", "agent-1", "again")
	if err != nil {
		t.Fatalf("second create failed: %v", err)
	}
	if err := svc.UpdateStatus(run2.ID, StatusFailed, ""); err != nil {
		t.Fatalf("failed transition failed: %v", err)
	}
	if got := runsCount(t, m); got != 2 {
		t.Fatalf("after failure got %d runs, want 2", got)
	}
}

// TestIncRunsCancelPath covers the API-side control-plane path: cancelling a
// run transitions it to a terminal status through the same UpdateStatusCtx
// choke point and must feed the counter.
func TestIncRunsCancelPath(t *testing.T) {
	m := observability.NewMetrics()
	svc := NewService()
	svc.SetMetrics(m)

	run, err := svc.Create("org-1", "agent-1", "cancel me")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if _, err := svc.CancelRun(context.Background(), "org-1", run.ID); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if got := runsCount(t, m); got != 1 {
		t.Fatalf("after cancel got %d runs, want 1", got)
	}

	// Cancelling again is an idempotent no-op and must not re-count.
	if _, err := svc.CancelRun(context.Background(), "org-1", run.ID); err != nil {
		t.Fatalf("idempotent cancel failed: %v", err)
	}
	if got := runsCount(t, m); got != 1 {
		t.Fatalf("idempotent cancel re-counted: got %d, want 1", got)
	}
}

// TestIncRunsWorkerFireAndForget covers the worker-executed path: the worker
// process does not hold API-created run rows, so its UpdateStatus calls take
// the fire-and-forget branch. The terminal transition must still be counted
// (and non-terminal ones not), while ErrRunNotFound is preserved for backward
// compatibility with every existing caller.
func TestIncRunsWorkerFireAndForget(t *testing.T) {
	m := observability.NewMetrics()
	svc := NewService() // no run rows: emulates the worker process
	svc.SetMetrics(m)

	// Non-terminal fire-and-forget transition: not counted.
	if err := svc.UpdateStatus("run-unknown", StatusRunning, ""); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expected ErrRunNotFound, got %v", err)
	}
	if got := runsCount(t, m); got != 0 {
		t.Fatalf("non-terminal fire-and-forget counted: got %d, want 0", got)
	}

	// Worker observes the terminal outcome: counted once.
	if err := svc.UpdateStatus("run-unknown", StatusCompleted, "out"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expected ErrRunNotFound, got %v", err)
	}
	if got := runsCount(t, m); got != 1 {
		t.Fatalf("worker-executed terminal not counted: got %d, want 1", got)
	}

	// A retry of the same run (queue redelivery) must not double count.
	if err := svc.UpdateStatus("run-unknown", StatusCompleted, "out"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expected ErrRunNotFound, got %v", err)
	}
	if got := runsCount(t, m); got != 1 {
		t.Fatalf("retry double counted: got %d, want 1", got)
	}
}

// TestIncRunsStoreMode asserts the counter also works for the durable
// (dual-mode) service, counting the terminal transition once whether or not
// the run is present in the in-memory write-through cache.
func TestIncRunsStoreMode(t *testing.T) {
	m := observability.NewMetrics()
	svc := NewServiceWithStore(newFakeStore())
	svc.SetMetrics(m)

	run, err := svc.CreateRunCtx(context.Background(), "org-1", "agent-1", "durable")
	if err != nil {
		t.Fatalf("store-mode create failed: %v", err)
	}
	if err := svc.UpdateStatusCtx(context.Background(), "org-1", run.ID, StatusCompleted, "out"); err != nil {
		t.Fatalf("store-mode transition failed: %v", err)
	}
	if got := runsCount(t, m); got != 1 {
		t.Fatalf("store-mode completion got %d runs, want 1", got)
	}

	// Unknown run with an empty org (trusted worker path): the store lookup
	// fails, so the update errors and nothing is counted.
	if err := svc.UpdateStatusCtx(context.Background(), "", "missing", StatusFailed, ""); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expected ErrRunNotFound, got %v", err)
	}
	if got := runsCount(t, m); got != 1 {
		t.Fatalf("failed store lookup counted: got %d, want 1", got)
	}
}

// TestMetricsWiringNilSafe asserts the nil-safe dependency-injection contract:
// without metrics (or without the service) every code path stays green.
func TestMetricsWiringNilSafe(t *testing.T) {
	// Setter on a nil service must not panic.
	var svc *Service
	svc.SetMetrics(observability.NewMetrics())

	// Service without metrics: terminal transitions must not panic and must
	// keep their normal behavior.
	plain := NewService()
	run, err := plain.Create("org-1", "agent-1", "no metrics")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if err := plain.UpdateStatus(run.ID, StatusFailed, ""); err != nil {
		t.Fatalf("terminal transition without metrics failed: %v", err)
	}
	if got, ok := plain.Get(run.ID); !ok || got.Status != StatusFailed {
		t.Fatalf("run status not applied without metrics: %+v ok=%v", got, ok)
	}

	// Explicit nil registry is equally tolerated.
	plain.SetMetrics(nil)
	if err := plain.UpdateStatus(run.ID, StatusCompleted, ""); err != nil {
		t.Fatalf("terminal transition with nil metrics failed: %v", err)
	}
}
