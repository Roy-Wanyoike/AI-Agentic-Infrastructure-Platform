package synth

// schedule_test.go — issue #156 tests: verified-production-only scheduling,
// server-side read-only default, timezone-aware windows with an injectable
// clock, kill switches, tenant budgets/concurrency, retry policy,
// deployment hooks, synthetic-account isolation + cleanup registry. All
// deterministic (fake clock, scripted enqueuer, no sleeps).

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// ---- test doubles -------------------------------------------------------

// mapResolver resolves environments/targets from a fixed table.
type mapResolver struct {
	byEnv map[string]Target
	envs  map[string]Environment
}

func (m mapResolver) Resolve(_ context.Context, orgID, environmentID string) (Target, Environment, error) {
	t, ok := m.byEnv[environmentID]
	if !ok {
		return Target{}, Environment{}, fmt.Errorf("%w: environment %s", ErrNotFound, environmentID)
	}
	e := m.envs[environmentID]
	if t.OrgID != orgID {
		return Target{}, Environment{}, ErrNotFound
	}
	return t, e, nil
}

// recEnqueuer records RunRequests; optional scripted errors per call.
type recEnqueuer struct {
	reqs   []RunRequest
	ids    []string
	failAt map[int]error // 1-based call index -> error
	calls  int
}

func (r *recEnqueuer) Enqueue(_ context.Context, req RunRequest) (string, error) {
	r.calls++
	if err := r.failAt[r.calls]; err != nil {
		return "", err
	}
	r.reqs = append(r.reqs, req)
	id := fmt.Sprintf("run-%d", r.calls)
	r.ids = append(r.ids, id)
	return id, nil
}

// schedEnv builds a verified production target + environment pair.
func schedEnv(mode EnvironmentMode) (Target, Environment) {
	t := Target{ID: "tgt-p", OrgID: "org-1", ProjectID: "proj-1", Name: "prod", BaseURL: "https://app.example.com", Verified: true}
	e := Environment{
		ID:             "env-p",
		OrgID:          "org-1",
		TargetID:       "tgt-p",
		Kind:           EnvProduction,
		Mode:           mode,
		AllowedDomains: []string{"app.example.com"},
	}
	return t, e
}

func newTestScheduler(t *testing.T, mode EnvironmentMode, now func() time.Time) (*ProductionScheduler, *MemScheduleStore, *recEnqueuer, *KillSwitch, *TenantBudgets, *CleanupRegistry) {
	t.Helper()
	tgt, env := schedEnv(mode)
	store := NewMemScheduleStore()
	res := mapResolver{byEnv: map[string]Target{"env-p": tgt}, envs: map[string]Environment{"env-p": env}}
	enq := &recEnqueuer{}
	kill := NewKillSwitch()
	limits := NewTenantBudgets()
	cleanup := NewCleanupRegistry()
	s, err := NewProductionScheduler(store, res, enq, kill, limits, cleanup, ProductionSchedulerConfig{Now: now})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	return s, store, enq, kill, limits, cleanup
}

func recurringSchedule() ProductionSchedule {
	return ProductionSchedule{
		ID:             "sched-1",
		OrgID:          "org-1",
		ProjectID:      "proj-1",
		JourneyID:      "j-1",
		JourneyVersion: 3,
		EnvironmentID:  "env-p",
		PersonaID:      "per-1",
		PersonaVersion: 2,
		Interval:       time.Hour,
	}
}

// ---- tests --------------------------------------------------------------

func TestMemScheduleStoreCRUDAndOrdering(t *testing.T) {
	store := NewMemScheduleStore()
	a := recurringSchedule()
	a.ID, a.CreatedAt = "sched-a", t0
	b := recurringSchedule()
	b.ID, b.CreatedAt = "sched-b", t0.Add(-time.Hour) // older, must list first
	for _, s := range []ProductionSchedule{a, b} {
		if err := store.Put(s); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	got, err := store.List("org-1", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].ID != "sched-b" || got[1].ID != "sched-a" {
		t.Fatalf("ordering broken: %v %v", got[0].ID, got[1].ID)
	}
	if _, err := store.Get("org-2", "sched-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get must be ErrNotFound, got %v", err)
	}
	if err := store.Delete("org-1", "sched-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Delete("org-1", "sched-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete must be ErrNotFound, got %v", err)
	}
}

func TestScheduleValidation(t *testing.T) {
	sc := recurringSchedule()
	if err := sc.Validate(); err != nil {
		t.Fatalf("valid schedule rejected: %v", err)
	}
	bad := sc
	bad.OrgID = ""
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing org must fail, got %v", err)
	}
	noRec := sc
	noRec.Interval, noRec.OnDeployment = 0, nil
	if err := noRec.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no recurrence/trigger must fail, got %v", err)
	}
	writeNoAcct := sc
	writeNoAcct.HasWriteOps = true
	if err := writeNoAcct.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("write schedule without account must fail, got %v", err)
	}
	deployOnly := sc
	deployOnly.Interval = 0
	deployOnly.OnDeployment = &AfterDeployment{AppVersion: "v2"}
	if err := deployOnly.Validate(); err != nil {
		t.Fatalf("deployment-only schedule should validate, got %v", err)
	}
}

func TestCreateScheduleVerifiedProductionOnly(t *testing.T) {
	now := t0
	s, store, _, _, _, _ := newTestScheduler(t, ModeProductionReadOnly, func() time.Time { return now })

	// Happy path: read-only recurring production schedule.
	sc, err := s.CreateSchedule(context.Background(), recurringSchedule())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sc.NextRunAt != t0.Add(time.Hour) {
		t.Fatalf("NextRunAt = %v, want one interval out", sc.NextRunAt)
	}
	if _, err := store.Get("org-1", "sched-1"); err != nil {
		t.Fatalf("not persisted: %v", err)
	}

	// Unverified target.
	tgt, env := schedEnv(ModeProductionReadOnly)
	tgt.Verified = false
	res := mapResolver{byEnv: map[string]Target{"env-u": tgt}, envs: map[string]Environment{"env-u": env}}
	s2, err := NewProductionScheduler(NewMemScheduleStore(), res, &recEnqueuer{}, nil, nil, nil, ProductionSchedulerConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("scheduler2: %v", err)
	}
	bad := recurringSchedule()
	bad.EnvironmentID = "env-u"
	if _, err := s2.CreateSchedule(context.Background(), bad); !errors.Is(err, ErrTargetUnverified) {
		t.Fatalf("want ErrTargetUnverified, got %v", err)
	}

	// Non-production environment.
	tgt2, env2 := schedEnv(ModePreProduction)
	env2.Kind = EnvStaging
	res2 := mapResolver{byEnv: map[string]Target{"env-s": tgt2}, envs: map[string]Environment{"env-s": env2}}
	s3, _ := NewProductionScheduler(NewMemScheduleStore(), res2, &recEnqueuer{}, nil, nil, nil, ProductionSchedulerConfig{Now: func() time.Time { return now }})
	bad2 := recurringSchedule()
	bad2.EnvironmentID = "env-s"
	if _, err := s3.CreateSchedule(context.Background(), bad2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid for staging schedule, got %v", err)
	}

	// Write journey in production_read_only mode: ErrReadOnlyMode.
	write := recurringSchedule()
	write.HasWriteOps = true
	write.SyntheticAccount = "synth-acct-1"
	if _, err := s.CreateSchedule(context.Background(), write); !errors.Is(err, ErrReadOnlyMode) {
		t.Fatalf("want ErrReadOnlyMode, got %v", err)
	}
}

func TestCreateScheduleWriteRequiresReleaseComparison(t *testing.T) {
	now := t0
	s, _, _, _, _, _ := newTestScheduler(t, ModeReleaseComparison, func() time.Time { return now })
	write := recurringSchedule()
	write.HasWriteOps = true
	write.SyntheticAccount = "synth-acct-1"
	sc, err := s.CreateSchedule(context.Background(), write)
	if err != nil {
		t.Fatalf("write schedule in release_comparison should create: %v", err)
	}
	if sc.SyntheticAccount != "synth-acct-1" {
		t.Fatalf("synthetic account lost: %+v", sc)
	}
}

func TestTickDueSchedulesWindowsAndPause(t *testing.T) {
	now := t0
	s, store, enq, _, _, _ := newTestScheduler(t, ModeProductionReadOnly, func() time.Time { return now })
	if _, err := s.CreateSchedule(context.Background(), recurringSchedule()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Not yet due (NextRunAt = t0+1h).
	reqs, err := s.Tick(context.Background(), "org-1")
	if err != nil || len(reqs) != 0 {
		t.Fatalf("premature tick: %d reqs, err %v", len(reqs), err)
	}

	now = t0.Add(time.Hour) // due
	reqs, err = s.Tick(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	r := reqs[0]
	if !r.ReadOnly || r.Trigger != TriggerRecurring || r.JourneyVersion != 3 || r.PersonaVersion != 2 {
		t.Fatalf("request not pinned read-only/versioned: %+v", r)
	}
	// NextRunAt advanced by one interval — no double-fire.
	reqs, _ = s.Tick(context.Background(), "org-1")
	if len(reqs) != 0 {
		t.Fatalf("double fire: %d", len(reqs))
	}

	// Pause stops ticks; resume restarts a full interval later.
	if err := s.PauseSchedule("org-1", "sched-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	now = t0.Add(3 * time.Hour)
	if reqs, _ = s.Tick(context.Background(), "org-1"); len(reqs) != 0 {
		t.Fatal("paused schedule must not run")
	}
	if err := s.ResumeSchedule("org-1", "sched-1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if reqs, _ = s.Tick(context.Background(), "org-1"); len(reqs) != 0 {
		t.Fatal("resumed schedule must wait a full interval")
	}
	now = t0.Add(4 * time.Hour)
	if reqs, _ = s.Tick(context.Background(), "org-1"); len(reqs) != 1 {
		t.Fatal("resumed schedule should be due one interval after resume")
	}

	// Quiet window defers the run to the window end (timezone-aware).
	sc, _ := store.Get("org-1", "sched-1")
	// The successful resume tick advanced NextRunAt; re-due the schedule and
	// shorten the interval so the window end dominates the deferred time.
	sc.Interval = 15 * time.Minute
	now = t0.Add(4 * time.Hour) // 14:00 UTC = 10:00 EDT Monday
	sc.NextRunAt = now
	_ = store.Put(sc)
	tgt, env := schedEnv(ModeProductionReadOnly)
	env.QuietWindows = []QuietWindow{{Weekdays: []int{1}, Start: "04:00", End: "05:00", Location: "America/New_York", Reason: "peak"}}
	res := mapResolver{byEnv: map[string]Target{"env-p": tgt}, envs: map[string]Environment{"env-p": env}}
	s2, _ := NewProductionScheduler(store, res, enq, nil, nil, nil, ProductionSchedulerConfig{Now: func() time.Time { return now }})
	// 10:00 EDT Monday — outside the 04:00-05:00 EDT window.
	reqs, err = s2.Tick(context.Background(), "org-1")
	if err != nil || len(reqs) != 1 {
		t.Fatalf("outside quiet window should run: %d %v", len(reqs), err)
	}
	// Move inside the window: 04:30 EDT = 08:30 UTC Monday.
	now = time.Date(2025, 6, 2, 8, 30, 0, 0, time.UTC)
	sc.NextRunAt = now
	_ = store.Put(sc)
	reqs, err = s2.Tick(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("tick in window: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatal("quiet window must defer the run")
	}
	after, _ := store.Get("org-1", "sched-1")
	ny, _ := time.LoadLocation("America/New_York")
	wantEnd := time.Date(2025, 6, 2, 5, 0, 0, 0, ny) // 05:00 EDT
	if !after.NextRunAt.Equal(wantEnd) {
		t.Fatalf("NextRunAt = %v, want quiet window end %v", after.NextRunAt, wantEnd)
	}
	// Cancel removes the schedule.
	if err := s2.CancelSchedule("org-1", "sched-1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := store.Get("org-1", "sched-1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("cancelled schedule must be gone")
	}
}

func TestTickKillSwitchStopsScheduling(t *testing.T) {
	now := t0
	s, store, _, kill, _, _ := newTestScheduler(t, ModeProductionReadOnly, func() time.Time { return now })
	if _, err := s.CreateSchedule(context.Background(), recurringSchedule()); err != nil {
		t.Fatalf("create: %v", err)
	}
	kill.TripProject("proj-1", "incident")
	now = t0.Add(time.Hour)
	if reqs, err := s.Tick(context.Background(), "org-1"); err != nil || len(reqs) != 0 {
		t.Fatalf("project kill switch must stop scheduling: %d %v", len(reqs), err)
	}
	kill.ResetProject("proj-1")
	kill.TripGlobal("platform freeze")
	if reqs, _ := s.Tick(context.Background(), "org-1"); len(reqs) != 0 {
		t.Fatal("global kill switch must stop scheduling")
	}
	kill.ResetGlobal()
	now = t0.Add(2 * time.Hour) // skips advanced NextRunAt to t0+2h
	if reqs, err := s.Tick(context.Background(), "org-1"); err != nil || len(reqs) != 1 {
		t.Fatalf("after reset scheduling resumes: %d %v", len(reqs), err)
	}
	_ = store
}

func TestTenantBudgetAndConcurrencyCaps(t *testing.T) {
	now := t0
	s, _, enq, _, limits, _ := newTestScheduler(t, ModeProductionReadOnly, func() time.Time { return now })
	limits.SetLimits("org-1", TenantLimits{MaxRunsPerWindow: 1, Window: time.Hour, MaxConcurrentRuns: 1})

	a := recurringSchedule()
	a.ID = "sched-a"
	a.Interval = time.Minute
	b := recurringSchedule()
	b.ID = "sched-b"
	b.Interval = time.Minute
	if _, err := s.CreateSchedule(context.Background(), a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := s.CreateSchedule(context.Background(), b); err != nil {
		t.Fatalf("create b: %v", err)
	}

	now = t0.Add(2 * time.Minute) // both due
	reqs, err := s.Tick(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("concurrency cap 1 must allow exactly one run, got %d", len(reqs))
	}
	if enq.calls != 1 {
		t.Fatalf("one enqueue expected, got %d", enq.calls)
	}

	// Release the slot: budget window still open (MaxRunsPerWindow=1) so the
	// second schedule stays blocked until the window rolls.
	s.ReleaseRun("org-1")
	now = t0.Add(3 * time.Minute)
	reqs, _ = s.Tick(context.Background(), "org-1")
	if len(reqs) != 0 {
		t.Fatal("run budget must hold within the window")
	}
	now = t0.Add(time.Hour + 3*time.Minute) // window rolls over
	reqs, err = s.Tick(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("tick after window: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("new window should admit one run, got %d", len(reqs))
	}
}

func TestRetryPolicyBackoffAndExhaustion(t *testing.T) {
	now := t0
	s, store, enq, _, _, _ := newTestScheduler(t, ModeProductionReadOnly, func() time.Time { return now })
	sc := recurringSchedule()
	sc.Retry = RetryPolicy{MaxAttempts: 3, Backoff: time.Minute, MaxBackoff: time.Hour}
	if _, err := s.CreateSchedule(context.Background(), sc); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Enqueue failure #1 -> retry in 1 minute (attempt 1).
	enq.failAt = map[int]error{1: errors.New("worker down")}
	now = t0.Add(time.Hour)
	if _, err := s.Tick(context.Background(), "org-1"); err == nil {
		t.Fatal("enqueue failure must surface")
	}
	got, _ := store.Get("org-1", "sched-1")
	if got.Attempts != 1 || !got.NextRunAt.Equal(t0.Add(61*time.Minute)) {
		t.Fatalf("retry not scheduled: attempts=%d next=%v", got.Attempts, got.NextRunAt)
	}

	// Retry succeeds -> attempts reset, normal cadence resumes.
	enq.failAt = nil
	now = t0.Add(61 * time.Minute)
	reqs, err := s.Tick(context.Background(), "org-1")
	if err != nil || len(reqs) != 1 || reqs[0].Trigger != TriggerRetry {
		t.Fatalf("retry request wrong: %+v err %v", reqs, err)
	}
	got, _ = store.Get("org-1", "sched-1")
	if got.Attempts != 0 {
		t.Fatalf("attempts must reset on success, got %d", got.Attempts)
	}

	// Run-level failures honor the policy via ReportRun (2 -> 3 -> exhausted).
	if err := s.ReportRun("org-1", "sched-1", RunFailed, t0.Add(62*time.Minute)); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, _ = store.Get("org-1", "sched-1")
	if got.Attempts != 1 || !got.NextRunAt.Equal(t0.Add(63*time.Minute)) {
		t.Fatalf("backoff attempt 1 wrong: %+v", got)
	}
	if err := s.ReportRun("org-1", "sched-1", RunInconclusive, t0.Add(63*time.Minute)); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, _ = store.Get("org-1", "sched-1")
	if got.Attempts != 2 || !got.NextRunAt.Equal(t0.Add(65*time.Minute)) {
		t.Fatalf("backoff attempt 2 wrong: %+v", got)
	}
	// Third failure exhausts the policy (attempts reset, no re-due).
	if err := s.ReportRun("org-1", "sched-1", RunFailed, t0.Add(65*time.Minute)); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, _ = store.Get("org-1", "sched-1")
	if got.Attempts != 0 {
		t.Fatalf("exhausted policy must reset attempts, got %d", got.Attempts)
	}
	// Success resets too.
	if err := s.ReportRun("org-1", "sched-1", RunCompleted, t0.Add(66*time.Minute)); err != nil {
		t.Fatalf("report: %v", err)
	}
	if got, _ = store.Get("org-1", "sched-1"); got.Attempts != 0 {
		t.Fatal("success must reset attempts")
	}
	// Default policy (unset) = no retries.
	def := recurringSchedule()
	def.ID = "sched-def"
	def.Retry = RetryPolicy{}
	if _, err := s.CreateSchedule(context.Background(), def); err != nil {
		t.Fatalf("create def: %v", err)
	}
	if d := retryDelay(def.Retry, 3); d != 30*time.Second*4 {
		t.Fatalf("default backoff = %v", d)
	}
}

func TestOnDeploymentTriggers(t *testing.T) {
	now := t0
	s, _, enq, _, _, _ := newTestScheduler(t, ModeProductionReadOnly, func() time.Time { return now })
	pinned := recurringSchedule()
	pinned.ID = "sched-dep"
	pinned.Interval = 0
	pinned.OnDeployment = &AfterDeployment{AppVersion: "v2.3.0"}
	wild := recurringSchedule()
	wild.ID = "sched-any"
	wild.Interval = 0
	wild.OnDeployment = &AfterDeployment{}
	if _, err := s.CreateSchedule(context.Background(), pinned); err != nil {
		t.Fatalf("create pinned: %v", err)
	}
	if _, err := s.CreateSchedule(context.Background(), wild); err != nil {
		t.Fatalf("create wild: %v", err)
	}

	reqs, err := s.OnDeployment(context.Background(), "org-1", "proj-1", "v2.3.0")
	if err != nil {
		t.Fatalf("onDeployment: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("want 2 triggered runs, got %d", len(reqs))
	}
	for _, r := range reqs {
		if r.Trigger != TriggerAfterDeployment || r.AppVersion != "v2.3.0" || !r.ReadOnly {
			t.Fatalf("bad deployment request: %+v", r)
		}
	}
	// Version mismatch triggers only the wildcard.
	enq.reqs = nil
	reqs, _ = s.OnDeployment(context.Background(), "org-1", "proj-1", "v9.9.9")
	if len(reqs) != 1 || reqs[0].ScheduleID != "sched-any" {
		t.Fatalf("wildcard-only trigger expected: %+v", reqs)
	}
	// Paused schedules never fire.
	_ = s.PauseSchedule("org-1", "sched-any")
	reqs, _ = s.OnDeployment(context.Background(), "org-1", "proj-1", "v9.9.9")
	if len(reqs) != 0 {
		t.Fatal("paused schedule must not fire on deployment")
	}
}

func TestWriteRunIsolationAndCleanupRegistry(t *testing.T) {
	now := t0
	s, _, enq, _, _, cleanup := newTestScheduler(t, ModeReleaseComparison, func() time.Time { return now })
	write := recurringSchedule()
	write.ID = "sched-w"
	write.HasWriteOps = true
	write.SyntheticAccount = "synth-acct-9"
	if _, err := s.CreateSchedule(context.Background(), write); err != nil {
		t.Fatalf("create: %v", err)
	}
	now = t0.Add(time.Hour)
	reqs, err := s.Tick(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("want 1 write run, got %d", len(reqs))
	}
	r := reqs[0]
	if r.ReadOnly {
		t.Fatal("approved write flow must not be read-only")
	}
	if r.SyntheticAccount != "synth-acct-9" {
		t.Fatalf("synthetic account not carried: %+v", r)
	}
	// Cleanup registered for the write-test data and becomes due.
	tasks := cleanup.Pending(now.Add(s.cleanupGrace))
	if len(tasks) != 1 || tasks[0].SyntheticAccount != "synth-acct-9" || tasks[0].ScheduleID != "sched-w" {
		t.Fatalf("cleanup task missing/wrong: %+v", tasks)
	}
	if err := cleanup.MarkDone(tasks[0].ID, now); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if tasks = cleanup.Pending(now); len(tasks) != 0 {
		t.Fatalf("done task still pending: %+v", tasks)
	}
	if err := cleanup.MarkDone("nope", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cleanup id must be ErrNotFound, got %v", err)
	}
	_ = enq
}

func TestTickReadonlyDefaultEnforcedServerSide(t *testing.T) {
	// A write schedule whose environment was flipped back to
	// production_read_only is refused at evaluation time — the read-only
	// default is enforced server-side on EVERY tick, not only at creation.
	now := t0
	s, store, _, _, _, _ := newTestScheduler(t, ModeReleaseComparison, func() time.Time { return now })
	write := recurringSchedule()
	write.ID = "sched-w2"
	write.HasWriteOps = true
	write.SyntheticAccount = "synth-acct-2"
	if _, err := s.CreateSchedule(context.Background(), write); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Swap the environment's mode behind the scheduler's back.
	tgt, env := schedEnv(ModeProductionReadOnly)
	res := mapResolver{byEnv: map[string]Target{"env-p": tgt}, envs: map[string]Environment{"env-p": env}}
	s2, _ := NewProductionScheduler(store, res, &recEnqueuer{}, nil, nil, nil, ProductionSchedulerConfig{Now: func() time.Time { return now }})
	now = t0.Add(time.Hour)
	reqs, err := s2.Tick(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatal("write schedule must be refused when mode is read-only")
	}
}
