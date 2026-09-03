package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is the injectable clock shared by service/worker tests.
type fakeClock struct {
	mu      sync.Mutex
	current time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{current: start.UTC()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
}

var testBase = time.Date(2025, time.June, 2, 10, 0, 0, 0, time.UTC)

func validOnceInput() CreateInput {
	return CreateInput{
		AgentID: "agent-1",
		Input:   "hello",
		Kind:    KindOnce,
		RunAt:   testBase.Add(time.Minute).Format(time.RFC3339),
	}
}

func validRecurringInput() CreateInput {
	return CreateInput{AgentID: "agent-1", Input: "tick", Kind: KindRecurring, IntervalSeconds: 300}
}

func validCronInput() CreateInput {
	return CreateInput{AgentID: "agent-1", Input: "cron", Kind: KindCron, CronExpr: "0 9 * * *", Timezone: "UTC"}
}

// TestCreateValidation covers the contract rules: once requires run_at;
// recurring requires interval_seconds >= 60; cron requires a valid expression
// plus an IANA timezone.
func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		orgID   string
		in      CreateInput
		wantErr error
	}{
		{"missing org", "", validOnceInput(), ErrOrgIDRequired},
		{"missing agent", "org-1", CreateInput{Kind: KindOnce, RunAt: testBase.Format(time.RFC3339)}, ErrAgentRequired},
		{"invalid kind", "org-1", CreateInput{AgentID: "a", Kind: "hourly"}, ErrInvalidKind},
		{"once missing run_at", "org-1", CreateInput{AgentID: "a", Kind: KindOnce}, ErrRunAtRequired},
		{"once bad run_at", "org-1", CreateInput{AgentID: "a", Kind: KindOnce, RunAt: "not-a-time"}, nil},
		{"recurring zero interval", "org-1", CreateInput{AgentID: "a", Kind: KindRecurring}, ErrIntervalTooSmall},
		{"recurring 59s interval", "org-1", CreateInput{AgentID: "a", Kind: KindRecurring, IntervalSeconds: 59}, ErrIntervalTooSmall},
		{"cron missing expr", "org-1", CreateInput{AgentID: "a", Kind: KindCron, Timezone: "UTC"}, ErrCronExprRequired},
		{"cron bad expr", "org-1", CreateInput{AgentID: "a", Kind: KindCron, CronExpr: "60 * * * *", Timezone: "UTC"}, nil},
		{"cron missing timezone", "org-1", CreateInput{AgentID: "a", Kind: KindCron, CronExpr: "0 9 * * *"}, ErrTimezoneRequired},
		{"cron bad timezone", "org-1", CreateInput{AgentID: "a", Kind: KindCron, CronExpr: "0 9 * * *", Timezone: "Mars/Olympus"}, nil},
		{"bad timezone other kind", "org-1", CreateInput{AgentID: "a", Kind: KindRecurring, IntervalSeconds: 120, Timezone: "Nope/Nope"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService()
			_, err := svc.Create(ctx, tc.orgID, tc.in)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if tc.wantErr != nil && err != tc.wantErr {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestCreateSuccessAndComputedFields checks each kind computes next_run_at.
func TestCreateSuccessAndComputedFields(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)

	t.Run("once", func(t *testing.T) {
		svc := NewService().WithClock(clock)
		runAt := testBase.Add(30 * time.Minute).Format(time.RFC3339)
		sched, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindOnce, RunAt: runAt})
		if err != nil {
			t.Fatalf("Create returned error: %v", err)
		}
		if sched.Status != StatusActive || sched.Kind != KindOnce {
			t.Fatalf("unexpected schedule state: %+v", sched)
		}
		if sched.NextRunAt == nil || !sched.NextRunAt.Equal(testBase.Add(30*time.Minute)) {
			t.Fatalf("next_run_at should equal run_at, got %v", sched.NextRunAt)
		}
		if sched.RunAt == nil || !sched.RunAt.Equal(testBase.Add(30*time.Minute)) {
			t.Fatalf("run_at mismatch: %v", sched.RunAt)
		}
		if sched.Timezone != DefaultTimezone {
			t.Fatalf("default timezone should be UTC, got %q", sched.Timezone)
		}
	})

	t.Run("recurring", func(t *testing.T) {
		svc := NewService().WithClock(clock)
		sched, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindRecurring, IntervalSeconds: 600})
		if err != nil {
			t.Fatalf("Create returned error: %v", err)
		}
		if sched.NextRunAt == nil || !sched.NextRunAt.Equal(testBase.Add(10*time.Minute)) {
			t.Fatalf("next_run_at should be now+interval, got %v", sched.NextRunAt)
		}
	})

	t.Run("cron with explicit timezone", func(t *testing.T) {
		svc := NewService().WithClock(clock)
		sched, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindCron, CronExpr: "0 9 * * *", Timezone: "Asia/Kolkata"})
		if err != nil {
			t.Fatalf("Create returned error: %v", err)
		}
		// 09:00 IST = 03:30 UTC; next occurrence after 10:00 UTC (=15:30 IST) is next day.
		want := time.Date(2025, time.June, 3, 3, 30, 0, 0, time.UTC)
		if sched.NextRunAt == nil || !sched.NextRunAt.Equal(want) {
			t.Fatalf("next_run_at = %v, want %v", sched.NextRunAt, want)
		}
	})

	t.Run("cron never fires rejected", func(t *testing.T) {
		svc := NewService().WithClock(clock)
		if _, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindCron, CronExpr: "0 0 31 2 *", Timezone: "UTC"}); err != ErrCronNeverFires {
			t.Fatalf("expected ErrCronNeverFires, got %v", err)
		}
	})
}

// TestDueAndTenantIsolation: Due returns only active schedules whose
// next_run_at has passed, and every read path is organization-scoped.
func TestDueAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	svc := NewService().WithClock(clock)

	orgA, orgB := "org-a", "org-b"
	schedA, err := svc.Create(ctx, orgA, validRecurringInput())
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	schedB, err := svc.Create(ctx, orgB, validOnceInput())
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	// Foreign tenant must not see org-a's schedule.
	if _, err := svc.Get(ctx, orgB, schedA.ID); err != ErrScheduleNotFound {
		t.Fatalf("cross-tenant Get should 404, got %v", err)
	}
	if _, err := svc.GetByID(ctx, schedA.ID); err != nil {
		t.Fatalf("trusted GetByID failed: %v", err)
	}

	// Before any due time: empty.
	due, err := svc.Due(ctx, testBase.Add(30*time.Second))
	if err != nil {
		t.Fatalf("Due returned error: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected no due schedules, got %d", len(due))
	}

	// At +2min: only org-b's once schedule (run_at = base+1min) is due.
	due, err = svc.Due(ctx, testBase.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Due returned error: %v", err)
	}
	if len(due) != 1 || due[0].ID != schedB.ID {
		t.Fatalf("expected only schedB due, got %+v", due)
	}

	// At +5min: both are due (schedA recurring next = base+5min), ordered by
	// next_run_at.
	due, err = svc.Due(ctx, testBase.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("Due returned error: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("expected 2 due schedules, got %d", len(due))
	}
	if !due[0].NextRunAt.Before(*due[1].NextRunAt) {
		t.Fatal("due schedules should be ordered by next_run_at")
	}

	// List stays tenant-scoped.
	listA, err := svc.List(ctx, orgA)
	if err != nil || len(listA) != 1 || listA[0].ID != schedA.ID {
		t.Fatalf("List(org-a) = %+v err=%v", listA, err)
	}
	listB, err := svc.List(ctx, orgB)
	if err != nil || len(listB) != 1 || listB[0].ID != schedB.ID {
		t.Fatalf("List(org-b) = %+v err=%v", listB, err)
	}
}

// TestClaimForFireOnceCompletes: a once schedule is claimed exactly once and
// becomes completed (terminal); catch-up protection refuses a second claim.
func TestClaimForFireOnceCompletes(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	svc := NewService().WithClock(clock)

	sched, err := svc.Create(ctx, "org-1", validOnceInput()) // run_at = base+1min
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	// Not yet due.
	if _, ok, err := svc.ClaimForFire(ctx, sched.ID, testBase); ok || err != nil {
		t.Fatalf("claim before due should be skipped, got ok=%v err=%v", ok, err)
	}

	clock.Set(testBase.Add(2 * time.Minute))
	claimed, ok, err := svc.ClaimForFire(ctx, sched.ID, clock.Now())
	if err != nil || !ok {
		t.Fatalf("claim should succeed, ok=%v err=%v", ok, err)
	}
	if claimed.Status != StatusCompleted {
		t.Fatalf("once schedule should be completed after firing, got %q", claimed.Status)
	}
	if claimed.NextRunAt != nil {
		t.Fatalf("completed schedule should have no next_run_at, got %v", claimed.NextRunAt)
	}
	if claimed.LastFiredAt == nil || !claimed.LastFiredAt.Equal(testBase.Add(2*time.Minute)) {
		t.Fatalf("last_fired_at should be the claim time, got %v", claimed.LastFiredAt)
	}

	// Restart/re-poll at the same instant: never double-fire.
	if _, ok, _ := svc.ClaimForFire(ctx, sched.ID, clock.Now()); ok {
		t.Fatal("completed schedule must not fire again (catch-up protection)")
	}
	// Due no longer returns it.
	due, _ := svc.Due(ctx, clock.Now())
	if len(due) != 0 {
		t.Fatalf("completed schedule must not be due, got %+v", due)
	}
}

// TestClaimForFireRecurringAdvances: recurring schedules advance to
// now+interval, never a burst of catch-up fires, and repeated claims at the
// same instant are refused.
func TestClaimForFireRecurringAdvances(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	svc := NewService().WithClock(clock)

	sched, err := svc.Create(ctx, "org-1", CreateInput{AgentID: "a", Kind: KindRecurring, IntervalSeconds: 600})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	first := testBase.Add(10 * time.Minute)
	clock.Set(first)
	claimed, ok, err := svc.ClaimForFire(ctx, sched.ID, clock.Now())
	if err != nil || !ok {
		t.Fatalf("claim should succeed, ok=%v err=%v", ok, err)
	}
	if claimed.NextRunAt == nil || !claimed.NextRunAt.Equal(first.Add(10*time.Minute)) {
		t.Fatalf("next_run_at should advance to now+interval, got %v", claimed.NextRunAt)
	}
	if claimed.LastFiredAt == nil || !claimed.LastFiredAt.Equal(first) {
		t.Fatalf("last_fired_at should be set, got %v", claimed.LastFiredAt)
	}

	// Immediately after (same clock instant): already consumed.
	if _, ok, _ := svc.ClaimForFire(ctx, sched.ID, clock.Now()); ok {
		t.Fatal("double claim at the same instant must be refused")
	}

	// Simulate a restart that misses 5 intervals: exactly one fire happens and
	// the schedule advances from "now", not per-missed-interval.
	clock.Set(first.Add(50 * time.Minute))
	claimed, ok, err = svc.ClaimForFire(ctx, sched.ID, clock.Now())
	if err != nil || !ok {
		t.Fatalf("claim after outage should succeed, ok=%v err=%v", ok, err)
	}
	if claimed.NextRunAt == nil || !claimed.NextRunAt.Equal(first.Add(60*time.Minute)) {
		t.Fatalf("catch-up should advance from now, got %v", claimed.NextRunAt)
	}
}

// TestClaimForFireCronAdvances checks the cron branch advances to the next
// matching wall-clock minute in the schedule's timezone.
func TestClaimForFireCronAdvances(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	svc := NewService().WithClock(clock)

	sched, err := svc.Create(ctx, "org-1", validCronInput()) // 09:00 UTC daily
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	// Create computed next = 2025-06-03 09:00 UTC (today's 09:00 passed at 10:00 UTC base? base is 10:00 UTC, so next is Jun 3 09:00)
	fireAt := time.Date(2025, time.June, 3, 9, 0, 0, 0, time.UTC)
	clock.Set(fireAt)
	claimed, ok, err := svc.ClaimForFire(ctx, sched.ID, clock.Now())
	if err != nil || !ok {
		t.Fatalf("claim should succeed, ok=%v err=%v", ok, err)
	}
	wantNext := time.Date(2025, time.June, 4, 9, 0, 0, 0, time.UTC)
	if claimed.NextRunAt == nil || !claimed.NextRunAt.Equal(wantNext) {
		t.Fatalf("cron next_run_at = %v, want %v", claimed.NextRunAt, wantNext)
	}
	if claimed.Status != StatusActive {
		t.Fatalf("cron schedule stays active, got %q", claimed.Status)
	}
}

// TestPauseResume: pause stops firing, resume restores it, transitions are
// tenant-scoped and validated.
func TestPauseResume(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	svc := NewService().WithClock(clock)

	sched, err := svc.Create(ctx, "org-1", validRecurringInput())
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	paused, err := svc.Pause(ctx, "org-1", sched.ID)
	if err != nil || paused.Status != StatusPaused {
		t.Fatalf("Pause returned %+v err=%v", paused, err)
	}
	// Foreign tenant cannot pause.
	if _, err := svc.Pause(ctx, "org-other", sched.ID); err != ErrScheduleNotFound {
		t.Fatalf("cross-tenant pause should 404, got %v", err)
	}
	if _, err := svc.Pause(ctx, "org-1", sched.ID); err != ErrScheduleNotActive {
		t.Fatalf("pausing a paused schedule should fail, got %v", err)
	}

	// Paused schedules never become due.
	due, _ := svc.Due(ctx, testBase.Add(10*time.Minute))
	if len(due) != 0 {
		t.Fatalf("paused schedule must not be due, got %+v", due)
	}

	resumed, err := svc.Resume(ctx, "org-1", sched.ID)
	if err != nil || resumed.Status != StatusActive {
		t.Fatalf("Resume returned %+v err=%v", resumed, err)
	}
	if _, err := svc.Resume(ctx, "org-1", sched.ID); err != ErrScheduleNotPaused {
		t.Fatalf("resuming an active schedule should fail, got %v", err)
	}

	// Overdue next_run_at survives pause/resume: fires on next poll.
	claimed, ok, err := svc.ClaimForFire(ctx, sched.ID, testBase.Add(10*time.Minute))
	if err != nil || !ok {
		t.Fatalf("resumed overdue schedule should fire, ok=%v err=%v", ok, err)
	}
	if claimed.NextRunAt == nil || !claimed.NextRunAt.Equal(testBase.Add(15*time.Minute)) {
		t.Fatalf("unexpected next_run_at after resume fire: %v", claimed.NextRunAt)
	}
}

// TestDelete: delete removes the schedule and is tenant-scoped.
func TestDelete(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	svc := NewService().WithClock(clock)

	sched, err := svc.Create(ctx, "org-1", validRecurringInput())
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := svc.Delete(ctx, "org-other", sched.ID); err != ErrScheduleNotFound {
		t.Fatalf("cross-tenant delete should 404, got %v", err)
	}
	if err := svc.Delete(ctx, "org-1", sched.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if _, err := svc.Get(ctx, "org-1", sched.ID); err != ErrScheduleNotFound {
		t.Fatalf("deleted schedule should be gone, got %v", err)
	}
}

// TestSetLastRunAndCompletedGuards: SetLastRun records the fired run;
// completed schedules reject pause/resume.
func TestSetLastRunAndCompletedGuards(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	svc := NewService().WithClock(clock)

	sched, err := svc.Create(ctx, "org-1", validOnceInput())
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := svc.SetLastRun(ctx, sched.ID, "run-123"); err != nil {
		t.Fatalf("SetLastRun returned error: %v", err)
	}
	got, _ := svc.Get(ctx, "org-1", sched.ID)
	if got.LastRunID != "run-123" {
		t.Fatalf("last_run_id = %q, want run-123", got.LastRunID)
	}
	if err := svc.SetLastRun(ctx, "unknown", "run-1"); err != ErrScheduleNotFound {
		t.Fatalf("SetLastRun for unknown id should fail, got %v", err)
	}

	clock.Set(testBase.Add(2 * time.Minute))
	if _, _, err := svc.ClaimForFire(ctx, sched.ID, clock.Now()); err != nil {
		t.Fatalf("ClaimForFire returned error: %v", err)
	}
	if _, err := svc.Pause(ctx, "org-1", sched.ID); err != ErrScheduleCompleted {
		t.Fatalf("pause of completed schedule should fail, got %v", err)
	}
	if _, err := svc.Resume(ctx, "org-1", sched.ID); err != ErrScheduleCompleted {
		t.Fatalf("resume of completed schedule should fail, got %v", err)
	}
}
