package runs

// recovery_test.go — issue #115: the stale-run recovery sweep, exercised with
// an injected clock (no flaky sleeps): the staleness cutoff is computed from
// the timeNowUTC seam, so tests freeze the clock, backdate the runs and
// advance it deterministically.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRecoveryTargetMapping(t *testing.T) {
	cases := []struct {
		rec           StaleRunRecord
		wantFrom      RunState
		wantTo        RunState
		wantReconcile bool
	}{
		{StaleRunRecord{Status: "RUNNING", HasCheckpoint: true}, StateRunning, StatePaused, true},
		{StaleRunRecord{Status: "running", HasCheckpoint: false}, StateRunning, StateFailed, true},
		{StaleRunRecord{Status: "cancelling", HasCheckpoint: false}, StateCancelling, StateCancelled, true},
		{StaleRunRecord{Status: "CANCELLING", HasCheckpoint: true}, StateCancelling, StateCancelled, true},
		{StaleRunRecord{Status: "paused", HasCheckpoint: true}, "", "", false},
		{StaleRunRecord{Status: "queued", HasCheckpoint: false}, "", "", false},
		{StaleRunRecord{Status: "", HasCheckpoint: false}, "", "", false},
		{StaleRunRecord{Status: "mangled", HasCheckpoint: false}, "", "", false},
	}
	for i, tc := range cases {
		from, to, ok := recoveryTarget(tc.rec)
		if ok != tc.wantReconcile || from != tc.wantFrom || to != tc.wantTo {
			t.Errorf("case %d (%s): got %s→%s,%v want %s→%s,%v", i, tc.rec.Status, from, to, ok, tc.wantFrom, tc.wantTo, tc.wantReconcile)
		}
	}
}

func TestRecoverStaleRunsWithInjectableClock(t *testing.T) {
	// Freeze the clock at T0: every create/update below stamps T0.
	T0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	timeNowUTC = func() time.Time { return T0 }
	t.Cleanup(func() { timeNowUTC = func() time.Time { return time.Now().UTC() } })

	svc := NewService()
	pub := &capturePublisher{}
	svc.SetEventPublisher(pub)
	svc.SetCheckpointStore(NewInMemoryCheckpointStore())
	ctx := context.Background()

	staleWithCP, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.TransitionRunStatus(ctx, "org-1", staleWithCP.ID, StateRunning, "")
	if err := svc.checkpoints.SaveCheckpoint(ctx, "org-1", staleWithCP.ID, 2, []byte(`{"step_index":2}`)); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	staleNoCP, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.TransitionRunStatus(ctx, "org-1", staleNoCP.ID, StateRunning, "")
	staleCancelling, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.TransitionRunStatus(ctx, "org-1", staleCancelling.ID, StateCancelling, "")
	// Fresh running run (updated AFTER the cutoff): never swept.
	freshRun, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	// Terminal runs are invisible to the sweep.
	doneRun, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.TransitionRunStatus(ctx, "org-1", doneRun.ID, StateCompleted, "")

	// Advance the clock past the staleness threshold and stamp the fresh run.
	T1 := T0.Add(DefaultRunStaleAfter + time.Minute)
	timeNowUTC = func() time.Time { return T1 }
	_ = svc.TransitionRunStatus(ctx, "org-1", freshRun.ID, StateRunning, "")

	got, err := svc.RecoverStaleRuns(ctx, DefaultRunStaleAfter, 50)
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("recovered %d runs (%+v), want 3", len(got), got)
	}
	byID := map[string]RunRecovery{}
	for _, rec := range got {
		byID[rec.RunID] = rec
	}
	if rec := byID[staleWithCP.ID]; rec.To != StatePaused || rec.From != StateRunning {
		t.Errorf("checkpointed run: %+v, want running→paused (resumable)", rec)
	}
	if rec := byID[staleNoCP.ID]; rec.To != StateFailed || rec.From != StateRunning {
		t.Errorf("checkpoint-less run: %+v, want running→failed", rec)
	}
	if rec := byID[staleCancelling.ID]; rec.To != StateCancelled || rec.From != StateCancelling {
		t.Errorf("cancelling run: %+v, want cancelling→cancelled", rec)
	}
	if _, ok := byID[freshRun.ID]; ok {
		t.Error("fresh run must never be swept")
	}
	if _, ok := byID[doneRun.ID]; ok {
		t.Error("terminal run must never be swept")
	}

	// Outcomes persisted: statuses + the recovery annotation on the output.
	for _, tc := range []struct {
		id     string
		status RunState
	}{{staleWithCP.ID, StatePaused}, {staleNoCP.ID, StateFailed}, {staleCancelling.ID, StateCancelled}} {
		run, err := svc.GetRunCtx(ctx, "org-1", tc.id)
		if err != nil || run.Status != RunStatus(tc.status) {
			t.Errorf("run %s: status %v err %v, want %s", tc.id, run, err, tc.status)
		}
		if run != nil && recoveryOutcomeReason(tc.status, true) != "" && run.Output == "" {
			t.Errorf("run %s: recovery annotation missing", tc.id)
		}
	}

	// One run.recovered event per reconciliation.
	types := pub.types()
	if len(types) != 3 {
		t.Fatalf("events = %v, want exactly 3 run.recovered", types)
	}
	for _, typ := range types {
		if typ != EventRunRecoveredType {
			t.Fatalf("event type = %s, want %s", typ, EventRunRecoveredType)
		}
	}

	// Idempotent: the second sweep finds nothing (statuses moved on).
	again, err := svc.RecoverStaleRuns(ctx, DefaultRunStaleAfter, 50)
	if err != nil || len(again) != 0 {
		t.Fatalf("second sweep = %+v err %v, want 0 recoveries", again, err)
	}
}

func TestRecoverStaleRunsDefaultsAndGuards(t *testing.T) {
	T0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	timeNowUTC = func() time.Time { return T0 }
	t.Cleanup(func() { timeNowUTC = func() time.Time { return time.Now().UTC() } })

	var nilSvc *Service
	if _, err := nilSvc.RecoverStaleRuns(context.Background(), time.Second, 10); err == nil {
		t.Fatal("nil service must error")
	}

	// staleAfter <= 0 falls back to the documented default (300s).
	svc := NewService()
	ctx := context.Background()
	run, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
	_ = svc.TransitionRunStatus(ctx, "org-1", run.ID, StateRunning, "")
	// 100s stale: under the default threshold → nothing swept.
	timeNowUTC = func() time.Time { return T0.Add(100 * time.Second) }
	got, err := svc.RecoverStaleRuns(ctx, 0, 50)
	if err != nil || len(got) != 0 {
		t.Fatalf("sub-threshold sweep = %+v err %v, want none (default 300s)", got, err)
	}
	// A store without StaleRunScanner reports ErrRecoveryUnsupported.
	backed := NewServiceWithStore(newFakeStore())
	if _, err := backed.RecoverStaleRuns(ctx, time.Second, 10); !errors.Is(err, ErrRecoveryUnsupported) {
		t.Fatalf("want ErrRecoveryUnsupported, got %v", err)
	}
}

func TestRecoverStaleRunsLimitBoundsInMemorySweep(t *testing.T) {
	T0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	timeNowUTC = func() time.Time { return T0 }
	t.Cleanup(func() { timeNowUTC = func() time.Time { return time.Now().UTC() } })

	svc := NewService()
	ctx := context.Background()
	ids := map[string]bool{}
	for i := 0; i < 5; i++ {
		run, _ := svc.CreateRunCtx(ctx, "org-1", "agent-1", "hi")
		_ = svc.TransitionRunStatus(ctx, "org-1", run.ID, StateRunning, "")
		ids[run.ID] = true
	}
	timeNowUTC = func() time.Time { return T0.Add(10 * time.Minute) }
	got, err := svc.RecoverStaleRuns(ctx, DefaultRunStaleAfter, 3)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit=3 recovered %d, want 3", len(got))
	}
	// Remaining two are reconciled by the next pass.
	rest, err := svc.RecoverStaleRuns(ctx, DefaultRunStaleAfter, 50)
	if err != nil || len(rest) != 2 {
		t.Fatalf("follow-up sweep = %+v err %v, want 2", rest, err)
	}
}
