package evaluations

// adversarial_store.go — persistence for adversarial suite outcomes
// (issue #153), in two additive layers:
//
//  1. AdversarialStore: the suite's own org-scoped store seam, implemented
//     here in memory (zero-infrastructure mode, matching the platform's
//     dual-mode convention). Every query is scoped by organization_id —
//     foreign or unknown ids surface as ErrAdversarialSuiteNotFound, never
//     as another tenant's data.
//
//  2. EvalHistorySink: integration with the EXISTING evaluation history.
//     The classic eval Store interface (store.go) is interface-based and
//     Postgres-backed; instead of extending it with adversarial-specific
//     methods, the sink maps one adversarial suite execution onto the
//     existing run/result shape and persists it through Store.CreateRun /
//     Store.CreateResults / Store.UpdateRunStatus:
//
//	EvalRun.DatasetID  = "adversarial:" + packID   (the pack is the dataset)
//	EvalRun.AgentID    = suite.AgentID             (target agent or "adversarial-suite")
//	Result.CaseID      = scenario ID
//	Result.Scorer      = "adversarial_control"     (string column, informative)
//	Result.Passed      = outcome == pass           (inconclusive is NOT a pass)
//	Result.Output      = observed action (e.g. "tool_not_invoked")
//	Result.Error       = evidence text for fail/inconclusive rows, else ""
//
//     Read-back uses the same Store seam (GetRun/ListResults) — no new
//     read models, no edits to store.go.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrAdversarialSuiteNotFound is returned when a suite result does not exist
// within the caller's organization (tenant guard: foreign suites are
// "not found", never a cross-tenant leak).
var ErrAdversarialSuiteNotFound = errors.New("adversarial suite result not found")

// AdversarialSuiteTargetAgent is the AgentID recorded on suite runs that do
// not target a specific agent.
const AdversarialSuiteTargetAgent = "adversarial-suite"

// AdversarialControlScorer is the informative scorer label written to the
// classic eval history for adversarial rows (see EvalHistorySink mapping).
const AdversarialControlScorer = Scorer("adversarial_control")

// AdversarialSuiteResult is one execution of one pack: the aggregate gate
// evidence plus every per-scenario result, scoped to one organization.
type AdversarialSuiteResult struct {
	ID             string              `json:"id"`
	OrganizationID string              `json:"organization_id,omitempty"`
	PackID         string              `json:"pack_id"`
	PackVersion    string              `json:"pack_version"`
	AgentID        string              `json:"agent_id"`
	Status         string              `json:"status"` // StatusCompleted
	Results        []AdversarialResult `json:"results"`
	Evidence       GateEvidence        `json:"evidence"`
	CreatedAt      time.Time           `json:"created_at,omitempty"`
	CompletedAt    *time.Time          `json:"completed_at,omitempty"`
}

// AdversarialStore is the suite's own org-scoped persistence seam.
type AdversarialStore interface {
	// CreateSuite inserts one suite result within one tenant.
	CreateSuite(ctx context.Context, suite *AdversarialSuiteResult) error
	// GetSuite fetches one suite result guarded by organization_id.
	GetSuite(ctx context.Context, orgID, id string) (*AdversarialSuiteResult, error)
	// ListSuites returns the tenant's suites newest first (limit-capped).
	ListSuites(ctx context.Context, orgID string, limit int) ([]*AdversarialSuiteResult, error)
}

// adversarialSuiteLimitCap bounds ListSuites so one caller cannot stream the
// whole suite history (the sampleLimitCap precedent).
const adversarialSuiteLimitCap = 500

// InMemoryAdversarialStore is the zero-infrastructure AdversarialStore.
// Tenant isolation is the same contract as the SQL-backed stores: rows carry
// their organization_id and every read filters on it.
type InMemoryAdversarialStore struct {
	mu     sync.Mutex
	suites map[string]*AdversarialSuiteResult
}

// NewInMemoryAdversarialStore returns an empty in-memory store.
func NewInMemoryAdversarialStore() *InMemoryAdversarialStore {
	return &InMemoryAdversarialStore{suites: make(map[string]*AdversarialSuiteResult)}
}

// CreateSuite stores a defensive copy of the suite result.
func (s *InMemoryAdversarialStore) CreateSuite(_ context.Context, suite *AdversarialSuiteResult) error {
	if s == nil || suite == nil {
		return errors.New("adversarial: suite store or suite is nil")
	}
	if strings.TrimSpace(suite.ID) == "" {
		return errors.New("adversarial: suite id is required")
	}
	if strings.TrimSpace(suite.OrganizationID) == "" {
		return errors.New("adversarial: suite organization id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := *suite
	stored.Results = append([]AdversarialResult(nil), suite.Results...)
	s.suites[stored.ID] = &stored
	return nil
}

// GetSuite fetches one suite guarded by organization_id.
func (s *InMemoryAdversarialStore) GetSuite(_ context.Context, orgID, id string) (*AdversarialSuiteResult, error) {
	if s == nil {
		return nil, ErrAdversarialSuiteNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	suite, ok := s.suites[id]
	if !ok || suite.OrganizationID != orgID {
		// Tenant guard: foreign or unknown suites are "not found".
		return nil, ErrAdversarialSuiteNotFound
	}
	out := *suite
	out.Results = append([]AdversarialResult(nil), suite.Results...)
	return &out, nil
}

// ListSuites returns the tenant's suites newest first (CreatedAt desc, then
// ID desc for determinism), capped by limit.
func (s *InMemoryAdversarialStore) ListSuites(_ context.Context, orgID string, limit int) ([]*AdversarialSuiteResult, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		return []*AdversarialSuiteResult{}, nil
	}
	if limit > adversarialSuiteLimitCap {
		limit = adversarialSuiteLimitCap
	}
	s.mu.Lock()
	matches := make([]*AdversarialSuiteResult, 0)
	for _, suite := range s.suites {
		if suite.OrganizationID == orgID {
			matches = append(matches, suite)
		}
	}
	s.mu.Unlock()
	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
			return matches[i].CreatedAt.After(matches[j].CreatedAt)
		}
		return matches[i].ID > matches[j].ID
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]*AdversarialSuiteResult, 0, len(matches))
	for _, suite := range matches {
		copied := *suite
		copied.Results = append([]AdversarialResult(nil), suite.Results...)
		out = append(out, &copied)
	}
	return out, nil
}

// SetStore attaches an optional AdversarialStore to the runner; when wired,
// RunSuite persists every suite execution after completion.
func (r *AdversarialRunner) SetStore(store AdversarialStore) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = store
}

// RunSuite executes the named pack against the harness, aggregates the gate
// evidence and (when a store is attached) persists the suite result within
// orgID. Unknown packs are a caller error. The returned suite is the same
// object that was persisted (if any).
func (r *AdversarialRunner) RunSuite(ctx context.Context, orgID, packID, agentID string, harness AdversarialHarness) (*AdversarialSuiteResult, error) {
	if r == nil || r.registry == nil {
		return nil, errors.New("adversarial: runner is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("adversarial: organization id is required")
	}
	pack, ok := r.registry.Pack(packID)
	if !ok {
		return nil, errors.New("adversarial: unknown pack " + packID)
	}
	results, err := r.ExecutePack(ctx, packID, harness)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(agentID) == "" {
		agentID = AdversarialSuiteTargetAgent
	}
	now := time.Now().UTC()
	suite := &AdversarialSuiteResult{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		PackID:         pack.ID,
		PackVersion:    pack.Version,
		AgentID:        agentID,
		Status:         StatusCompleted,
		Results:        results,
		Evidence:       ExportGateEvidence(results),
		CreatedAt:      now,
		CompletedAt:    &now,
	}
	r.mu.Lock()
	store := r.store
	r.mu.Unlock()
	if store != nil {
		if err := store.CreateSuite(ctx, suite); err != nil {
			return nil, err
		}
	}
	return suite, nil
}

// EvalHistorySink persists adversarial suite outcomes through the EXISTING
// evaluations Store seam (see the file-level mapping doc). It is additive:
// no new methods on Store, no edits to store.go, and a sink failure is
// returned to the caller — it never mutates the suite outcome itself.
type EvalHistorySink struct {
	store Store
}

// NewEvalHistorySink returns a sink over the classic evaluation Store.
func NewEvalHistorySink(store Store) *EvalHistorySink {
	return &EvalHistorySink{store: store}
}

// RecordSuite writes one suite execution into the classic eval history as a
// completed EvalRun with one Result per scenario (mapping documented at the
// top of this file). The returned run is the persisted record; load it back
// with Store.GetRun/ListResults.
func (s *EvalHistorySink) RecordSuite(ctx context.Context, orgID string, suite *AdversarialSuiteResult) (*EvalRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("adversarial: eval history sink has no store")
	}
	if suite == nil {
		return nil, errors.New("adversarial: suite is nil")
	}
	agentID := suite.AgentID
	if strings.TrimSpace(agentID) == "" {
		agentID = AdversarialSuiteTargetAgent
	}
	now := time.Now().UTC()
	run := &EvalRun{
		ID:             suite.ID,
		OrganizationID: orgID,
		DatasetID:      "adversarial:" + suite.PackID,
		AgentID:        agentID,
		Status:         StatusRunning,
		CreatedAt:      suite.CreatedAt,
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	if err := s.store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(suite.Results))
	for _, r := range suite.Results {
		mapped := Result{
			// Deterministic per-scenario row id: suite id + scenario id.
			ID:        suite.ID + ":" + r.ScenarioID,
			CaseID:    r.ScenarioID,
			Scorer:    AdversarialControlScorer,
			Output:    r.ObservedAction,
			LatencyMS: r.DurationMS,
		}
		switch r.Outcome {
		case OutcomePass:
			mapped.Passed = true
			mapped.Score = 1
		case OutcomeFail, OutcomeInconclusive:
			mapped.Passed = false
			mapped.Score = 0
			// Inconclusive rows carry their reason in error too — an
			// unprovable control must not read as a clean fail.
			mapped.Error = firstNonEmpty(r.Error, r.Evidence)
		}
		results = append(results, mapped)
	}
	if err := s.store.CreateResults(ctx, orgID, run.ID, results); err != nil {
		return nil, err
	}
	completedAt := now
	if suite.CompletedAt != nil {
		completedAt = *suite.CompletedAt
	}
	if err := s.store.UpdateRunStatus(ctx, orgID, run.ID, StatusCompleted, &completedAt); err != nil {
		return nil, err
	}
	run.Status = StatusCompleted
	run.CompletedAt = &completedAt
	run.Results = results
	run.Summary = computeSummary(results)
	return run, nil
}
