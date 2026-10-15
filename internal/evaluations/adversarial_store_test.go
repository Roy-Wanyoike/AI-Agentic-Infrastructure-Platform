package evaluations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// recordingEvalStore is a test recorder of the EXISTING evaluations Store
// seam (interface-based per store.go). It captures the calls the
// EvalHistorySink makes so the mapping contract is asserted against the real
// interface, not against a parallel shape. It is a test fixture for the
// sink's write path only — production uses the Postgres-backed Store.
type recordingEvalStore struct {
	runs    []*EvalRun
	results map[string][]Result
}

func newRecordingEvalStore() *recordingEvalStore {
	return &recordingEvalStore{results: make(map[string][]Result)}
}

func (s *recordingEvalStore) CreateDataset(_ context.Context, _ *Dataset) error { return nil }
func (s *recordingEvalStore) GetDataset(_ context.Context, _, _ string) (*Dataset, error) {
	return nil, ErrDatasetNotFound
}
func (s *recordingEvalStore) ListDatasets(_ context.Context, _ string) ([]*Dataset, error) {
	return nil, nil
}
func (s *recordingEvalStore) GetDatasetCases(_ context.Context, _, _ string) ([]Case, error) {
	return nil, nil
}
func (s *recordingEvalStore) CreateRun(_ context.Context, run *EvalRun) error {
	s.runs = append(s.runs, run)
	return nil
}
func (s *recordingEvalStore) UpdateRunStatus(_ context.Context, orgID, runID, status string, completedAt *time.Time) error {
	for _, run := range s.runs {
		if run.ID == runID && run.OrganizationID == orgID {
			run.Status = status
			run.CompletedAt = completedAt
			return nil
		}
	}
	return ErrRunNotFound
}
func (s *recordingEvalStore) CreateResults(_ context.Context, orgID, runID string, results []Result) error {
	// Mirror the pgStore tenant guard: the run must exist within the org.
	for _, run := range s.runs {
		if run.ID == runID && run.OrganizationID == orgID {
			s.results[runID] = append([]Result(nil), results...)
			return nil
		}
	}
	return ErrRunNotFound
}
func (s *recordingEvalStore) GetRun(_ context.Context, orgID, id string) (*EvalRun, error) {
	for _, run := range s.runs {
		if run.ID == id && run.OrganizationID == orgID {
			return run, nil
		}
	}
	return nil, ErrRunNotFound
}
func (s *recordingEvalStore) ListResults(_ context.Context, orgID, runID string) ([]Result, error) {
	if _, err := s.GetRun(context.Background(), orgID, runID); err != nil {
		return nil, err
	}
	return s.results[runID], nil
}
func (s *recordingEvalStore) ListCompletedRuns(_ context.Context, _, _ string, _ int) ([]*EvalRun, error) {
	return nil, nil
}

// TestInMemoryAdversarialStoreTenantIsolation pins the org-scoped contract:
// foreign suites surface as not-found (never another tenant's data) and
// listings only contain the caller's own suites.
func TestInMemoryAdversarialStoreTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryAdversarialStore()

	suiteA := &AdversarialSuiteResult{ID: "suite-a", OrganizationID: "org-a", PackID: "builtin-security-adversarial", Status: StatusCompleted, CreatedAt: time.Now().UTC()}
	suiteB := &AdversarialSuiteResult{ID: "suite-b", OrganizationID: "org-b", PackID: "builtin-security-adversarial", Status: StatusCompleted, CreatedAt: time.Now().UTC()}
	for _, suite := range []*AdversarialSuiteResult{suiteA, suiteB} {
		if err := store.CreateSuite(ctx, suite); err != nil {
			t.Fatalf("CreateSuite(%s): %v", suite.ID, err)
		}
	}

	if _, err := store.GetSuite(ctx, "org-b", "suite-a"); !errors.Is(err, ErrAdversarialSuiteNotFound) {
		t.Fatalf("cross-tenant GetSuite err = %v, want ErrAdversarialSuiteNotFound", err)
	}
	if _, err := store.GetSuite(ctx, "org-a", "unknown"); !errors.Is(err, ErrAdversarialSuiteNotFound) {
		t.Fatalf("unknown GetSuite err = %v, want ErrAdversarialSuiteNotFound", err)
	}
	got, err := store.GetSuite(ctx, "org-a", "suite-a")
	if err != nil || got.PackID != "builtin-security-adversarial" {
		t.Fatalf("owner GetSuite = %+v, %v", got, err)
	}

	forOrgA, err := store.ListSuites(ctx, "org-a", 10)
	if err != nil || len(forOrgA) != 1 || forOrgA[0].ID != "suite-a" {
		t.Fatalf("ListSuites(org-a) = %+v, %v; want only suite-a", forOrgA, err)
	}
	empty, err := store.ListSuites(ctx, "org-c", 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("ListSuites(org-c) = %+v, %v; want empty", empty, err)
	}
	if none, err := store.ListSuites(ctx, "org-a", 0); err != nil || len(none) != 0 {
		t.Fatalf("ListSuites limit 0 = %+v, %v; want empty", none, err)
	}

	// Store mutation guard: returned copies must not alias internal state.
	got.Results = append(got.Results, AdversarialResult{ScenarioID: "polluted"})
	again, _ := store.GetSuite(ctx, "org-a", "suite-a")
	if len(again.Results) != 0 {
		t.Fatalf("GetSuite returned aliased state: %d results", len(again.Results))
	}
}

// TestRunSuitePersistsThroughAdversarialStore covers the runner→store path:
// RunSuite executes the pack, aggregates evidence and persists the suite
// within the caller's org.
func TestRunSuitePersistsThroughAdversarialStore(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryAdversarialStore()
	runner := NewAdversarialRunner()
	runner.SetStore(store)

	suite, err := runner.RunSuite(ctx, "org-under-test", "builtin-security-adversarial", "agent-under-test", newGateHarness(t, true))
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if suite.Evidence.Suite != GateSuiteName || suite.Evidence.Total != 7 || suite.Evidence.Passed != 7 {
		t.Fatalf("suite evidence wrong: %+v", suite.Evidence)
	}
	if suite.AgentID != "agent-under-test" || suite.Status != StatusCompleted || suite.CompletedAt == nil {
		t.Fatalf("suite bookkeeping wrong: %+v", suite)
	}

	persisted, err := store.GetSuite(ctx, "org-under-test", suite.ID)
	if err != nil {
		t.Fatalf("persisted suite not retrievable: %v", err)
	}
	if len(persisted.Results) != len(suite.Results) {
		t.Fatalf("persisted results = %d, want %d", len(persisted.Results), len(suite.Results))
	}
	if _, err := store.GetSuite(ctx, "org-other", suite.ID); !errors.Is(err, ErrAdversarialSuiteNotFound) {
		t.Fatalf("cross-tenant suite read err = %v, want ErrAdversarialSuiteNotFound", err)
	}

	if _, err := runner.RunSuite(ctx, "org-under-test", "no-such-pack", "", newGateHarness(t, true)); err == nil {
		t.Fatalf("RunSuite with unknown pack must error")
	}
	if _, err := runner.RunSuite(ctx, "", "builtin-security-adversarial", "", newGateHarness(t, true)); err == nil {
		t.Fatalf("RunSuite without org must error")
	}
}

// TestEvalHistorySinkMapping pins the additive integration with the EXISTING
// evaluation history: one eval run per suite (dataset id prefixed
// "adversarial:"), one result row per scenario (passed only for the pass
// outcome; fail/inconclusive rows carry their reason), and a completed
// status transition — all through the classic Store interface.
func TestEvalHistorySinkMapping(t *testing.T) {
	ctx := context.Background()
	recorder := newRecordingEvalStore()
	sink := NewEvalHistorySink(recorder)

	suite := &AdversarialSuiteResult{
		ID:             "suite-1",
		OrganizationID: "org-under-test",
		PackID:         "builtin-security-adversarial",
		PackVersion:    "1.0.0",
		AgentID:        "agent-under-test",
		Status:         StatusCompleted,
		Results: []AdversarialResult{
			{Suite: GateSuiteName, ScenarioID: "s-pass", Outcome: OutcomePass, ObservedAction: "tool_not_invoked", Evidence: "ok"},
			{Suite: GateSuiteName, ScenarioID: "s-fail", Outcome: OutcomeFail, ObservedAction: "tool_invoked", Evidence: "side effect reached", Error: "tool executed"},
			{Suite: GateSuiteName, ScenarioID: "s-inc", Outcome: OutcomeInconclusive, ObservedAction: "seam_unavailable", Evidence: "seam not wired", Error: "seam not wired"},
		},
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	run, err := sink.RecordSuite(ctx, "org-under-test", suite)
	if err != nil {
		t.Fatalf("RecordSuite: %v", err)
	}
	if run.DatasetID != "adversarial:builtin-security-adversarial" {
		t.Fatalf("run dataset id = %q, want adversarial-prefixed pack id", run.DatasetID)
	}
	if run.AgentID != "agent-under-test" || run.Status != StatusCompleted || run.CompletedAt == nil {
		t.Fatalf("run bookkeeping wrong: %+v", run)
	}
	if len(recorder.runs) != 1 || len(recorder.results["suite-1"]) != 3 {
		t.Fatalf("sink wrote wrong row counts: runs=%d results=%d", len(recorder.runs), len(recorder.results["suite-1"]))
	}
	byCase := make(map[string]Result)
	for _, r := range recorder.results["suite-1"] {
		byCase[r.CaseID] = r
	}
	if !byCase["s-pass"].Passed || byCase["s-pass"].Score != 1 || byCase["s-pass"].Error != "" {
		t.Fatalf("pass row mapping wrong: %+v", byCase["s-pass"])
	}
	if byCase["s-fail"].Passed || byCase["s-fail"].Score != 0 || byCase["s-fail"].Error != "tool executed" {
		t.Fatalf("fail row mapping wrong: %+v", byCase["s-fail"])
	}
	if byCase["s-inc"].Passed || byCase["s-inc"].Error != "seam not wired" {
		t.Fatalf("inconclusive row mapping wrong: %+v", byCase["s-inc"])
	}
	if byCase["s-pass"].Scorer != AdversarialControlScorer {
		t.Fatalf("scorer label = %q, want %q", byCase["s-pass"].Scorer, AdversarialControlScorer)
	}
	if run.Summary == nil || run.Summary.PassRate != 1.0/3.0 {
		t.Fatalf("summary pass rate = %+v, want 1/3 (inconclusive is not a pass)", run.Summary)
	}

	// Read-back through the same seam reproduces the run.
	back, err := recorder.GetRun(ctx, "org-under-test", "suite-1")
	if err != nil || back.Status != StatusCompleted {
		t.Fatalf("read-back = %+v, %v", back, err)
	}

	if _, err := sink.RecordSuite(ctx, "org-under-test", nil); err == nil {
		t.Fatalf("nil suite must error")
	}
}

// TestEvalHistorySinkRoundTripThroughSuiteRun ties the layers together:
// RunSuite (with the adversarial store attached) then RecordSuite produce a
// classic eval run whose rows match the suite's evidence exactly.
func TestEvalHistorySinkRoundTripThroughSuiteRun(t *testing.T) {
	ctx := context.Background()
	runner := NewAdversarialRunner()
	suite, err := runner.RunSuite(ctx, "org-under-test", "builtin-security-adversarial", "", newGateHarness(t, true))
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if suite.AgentID != AdversarialSuiteTargetAgent {
		t.Fatalf("empty agent id must default to %q, got %q", AdversarialSuiteTargetAgent, suite.AgentID)
	}

	recorder := newRecordingEvalStore()
	run, err := NewEvalHistorySink(recorder).RecordSuite(ctx, "org-under-test", suite)
	if err != nil {
		t.Fatalf("RecordSuite: %v", err)
	}
	if run.Summary == nil || run.Summary.PassRate != 1.0 {
		t.Fatalf("enforced suite must round-trip as all-pass: %+v", run.Summary)
	}
	if len(recorder.results[suite.ID]) != suite.Evidence.Total {
		t.Fatalf("result rows = %d, want evidence total %d", len(recorder.results[suite.ID]), suite.Evidence.Total)
	}
	for _, row := range recorder.results[suite.ID] {
		if !row.Passed || !strings.HasPrefix(row.ID, suite.ID+":") {
			t.Fatalf("row mapping broken: %+v", row)
		}
	}
}
