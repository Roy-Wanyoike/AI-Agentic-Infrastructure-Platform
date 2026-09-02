package scheduler

import (
	"context"
	"testing"
	"time"

	"agentos/internal/queue"
	"agentos/internal/runs"
)

// newTestWorker builds a worker + the in-memory runs/queue services it fires
// into, all driven by the fake clock.
func newTestWorker(t *testing.T, clock *fakeClock) (*Worker, *Service, *runs.Service, *queue.Queue) {
	t.Helper()
	svc := NewService().WithClock(clock)
	runsSvc := runs.NewService()
	q := queue.NewQueue()
	return NewWorker(svc, runsSvc, q, time.Second).WithClock(clock), svc, runsSvc, q
}

// TestWorkerFiresRecurringOncePerInterval drives the full loop with a fake
// clock: due -> claim -> create run -> enqueue -> advance next_run_at, and
// verifies repeated ticks at the same instant never double-fire.
func TestWorkerFiresRecurringOncePerInterval(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	worker, svc, runsSvc, q := newTestWorker(t, clock)

	sched, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "agent-1", Input: "tick", Kind: KindRecurring, IntervalSeconds: 300})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	// Nothing due yet.
	if n := worker.Tick(ctx, clock.Now()); n != 0 {
		t.Fatalf("expected 0 fires, got %d", n)
	}
	if q.Length() != 0 || len(runsSvc.List()) != 0 {
		t.Fatal("nothing should be enqueued before the first due instant")
	}

	// First due instant: exactly one run + one task.
	clock.Set(testBase.Add(5 * time.Minute))
	if n := worker.Tick(ctx, clock.Now()); n != 1 {
		t.Fatalf("expected 1 fire, got %d", n)
	}
	if q.Length() != 1 {
		t.Fatalf("expected 1 queued task, got %d", q.Length())
	}
	task := q.Peek()
	if task.Type != "agent.run" {
		t.Fatalf("expected agent.run task, got %q", task.Type)
	}
	if task.Payload["run_id"] == "" || task.Payload["organization_id"] != "org-1" ||
		task.Payload["agent_id"] != "agent-1" || task.Payload["input"] != "tick" ||
		task.Payload["schedule_id"] != sched.ID {
		t.Fatalf("unexpected task payload: %+v", task.Payload)
	}
	created := runsSvc.List()
	if len(created) != 1 {
		t.Fatalf("expected 1 run created, got %d", len(created))
	}
	if created[0].OrganizationID != "org-1" || created[0].AgentID != "agent-1" {
		t.Fatalf("run must carry the schedule's tenant scope, got %+v", created[0])
	}

	// last_run_id + last_fired_at recorded on the schedule.
	stored, _ := svc.GetByID(ctx, sched.ID)
	if stored.LastRunID != created[0].ID {
		t.Fatalf("last_run_id = %q, want %q", stored.LastRunID, created[0].ID)
	}
	if stored.LastFiredAt == nil || !stored.LastFiredAt.Equal(testBase.Add(5*time.Minute)) {
		t.Fatalf("last_fired_at = %v", stored.LastFiredAt)
	}
	if stored.NextRunAt == nil || !stored.NextRunAt.Equal(testBase.Add(10*time.Minute)) {
		t.Fatalf("next_run_at should advance to now+interval, got %v", stored.NextRunAt)
	}

	// Immediate re-poll (worker restart at the same instant): no double fire.
	if n := worker.Tick(ctx, clock.Now()); n != 0 {
		t.Fatalf("re-poll at the same instant must not fire, got %d", n)
	}
	if q.Length() != 1 || len(runsSvc.List()) != 1 {
		t.Fatal("queue/runs must be unchanged after a re-poll")
	}
}

// TestWorkerNeverDoubleFiresAcrossRestarts simulates a worker crash: a new
// Worker instance (fresh process) polls at the same due instant and must not
// fire the schedule a second time.
func TestWorkerNeverDoubleFiresAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	worker, svc, _, q := newTestWorker(t, clock)

	if _, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindRecurring, IntervalSeconds: 60}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	clock.Set(testBase.Add(60 * time.Second))
	if n := worker.Tick(ctx, clock.Now()); n != 1 {
		t.Fatalf("expected 1 fire, got %d", n)
	}

	// "Restart": brand-new worker sharing the same service + clock.
	restarted := NewWorker(svc, runs.NewService(), q, time.Second).WithClock(clock)
	if n := restarted.Tick(ctx, clock.Now()); n != 0 {
		t.Fatalf("restarted worker must not double-fire, got %d", n)
	}
	if q.Length() != 1 {
		t.Fatalf("queue must still hold exactly one task, got %d", q.Length())
	}

	// The next interval fires exactly once again.
	clock.Set(testBase.Add(120 * time.Second))
	if n := restarted.Tick(ctx, clock.Now()); n != 1 {
		t.Fatalf("expected the next interval to fire once, got %d", n)
	}
	if q.Length() != 2 {
		t.Fatalf("expected 2 total tasks, got %d", q.Length())
	}
}

// TestWorkerOnceCompletesAfterFiring: a once schedule creates its run and
// becomes completed; it never fires again afterwards.
func TestWorkerOnceCompletesAfterFiring(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	worker, svc, runsSvc, q := newTestWorker(t, clock)

	sched, err := svc.Create(ctx, "org-1", validOnceInput()) // run_at = base+1min
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	clock.Set(testBase.Add(2 * time.Minute))
	if n := worker.Tick(ctx, clock.Now()); n != 1 {
		t.Fatalf("expected 1 fire, got %d", n)
	}
	stored, _ := svc.GetByID(ctx, sched.ID)
	if stored.Status != StatusCompleted {
		t.Fatalf("once schedule should be completed, got %q", stored.Status)
	}
	if len(runsSvc.List()) != 1 || q.Length() != 1 {
		t.Fatalf("expected 1 run + 1 task, got %d runs / %d tasks", len(runsSvc.List()), q.Length())
	}

	// Far in the future: still nothing.
	clock.Set(testBase.Add(2 * time.Hour))
	if n := worker.Tick(ctx, clock.Now()); n != 0 {
		t.Fatalf("completed schedule must never fire again, got %d", n)
	}
}

// TestWorkerCronFiresInTimezone: a cron schedule in America/New_York fires at
// the right UTC instant and advances to the next matching minute.
func TestWorkerCronFiresInTimezone(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	worker, svc, runsSvc, _ := newTestWorker(t, clock)

	if _, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindCron, CronExpr: "0 9 * * *", Timezone: "America/New_York"}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	// 09:00 EDT = 13:00 UTC.
	clock.Set(time.Date(2025, time.June, 2, 13, 0, 0, 0, time.UTC))
	if n := worker.Tick(ctx, clock.Now()); n != 1 {
		t.Fatalf("expected 1 fire at 13:00 UTC, got %d", n)
	}
	if len(runsSvc.List()) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runsSvc.List()))
	}

	// 09:05 EDT: nothing due (expression is minute 0).
	clock.Set(time.Date(2025, time.June, 2, 13, 5, 0, 0, time.UTC))
	if n := worker.Tick(ctx, clock.Now()); n != 0 {
		t.Fatalf("cron schedule must not fire off its matching minute, got %d", n)
	}
}

// TestWorkerPausedScheduleNeverFires: pause blocks firing; resume lets the
// overdue slot fire exactly once.
func TestWorkerPausedScheduleNeverFires(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	worker, svc, _, q := newTestWorker(t, clock)

	sched, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindRecurring, IntervalSeconds: 60})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := svc.Pause(ctx, "org-1", sched.ID); err != nil {
		t.Fatalf("Pause returned error: %v", err)
	}
	clock.Set(testBase.Add(10 * time.Minute))
	if n := worker.Tick(ctx, clock.Now()); n != 0 {
		t.Fatalf("paused schedule must not fire, got %d", n)
	}
	if _, err := svc.Resume(ctx, "org-1", sched.ID); err != nil {
		t.Fatalf("Resume returned error: %v", err)
	}
	if n := worker.Tick(ctx, clock.Now()); n != 1 {
		t.Fatalf("resumed overdue schedule should fire once, got %d", n)
	}
	if q.Length() != 1 {
		t.Fatalf("expected 1 task after resume, got %d", q.Length())
	}
}

// TestWorkerRunCreationFailureConsumesSlot documents the at-most-once
// trade-off: when run creation fails the claim is NOT undone (no duplicate
// runs); the schedule advances and retries at its next scheduled instant.
func TestWorkerRunCreationFailureConsumesSlot(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	svc := NewService().WithClock(clock)
	// runsSvc with a store-backed failure is complex; instead use a nil runs
	// service (degenerate embed): no run is created, claim still advances.
	worker := NewWorker(svc, nil, queue.NewQueue(), time.Second).WithClock(clock)

	sched, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindRecurring, IntervalSeconds: 60})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	clock.Set(testBase.Add(60 * time.Second))
	if n := worker.Tick(ctx, clock.Now()); n != 1 {
		t.Fatalf("expected 1 fire (claim-only), got %d", n)
	}
	stored, _ := svc.GetByID(ctx, sched.ID)
	if stored.LastRunID != "" {
		t.Fatalf("no run should be recorded, got %q", stored.LastRunID)
	}
	if stored.NextRunAt == nil || !stored.NextRunAt.Equal(testBase.Add(120*time.Second)) {
		t.Fatalf("schedule must still advance, got %v", stored.NextRunAt)
	}
}

// TestWorkerRunLoopStartsAndStops: Run ticks immediately (catch-up on
// startup) and exits when the context is cancelled (loop is started
// explicitly by the embedder).
func TestWorkerRunLoopStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newFakeClock(testBase)
	worker, svc, runsSvc, _ := newTestWorker(t, clock)

	if _, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindRecurring, IntervalSeconds: 60}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	// The schedule is already due when Run starts: the immediate first tick
	// must fire it deterministically (no reliance on the ticker).
	clock.Set(testBase.Add(61 * time.Second))

	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after ctx cancel")
	}
	if len(runsSvc.List()) != 1 {
		t.Fatalf("expected the immediate tick to fire once, got %d runs", len(runsSvc.List()))
	}
}
