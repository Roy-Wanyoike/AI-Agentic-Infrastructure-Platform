package runs

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentos/internal/streaming"
)

// fakeStore is a minimal Store double for exercising the store-backed
// (dual-mode) service paths without SQL.
type fakeStore struct {
	runs      map[string]*Run
	steps     []*Step
	inserted  []*Step
	aggregate func(ctx context.Context, orgID string, from, to time.Time, groupBy CostGroupBy) ([]CostBucket, error)
}

func newFakeStore() *fakeStore {
	return &fakeStore{runs: map[string]*Run{}}
}

func (f *fakeStore) CreateRun(_ context.Context, run *Run) error {
	f.runs[run.ID] = run
	return nil
}

func (f *fakeStore) GetRun(_ context.Context, orgID, id string) (*Run, error) {
	run, ok := f.runs[id]
	if !ok || run.OrganizationID != orgID {
		return nil, ErrRunNotFound
	}
	return run, nil
}

func (f *fakeStore) GetRunByID(_ context.Context, id string) (*Run, error) {
	run, ok := f.runs[id]
	if !ok {
		return nil, ErrRunNotFound
	}
	return run, nil
}

func (f *fakeStore) ListRuns(_ context.Context, orgID string) ([]*Run, error) {
	out := []*Run{}
	for _, run := range f.runs {
		if run.OrganizationID == orgID {
			out = append(out, run)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateRunStatus(_ context.Context, orgID, id string, status RunStatus, output string) error {
	run, ok := f.runs[id]
	if !ok || run.OrganizationID != orgID {
		return ErrRunNotFound
	}
	run.Status = status
	if output != "" {
		run.Output = output
	}
	return nil
}

func (f *fakeStore) InsertStep(_ context.Context, orgID string, step *Step) error {
	run, ok := f.runs[step.RunID]
	if !ok || run.OrganizationID != orgID {
		return ErrRunNotFound
	}
	f.inserted = append(f.inserted, step)
	f.steps = append(f.steps, step)
	// The pgStore bumps runs.cost_cents in the same statement; the fake keeps
	// the durable total in sync so the service can be compared against it.
	run.TotalCostCents += step.Cost
	return nil
}

func (f *fakeStore) ListSteps(_ context.Context, _, _ string) ([]*Step, error) {
	return f.steps, nil
}

func (f *fakeStore) AggregateCosts(ctx context.Context, orgID string, from, to time.Time, groupBy CostGroupBy) ([]CostBucket, error) {
	if f.aggregate != nil {
		return f.aggregate(ctx, orgID, from, to, groupBy)
	}
	return nil, errors.New("not implemented")
}

func TestUpdateStatusPublishesEvent(t *testing.T) {
	svc := NewService()
	streamer := streaming.NewService()
	svc.SetStreamer(streamer)

	run, err := svc.Create("org-1", "agent-1", "input")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := svc.UpdateStatus(run.ID, StatusRunning, ""); err != nil {
		t.Fatalf("update status failed: %v", err)
	}

	history := streamer.History(run.ID)
	if len(history) == 0 {
		t.Fatalf("expected at least one published event, got 0")
	}
	ev := history[len(history)-1]
	if ev.Type != "status" || ev.Name != "status.changed" {
		t.Fatalf("unexpected event: %#v", ev)
	}
}

func TestRecordStepTracksRunCostInMemory(t *testing.T) {
	svc := NewService()
	run, err := svc.CreateRunCtx(context.Background(), "org-1", "agent-1", "hello")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}

	if err := svc.RecordStep(context.Background(), "org-1", run.ID, &Step{StepType: "model", Status: "SUCCEEDED", Cost: 0.25}); err != nil {
		t.Fatalf("RecordStep returned error: %v", err)
	}
	if err := svc.RecordStep(context.Background(), "org-1", run.ID, &Step{StepType: "model", Status: "SUCCEEDED", Cost: 0.75}); err != nil {
		t.Fatalf("RecordStep returned error: %v", err)
	}

	got, err := svc.GetRunCtx(context.Background(), "org-1", run.ID)
	if err != nil {
		t.Fatalf("GetRunCtx returned error: %v", err)
	}
	if got.TotalCostCents != 1.0 {
		t.Fatalf("run total cost should be the sum of step costs, got %v want 1.0", got.TotalCostCents)
	}
}

func TestRecordStepCostPersistedThroughStore(t *testing.T) {
	store := newFakeStore()
	svc := NewServiceWithStore(store)
	run, err := svc.CreateRunCtx(context.Background(), "org-1", "agent-1", "hello")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}

	if err := svc.RecordStep(context.Background(), "org-1", run.ID, &Step{StepType: "model", Status: "SUCCEEDED", Cost: 1.5}); err != nil {
		t.Fatalf("RecordStep returned error: %v", err)
	}
	if len(store.inserted) != 1 || store.inserted[0].Cost != 1.5 {
		t.Fatalf("costed step should be persisted through the store: %+v", store.inserted)
	}
	if got := store.runs[run.ID].TotalCostCents; got != 1.5 {
		t.Fatalf("durable run total should be bumped with the step cost, got %v want 1.5", got)
	}
	if cached, err := svc.GetRunCtx(context.Background(), "org-1", run.ID); err != nil || cached.TotalCostCents != 1.5 {
		t.Fatalf("cached run total should mirror the durable bump: %+v %v", cached, err)
	}

	// Tenant guard: a step for a foreign run is rejected by the store.
	if err := svc.RecordStep(context.Background(), "org-2", run.ID, &Step{StepType: "model", Status: "SUCCEEDED"}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign-org step should be ErrRunNotFound, got %v", err)
	}
}

func TestAggregateCostsCtxValidatesInput(t *testing.T) {
	svc := NewService()
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(72 * time.Hour)

	if _, _, err := svc.AggregateCostsCtx(context.Background(), "", from, to, CostGroupByDay); err == nil {
		t.Fatal("empty organization should be rejected")
	}
	if _, _, err := svc.AggregateCostsCtx(context.Background(), "org-1", from, to, CostGroupBy("week")); !errors.Is(err, ErrInvalidGroupBy) {
		t.Fatalf("unknown grouping should be ErrInvalidGroupBy, got %v", err)
	}
	if _, _, err := svc.AggregateCostsCtx(context.Background(), "org-1", time.Time{}, to, CostGroupByDay); err == nil {
		t.Fatal("zero from should be rejected")
	}
	if _, _, err := svc.AggregateCostsCtx(context.Background(), "org-1", to, from, CostGroupByDay); err == nil {
		t.Fatal("to <= from should be rejected")
	}
}

func TestAggregateCostsCtxInMemory(t *testing.T) {
	svc := NewService()
	day1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)

	r1, err := svc.CreateRunCtx(context.Background(), "org-1", "agent-1", "a")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}
	r1.CreatedAt = day1
	if err := svc.RecordStep(context.Background(), "org-1", r1.ID, &Step{StepType: "model", Status: "SUCCEEDED", Cost: 0.5}); err != nil {
		t.Fatalf("RecordStep returned error: %v", err)
	}
	r2, err := svc.CreateRunCtx(context.Background(), "org-1", "agent-2", "b")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}
	r2.CreatedAt = day2
	if err := svc.RecordStep(context.Background(), "org-1", r2.ID, &Step{StepType: "model", Status: "SUCCEEDED", Cost: 0.75}); err != nil {
		t.Fatalf("RecordStep returned error: %v", err)
	}
	// Foreign tenant + outside-window runs must never aggregate.
	if foreign, err := svc.CreateRunCtx(context.Background(), "org-2", "agent-1", "c"); err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	} else {
		foreign.CreatedAt = day1
		if err := svc.RecordStep(context.Background(), "org-2", foreign.ID, &Step{StepType: "model", Status: "SUCCEEDED", Cost: 99}); err != nil {
			t.Fatalf("RecordStep returned error: %v", err)
		}
	}

	// by day
	series, total, err := svc.AggregateCostsCtx(context.Background(), "org-1", day1, day2.Add(time.Hour), CostGroupByDay)
	if err != nil {
		t.Fatalf("AggregateCostsCtx(day) returned error: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 day buckets, got %+v", series)
	}
	if series[0].Bucket != "2026-09-01" || series[0].Runs != 1 || series[0].CostCents != 0.5 {
		t.Fatalf("unexpected first bucket: %+v", series[0])
	}
	if series[1].Bucket != "2026-09-02" || series[1].Runs != 1 || series[1].CostCents != 0.75 {
		t.Fatalf("unexpected second bucket: %+v", series[1])
	}
	if total != 1.25 {
		t.Fatalf("total should be the series sum, got %v want 1.25", total)
	}

	// by agent
	series, total, err = svc.AggregateCostsCtx(context.Background(), "org-1", day1, day2.Add(time.Hour), CostGroupByAgent)
	if err != nil {
		t.Fatalf("AggregateCostsCtx(agent) returned error: %v", err)
	}
	if len(series) != 2 || series[0].AgentID != "agent-1" || series[1].AgentID != "agent-2" || total != 1.25 {
		t.Fatalf("unexpected agent buckets: %+v (total %v)", series, total)
	}

	// window is half-open [from, to): day2 alone excludes the day1 run.
	series, _, err = svc.AggregateCostsCtx(context.Background(), "org-1", day2, day2.Add(time.Hour), CostGroupByDay)
	if err != nil {
		t.Fatalf("AggregateCostsCtx(window) returned error: %v", err)
	}
	if len(series) != 1 || series[0].Bucket != "2026-09-02" || series[0].CostCents != 0.75 {
		t.Fatalf("half-open window should exclude day1: %+v", series)
	}
}

func TestAggregateCostsCtxThroughStore(t *testing.T) {
	store := newFakeStore()
	svc := NewServiceWithStore(store)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(72 * time.Hour)
	store.aggregate = func(_ context.Context, orgID string, f, tt time.Time, groupBy CostGroupBy) ([]CostBucket, error) {
		if orgID != "org-1" || !f.Equal(from) || !tt.Equal(to) || groupBy != CostGroupByModel {
			t.Fatalf("unexpected aggregate args: org=%q from=%v to=%v groupBy=%q", orgID, f, tt, groupBy)
		}
		return []CostBucket{{Model: "gpt-4o", CostCents: 2.5, Runs: 3}}, nil
	}

	series, total, err := svc.AggregateCostsCtx(context.Background(), "org-1", from, to, CostGroupByModel)
	if err != nil {
		t.Fatalf("AggregateCostsCtx returned error: %v", err)
	}
	if len(series) != 1 || series[0].Model != "gpt-4o" || series[0].Runs != 3 {
		t.Fatalf("unexpected series: %+v", series)
	}
	if total != 2.5 {
		t.Fatalf("total should be the series sum, got %v want 2.5", total)
	}
}
