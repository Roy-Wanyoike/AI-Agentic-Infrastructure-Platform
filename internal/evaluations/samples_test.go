package evaluations

// Issue #51 tests: the eval-run sample feed (memory + store modes) and the
// completion-observer seam that drives the canary promotion engine. The
// pgStore SQL of ListCompletedRuns is pinned separately in store_test.go
// (TestPGListCompletedRuns), mirroring the package's store/service split.

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentos/internal/agents"
	"agentos/internal/runtime"
)

// constantRunner returns a fake runner whose outputs always match the
// expected string of the exact scorer.
func constantRunner(out string) func(ctx context.Context, agentID, input string) (*runtime.Run, error) {
	return func(_ context.Context, _ string, _ string) (*runtime.Run, error) {
		return &runtime.Run{Output: out}, nil
	}
}

// mustAgentID resolves the id of the agent seeded by newTestService (agent
// ids are UUIDs, never the literal "agent-1").
func mustAgentID(t *testing.T, s *Service) string {
	t.Helper()
	list := s.deps.Agents.List("org-1")
	if len(list) == 0 {
		t.Fatal("newTestService should seed one agent for org-1")
	}
	return list[0].ID
}

func TestListRunSamplesMemoryMode(t *testing.T) {
	s := newTestService(constantRunner("answer"))
	ds := mustCreateDataset(t, s, []Case{
		{ID: "c1", Input: "q1", Expected: "answer", Scorer: ScorerExact},
		{ID: "c2", Input: "q2", Expected: "answer", Scorer: ScorerExact},
	})
	ctx := context.Background()
	agentID := mustAgentID(t, s)
	// Two completed runs against the seeded agent.
	for range 2 {
		if _, err := s.RunDataset(ctx, "org-1", ds.ID, agentID); err != nil {
			t.Fatalf("RunDataset returned error: %v", err)
		}
	}

	samples, err := s.ListRunSamples(ctx, "org-1", agentID, 10)
	if err != nil {
		t.Fatalf("ListRunSamples returned error: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	for _, sample := range samples {
		if sample.Cases != 2 || sample.Passed != 2 {
			t.Fatalf("sample should aggregate 2/2 passed cases, got %+v", sample)
		}
		if len(sample.LatenciesMS) != 2 {
			t.Fatalf("sample should carry one latency per case, got %d", len(sample.LatenciesMS))
		}
		if sample.CostCents != 0 {
			t.Fatalf("offline runner reports no usage -> zero cost, got %v", sample.CostCents)
		}
	}

	// Tenant guard: another org / agent sees nothing.
	if got, _ := s.ListRunSamples(ctx, "org-2", agentID, 10); len(got) != 0 {
		t.Fatalf("foreign org must see no samples, got %d", len(got))
	}
	if got, _ := s.ListRunSamples(ctx, "org-1", "agent-unknown", 10); len(got) != 0 {
		t.Fatalf("unknown agent must see no samples, got %d", len(got))
	}
	if got, _ := s.ListRunSamples(ctx, "org-1", agentID, 0); len(got) != 0 {
		t.Fatalf("limit 0 must return nothing, got %d", len(got))
	}

	// Limit caps the newest-first listing to the most recent runs.
	if got, _ := s.ListRunSamples(ctx, "org-1", agentID, 1); len(got) != 1 {
		t.Fatalf("limit 1 must return the single newest sample, got %d", len(got))
	}
}

func TestListRunSamplesOnlyCompletedRunsCount(t *testing.T) {
	// A failing runner still completes the RUN (per-case failures are
	// results); a run that never starts (dataset missing) never appears.
	s := newTestService(func(context.Context, string, string) (*runtime.Run, error) {
		return nil, errors.New("boom")
	})
	ds := mustCreateDataset(t, s, []Case{{ID: "c1", Input: "q1", Expected: "a", Scorer: ScorerExact}})
	if _, err := s.RunDataset(context.Background(), "org-1", ds.ID, mustAgentID(t, s)); err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	samples, err := s.ListRunSamples(context.Background(), "org-1", mustAgentID(t, s), 10)
	if err != nil {
		t.Fatalf("ListRunSamples returned error: %v", err)
	}
	if len(samples) != 1 || samples[0].Passed != 0 {
		t.Fatalf("failed cases still form a completed-run sample with 0 passed, got %+v", samples)
	}
}

// TestListRunSamplesStoreMode exercises the store-backed service path: reads
// go through the Store (ListCompletedRuns + ListResults), like every other
// dual-mode service read.
func TestListRunSamplesStoreMode(t *testing.T) {
	store := newRecordingStore()
	agentSvc := agents.NewService()
	agent, err := agentSvc.Create("org-1", "Eval Agent", "d", "be deterministic", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Create agent returned error: %v", err)
	}
	s := NewServiceWithStore(store, Deps{
		Agents:      agentSvc,
		Runner:      &fakeRunner{fn: constantRunner("answer")},
		CaseTimeout: 2 * time.Second,
	})
	ds := mustCreateDataset(t, s, []Case{
		{ID: "c1", Input: "q1", Expected: "answer", Scorer: ScorerExact},
		{ID: "c2", Input: "q2", Expected: "answer", Scorer: ScorerExact},
	})
	ctx := context.Background()
	if _, err := s.RunDataset(ctx, "org-1", ds.ID, agent.ID); err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}

	samples, err := s.ListRunSamples(ctx, "org-1", agent.ID, 10)
	if err != nil {
		t.Fatalf("ListRunSamples (store mode) returned error: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	sample := samples[0]
	if sample.RunID == "" || sample.Cases != 2 || sample.Passed != 2 {
		t.Fatalf("unexpected sample aggregates: %+v", sample)
	}
	if len(sample.LatenciesMS) != 2 {
		t.Fatalf("sample should carry per-case latencies, got %d", len(sample.LatenciesMS))
	}

	// Tenant guard on the store path.
	if got, _ := s.ListRunSamples(ctx, "org-2", agent.ID, 10); len(got) != 0 {
		t.Fatalf("foreign org must see no samples (store mode), got %d", len(got))
	}
}

// TestCompletionObserverFiredPerCompletedRun pins the seam contract: fired
// exactly once per COMPLETED run, asynchronously, with the completed run; a
// nil observer is inert (legacy behavior).
func TestCompletionObserverFiredPerCompletedRun(t *testing.T) {
	s := newTestService(constantRunner("answer"))
	ds := mustCreateDataset(t, s, []Case{{ID: "c1", Input: "q1", Expected: "answer", Scorer: ScorerExact}})
	agentID := mustAgentID(t, s)

	observed := make(chan *EvalRun, 4)
	s.SetCompletionObserver(func(_ context.Context, run *EvalRun) {
		observed <- run
	})

	ctx := context.Background()
	run, err := s.RunDataset(ctx, "org-1", ds.ID, agentID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	select {
	case got := <-observed:
		if got.ID != run.ID || got.Status != StatusCompleted {
			t.Fatalf("observer should receive the completed run, got %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("completion observer was not fired for a completed run")
	}

	// No observer wired -> the seam is inert (default legacy behavior).
	s.SetCompletionObserver(nil)
	if _, err := s.RunDataset(ctx, "org-1", ds.ID, agentID); err != nil {
		t.Fatalf("RunDataset with nil observer returned error: %v", err)
	}
	select {
	case run := <-observed:
		t.Fatalf("nil observer must not fire, got %+v", run)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestListRunSamplesNewestFirst pins the ordering contract the canary window
// filter relies on (newest run first).
func TestListRunSamplesNewestFirst(t *testing.T) {
	s := newTestService(constantRunner("answer"))
	ds := mustCreateDataset(t, s, []Case{{ID: "c1", Input: "q1", Expected: "answer", Scorer: ScorerExact}})
	ctx := context.Background()
	agentID := mustAgentID(t, s)
	first, err := s.RunDataset(ctx, "org-1", ds.ID, agentID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := s.RunDataset(ctx, "org-1", ds.ID, agentID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	samples, err := s.ListRunSamples(ctx, "org-1", agentID, 10)
	if err != nil {
		t.Fatalf("ListRunSamples returned error: %v", err)
	}
	if len(samples) != 2 || samples[0].RunID != second.ID || samples[1].RunID != first.ID {
		t.Fatalf("samples must be newest-first, got %+v", samples)
	}
}
