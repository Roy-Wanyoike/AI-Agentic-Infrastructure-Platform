package synth

// schedule.go — issue #156: continuous production journeys. The
// ProductionScheduler turns reviewed journeys into recurring (or
// deployment-triggered) run requests against VERIFIED production targets
// ONLY, with every safety rail enforced server-side:
//
//   - target.Verified + Environment.Kind == EnvProduction are hard
//     prerequisites (ErrTargetUnverified / ErrInvalid otherwise),
//   - the read-only default is enforced server-side: write journeys run
//     ONLY in ModeReleaseComparison AND with an isolated synthetic account
//     (ErrReadOnlyMode / ErrInvalid otherwise); every non-write production
//     run is pinned ReadOnly in the RunRequest,
//   - timezone-aware QuietWindow/TestWindows (evaluateWindows from
//     explore.go, injectable clock, fail-closed on unparsable policy),
//   - retry policy with capped exponential backoff,
//   - pause/cancel, tenant run budgets + concurrency caps, and the shared
//     global/per-project KillSwitch (explore.go),
//   - synthetic-account isolation + a cleanup registry for approved
//     write-test data.
//
// Persistence is an in-memory ScheduleStore implementation (no migration —
// a Postgres store is a documented follow-up behind the same interface).

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Run trigger reasons carried on RunRequest.
const (
	TriggerRecurring       = "recurring"
	TriggerAfterDeployment = "after_deployment"
	TriggerRetry           = "retry"
)

// ProductionSchedule is a recurring (or deployment-triggered) journey
// schedule against a verified production environment. Create it through
// ProductionScheduler.CreateSchedule, which enforces the production/read-only
// policy — schedules constructed by hand carry no enforcement.
type ProductionSchedule struct {
	ID             string
	OrgID          string
	ProjectID      string
	JourneyID      string
	JourneyVersion int
	EnvironmentID  string
	PersonaID      string
	PersonaVersion int
	// HasWriteOps marks journeys containing state-changing steps. Such
	// schedules REQUIRE ModeReleaseComparison + a synthetic account.
	HasWriteOps bool
	// Interval is the recurrence period (> 0 for time-based schedules).
	Interval time.Duration
	// OnDeployment, when non-nil, additionally triggers a run after each
	// deployment of AppVersion ("" = any version).
	OnDeployment *AfterDeployment
	// Retry is the failure/inconclusive retry policy.
	Retry RetryPolicy
	// Paused schedules are skipped by Tick and OnDeployment.
	Paused bool
	// SyntheticAccount is the isolated account reference for approved write
	// flows (ModeReleaseComparison only; required when HasWriteOps).
	SyntheticAccount string
	// NextRunAt is the next due instant for time-based schedules.
	NextRunAt time.Time
	// Attempts counts consecutive retry attempts consumed for the current
	// journey run cycle (reset on success/exhaustion).
	Attempts  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate checks schedule invariants (identity fields, recurrence or
// deployment trigger, synthetic-account requirement for write flows).
func (s ProductionSchedule) Validate() error {
	var missing []string
	for k, v := range map[string]string{
		"org_id":         s.OrgID,
		"project_id":     s.ProjectID,
		"journey_id":     s.JourneyID,
		"environment_id": s.EnvironmentID,
		"persona_id":     s.PersonaID,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: production schedule missing %s", ErrInvalid, strings.Join(missing, ", "))
	}
	if s.Interval < 0 {
		return fmt.Errorf("%w: negative interval", ErrInvalid)
	}
	if s.Interval == 0 && s.OnDeployment == nil {
		return fmt.Errorf("%w: schedule requires Interval > 0 or an OnDeployment trigger", ErrInvalid)
	}
	if s.HasWriteOps && strings.TrimSpace(s.SyntheticAccount) == "" {
		return fmt.Errorf("%w: write schedules require an isolated SyntheticAccount", ErrInvalid)
	}
	return nil
}

// AfterDeployment is the event-trigger hook: fire after a deployment of
// AppVersion ("" matches any version).
type AfterDeployment struct {
	AppVersion string
}

// RetryPolicy is the retry policy for failed/inconclusive runs. MaxAttempts
// counts the INITIAL attempt (1 = no retries); values <= 0 normalize to 1.
// Backoff defaults to 30s, MaxBackoff to 10m when unset.
type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
	MaxBackoff  time.Duration
}

// RunRequest is one due execution handed to the RunEnqueuer seam. ReadOnly
// is pinned by the scheduler: true for every production run except approved
// write flows in ModeReleaseComparison with a synthetic account.
type RunRequest struct {
	ScheduleID       string
	OrgID            string
	ProjectID        string
	JourneyID        string
	JourneyVersion   int
	EnvironmentID    string
	PersonaID        string
	PersonaVersion   int
	AppVersion       string // set for after_deployment triggers
	ReadOnly         bool
	SyntheticAccount string
	Trigger          string // TriggerRecurring | TriggerAfterDeployment | TriggerRetry
	DueAt            time.Time
}

// ScheduleStore persists production schedules (org-scoped). The in-memory
// implementation ships with #156; a Postgres store is a documented
// follow-up behind this same interface (no migration in this track).
type ScheduleStore interface {
	Put(ProductionSchedule) error
	Get(orgID, id string) (ProductionSchedule, error)
	List(orgID, projectID string) ([]ProductionSchedule, error) // projectID "" = whole org
	Delete(orgID, id string) error
}

// MemScheduleStore is the in-memory ScheduleStore. Deterministic List
// ordering (CreatedAt, then ID). Stored values are shallow copies — treat
// ProductionSchedule (with its trigger pointer) as immutable once stored.
type MemScheduleStore struct {
	mu    sync.RWMutex
	byID  map[string]ProductionSchedule
	order []string
}

// NewMemScheduleStore returns an empty in-memory store.
func NewMemScheduleStore() *MemScheduleStore {
	return &MemScheduleStore{byID: make(map[string]ProductionSchedule)}
}

// Put inserts or replaces a schedule (id required).
func (m *MemScheduleStore) Put(s ProductionSchedule) error {
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("%w: schedule id required", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[s.ID]; !ok {
		m.order = append(m.order, s.ID)
	}
	m.byID[s.ID] = s
	return nil
}

// Get returns one schedule; unknown ids surface ErrNotFound (no existence
// leak across tenants — the org id is part of the key).
func (m *MemScheduleStore) Get(orgID, id string) (ProductionSchedule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.byID[id]
	if !ok || s.OrgID != orgID {
		return ProductionSchedule{}, ErrNotFound
	}
	return s, nil
}

// List returns org schedules (optionally filtered by project) ordered by
// CreatedAt then ID.
func (m *MemScheduleStore) List(orgID, projectID string) ([]ProductionSchedule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ProductionSchedule, 0, len(m.order))
	for _, id := range m.order {
		s, ok := m.byID[id]
		if !ok || s.OrgID != orgID {
			continue
		}
		if projectID != "" && s.ProjectID != projectID {
			continue
		}
		out = append(out, s)
	}
	// Insertion order is stable; sort deterministically by (CreatedAt, ID).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if b.CreatedAt.Before(a.CreatedAt) || (b.CreatedAt.Equal(a.CreatedAt) && b.ID < a.ID) {
				out[j-1], out[j] = out[j], out[j-1]
				continue
			}
			break
		}
	}
	return out, nil
}

// Delete removes a schedule; unknown/foreign ids are ErrNotFound.
func (m *MemScheduleStore) Delete(orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok || s.OrgID != orgID {
		return ErrNotFound
	}
	delete(m.byID, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}

// EnvironmentResolver resolves the Target + Environment backing a schedule
// (the registry track owns the authoritative records; #156 only reads them).
type EnvironmentResolver interface {
	Resolve(ctx context.Context, orgID, environmentID string) (Target, Environment, error)
}

// RunEnqueuer is the worker seam: due RunRequests are handed here. The
// scheduler acquires a tenant concurrency slot per accepted run; the runtime
// MUST call ProductionScheduler.ReleaseRun when the run reaches a terminal
// status (the scheduler cannot observe asynchronous completion).
type RunEnqueuer interface {
	Enqueue(ctx context.Context, req RunRequest) (runID string, err error)
}

// TenantLimits are the per-tenant run budget and concurrency caps
// (zero value = unlimited for that dimension).
type TenantLimits struct {
	MaxRunsPerWindow  int
	Window            time.Duration
	MaxConcurrentRuns int
}

type tenantUsage struct {
	windowStart time.Time
	runsUsed    int
	active      int
}

// TenantBudgets is the thread-safe per-tenant budget/concurrency ledger.
type TenantBudgets struct {
	mu     sync.Mutex
	limits map[string]TenantLimits
	state  map[string]*tenantUsage
}

// NewTenantBudgets returns an empty budget ledger (no limits set).
func NewTenantBudgets() *TenantBudgets {
	return &TenantBudgets{limits: make(map[string]TenantLimits), state: make(map[string]*tenantUsage)}
}

// SetLimits configures the caps for one tenant.
func (b *TenantBudgets) SetLimits(orgID string, l TenantLimits) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limits[orgID] = l
}

// Acquire atomically rolls the usage window (when expired) and takes one
// run slot; ErrBudgetExceeded when the run budget or concurrency cap is
// exhausted (nothing is consumed on denial).
func (b *TenantBudgets) Acquire(orgID string, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.state[orgID]
	if u == nil {
		u = &tenantUsage{}
		b.state[orgID] = u
	}
	l := b.limits[orgID]
	if l.Window > 0 && (u.windowStart.IsZero() || !now.Before(u.windowStart.Add(l.Window))) {
		u.windowStart = now
		u.runsUsed = 0
	}
	if l.MaxRunsPerWindow > 0 && u.runsUsed >= l.MaxRunsPerWindow {
		return fmt.Errorf("%w: tenant %s run budget", ErrBudgetExceeded, orgID)
	}
	if l.MaxConcurrentRuns > 0 && u.active >= l.MaxConcurrentRuns {
		return fmt.Errorf("%w: tenant %s concurrency cap", ErrBudgetExceeded, orgID)
	}
	u.runsUsed++
	u.active++
	return nil
}

// Release returns one concurrency slot for the tenant.
func (b *TenantBudgets) Release(orgID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u := b.state[orgID]; u != nil && u.active > 0 {
		u.active--
	}
}

// CleanupTask is one pending cleanup of approved write-test data (created
// only for ModeReleaseComparison write runs with a synthetic account).
type CleanupTask struct {
	ID               string
	OrgID            string
	ProjectID        string
	ScheduleID       string
	RunID            string
	SyntheticAccount string
	Description      string
	DueAt            time.Time
	Done             bool
	DoneAt           *time.Time
}

// CleanupRegistry tracks approved write-test data that must be cleaned up.
type CleanupRegistry struct {
	mu    sync.Mutex
	tasks map[string]CleanupTask
	order []string
	seq   atomic.Uint64
}

// NewCleanupRegistry returns an empty registry.
func NewCleanupRegistry() *CleanupRegistry {
	return &CleanupRegistry{tasks: make(map[string]CleanupTask)}
}

// Register stores a task, assigning its id and Done=false.
func (r *CleanupRegistry) Register(t CleanupTask) CleanupTask {
	t.ID = fmt.Sprintf("cleanup-%d", r.seq.Add(1))
	t.Done = false
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[t.ID] = t
	r.order = append(r.order, t.ID)
	return t
}

// Pending returns due, not-yet-done tasks in registration order.
func (r *CleanupRegistry) Pending(now time.Time) []CleanupTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]CleanupTask, 0, len(r.order))
	for _, id := range r.order {
		t := r.tasks[id]
		if !t.Done && !now.Before(t.DueAt) {
			out = append(out, t)
		}
	}
	return out
}

// MarkDone completes a task; unknown ids are ErrNotFound.
func (r *CleanupRegistry) MarkDone(id string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok {
		return ErrNotFound
	}
	t.Done = true
	a := at.UTC()
	t.DoneAt = &a
	r.tasks[id] = t
	return nil
}

// ProductionScheduler evaluates due schedules and emits RunRequests through
// the RunEnqueuer seam. Construct with NewProductionScheduler.
type ProductionScheduler struct {
	store   ScheduleStore
	envs    EnvironmentResolver
	enqueue RunEnqueuer
	kill    *KillSwitch
	limits  *TenantBudgets
	cleanup *CleanupRegistry

	now          func() time.Time
	cleanupGrace time.Duration
}

// ProductionSchedulerConfig carries optional scheduler knobs (zero values
// select the documented defaults).
type ProductionSchedulerConfig struct {
	// Now is the injectable clock (default time.Now).
	Now func() time.Time
	// CleanupGrace delays write-data cleanup due times (default 0 = due
	// immediately after registration).
	CleanupGrace time.Duration
}

// NewProductionScheduler wires the scheduler. store/envs/enqueue are
// required; kill/limits/cleanup default to fresh instances when nil.
func NewProductionScheduler(store ScheduleStore, envs EnvironmentResolver, enqueue RunEnqueuer, kill *KillSwitch, limits *TenantBudgets, cleanup *CleanupRegistry, cfg ProductionSchedulerConfig) (*ProductionScheduler, error) {
	if store == nil || envs == nil || enqueue == nil {
		return nil, fmt.Errorf("%w: scheduler requires store, environment resolver and enqueuer", ErrInvalid)
	}
	if kill == nil {
		kill = NewKillSwitch()
	}
	if limits == nil {
		limits = NewTenantBudgets()
	}
	if cleanup == nil {
		cleanup = NewCleanupRegistry()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &ProductionScheduler{
		store:        store,
		envs:         envs,
		enqueue:      enqueue,
		kill:         kill,
		limits:       limits,
		cleanup:      cleanup,
		now:          now,
		cleanupGrace: cfg.CleanupGrace,
	}, nil
}

var scheduleSeq atomic.Uint64

// newScheduleID derives a unique, deterministic-enough schedule id.
func newScheduleID(s ProductionSchedule, now time.Time) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.Join([]string{s.OrgID, s.ProjectID, s.JourneyID, s.EnvironmentID, now.String(), fmt.Sprint(scheduleSeq.Add(1))}, "\x1e")))
	return "sched-" + fmt.Sprintf("%x", h.Sum64())
}

// CreateSchedule validates and persists a production schedule, enforcing
// the verified-production-only + read-only-default policy at creation time:
// unverified targets are ErrTargetUnverified, non-production environments
// are ErrInvalid, and write journeys outside ModeReleaseComparison (or
// without a synthetic account) are ErrReadOnlyMode / ErrInvalid.
func (s *ProductionScheduler) CreateSchedule(ctx context.Context, sc ProductionSchedule) (ProductionSchedule, error) {
	if err := sc.Validate(); err != nil {
		return ProductionSchedule{}, err
	}
	tgt, env, err := s.envs.Resolve(ctx, sc.OrgID, sc.EnvironmentID)
	if err != nil {
		return ProductionSchedule{}, err
	}
	if !tgt.Verified {
		return ProductionSchedule{}, fmt.Errorf("%w: target %s", ErrTargetUnverified, tgt.ID)
	}
	if env.Kind != EnvProduction {
		return ProductionSchedule{}, fmt.Errorf("%w: schedules run against production environments only", ErrInvalid)
	}
	if sc.HasWriteOps {
		if env.Mode != ModeReleaseComparison {
			return ProductionSchedule{}, fmt.Errorf("%w: write journeys in production require %s mode", ErrReadOnlyMode, ModeReleaseComparison)
		}
		if strings.TrimSpace(sc.SyntheticAccount) == "" {
			return ProductionSchedule{}, fmt.Errorf("%w: write schedules require an isolated synthetic account", ErrInvalid)
		}
	}
	now := s.now().UTC()
	if strings.TrimSpace(sc.ID) == "" {
		sc.ID = newScheduleID(sc, now)
	}
	sc.CreatedAt, sc.UpdatedAt = now, now
	if sc.Interval > 0 {
		sc.NextRunAt = now.Add(sc.Interval)
	}
	if err := s.store.Put(sc); err != nil {
		return ProductionSchedule{}, err
	}
	return sc, nil
}

// PauseSchedule suspends a schedule (Tick/OnDeployment skip it).
func (s *ProductionScheduler) PauseSchedule(orgID, id string) error {
	sc, err := s.store.Get(orgID, id)
	if err != nil {
		return err
	}
	sc.Paused = true
	sc.UpdatedAt = s.now().UTC()
	return s.store.Put(sc)
}

// ResumeSchedule re-activates a schedule; the next run is due one full
// interval after the resume instant (fresh period, no immediate burst).
func (s *ProductionScheduler) ResumeSchedule(orgID, id string) error {
	sc, err := s.store.Get(orgID, id)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	sc.Paused = false
	if sc.Interval > 0 {
		sc.NextRunAt = now.Add(sc.Interval)
	}
	sc.UpdatedAt = now
	return s.store.Put(sc)
}

// CancelSchedule removes a schedule (recurring runs stop immediately).
func (s *ProductionScheduler) CancelSchedule(orgID, id string) error {
	return s.store.Delete(orgID, id)
}

// ReleaseRun returns the concurrency slot taken when the run was enqueued.
// The runtime must call this when a run reaches a terminal status.
func (s *ProductionScheduler) ReleaseRun(orgID string) {
	s.limits.Release(orgID)
}

// evalOutcome is the per-schedule evaluation result.
type evalOutcome struct {
	req         RunRequest
	release     func() // returns the acquired concurrency slot (noop when none)
	skipReason  string // "" = proceed to enqueue
	nextAllowed time.Time
}

// evaluate applies every server-side guard to one due schedule. Guards in
// order: kill switches, verified production target, read-only/write policy,
// window policy, tenant budget + concurrency. On success the concurrency
// slot is HELD (caller releases it on enqueue failure; the runtime owns it
// afterwards) and write runs get a cleanup task registered.
func (s *ProductionScheduler) evaluate(ctx context.Context, sc ProductionSchedule, now time.Time, trigger, appVersion string) (evalOutcome, error) {
	noop := func() {}
	if tripped, reason := s.kill.Tripped(sc.ProjectID); tripped {
		return evalOutcome{release: noop, skipReason: "kill_switch: " + reason}, nil
	}
	tgt, env, err := s.envs.Resolve(ctx, sc.OrgID, sc.EnvironmentID)
	if err != nil {
		return evalOutcome{release: noop}, err
	}
	if !tgt.Verified {
		return evalOutcome{release: noop, skipReason: "target_unverified"}, nil
	}
	if env.Kind != EnvProduction {
		return evalOutcome{release: noop, skipReason: "not_production"}, nil
	}
	// Read-only default, enforced server-side.
	readOnly := true
	if sc.HasWriteOps {
		if env.Mode != ModeReleaseComparison {
			return evalOutcome{release: noop, skipReason: "write_denied_readonly"}, nil
		}
		if strings.TrimSpace(sc.SyntheticAccount) == "" {
			return evalOutcome{release: noop}, fmt.Errorf("%w: write schedule %s lost its synthetic account", ErrInvalid, sc.ID)
		}
		readOnly = false
	} else if env.Mode == ModeProductionReadOnly {
		readOnly = true
	}
	blocked, nextAllowed, werr := evaluateWindows(env, now)
	if werr != nil {
		// Fail-closed: unparsable window policy blocks the schedule.
		return evalOutcome{release: noop, skipReason: "invalid_window_policy"}, nil
	}
	if blocked {
		return evalOutcome{release: noop, skipReason: "quiet_window", nextAllowed: nextAllowed}, nil
	}
	if err := s.limits.Acquire(sc.OrgID, now); err != nil {
		return evalOutcome{release: noop, skipReason: "tenant_budget"}, nil
	}
	req := RunRequest{
		ScheduleID:       sc.ID,
		OrgID:            sc.OrgID,
		ProjectID:        sc.ProjectID,
		JourneyID:        sc.JourneyID,
		JourneyVersion:   sc.JourneyVersion,
		EnvironmentID:    sc.EnvironmentID,
		PersonaID:        sc.PersonaID,
		PersonaVersion:   sc.PersonaVersion,
		AppVersion:       appVersion,
		ReadOnly:         readOnly,
		SyntheticAccount: sc.SyntheticAccount,
		Trigger:          trigger,
		DueAt:            now,
	}
	if !readOnly && s.cleanup != nil {
		s.cleanup.Register(CleanupTask{
			OrgID:            sc.OrgID,
			ProjectID:        sc.ProjectID,
			ScheduleID:       sc.ID,
			SyntheticAccount: sc.SyntheticAccount,
			Description:      fmt.Sprintf("clean write-test data for journey %s (schedule %s)", sc.JourneyID, sc.ID),
			DueAt:            now.Add(s.cleanupGrace),
		})
	}
	return evalOutcome{req: req, release: func() { s.limits.Release(sc.OrgID) }}, nil
}

// Tick evaluates every due time-based schedule in the org and enqueues the
// accepted ones. Per-schedule outcomes: skip reasons advance NextRunAt (to
// nextAllowed for window blocks, else one interval — never a hot loop);
// enqueue failures consume retry attempts (TriggerRetry on the next due);
// successes reset the retry cycle, advance NextRunAt by the interval and
// keep the concurrency slot held until ReleaseRun. Returned error joins
// per-schedule failures.
func (s *ProductionScheduler) Tick(ctx context.Context, orgID string) ([]RunRequest, error) {
	scheds, err := s.store.List(orgID, "")
	if err != nil {
		return nil, err
	}
	var (
		out  []RunRequest
		errs []error
	)
	now := s.now().UTC()
	for _, sc := range scheds {
		if sc.Paused || sc.Interval <= 0 || now.Before(sc.NextRunAt) {
			continue
		}
		trigger := TriggerRecurring
		if sc.Attempts > 0 {
			trigger = TriggerRetry
		}
		oc, err := s.evaluate(ctx, sc, now, trigger, "")
		if err != nil {
			errs = append(errs, fmt.Errorf("schedule %s: %w", sc.ID, err))
			continue
		}
		if oc.skipReason != "" {
			sc.NextRunAt = now.Add(sc.Interval)
			if !oc.nextAllowed.IsZero() && oc.nextAllowed.After(sc.NextRunAt) {
				sc.NextRunAt = oc.nextAllowed
			}
			sc.UpdatedAt = now
			if perr := s.store.Put(sc); perr != nil {
				errs = append(errs, perr)
			}
			continue
		}
		if _, err := s.enqueue.Enqueue(ctx, oc.req); err != nil {
			oc.release()
			errs = append(errs, fmt.Errorf("schedule %s enqueue: %w", sc.ID, err))
			s.applyRetry(&sc, now)
			if perr := s.store.Put(sc); perr != nil {
				errs = append(errs, perr)
			}
			continue
		}
		// A run was finally enqueued: the retry cycle is complete.
		sc.Attempts = 0
		sc.NextRunAt = now.Add(sc.Interval)
		sc.UpdatedAt = now
		if perr := s.store.Put(sc); perr != nil {
			errs = append(errs, perr)
			oc.release()
			continue
		}
		out = append(out, oc.req)
	}
	return out, errors.Join(errs...)
}

// OnDeployment fires the AfterDeployment hooks: every non-paused schedule
// in the org/project whose trigger matches appVersion (exact match, or ""
// wildcard) is evaluated with the same server-side guards as Tick. Event
// runs do not touch NextRunAt and are not retried on enqueue failure (the
// next deployment re-triggers).
func (s *ProductionScheduler) OnDeployment(ctx context.Context, orgID, projectID, appVersion string) ([]RunRequest, error) {
	scheds, err := s.store.List(orgID, projectID)
	if err != nil {
		return nil, err
	}
	var (
		out  []RunRequest
		errs []error
	)
	now := s.now().UTC()
	for _, sc := range scheds {
		if sc.Paused || sc.OnDeployment == nil {
			continue
		}
		if sc.OnDeployment.AppVersion != "" && sc.OnDeployment.AppVersion != appVersion {
			continue
		}
		oc, err := s.evaluate(ctx, sc, now, TriggerAfterDeployment, appVersion)
		if err != nil {
			errs = append(errs, fmt.Errorf("schedule %s: %w", sc.ID, err))
			continue
		}
		if oc.skipReason != "" {
			continue
		}
		if _, err := s.enqueue.Enqueue(ctx, oc.req); err != nil {
			oc.release()
			errs = append(errs, fmt.Errorf("schedule %s enqueue: %w", sc.ID, err))
			continue
		}
		out = append(out, oc.req)
	}
	return out, errors.Join(errs...)
}

// ReportRun feeds a terminal run status back into the schedule's retry
// policy: failed/inconclusive runs are re-due with capped exponential
// backoff while retry attempts remain (attempts reset afterwards);
// successes and exhausted policies reset the attempt counter.
func (s *ProductionScheduler) ReportRun(orgID, scheduleID string, status RunStatus, at time.Time) error {
	sc, err := s.store.Get(orgID, scheduleID)
	if err != nil {
		return err
	}
	at = at.UTC()
	if status == RunFailed || status == RunInconclusive {
		max := sc.Retry.MaxAttempts
		if max <= 0 {
			max = 1
		}
		if sc.Attempts+1 < max {
			sc.Attempts++
			sc.NextRunAt = at.Add(retryDelay(sc.Retry, sc.Attempts))
		} else {
			sc.Attempts = 0
		}
	} else {
		sc.Attempts = 0
	}
	sc.UpdatedAt = at
	return s.store.Put(sc)
}

// applyRetry schedules the next attempt after an enqueue failure (or falls
// back to the regular interval when the policy is exhausted).
func (s *ProductionScheduler) applyRetry(sc *ProductionSchedule, now time.Time) {
	max := sc.Retry.MaxAttempts
	if max <= 0 {
		max = 1
	}
	if sc.Attempts+1 < max {
		sc.Attempts++
		sc.NextRunAt = now.Add(retryDelay(sc.Retry, sc.Attempts))
		return
	}
	sc.Attempts = 0
	sc.NextRunAt = now.Add(sc.Interval)
}

// retryDelay computes the capped exponential backoff for attempt n
// (1-based): base * 2^(n-1), clamped to MaxBackoff. Defaults: base 30s,
// cap 10m.
func retryDelay(r RetryPolicy, attempt int) time.Duration {
	base := r.Backoff
	if base <= 0 {
		base = 30 * time.Second
	}
	max := r.MaxBackoff
	if max <= 0 {
		max = 10 * time.Minute
	}
	d := base
	for i := 1; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}
