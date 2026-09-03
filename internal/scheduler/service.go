package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Schedule kinds and statuses (wave-2 contract, track 2-f).
const (
	KindOnce      = "once"
	KindRecurring = "recurring"
	KindCron      = "cron"

	// StatusActive is the normal runnable state; StatusPaused suspends firing
	// without deleting the schedule. StatusCompleted is the terminal state of
	// a once schedule after its single firing ("mark completed for once" in
	// the worker contract); it never returns to active.
	StatusActive    = "active"
	StatusPaused    = "paused"
	StatusCompleted = "completed"

	// MinIntervalSeconds is the smallest allowed recurring interval (contract:
	// "recurring requires interval_seconds >= 60").
	MinIntervalSeconds = 60

	// DefaultTimezone is applied when a create request omits timezone for
	// once/recurring kinds. Cron kind REQUIRES an explicit IANA timezone.
	DefaultTimezone = "UTC"
)

var (
	ErrScheduleNotFound   = errors.New("schedule not found")
	ErrScheduleNotActive  = errors.New("schedule is not active")
	ErrScheduleNotPaused  = errors.New("schedule is not paused")
	ErrScheduleCompleted  = errors.New("schedule is completed")
	ErrInvalidKind        = errors.New("kind must be one of: once, recurring, cron")
	ErrAgentRequired      = errors.New("agent_id is required")
	ErrRunAtRequired      = errors.New("run_at is required for once schedules")
	ErrIntervalTooSmall   = errors.New("interval_seconds must be >= 60 for recurring schedules")
	ErrCronExprRequired   = errors.New("cron_expr is required for cron schedules")
	ErrTimezoneRequired   = errors.New("timezone is required for cron schedules (IANA name, e.g. UTC)")
	ErrCronNeverFires     = errors.New("cron expression never fires within 5 years")
	ErrInvalidRunAt       = errors.New("run_at must be an RFC3339 timestamp")
	ErrInvalidTimezone    = errors.New("timezone must be a valid IANA location name")
	ErrInvalidInterval    = errors.New("interval_seconds must be a positive integer")
	ErrOrgIDRequired      = errors.New("organization_id is required")
	ErrScheduleIDRequired = errors.New("schedule id is required")
)

// ValidationError describes a field-level validation failure; handlers map it
// to HTTP 400 with code VALIDATION_ERROR.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Message)
}

// Schedule is one tenant-scoped trigger definition. Timestamps are UTC
// instants; RunAt/NextRunAt/LastFiredAt are pointers because "unset" is
// meaningful (nil next_run_at = nothing pending).
type Schedule struct {
	ID              string
	OrganizationID  string
	AgentID         string
	Input           string
	Kind            string // once | recurring | cron
	RunAt           *time.Time
	IntervalSeconds int
	CronExpr        string
	Timezone        string
	Status          string // active | paused | completed
	NextRunAt       *time.Time
	LastRunID       string
	LastFiredAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CreateInput is the validated-on-arrival create request (raw client values).
type CreateInput struct {
	AgentID         string
	Input           string
	Kind            string
	RunAt           string // RFC3339, required for kind=once
	IntervalSeconds int    // required >= 60 for kind=recurring
	CronExpr        string // required for kind=cron
	Timezone        string // required IANA name for kind=cron
}

// Store abstracts durable schedule storage. Every tenant-facing query is
// guarded by organization_id; GetByID/Due/ClaimForFire/SetLastRun are trusted
// internal paths used by the scheduler worker across all tenants.
type Store interface {
	// Create inserts a schedule row (organization_id scope carried on the row).
	Create(ctx context.Context, s *Schedule) error
	// Get fetches one schedule strictly within one tenant (org guard).
	Get(ctx context.Context, orgID, id string) (*Schedule, error)
	// GetByID resolves by primary key (trusted internal worker path).
	GetByID(ctx context.Context, id string) (*Schedule, error)
	// List returns all schedules of exactly one tenant (org guard).
	List(ctx context.Context, orgID string) ([]*Schedule, error)
	// Due returns active schedules whose next_run_at <= now (trusted worker
	// path, all tenants; callers act on the row's own organization_id).
	Due(ctx context.Context, now time.Time) ([]*Schedule, error)
	// ClaimForFire atomically consumes a due firing slot: it only updates the
	// row when status='active' AND next_run_at <= firedAt, preventing
	// double-fires across worker restarts and concurrent workers. nextRunAt
	// nil with status 'completed' marks a once schedule terminal.
	ClaimForFire(ctx context.Context, id string, firedAt time.Time, newStatus string, nextRunAt *time.Time) (bool, error)
	// SetLastRun records the run created for the claimed firing.
	SetLastRun(ctx context.Context, id, runID string) error
	// UpdateStatus transitions status within one tenant (org guard).
	UpdateStatus(ctx context.Context, orgID, id string, status string) error
	// Delete removes one schedule within one tenant (org guard).
	Delete(ctx context.Context, orgID, id string) error
}

// Service is the dual-mode scheduler service: NewService() keeps everything in
// memory (zero-infrastructure mode); NewServiceWithStore(NewPostgresStore(db))
// delegates to Postgres as the source of truth.
type Service struct {
	mu        sync.Mutex
	schedules map[string]*Schedule
	store     Store
	clock     Clock
}

// NewService returns the in-memory scheduler service.
func NewService() *Service {
	return &Service{
		schedules: make(map[string]*Schedule),
		clock:     realClock{},
	}
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store (Postgres in production, sqlmock in tests).
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	return s
}

// WithClock injects a fake clock (tests only).
func (s *Service) WithClock(c Clock) *Service {
	if s == nil || c == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = c
	return s
}

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now().UTC()
	}
	return time.Now().UTC()
}

// resolveTimezone validates an optional IANA timezone name, applying the
// default for once/recurring kinds and requiring a valid name for cron.
func resolveTimezone(kind, tz string) (string, *time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		if kind == KindCron {
			return "", nil, ErrTimezoneRequired
		}
		tz = DefaultTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return "", nil, ErrInvalidTimezone
	}
	return tz, loc, nil
}

// computeNextRunAt derives the first pending run time for a new schedule.
func computeNextRunAt(kind string, in CreateInput, runAt *time.Time, loc *time.Location, now time.Time) (*time.Time, error) {
	switch kind {
	case KindOnce:
		t := runAt.UTC()
		return &t, nil
	case KindRecurring:
		t := now.UTC().Add(time.Duration(in.IntervalSeconds) * time.Second)
		return &t, nil
	case KindCron:
		t, ok := NextCronTime(in.CronExpr, loc, now)
		if !ok {
			return nil, ErrCronNeverFires
		}
		return &t, nil
	}
	return nil, ErrInvalidKind
}

// validateCreate applies the contract validation rules:
//   - once requires run_at (RFC3339);
//   - recurring requires interval_seconds >= 60;
//   - cron requires a valid 5-field expression + an IANA timezone.
func validateCreate(in CreateInput) (string, *time.Time, *time.Location, error) {
	if strings.TrimSpace(in.AgentID) == "" {
		return "", nil, nil, ErrAgentRequired
	}
	switch in.Kind {
	case KindOnce:
		if strings.TrimSpace(in.RunAt) == "" {
			return "", nil, nil, ErrRunAtRequired
		}
		runAt, err := time.Parse(time.RFC3339, in.RunAt)
		if err != nil {
			return "", nil, nil, &ValidationError{Field: "run_at", Message: ErrInvalidRunAt.Error()}
		}
		tz, loc, err := resolveTimezone(in.Kind, in.Timezone)
		if err != nil {
			return "", nil, nil, err
		}
		return tz, &runAt, loc, nil
	case KindRecurring:
		if in.IntervalSeconds < MinIntervalSeconds {
			return "", nil, nil, ErrIntervalTooSmall
		}
		tz, loc, err := resolveTimezone(in.Kind, in.Timezone)
		if err != nil {
			return "", nil, nil, err
		}
		return tz, nil, loc, nil
	case KindCron:
		if strings.TrimSpace(in.CronExpr) == "" {
			return "", nil, nil, ErrCronExprRequired
		}
		if err := ParseCron(in.CronExpr); err != nil {
			return "", nil, nil, &ValidationError{Field: "cron_expr", Message: err.Error()}
		}
		tz, loc, err := resolveTimezone(in.Kind, in.Timezone)
		if err != nil {
			return "", nil, nil, err
		}
		return tz, nil, loc, nil
	default:
		return "", nil, nil, ErrInvalidKind
	}
}

// Create validates the input and persists a new active schedule with a
// computed next_run_at.
func (s *Service) Create(ctx context.Context, orgID string, in CreateInput) (*Schedule, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgIDRequired
	}
	tz, runAt, loc, err := validateCreate(in)
	if err != nil {
		return nil, err
	}
	now := s.now()
	nextRunAt, err := computeNextRunAt(in.Kind, in, runAt, loc, now)
	if err != nil {
		return nil, err
	}
	schedule := &Schedule{
		ID:              uuid.NewString(),
		OrganizationID:  orgID,
		AgentID:         strings.TrimSpace(in.AgentID),
		Input:           in.Input,
		Kind:            in.Kind,
		RunAt:           runAt,
		IntervalSeconds: in.IntervalSeconds,
		CronExpr:        strings.TrimSpace(in.CronExpr),
		Timezone:        tz,
		Status:          StatusActive,
		NextRunAt:       nextRunAt,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if s.store != nil {
		if err := s.store.Create(ctx, schedule); err != nil {
			return nil, err
		}
		return schedule, nil
	}
	s.mu.Lock()
	s.schedules[schedule.ID] = schedule
	s.mu.Unlock()
	return schedule, nil
}

// Get resolves one schedule strictly within one tenant.
func (s *Service) Get(ctx context.Context, orgID, id string) (*Schedule, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrScheduleNotFound
	}
	if s.store != nil {
		return s.store.Get(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.schedules[id]
	if !ok || sched.OrganizationID != orgID {
		return nil, ErrScheduleNotFound
	}
	return sched, nil
}

// GetByID resolves by primary key (trusted internal worker path).
func (s *Service) GetByID(ctx context.Context, id string) (*Schedule, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrScheduleNotFound
	}
	if s.store != nil {
		return s.store.GetByID(ctx, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.schedules[id]
	if !ok {
		return nil, ErrScheduleNotFound
	}
	return sched, nil
}

// List returns all schedules of exactly one tenant, oldest first.
func (s *Service) List(ctx context.Context, orgID string) ([]*Schedule, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgIDRequired
	}
	if s.store != nil {
		return s.store.List(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Schedule, 0)
	for _, sched := range s.schedules {
		if sched.OrganizationID == orgID {
			out = append(out, sched)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Due returns active schedules with next_run_at <= now (trusted worker path,
// all tenants). This is the polling entry point for scheduler.NewWorker.
func (s *Service) Due(ctx context.Context, now time.Time) ([]*Schedule, error) {
	if s.store != nil {
		return s.store.Due(ctx, now.UTC())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Schedule, 0)
	for _, sched := range s.schedules {
		if sched.Status == StatusActive && sched.NextRunAt != nil && !sched.NextRunAt.After(now) {
			out = append(out, sched)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextRunAt.Before(*out[j].NextRunAt) })
	return out, nil
}

// ClaimForFire atomically consumes a due firing slot and advances the
// schedule. It is the catch-up protection primitive: the conditional guard
// (status active + next_run_at still due) means a schedule fires AT MOST ONCE
// per due instant, even across worker restarts or concurrent workers. For
// kind=once the schedule is marked completed; recurring advances to
// now+interval (never a burst of catch-up fires) and cron advances to the
// next matching wall-clock minute in its timezone.
func (s *Service) ClaimForFire(ctx context.Context, id string, now time.Time) (*Schedule, bool, error) {
	sched, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if sched.Status != StatusActive {
		return nil, false, nil
	}
	if sched.NextRunAt == nil || sched.NextRunAt.After(now) {
		return nil, false, nil
	}
	firedAt := now.UTC()
	newStatus := StatusActive
	var nextRunAt *time.Time
	switch sched.Kind {
	case KindOnce:
		newStatus = StatusCompleted
	case KindRecurring:
		t := firedAt.Add(time.Duration(sched.IntervalSeconds) * time.Second)
		nextRunAt = &t
	case KindCron:
		loc, lerr := time.LoadLocation(sched.Timezone)
		if lerr != nil {
			loc = time.UTC
		}
		if t, ok := NextCronTime(sched.CronExpr, loc, firedAt); ok {
			nt := t
			nextRunAt = &nt
		} else {
			// Expression can never fire again (e.g. Feb 31): complete it.
			newStatus = StatusCompleted
		}
	default:
		return nil, false, fmt.Errorf("schedule %s has unknown kind %q", sched.ID, sched.Kind)
	}

	ok, err := s.claimStore(ctx, sched.ID, firedAt, newStatus, nextRunAt)
	if err != nil || !ok {
		return nil, ok, err
	}
	claimed, err := s.GetByID(ctx, sched.ID)
	if err != nil {
		return nil, true, err
	}
	return claimed, true, nil
}

// claimStore applies the claim to the active backend (store first, memory
// under the service mutex).
func (s *Service) claimStore(ctx context.Context, id string, firedAt time.Time, newStatus string, nextRunAt *time.Time) (bool, error) {
	if s.store != nil {
		return s.store.ClaimForFire(ctx, id, firedAt, newStatus, nextRunAt)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.schedules[id]
	if !ok || sched.Status != StatusActive || sched.NextRunAt == nil || sched.NextRunAt.After(firedAt) {
		return false, nil
	}
	sched.Status = newStatus
	sched.NextRunAt = nextRunAt
	sched.LastFiredAt = &firedAt
	sched.UpdatedAt = firedAt
	return true, nil
}

// SetLastRun records the run created for a claimed firing (trusted path).
func (s *Service) SetLastRun(ctx context.Context, id, runID string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(runID) == "" {
		return ErrScheduleIDRequired
	}
	if s.store != nil {
		return s.store.SetLastRun(ctx, id, runID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.schedules[id]
	if !ok {
		return ErrScheduleNotFound
	}
	sched.LastRunID = runID
	return nil
}

// Pause suspends a schedule within one tenant (active -> paused).
func (s *Service) Pause(ctx context.Context, orgID, id string) (*Schedule, error) {
	return s.transition(ctx, orgID, id, StatusActive, StatusPaused, ErrScheduleNotActive)
}

// Resume reactivates a paused schedule within one tenant (paused -> active).
// next_run_at is preserved: an overdue schedule fires on the next poll.
func (s *Service) Resume(ctx context.Context, orgID, id string) (*Schedule, error) {
	return s.transition(ctx, orgID, id, StatusPaused, StatusActive, ErrScheduleNotPaused)
}

func (s *Service) transition(ctx context.Context, orgID, id, from, to string, notInState error) (*Schedule, error) {
	sched, err := s.Get(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if sched.Status == StatusCompleted {
		return nil, ErrScheduleCompleted
	}
	if sched.Status != from {
		return nil, notInState
	}
	if s.store != nil {
		if err := s.store.UpdateStatus(ctx, orgID, id, to); err != nil {
			return nil, err
		}
		return s.Get(ctx, orgID, id)
	}
	s.mu.Lock()
	sched.Status = to
	sched.UpdatedAt = s.now()
	s.mu.Unlock()
	return sched, nil
}

// Delete removes one schedule within one tenant (idempotent for the caller:
// deleting an unknown/foreign id surfaces as ErrScheduleNotFound).
func (s *Service) Delete(ctx context.Context, orgID, id string) error {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return ErrScheduleNotFound
	}
	if s.store != nil {
		return s.store.Delete(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.schedules[id]
	if !ok || sched.OrganizationID != orgID {
		return ErrScheduleNotFound
	}
	delete(s.schedules, id)
	return nil
}
