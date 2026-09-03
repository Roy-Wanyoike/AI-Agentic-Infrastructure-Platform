package evaluations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/agents"
	"agentos/internal/runtime"
)

// Execution bounds.
const (
	// MaxCasesPerDataset caps dataset size so one evaluation run cannot
	// monopolize the runtime (contract: bounded execution, max 50 cases).
	MaxCasesPerDataset = 50
	// DefaultCaseTimeout bounds every single case execution (contract: 30s
	// per case). Override with Deps.CaseTimeout (tests / tighter budgets).
	DefaultCaseTimeout = 30 * time.Second
)

// Eval run statuses.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Typed errors surfaced to handlers for status mapping.
var (
	// ErrDatasetNotFound is returned when a dataset does not exist within the
	// caller's organization (tenant guard: foreign datasets are "not found").
	ErrDatasetNotFound = errors.New("eval dataset not found")
	// ErrRunNotFound is returned when an eval run does not exist within the
	// caller's organization.
	ErrRunNotFound = errors.New("eval run not found")
	// ErrAgentNotFound is returned when the target agent does not exist
	// within the caller's organization.
	ErrAgentNotFound = errors.New("agent not found")
	// ErrRunnerNotConfigured is returned when the service was constructed
	// without an AgentRunner.
	ErrRunnerNotConfigured = errors.New("evaluations: runner not configured")
)

// Case is one evaluation input with its expected outcome and scorer.
type Case struct {
	ID       string `json:"id"`
	Input    string `json:"input"`
	Expected string `json:"expected"`
	Scorer   Scorer `json:"scorer"`
	Params   Params `json:"params,omitempty"`
}

// Dataset is a named collection of evaluation cases belonging to one tenant.
type Dataset struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id,omitempty"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	CaseCount      int       `json:"case_count"`
	Cases          []Case    `json:"cases,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

// Result is the outcome of one case inside one eval run. Scorer is stored for
// by_scorer summaries and persistence but excluded from the JSON shape.
type Result struct {
	ID        string  `json:"-"`
	CaseID    string  `json:"case_id"`
	Scorer    Scorer  `json:"-"`
	Output    string  `json:"output"`
	Passed    bool    `json:"passed"`
	Score     float64 `json:"score"`
	LatencyMS float64 `json:"latency_ms"`
	CostCents float64 `json:"cost_cents"`
	Error     string  `json:"error"`
}

// ScorerCounts aggregates pass/fail counts for one scorer kind.
type ScorerCounts struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

// Summary is the aggregate outcome of an eval run.
type Summary struct {
	PassRate       float64                 `json:"pass_rate"`
	AvgLatencyMS   float64                 `json:"avg_latency_ms"`
	TotalCostCents float64                 `json:"total_cost_cents"`
	ByScorer       map[string]ScorerCounts `json:"by_scorer"`
}

// EvalRun is one synchronous execution of a dataset against an agent.
type EvalRun struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id,omitempty"`
	DatasetID      string     `json:"dataset_id"`
	AgentID        string     `json:"agent_id"`
	Status         string     `json:"status"`
	Results        []Result   `json:"results,omitempty"`
	Summary        *Summary   `json:"summary,omitempty"`
	CreatedAt      time.Time  `json:"created_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// CaseComparison is one case-level delta between two runs.
type CaseComparison struct {
	CaseID          string `json:"case_id"`
	BaselinePassed  bool   `json:"baseline_passed"`
	CandidatePassed bool   `json:"candidate_passed"`
}

// Comparison is the outcome of comparing a baseline run to a candidate run.
type Comparison struct {
	Baseline     *Summary         `json:"baseline"`
	Candidate    *Summary         `json:"candidate"`
	Regressions  []CaseComparison `json:"regressions"`
	Improvements []CaseComparison `json:"improvements"`
}

// AgentRunner executes one agent turn. *runtime.Runner satisfies this
// interface; tests inject fakes through Deps.Runner.
type AgentRunner interface {
	Run(ctx context.Context, agentID, input string) (*runtime.Run, error)
}

// Deps bundles the collaborators the evaluation service needs. Agents is
// required for the tenant guard before execution; Runner is required to
// execute cases; CaseTimeout defaults to DefaultCaseTimeout.
type Deps struct {
	Agents      *agents.Service
	Runner      AgentRunner
	CaseTimeout time.Duration
}

func (d Deps) caseTimeout() time.Duration {
	if d.CaseTimeout > 0 {
		return d.CaseTimeout
	}
	return DefaultCaseTimeout
}

// Service is the dual-mode evaluation service: pure in-memory maps (zero
// infrastructure mode) or Postgres-backed store with the in-memory maps as a
// write-through cache.
type Service struct {
	mu       sync.Mutex
	datasets map[string]*Dataset
	runs     map[string]*EvalRun
	store    Store
	deps     Deps
}

// NewService returns the in-memory service (zero-infrastructure mode).
func NewService(deps Deps) *Service {
	return &Service{
		datasets: make(map[string]*Dataset),
		runs:     make(map[string]*EvalRun),
		deps:     deps,
	}
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store; the in-memory maps act as a write-through cache.
func NewServiceWithStore(store Store, deps Deps) *Service {
	s := NewService(deps)
	s.store = store
	return s
}

// CreateDataset validates and persists a new dataset with its cases.
func (s *Service) CreateDataset(ctx context.Context, orgID, name, description string, cases []Case) (*Dataset, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("dataset name is required")
	}
	if len(cases) > MaxCasesPerDataset {
		return nil, fmt.Errorf("too many cases: maximum is %d per dataset, got %d", MaxCasesPerDataset, len(cases))
	}

	normalized := make([]Case, 0, len(cases))
	seen := make(map[string]bool, len(cases))
	for i, c := range cases {
		if !c.Scorer.Valid() {
			return nil, fmt.Errorf("case %d: unknown scorer %q", i, c.Scorer)
		}
		if err := c.Params.Validate(c.Scorer); err != nil {
			return nil, fmt.Errorf("case %d: %w", i, err)
		}
		caseID := strings.TrimSpace(c.ID)
		if caseID == "" {
			caseID = uuid.NewString()
		}
		if seen[caseID] {
			return nil, fmt.Errorf("duplicate case id %q", caseID)
		}
		seen[caseID] = true
		normalized = append(normalized, Case{
			ID:       caseID,
			Input:    c.Input,
			Expected: c.Expected,
			Scorer:   c.Scorer,
			Params:   c.Params,
		})
	}

	now := time.Now().UTC()
	dataset := &Dataset{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Name:           strings.TrimSpace(name),
		Description:    description,
		CaseCount:      len(normalized),
		Cases:          normalized,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if s.store != nil {
		if err := s.store.CreateDataset(ctx, dataset); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.datasets[dataset.ID] = dataset
	s.mu.Unlock()
	return dataset, nil
}

// ListDatasets returns the datasets of one tenant (without case bodies, with
// case counts), newest first.
func (s *Service) ListDatasets(ctx context.Context, orgID string) ([]*Dataset, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if s.store != nil {
		return s.store.ListDatasets(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Dataset, 0)
	for _, d := range s.datasets {
		if d.OrganizationID != orgID {
			continue
		}
		summary := *d
		summary.Cases = nil
		out = append(out, &summary)
	}
	return out, nil
}

// GetDataset returns one dataset of one tenant including its cases.
func (s *Service) GetDataset(ctx context.Context, orgID, id string) (*Dataset, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrDatasetNotFound
	}
	if s.store != nil {
		dataset, err := s.store.GetDataset(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		cases, err := s.store.GetDatasetCases(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		dataset.Cases = cases
		dataset.CaseCount = len(cases)
		if dataset.Cases == nil {
			dataset.Cases = []Case{}
		}
		return dataset, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dataset, ok := s.datasets[id]
	if !ok || dataset.OrganizationID != orgID {
		return nil, ErrDatasetNotFound
	}
	return dataset, nil
}

// RunDataset executes every case of the dataset against agentID through the
// configured AgentRunner, synchronously and bounded: at most
// MaxCasesPerDataset cases, each with a Deps.CaseTimeout (default 30s)
// context deadline. Returns the completed run with per-case results and the
// aggregate summary.
func (s *Service) RunDataset(ctx context.Context, orgID, datasetID, agentID string) (*EvalRun, error) {
	if s.deps.Runner == nil {
		return nil, ErrRunnerNotConfigured
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(datasetID) == "" || strings.TrimSpace(agentID) == "" {
		return nil, errors.New("organization, dataset and agent ids are required")
	}
	// Tenant guard: the dataset and the agent must both belong to the caller's
	// organization before any execution happens.
	dataset, err := s.GetDataset(ctx, orgID, datasetID)
	if err != nil {
		return nil, err
	}
	if _, err := s.deps.Agents.GetAgentCtx(ctx, orgID, agentID); err != nil {
		return nil, ErrAgentNotFound
	}

	caseTimeout := s.deps.caseTimeout()
	now := time.Now().UTC()
	run := &EvalRun{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		DatasetID:      dataset.ID,
		AgentID:        agentID,
		Status:         StatusRunning,
		CreatedAt:      now,
	}
	if s.store != nil {
		if err := s.store.CreateRun(ctx, run); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.runs[run.ID] = run
	s.mu.Unlock()

	// Bound the batch: execute at most MaxCasesPerDataset cases even if a
	// dataset somehow holds more (defense in depth behind CreateDataset).
	cases := dataset.Cases
	if len(cases) > MaxCasesPerDataset {
		cases = cases[:MaxCasesPerDataset]
	}

	results := make([]Result, 0, len(cases))
	for _, c := range cases {
		results = append(results, s.runCase(ctx, run.ID, agentID, c, caseTimeout))
	}

	run.Results = results
	run.Summary = computeSummary(results)
	completedAt := time.Now().UTC()
	run.Status = StatusCompleted
	run.CompletedAt = &completedAt

	if s.store != nil {
		if err := s.store.CreateResults(ctx, orgID, run.ID, results); err != nil {
			return nil, err
		}
		if err := s.store.UpdateRunStatus(ctx, orgID, run.ID, StatusCompleted, &completedAt); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.runs[run.ID] = run
	s.mu.Unlock()
	return run, nil
}

// runCase executes and scores one case. Every case gets its own timeout
// context so a stuck agent turn cannot consume the whole run budget.
func (s *Service) runCase(ctx context.Context, runID, agentID string, c Case, caseTimeout time.Duration) Result {
	result := Result{CaseID: c.ID, Scorer: c.Scorer}

	caseCtx, cancel := context.WithTimeout(ctx, caseTimeout)
	defer cancel()

	started := time.Now()
	out, err := s.deps.Runner.Run(caseCtx, agentID, c.Input)
	result.LatencyMS = float64(time.Since(started).Nanoseconds()) / 1e6
	if out != nil {
		result.Output = out.Output
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(caseCtx.Err(), context.DeadlineExceeded) {
			result.Error = fmt.Sprintf("case timed out after %s: %v", caseTimeout, err)
		} else {
			result.Error = err.Error()
		}
		result.Passed = false
		result.Score = 0
		return result
	}

	// Cost: the runtime Run outcome does not expose cost today (token usage
	// only), so the recorded cost is 0 until a pricing hook exists.
	const costCents = 0.0
	score, passed, scoreErr := c.Score(result.Output, result.LatencyMS, costCents)
	if scoreErr != nil {
		result.Error = scoreErr.Error()
		result.Passed = false
		result.Score = 0
		return result
	}
	result.Score = score
	result.Passed = passed
	return result
}

// GetRun returns one eval run of one tenant including results and summary.
func (s *Service) GetRun(ctx context.Context, orgID, id string) (*EvalRun, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrRunNotFound
	}
	if s.store != nil {
		run, err := s.store.GetRun(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		results, err := s.store.ListResults(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		run.Results = results
		run.Summary = computeSummary(results)
		return run, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	if !ok || run.OrganizationID != orgID {
		return nil, ErrRunNotFound
	}
	return run, nil
}

// CompareRuns compares a candidate run against a baseline run of one tenant.
// Cases are matched by case_id; only the intersection of both runs is judged.
// A regression is a case the baseline passed but the candidate failed; an
// improvement is the reverse.
func (s *Service) CompareRuns(ctx context.Context, orgID, baselineID, candidateID string) (*Comparison, error) {
	baseline, err := s.GetRun(ctx, orgID, baselineID)
	if err != nil {
		return nil, fmt.Errorf("baseline run: %w", err)
	}
	candidate, err := s.GetRun(ctx, orgID, candidateID)
	if err != nil {
		return nil, fmt.Errorf("candidate run: %w", err)
	}

	baselinePassed := make(map[string]bool, len(baseline.Results))
	for _, r := range baseline.Results {
		baselinePassed[r.CaseID] = r.Passed
	}

	comparison := &Comparison{
		Baseline:     baseline.Summary,
		Candidate:    candidate.Summary,
		Regressions:  []CaseComparison{},
		Improvements: []CaseComparison{},
	}
	for _, r := range candidate.Results {
		basePassed, inBaseline := baselinePassed[r.CaseID]
		if !inBaseline {
			continue
		}
		switch {
		case basePassed && !r.Passed:
			comparison.Regressions = append(comparison.Regressions, CaseComparison{
				CaseID: r.CaseID, BaselinePassed: true, CandidatePassed: false,
			})
		case !basePassed && r.Passed:
			comparison.Improvements = append(comparison.Improvements, CaseComparison{
				CaseID: r.CaseID, BaselinePassed: false, CandidatePassed: true,
			})
		}
	}
	return comparison, nil
}

// computeSummary aggregates results into the run summary: pass rate, average
// latency, total cost, and pass/fail counts grouped by scorer kind.
func computeSummary(results []Result) *Summary {
	summary := &Summary{ByScorer: make(map[string]ScorerCounts)}
	if len(results) == 0 {
		return summary
	}
	var passed, totalLatency, totalCost float64
	for _, r := range results {
		if r.Passed {
			passed++
		}
		totalLatency += r.LatencyMS
		totalCost += r.CostCents
		counts := summary.ByScorer[string(r.Scorer)]
		if r.Passed {
			counts.Passed++
		} else {
			counts.Failed++
		}
		summary.ByScorer[string(r.Scorer)] = counts
	}
	summary.PassRate = passed / float64(len(results))
	summary.AvgLatencyMS = totalLatency / float64(len(results))
	summary.TotalCostCents = totalCost
	return summary
}
