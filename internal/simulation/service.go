package simulation

// service.go — issue #133 service: org-scoped simulation records + async
// execution on top of the Runner.
//
// DUAL-MODE NOTE (documented scope decision, mirrors internal/orchestration):
// the store is IN-MEMORY in BOTH API modes this round — no simulation
// migration is assigned. The durable artifact of a simulation is its
// SimulationResult, which callers export/render through the HTTP surface;
// persistence is a follow-up track with its own migration number.
//
// Async contract (mirrors cmd/api/orchestration.go): POST /simulations
// validates SYNCHRONOUSLY on the request path (fail fast, 422) and returns
// 202 with a queued record; execution happens on a background goroutine
// that is DETACHED from the request context (context.WithoutCancel — a
// completed HTTP request must never abort a running simulation). Status
// transitions: queued -> running -> completed|failed.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Simulation is one org-scoped simulation record (the HTTP resource).
type Simulation struct {
	ID        string            `json:"id"`
	OrgID     string            `json:"org_id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Config    SimConfig         `json:"config"`
	Result    *SimulationResult `json:"result,omitempty"`
	Error     string            `json:"error,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Service owns simulation records and drives runs through a Runner.
type Service struct {
	mu     sync.Mutex
	runner *Runner
	sims   map[string]*Simulation // id -> record (all orgs; access is org-guarded)
	order  []string               // insertion order for stable listing
	nowID  func() string
}

// ServiceOption customizes a Service.
type ServiceOption func(*Service)

// WithRunner overrides the runner (custom registry/clock for tests).
func WithRunner(r *Runner) ServiceOption {
	return func(s *Service) { s.runner = r }
}

// WithIDGenerator overrides the id source (deterministic tests).
func WithIDGenerator(f func() string) ServiceOption {
	return func(s *Service) { s.nowID = f }
}

// NewService returns the in-memory service (both API modes this round).
func NewService(opts ...ServiceOption) *Service {
	s := &Service{
		runner: NewRunner(),
		sims:   make(map[string]*Simulation),
		nowID:  func() string { return uuid.NewString() },
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Create validates cfg, stores a queued record, and starts the async run.
// The returned record is a SNAPSHOT copy (status "queued" at minimum).
func (s *Service) Create(ctx context.Context, orgID string, cfg SimConfig) (*Simulation, error) {
	if orgID == "" {
		return nil, fmt.Errorf("%w: organization scope is required", ErrInvalid)
	}
	cfg.OrgID = orgID // tenant scope ALWAYS from the claims (never client-supplied)
	if err := ValidateConfig(cfg, s.runner.Registry()); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sim := &Simulation{
		ID:        s.nowID(),
		OrgID:     orgID,
		Name:      cfg.Name,
		Status:    StatusQueued,
		Config:    cfg,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.mu.Lock()
	s.sims[sim.ID] = sim
	s.order = append(s.order, sim.ID)
	s.mu.Unlock()

	// Detached execution: the request ctx dies with the HTTP response; the
	// simulation must not (context.WithoutCancel keeps values, drops cancel).
	go s.run(context.WithoutCancel(ctx), sim.ID)
	return s.Get(orgID, sim.ID)
}

// run executes the simulation and lands the terminal status. Best-effort:
// an unknown id (deleted mid-run) simply aborts.
func (s *Service) run(ctx context.Context, id string) {
	s.mu.Lock()
	sim := s.sims[id]
	if sim == nil {
		s.mu.Unlock()
		return
	}
	sim.Status = StatusRunning
	sim.UpdatedAt = time.Now().UTC()
	cfg := sim.Config
	s.mu.Unlock()

	result, err := s.runner.RunSimulation(ctx, cfg)

	s.mu.Lock()
	defer s.mu.Unlock()
	if sim := s.sims[id]; sim != nil {
		if err != nil {
			sim.Status = StatusFailed
			sim.Error = err.Error()
		} else {
			sim.Status = StatusComplete
			sim.Result = result
		}
		sim.UpdatedAt = time.Now().UTC()
	}
}

// Get returns a COPY of the record; unknown or foreign-org ids are the same
// ErrNotFound (no existence leak across tenants).
func (s *Service) Get(orgID, id string) (*Simulation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sim, ok := s.sims[id]
	if !ok || sim.OrgID != orgID {
		return nil, ErrNotFound
	}
	return copySimulation(sim), nil
}

// List returns the org's records, newest first (stable, copied).
func (s *Service) List(orgID string) ([]*Simulation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Simulation, 0, len(s.order))
	for _, id := range s.order {
		if sim, ok := s.sims[id]; ok && sim.OrgID == orgID {
			out = append(out, copySimulation(sim))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// copySimulation deep-copies the record (results are shared read-only after
// completion, but the config slices/must not alias the store's mutation window).
func copySimulation(sim *Simulation) *Simulation {
	out := *sim
	out.Config = cloneConfig(sim.Config)
	if sim.Result != nil {
		res := *sim.Result
		out.Result = &res
	}
	return &out
}

// cloneConfig copies the slice/map fields of a SimConfig.
func cloneConfig(cfg SimConfig) SimConfig {
	out := cfg
	out.Agents = append([]SimAgentPersona(nil), cfg.Agents...)
	out.Activity = append([]ActivityEvent(nil), cfg.Activity...)
	out.Params = cloneStrings(cfg.Params)
	return out
}

// cloneStrings copies a string map.
func cloneStrings(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
