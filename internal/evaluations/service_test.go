package evaluations

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"agentos/internal/agents"
	"agentos/internal/models"
	"agentos/internal/runtime"
)

// fakeRunner is the test double for AgentRunner.
type fakeRunner struct {
	fn func(ctx context.Context, agentID, input string) (*runtime.Run, error)
}

func (f *fakeRunner) Run(ctx context.Context, agentID, input string) (*runtime.Run, error) {
	return f.fn(ctx, agentID, input)
}

// recordingStore is a minimal in-memory Store used to exercise the
// store-backed (dual-mode) service path without SQL.
type recordingStore struct {
	datasets     map[string]*Dataset
	datasetCases map[string][]Case
	runs         map[string]*EvalRun
	results      map[string][]Result
	createRunErr error
}

func newRecordingStore() *recordingStore {
	return &recordingStore{
		datasets:     map[string]*Dataset{},
		datasetCases: map[string][]Case{},
		runs:         map[string]*EvalRun{},
		results:      map[string][]Result{},
	}
}

func (r *recordingStore) CreateDataset(_ context.Context, d *Dataset) error {
	r.datasets[d.ID] = d
	r.datasetCases[d.ID] = d.Cases
	return nil
}

func (r *recordingStore) GetDataset(_ context.Context, orgID, id string) (*Dataset, error) {
	d, ok := r.datasets[id]
	if !ok || d.OrganizationID != orgID {
		return nil, ErrDatasetNotFound
	}
	return d, nil
}

func (r *recordingStore) ListDatasets(_ context.Context, orgID string) ([]*Dataset, error) {
	out := []*Dataset{}
	for _, d := range r.datasets {
		if d.OrganizationID == orgID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (r *recordingStore) GetDatasetCases(_ context.Context, orgID, datasetID string) ([]Case, error) {
	if _, err := r.GetDataset(context.Background(), orgID, datasetID); err != nil {
		return nil, err
	}
	return r.datasetCases[datasetID], nil
}

func (r *recordingStore) CreateRun(_ context.Context, run *EvalRun) error {
	if r.createRunErr != nil {
		return r.createRunErr
	}
	r.runs[run.ID] = run
	return nil
}

func (r *recordingStore) UpdateRunStatus(_ context.Context, orgID, runID, status string, completedAt *time.Time) error {
	run, ok := r.runs[runID]
	if !ok || run.OrganizationID != orgID {
		return ErrRunNotFound
	}
	run.Status = status
	run.CompletedAt = completedAt
	return nil
}

func (r *recordingStore) CreateResults(_ context.Context, orgID, runID string, results []Result) error {
	if _, ok := r.runs[runID]; !ok {
		return ErrRunNotFound
	}
	r.results[runID] = results
	return nil
}

func (r *recordingStore) GetRun(_ context.Context, orgID, id string) (*EvalRun, error) {
	run, ok := r.runs[id]
	if !ok || run.OrganizationID != orgID {
		return nil, ErrRunNotFound
	}
	return run, nil
}

func (r *recordingStore) ListResults(_ context.Context, orgID, runID string) ([]Result, error) {
	if _, err := r.GetRun(context.Background(), orgID, runID); err != nil {
		return nil, err
	}
	return r.results[runID], nil
}

func newTestService(fn func(ctx context.Context, agentID, input string) (*runtime.Run, error)) *Service {
	agentSvc := agents.NewService()
	if _, err := agentSvc.Create("org-1", "Eval Agent", "d", "be deterministic", "gpt-4o-mini"); err != nil {
		panic(err)
	}
	return NewService(Deps{
		Agents:      agentSvc,
		Runner:      &fakeRunner{fn: fn},
		CaseTimeout: 2 * time.Second,
	})
}

func mustCreateDataset(t *testing.T, s *Service, cases []Case) *Dataset {
	t.Helper()
	ds, err := s.CreateDataset(context.Background(), "org-1", "Demo dataset", "desc", cases)
	if err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}
	return ds
}

func TestCreateDatasetValidation(t *testing.T) {
	s := newTestService(nil)

	if _, err := s.CreateDataset(context.Background(), "org-1", "", "d", nil); err == nil {
		t.Fatal("empty name should be rejected")
	}
	if _, err := s.CreateDataset(context.Background(), "", "name", "d", nil); err == nil {
		t.Fatal("empty organization should be rejected")
	}
	if _, err := s.CreateDataset(context.Background(), "org-1", "bad scorer", "", []Case{{ID: "c1", Scorer: Scorer("bogus")}}); err == nil {
		t.Fatal("unknown scorer should be rejected")
	}
	if _, err := s.CreateDataset(context.Background(), "org-1", "bad regex", "", []Case{{ID: "c1", Scorer: ScorerRegex, Params: Params{Pattern: "("}}}); err == nil {
		t.Fatal("invalid regex pattern should be rejected")
	}
	if _, err := s.CreateDataset(context.Background(), "org-1", "dupe ids", "", []Case{
		{ID: "c1", Scorer: ScorerExact, Expected: "a"},
		{ID: "c1", Scorer: ScorerExact, Expected: "b"},
	}); err == nil {
		t.Fatal("duplicate case ids should be rejected")
	}

	tooMany := make([]Case, MaxCasesPerDataset+1)
	for i := range tooMany {
		tooMany[i] = Case{Scorer: ScorerExact, Expected: "x"}
	}
	if _, err := s.CreateDataset(context.Background(), "org-1", "too many", "", tooMany); err == nil {
		t.Fatalf("more than %d cases should be rejected", MaxCasesPerDataset)
	}

	// Auto-generated case ids when the caller omits them.
	ds, err := s.CreateDataset(context.Background(), "org-1", "auto ids", "", []Case{
		{Scorer: ScorerExact, Expected: "a"},
		{Scorer: ScorerExact, Expected: "b"},
	})
	if err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}
	if ds.Cases[0].ID == "" || ds.Cases[1].ID == "" || ds.Cases[0].ID == ds.Cases[1].ID {
		t.Fatalf("case ids should be auto-generated and unique: %q %q", ds.Cases[0].ID, ds.Cases[1].ID)
	}
	if ds.CaseCount != 2 {
		t.Fatalf("case count should be 2, got %d", ds.CaseCount)
	}
}

func TestRunDatasetScoresAndSummary(t *testing.T) {
	s := newTestService(func(_ context.Context, _ string, input string) (*runtime.Run, error) {
		return &runtime.Run{Status: runtime.StatusCompleted, Output: "answer for " + input,
			Tokens: models.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
		}, nil
	})
	// Attach the pricing usage source exactly like the API-process wiring
	// does (docs/wiring/cost.md): the model is resolved from the agent's
	// configuration and the pricing hook prices the reported usage.
	s.AttachUsageSource(UsageSourceFunc(func(orgID, agentID string) (string, bool) {
		agent, err := s.deps.Agents.GetAgentCtx(context.Background(), orgID, agentID)
		if err != nil {
			return "", false
		}
		return agent.Model, true
	}))
	ds := mustCreateDataset(t, s, []Case{
		{ID: "c1", Input: "q1", Expected: "answer for q1", Scorer: ScorerExact},
		{ID: "c2", Input: "q2", Expected: "answer for q2", Scorer: ScorerExact},
		{ID: "c3", Input: "q3", Expected: "nothing matches", Scorer: ScorerContains},
		{ID: "c4", Input: "q4", Scorer: ScorerLatencyUnderMs, Params: Params{ThresholdMS: ptrFloat(60000)}},
	})

	run, err := s.RunDataset(context.Background(), "org-1", ds.ID, "agent-1-placeholder")
	if err == nil || !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("unknown agent id should map to ErrAgentNotFound, got %v", err)
	}

	agent := s.deps.Agents.List("org-1")[0]
	run, err = s.RunDataset(context.Background(), "org-1", ds.ID, agent.ID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("synchronous run should be completed, got %q", run.Status)
	}
	if len(run.Results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(run.Results))
	}
	if run.Results[0].CaseID != "c1" || !run.Results[0].Passed || run.Results[0].Score != 1.0 {
		t.Fatalf("c1 should pass exactly: %+v", run.Results[0])
	}
	if run.Results[2].CaseID != "c3" || run.Results[2].Passed {
		t.Fatalf("c3 should fail contains: %+v", run.Results[2])
	}
	if run.Results[3].CaseID != "c4" || !run.Results[3].Passed {
		t.Fatalf("c4 should pass latency budget: %+v", run.Results[3])
	}
	if run.Results[0].LatencyMS <= 0 {
		t.Fatalf("latency should be measured, got %v", run.Results[0].LatencyMS)
	}
	// Cost: the eval agent is served by gpt-4o-mini; every case reported
	// the same 1000 prompt / 500 completion tokens, so each result carries
	// the pricing-hook cost (1000*0.15 + 500*0.60)/1M*100 = 0.045 cents.
	wantCost := models.ComputeCostCents("gpt-4o-mini", 1000, 500)
	if wantCost <= 0 {
		t.Fatalf("pricing hook should return a positive cost, got %v", wantCost)
	}
	for i, res := range run.Results {
		if res.CostCents != wantCost {
			t.Fatalf("result %d (%s) cost should be computed by the pricing hook, got %v want %v", i, res.CaseID, res.CostCents, wantCost)
		}
	}
	if run.Summary == nil {
		t.Fatal("summary should be computed")
	}
	if wantTotal := 4 * wantCost; math.Abs(run.Summary.TotalCostCents-wantTotal) > 1e-9 {
		t.Fatalf("summary total cost should be %v, got %v", wantTotal, run.Summary.TotalCostCents)
	}
	want := 3.0 / 4.0
	if run.Summary.PassRate != want {
		t.Fatalf("pass rate should be %v, got %v", want, run.Summary.PassRate)
	}
	if run.Summary.AvgLatencyMS <= 0 {
		t.Fatalf("avg latency should be positive, got %v", run.Summary.AvgLatencyMS)
	}
	if got := run.Summary.ByScorer["exact"]; got.Passed != 2 || got.Failed != 0 {
		t.Fatalf("by_scorer exact should be {2 0}, got %+v", got)
	}
	if got := run.Summary.ByScorer["contains"]; got.Passed != 0 || got.Failed != 1 {
		t.Fatalf("by_scorer contains should be {0 1}, got %+v", got)
	}
	if got := run.Summary.ByScorer["latency_under_ms"]; got.Passed != 1 || got.Failed != 0 {
		t.Fatalf("by_scorer latency_under_ms should be {1 0}, got %+v", got)
	}

	fetched, err := s.GetRun(context.Background(), "org-1", run.ID)
	if err != nil {
		t.Fatalf("GetRun returned error: %v", err)
	}
	if len(fetched.Results) != 4 || fetched.Summary == nil || fetched.Summary.PassRate != want {
		t.Fatalf("fetched run should round-trip results and summary: %+v", fetched)
	}
	if math.Abs(fetched.Summary.TotalCostCents-4*wantCost) > 1e-9 {
		t.Fatalf("fetched summary should keep the priced totals, got %v want %v", fetched.Summary.TotalCostCents, 4*wantCost)
	}
}

// TestRunDatasetCostWithoutUsageSource pins the documented offline behavior:
// without an attached usage source (or without reported token usage) the case
// cost stays 0 and nothing fails.
func TestRunDatasetCostWithoutUsageSource(t *testing.T) {
	s := newTestService(func(_ context.Context, _ string, input string) (*runtime.Run, error) {
		return &runtime.Run{Status: runtime.StatusCompleted, Output: "answer for " + input,
			Tokens: models.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
		}, nil
	})
	ds := mustCreateDataset(t, s, []Case{
		{ID: "c1", Input: "q1", Expected: "answer for q1", Scorer: ScorerExact},
	})
	agent := s.deps.Agents.List("org-1")[0]
	run, err := s.RunDataset(context.Background(), "org-1", ds.ID, agent.ID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	if run.Results[0].CostCents != 0 || run.Summary.TotalCostCents != 0 {
		t.Fatalf("no usage source should keep cost at 0, got result=%v summary=%v", run.Results[0].CostCents, run.Summary.TotalCostCents)
	}

	// An attached source that cannot resolve the model also yields 0.
	s.AttachUsageSource(UsageSourceFunc(func(_, _ string) (string, bool) { return "", false }))
	run, err = s.RunDataset(context.Background(), "org-1", ds.ID, agent.ID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	if run.Results[0].CostCents != 0 {
		t.Fatalf("unresolvable model should keep cost at 0, got %v", run.Results[0].CostCents)
	}

	// Detaching (nil) restores the offline behavior.
	s.AttachUsageSource(nil)
	run, err = s.RunDataset(context.Background(), "org-1", ds.ID, agent.ID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	if run.Results[0].CostCents != 0 {
		t.Fatalf("detached source should keep cost at 0, got %v", run.Results[0].CostCents)
	}
}

func TestRunDatasetCaseTimeout(t *testing.T) {
	agentSvc := agents.NewService()
	agent, err := agentSvc.Create("org-1", "Slow Agent", "d", "i", "m")
	if err != nil {
		t.Fatalf("Create agent returned error: %v", err)
	}
	s := NewService(Deps{
		Agents: agentSvc,
		Runner: &fakeRunner{fn: func(ctx context.Context, _, _ string) (*runtime.Run, error) {
			// Simulate the runtime honoring its context deadline.
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		CaseTimeout: 30 * time.Millisecond,
	})
	ds := mustCreateDataset(t, s, []Case{
		{ID: "c1", Input: "q1", Scorer: ScorerExact, Expected: "never"},
	})

	run, err := s.RunDataset(context.Background(), "org-1", ds.ID, agent.ID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("timed-out cases still complete the run, got %q", run.Status)
	}
	if len(run.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(run.Results))
	}
	res := run.Results[0]
	if res.Passed || res.Score != 0 {
		t.Fatalf("timed-out case should fail with score 0: %+v", res)
	}
	if res.Error == "" || !strings.Contains(res.Error, "timed out") || !strings.Contains(res.Error, "context deadline exceeded") {
		t.Fatalf("timeout should be surfaced in result error, got %q", res.Error)
	}
	if run.Summary.PassRate != 0 {
		t.Fatalf("pass rate should be 0, got %v", run.Summary.PassRate)
	}
}

func TestRunDatasetRunnerError(t *testing.T) {
	s := newTestService(func(_ context.Context, _, _ string) (*runtime.Run, error) {
		return nil, fmt.Errorf("runtime: model call failed: boom")
	})
	agent := s.deps.Agents.List("org-1")[0]
	ds := mustCreateDataset(t, s, []Case{
		{ID: "c1", Input: "q1", Scorer: ScorerExact, Expected: "anything"},
	})

	run, err := s.RunDataset(context.Background(), "org-1", ds.ID, agent.ID)
	if err != nil {
		t.Fatalf("runner errors must not abort the run: %v", err)
	}
	res := run.Results[0]
	if res.Passed || res.Score != 0 || res.Error == "" {
		t.Fatalf("failing case should record error and zero score: %+v", res)
	}
	if run.Summary.PassRate != 0 {
		t.Fatalf("pass rate should be 0, got %v", run.Summary.PassRate)
	}
}

func TestRunDatasetTenantGuards(t *testing.T) {
	s := newTestService(func(_ context.Context, _, input string) (*runtime.Run, error) {
		return &runtime.Run{Status: runtime.StatusCompleted, Output: input}, nil
	})
	if _, err := s.deps.Agents.Create("org-2", "Other Org Agent", "d", "i", "m"); err != nil {
		t.Fatalf("Create agent returned error: %v", err)
	}
	ds := mustCreateDataset(t, s, []Case{{ID: "c1", Scorer: ScorerExact, Expected: "x"}})

	if _, err := s.GetDataset(context.Background(), "org-2", ds.ID); !errors.Is(err, ErrDatasetNotFound) {
		t.Fatalf("foreign dataset should be ErrDatasetNotFound, got %v", err)
	}
	if _, err := s.RunDataset(context.Background(), "org-2", ds.ID, "any"); !errors.Is(err, ErrDatasetNotFound) {
		t.Fatalf("foreign dataset run should be ErrDatasetNotFound, got %v", err)
	}
	agent2 := s.deps.Agents.List("org-2")[0]
	if _, err := s.RunDataset(context.Background(), "org-1", ds.ID, agent2.ID); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("foreign agent should be ErrAgentNotFound, got %v", err)
	}
	if _, err := s.GetRun(context.Background(), "org-2", "missing"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign/missing run should be ErrRunNotFound, got %v", err)
	}
}

func TestRunDatasetMissingRunner(t *testing.T) {
	s := NewService(Deps{})
	ds, err := s.CreateDataset(context.Background(), "org-1", "d", "", []Case{{ID: "c1", Scorer: ScorerExact}})
	if err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}
	if _, err := s.RunDataset(context.Background(), "org-1", ds.ID, "agent-1"); !errors.Is(err, ErrRunnerNotConfigured) {
		t.Fatalf("missing runner should be ErrRunnerNotConfigured, got %v", err)
	}
}

func TestCompareRunsRegressionLogic(t *testing.T) {
	out := map[string]string{"q1": "q1", "q2": "q2", "q3": "bad"} // baseline: c1/c2 pass, c3 fail
	s := newTestService(func(_ context.Context, _, input string) (*runtime.Run, error) {
		return &runtime.Run{Status: runtime.StatusCompleted, Output: out[input]}, nil
	})
	agent := s.deps.Agents.List("org-1")[0]
	ds := mustCreateDataset(t, s, []Case{
		{ID: "c1", Input: "q1", Scorer: ScorerExact, Expected: "q1"},
		{ID: "c2", Input: "q2", Scorer: ScorerExact, Expected: "q2"},
		{ID: "c3", Input: "q3", Scorer: ScorerExact, Expected: "q3"},
	})

	baseline, err := s.RunDataset(context.Background(), "org-1", ds.ID, agent.ID)
	if err != nil {
		t.Fatalf("baseline RunDataset returned error: %v", err)
	}
	out = map[string]string{"q1": "bad", "q2": "q2", "q3": "q3"} // candidate: c1 fail, c2/c3 pass
	candidate, err := s.RunDataset(context.Background(), "org-1", ds.ID, agent.ID)
	if err != nil {
		t.Fatalf("candidate RunDataset returned error: %v", err)
	}

	comparison, err := s.CompareRuns(context.Background(), "org-1", baseline.ID, candidate.ID)
	if err != nil {
		t.Fatalf("CompareRuns returned error: %v", err)
	}
	if len(comparison.Regressions) != 1 || comparison.Regressions[0].CaseID != "c1" ||
		!comparison.Regressions[0].BaselinePassed || comparison.Regressions[0].CandidatePassed {
		t.Fatalf("expected c1 regression, got %+v", comparison.Regressions)
	}
	if len(comparison.Improvements) != 1 || comparison.Improvements[0].CaseID != "c3" ||
		comparison.Improvements[0].BaselinePassed || !comparison.Improvements[0].CandidatePassed {
		t.Fatalf("expected c3 improvement, got %+v", comparison.Improvements)
	}
	if comparison.Baseline == nil || comparison.Candidate == nil {
		t.Fatal("comparison should embed both summaries")
	}
	if comparison.Baseline.PassRate != 2.0/3.0 || comparison.Candidate.PassRate != 2.0/3.0 {
		t.Fatalf("unexpected pass rates: baseline=%v candidate=%v",
			comparison.Baseline.PassRate, comparison.Candidate.PassRate)
	}

	if _, err := s.CompareRuns(context.Background(), "org-2", baseline.ID, candidate.ID); err == nil {
		t.Fatal("cross-tenant compare should fail")
	}
	if _, err := s.CompareRuns(context.Background(), "org-1", baseline.ID, "missing"); err == nil {
		t.Fatal("missing candidate should fail")
	}
}

func TestServiceWithStorePersistsThroughStore(t *testing.T) {
	store := newRecordingStore()
	agentSvc := agents.NewService()
	agent, err := agentSvc.Create("org-1", "A", "d", "i", "m")
	if err != nil {
		t.Fatalf("Create agent returned error: %v", err)
	}
	s := NewServiceWithStore(store, Deps{
		Agents: agentSvc,
		Runner: &fakeRunner{fn: func(_ context.Context, _, input string) (*runtime.Run, error) {
			return &runtime.Run{Status: runtime.StatusCompleted, Output: "out-" + input}, nil
		}},
		CaseTimeout: time.Second,
	})

	ds, err := s.CreateDataset(context.Background(), "org-1", "persisted", "", []Case{
		{ID: "c1", Input: "q1", Scorer: ScorerContains, Expected: "out-"},
	})
	if err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}
	if _, ok := store.datasets[ds.ID]; !ok {
		t.Fatal("dataset should be persisted through the store")
	}
	if len(store.datasetCases[ds.ID]) != 1 {
		t.Fatal("cases should be persisted through the store")
	}

	run, err := s.RunDataset(context.Background(), "org-1", ds.ID, agent.ID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	if store.runs[run.ID] == nil {
		t.Fatal("run should be persisted through the store")
	}
	if len(store.results[run.ID]) != 1 || !store.results[run.ID][0].Passed {
		t.Fatalf("results should be persisted through the store: %+v", store.results[run.ID])
	}
	if got := store.runs[run.ID].Status; got != StatusCompleted {
		t.Fatalf("run status should be updated in the store, got %q", got)
	}

	// Reads go through the store when present.
	fetched, err := s.GetRun(context.Background(), "org-1", run.ID)
	if err != nil {
		t.Fatalf("GetRun returned error: %v", err)
	}
	if len(fetched.Results) != 1 || fetched.Summary == nil {
		t.Fatalf("stored run should load results and summary: %+v", fetched)
	}

	if err := store.CreateDataset(context.Background(), &Dataset{ID: "orphan", OrganizationID: "org-1"}); err != nil {
		t.Fatalf("seeding orphan dataset failed: %v", err)
	}
	got, err := s.GetDataset(context.Background(), "org-1", "orphan")
	if err != nil {
		t.Fatalf("GetDataset returned error: %v", err)
	}
	if got.CaseCount != 0 || got.Cases == nil {
		t.Fatalf("dataset without cases should return empty case list, got %+v", got)
	}
}
