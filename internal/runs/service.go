package runs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/observability"
	"agentos/internal/streaming"
)

type RunStatus string

const (
	StatusQueued    RunStatus = "QUEUED"
	StatusRunning   RunStatus = "RUNNING"
	StatusCompleted RunStatus = "COMPLETED"
	StatusFailed    RunStatus = "FAILED"
)

type Run struct {
	ID             string
	OrganizationID string
	AgentID        string
	Input          string
	Output         string
	Status         RunStatus
	// TotalCostCents is the summed cost of the run's costed steps
	// (USD cents; 0 when no pricing data was available). Serialized
	// additively as snake_case; legacy PascalCase fields are unchanged.
	TotalCostCents float64 `json:"total_cost_cents"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Step captures one execution trace entry for a run (run_steps table).
type Step struct {
	ID          string
	RunID       string
	StepType    string
	Status      string
	InputMeta   map[string]any
	OutputMeta  map[string]any
	Error       string
	TokenUsage  map[string]any
	Cost        float64
	StartedAt   time.Time
	CompletedAt time.Time
	CreatedAt   time.Time
}

var ErrRunNotFound = errors.New("run not found")

// Store abstracts durable run storage. Every tenant-facing query is guarded by
// organization_id; GetRunByID/UpdateStatusCtx with an empty orgID are trusted
// internal paths for workers which resolve and re-apply the tenant scope from
// the run row itself.
type Store interface {
	// CreateRun inserts the run row with its organization_id scope.
	CreateRun(ctx context.Context, run *Run) error
	// GetRun fetches a run strictly within one tenant (organization_id guard).
	GetRun(ctx context.Context, orgID, id string) (*Run, error)
	// GetRunByID fetches by primary key (trusted internal worker path).
	GetRunByID(ctx context.Context, id string) (*Run, error)
	// ListRuns returns the runs of exactly one tenant (organization_id guard).
	ListRuns(ctx context.Context, orgID string) ([]*Run, error)
	// UpdateRunStatus transitions a run status within one tenant
	// (organization_id guard).
	UpdateRunStatus(ctx context.Context, orgID, id string, status RunStatus, output string) error
	// InsertStep appends one run_steps row within one tenant
	// (organization_id guard enforced via the runs join/exists check).
	// Implementations must ALSO bump the run's durable total
	// (runs.cost_cents += step.Cost) atomically in the same statement;
	// the service never adjusts the total itself in store mode.
	InsertStep(ctx context.Context, orgID string, step *Step) error
	// ListSteps returns the steps of one run within one tenant
	// (organization_id guard via the runs join).
	ListSteps(ctx context.Context, orgID, runID string) ([]*Step, error)
	// AggregateCosts sums runs.cost_cents for one tenant over the
	// half-open [from, to) window grouped by day, agent or model
	// (GET /v1/usage/costs; see cost.go).
	AggregateCosts(ctx context.Context, orgID string, from, to time.Time, groupBy CostGroupBy) ([]CostBucket, error)
}

type Service struct {
	mu       sync.Mutex
	runs     map[string]*Run
	steps    map[string][]*Step
	streamer *streaming.Service
	store    Store
	// metrics is optional (nil-safe DI, issue #12): when wired it feeds
	// the agentos_runs_total counter on terminal transitions. See
	// metrics.go for the counting rules and SetMetrics.
	metrics *observability.Metrics
	// terminalCounted dedupes the runs counter per run ID so queue
	// retries, replays and idempotent control transitions never double
	// count. Guarded by mu.
	terminalCounted map[string]bool
}

func NewService() *Service {
	return &Service{runs: make(map[string]*Run), steps: make(map[string][]*Step)}
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store; the in-memory maps act as a write-through cache for legacy callers.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	return s
}

func (s *Service) SetStore(store Store) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
}

func (s *Service) SetStreamer(st *streaming.Service) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamer = st
}

// Create is the legacy context-free entry point; see CreateRunCtx.
func (s *Service) Create(orgID, agentID, input string) (*Run, error) {
	return s.CreateRunCtx(context.Background(), orgID, agentID, input)
}

// CreateRunCtx persists a new QUEUED run for one tenant (organization_id scope).
func (s *Service) CreateRunCtx(ctx context.Context, orgID, agentID, input string) (*Run, error) {
	if orgID == "" || agentID == "" {
		return nil, errors.New("organization and agent id required")
	}
	now := time.Now().UTC()
	run := &Run{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		AgentID:        agentID,
		Input:          input,
		Status:         StatusQueued,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if s.store != nil {
		if err := s.store.CreateRun(ctx, run); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.runs[run.ID] = run
	s.mu.Unlock()
	return run, nil
}

// Get is the legacy context-free primary-key lookup used by trusted internal
// workers; tenant-scoped API callers must use GetRunCtx.
func (s *Service) Get(id string) (*Run, bool) {
	run, err := s.GetRunByIDCtx(context.Background(), id)
	if err != nil {
		return nil, false
	}
	return run, true
}

// GetRunByIDCtx resolves a run by primary key (trusted internal path).
func (s *Service) GetRunByIDCtx(ctx context.Context, id string) (*Run, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrRunNotFound
	}
	if s.store != nil {
		return s.store.GetRunByID(ctx, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	if !ok {
		return nil, ErrRunNotFound
	}
	return run, nil
}

// GetRunCtx resolves a run strictly within one tenant (organization_id guard) -
// the API-facing path.
func (s *Service) GetRunCtx(ctx context.Context, orgID, id string) (*Run, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrRunNotFound
	}
	if s.store != nil {
		return s.store.GetRun(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	if !ok || run.OrganizationID != orgID {
		return nil, ErrRunNotFound
	}
	return run, nil
}

// List is the legacy in-memory listing; see ListRunsCtx.
func (s *Service) List() []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Run, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, *r)
	}
	return out
}

// ListRunsCtx returns all runs of exactly one tenant (organization_id guard).
func (s *Service) ListRunsCtx(ctx context.Context, orgID string) ([]*Run, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if s.store != nil {
		return s.store.ListRuns(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Run, 0)
	for _, r := range s.runs {
		if r.OrganizationID == orgID {
			out = append(out, r)
		}
	}
	return out, nil
}

// UpdateStatus is the legacy context-free entry point used by workers; the
// tenant is resolved from the run row and the update stays organization-scoped.
func (s *Service) UpdateStatus(id string, status RunStatus, output string) error {
	return s.UpdateStatusCtx(context.Background(), "", id, status, output)
}

// UpdateStatusCtx transitions a run status. When orgID is empty (trusted
// internal worker path) it is resolved from the run row first so the update is
// always executed with the organization_id guard. Streams a status event.
func (s *Service) UpdateStatusCtx(ctx context.Context, orgID, id string, status RunStatus, output string) error {
	if strings.TrimSpace(id) == "" {
		return ErrRunNotFound
	}
	if s.store != nil {
		if strings.TrimSpace(orgID) == "" {
			run, err := s.store.GetRunByID(ctx, id)
			if err != nil {
				return err
			}
			orgID = run.OrganizationID
		}
		if err := s.store.UpdateRunStatus(ctx, orgID, id, status, output); err != nil {
			return err
		}
	}
	s.mu.Lock()
	run, ok := s.runs[id]
	if !ok {
		s.mu.Unlock()
		if s.store == nil {
			// Trusted worker path (issue #12): the run row lives in
			// another process or was pruned, but the terminal
			// outcome observed here is still this process's point
			// of truth - count it, preserving the legacy
			// ErrRunNotFound contract. No event is published
			// (unchanged legacy behavior).
			s.countTerminalRun(id, status)
			return ErrRunNotFound
		}
		// Store-backed update already succeeded above; the memory
		// cache simply does not know this run.
		s.countTerminalRun(id, status)
		s.publishStatus(id, status, output)
		return nil
	}
	run.Status = status
	if output != "" {
		run.Output = output
	}
	run.UpdatedAt = time.Now().UTC()
	runID := run.ID
	s.mu.Unlock()
	s.countTerminalRun(runID, status)
	s.publishStatus(runID, status, output)
	return nil
}

func (s *Service) publishStatus(runID string, status RunStatus, output string) {
	s.mu.Lock()
	streamer := s.streamer
	s.mu.Unlock()
	if streamer != nil {
		payload := map[string]any{"status": string(status)}
		if output != "" {
			payload["output"] = output
		}
		streamer.Publish(runID, "status", "status.changed", payload)
	}
}

// RecordStep appends one execution trace entry (run_steps table) for a run of
// one tenant. When no store is configured the step is kept in memory so the
// API still serves traces in zero-infrastructure mode.
func (s *Service) RecordStep(ctx context.Context, orgID, runID string, step *Step) error {
	if step == nil {
		return errors.New("step is required")
	}
	if strings.TrimSpace(runID) == "" {
		return ErrRunNotFound
	}
	if strings.TrimSpace(step.StepType) == "" {
		return errors.New("step type is required")
	}
	if strings.TrimSpace(step.Status) == "" {
		step.Status = "PENDING"
	}
	step.RunID = runID
	if step.ID == "" {
		step.ID = uuid.NewString()
	}
	if step.CreatedAt.IsZero() {
		step.CreatedAt = time.Now().UTC()
	}

	if s.store != nil {
		if strings.TrimSpace(orgID) == "" {
			run, err := s.store.GetRunByID(ctx, runID)
			if err != nil {
				return err
			}
			orgID = run.OrganizationID
		}
		if err := s.store.InsertStep(ctx, orgID, step); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if run, ok := s.runs[runID]; ok && orgID != "" && run.OrganizationID != orgID {
		s.mu.Unlock()
		return ErrRunNotFound
	}
	s.steps[runID] = append(s.steps[runID], step)
	// The durable runs.cost_cents total is owned by the Store: pgStore
	// bumps it in the same atomic statement as the step insert (see
	// sqlInsertRunStep) and Store implementations are required to do the
	// same. Only in zero-infrastructure mode (no store) is the service
	// the store of record and bumps the total itself — bumping here in
	// store mode would double-count against the store-side total.
	if s.store == nil {
		if run, ok := s.runs[runID]; ok {
			run.TotalCostCents += step.Cost
		}
	}
	streamer := s.streamer
	s.mu.Unlock()

	if streamer != nil {
		streamer.Publish(runID, "step", "step.recorded", map[string]any{
			"step_id":   step.ID,
			"step_type": step.StepType,
			"status":    step.Status,
			"cost":      step.Cost,
		})
	}
	return nil
}

// Steps returns the recorded trace of one run within one tenant
// (organization_id guard).
func (s *Service) Steps(ctx context.Context, orgID, runID string) ([]*Step, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(runID) == "" {
		return nil, ErrRunNotFound
	}
	if s.store != nil {
		return s.store.ListSteps(ctx, orgID, runID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run, ok := s.runs[runID]; ok && run.OrganizationID != orgID {
		return nil, ErrRunNotFound
	}
	out := make([]*Step, len(s.steps[runID]))
	copy(out, s.steps[runID])
	return out, nil
}
