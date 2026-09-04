package billing

// Meter aggregation tests (issue #57): the RunsMeterSource math over stubbed
// runs-ledger aggregates, window validation, nil-dependency behavior and
// error propagation. The runs-side SQL/in-memory implementations are tested
// in internal/runs; these tests pin the BILLING contract.

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentos/internal/runs"
)

// stubRunsAggregator implements RunsCostAggregator with canned buckets.
type stubRunsAggregator struct {
	buckets    []runs.CostBucket
	total      float64
	err        error
	calls      int
	lastOrg    string
	lastFrom   time.Time
	lastTo     time.Time
	lastGroupB runs.CostGroupBy
}

func (s *stubRunsAggregator) AggregateCostsCtx(_ context.Context, orgID string, from, to time.Time, groupBy runs.CostGroupBy) ([]runs.CostBucket, float64, error) {
	s.calls++
	s.lastOrg, s.lastFrom, s.lastTo, s.lastGroupB = orgID, from, to, groupBy
	if s.err != nil {
		return nil, 0, s.err
	}
	return s.buckets, s.total, nil
}

// stubStepCounter implements StepCountAggregator.
type stubStepCounter struct {
	count    int64
	err      error
	calls    int
	lastOrg  string
	lastType string
	lastFrom time.Time
	lastTo   time.Time
}

func (s *stubStepCounter) AggregateStepCountsCtx(_ context.Context, orgID, stepType string, from, to time.Time) (int64, error) {
	s.calls++
	s.lastOrg, s.lastType, s.lastFrom, s.lastTo = orgID, stepType, from, to
	if s.err != nil {
		return 0, s.err
	}
	return s.count, nil
}

func TestRunsMeterSourceAggregationMath(t *testing.T) {
	runsAgg := &stubRunsAggregator{buckets: []runs.CostBucket{
		{Bucket: "2026-01-01", Runs: 3, CostCents: 1.5},
		{Bucket: "2026-01-02", Runs: 4, CostCents: 2.5},
		{Bucket: "2026-01-03", Runs: 0, CostCents: 0}, // empty day is noise
	}}
	steps := &stubStepCounter{count: 9}
	src := NewRunsMeterSource(runsAgg, steps)

	from := base.Add(-24 * time.Hour)
	to := base.Add(48 * time.Hour)
	meters, err := src.MetersForPeriod(context.Background(), "org-1", from, to)
	if err != nil {
		t.Fatalf("MetersForPeriod returned error: %v", err)
	}
	if meters.RunsCount != 7 {
		t.Fatalf("RunsCount: expected 7 (3+4+0), got %d", meters.RunsCount)
	}
	if meters.ToolCallsCount != 9 {
		t.Fatalf("ToolCallsCount: expected 9, got %d", meters.ToolCallsCount)
	}
	// Provenance: the run counts come from the same aggregator as
	// GET /usage/costs, org-scoped, half-open window, grouped by day.
	if runsAgg.calls != 1 || runsAgg.lastOrg != "org-1" || runsAgg.lastGroupB != runs.CostGroupByDay {
		t.Fatalf("unexpected runs aggregator call: calls=%d org=%s groupBy=%s", runsAgg.calls, runsAgg.lastOrg, runsAgg.lastGroupB)
	}
	if !runsAgg.lastFrom.Equal(from) || !runsAgg.lastTo.Equal(to) {
		t.Fatalf("runs aggregator received window [%v,%v), expected [%v,%v)", runsAgg.lastFrom, runsAgg.lastTo, from, to)
	}
	if steps.calls != 1 || steps.lastOrg != "org-1" || steps.lastType != runs.StepTypeTool {
		t.Fatalf("unexpected step counter call: calls=%d org=%s type=%s", steps.calls, steps.lastOrg, steps.lastType)
	}
	if !steps.lastFrom.Equal(from) || !steps.lastTo.Equal(to) {
		t.Fatalf("step counter received window [%v,%v), expected [%v,%v)", steps.lastFrom, steps.lastTo, from, to)
	}
}

func TestRunsMeterSourceWindowValidation(t *testing.T) {
	runsAgg := &stubRunsAggregator{}
	steps := &stubStepCounter{}
	src := NewRunsMeterSource(runsAgg, steps)

	cases := []struct {
		name     string
		from, to time.Time
	}{
		{"zero from", time.Time{}, base},
		{"zero to", base, time.Time{}},
		{"inverted", base, base.Add(-time.Hour)},
		{"empty", base, base},
		{"over 366 days", base, base.Add(367 * 24 * time.Hour)},
	}
	for _, tc := range cases {
		meters, err := src.MetersForPeriod(context.Background(), "org-1", tc.from, tc.to)
		if !errors.Is(err, ErrInvalidPeriod) {
			t.Fatalf("%s: expected ErrInvalidPeriod, got %v", tc.name, err)
		}
		if meters != nil {
			t.Fatalf("%s: expected nil meters on error, got %v", tc.name, meters)
		}
	}
	if runsAgg.calls != 0 || steps.calls != 0 {
		t.Fatalf("aggregators must not be consulted for invalid windows (runs=%d steps=%d)", runsAgg.calls, steps.calls)
	}
}

func TestRunsMeterSourceNilDependenciesYieldZeros(t *testing.T) {
	// Fully unwired: meters are honest zeros (mirrors RunsUsageSource, which
	// yields an empty ledger when unwired) — never an error, never a fake
	// number other than the documented 0.
	src := NewRunsMeterSource(nil, nil)
	meters, err := src.MetersForPeriod(context.Background(), "org-1", base.Add(-time.Hour), base)
	if err != nil {
		t.Fatalf("MetersForPeriod with nil deps returned error: %v", err)
	}
	if meters == nil || meters.RunsCount != 0 || meters.ToolCallsCount != 0 {
		t.Fatalf("expected zero meters, got %+v", meters)
	}
	// Half-wired: the wired meter aggregates, the unwired one stays 0.
	src = NewRunsMeterSource(&stubRunsAggregator{buckets: []runs.CostBucket{{Runs: 5}}}, nil)
	meters, err = src.MetersForPeriod(context.Background(), "org-1", base.Add(-time.Hour), base)
	if err != nil {
		t.Fatalf("MetersForPeriod returned error: %v", err)
	}
	if meters.RunsCount != 5 || meters.ToolCallsCount != 0 {
		t.Fatalf("expected runs=5 tool_calls=0, got %+v", meters)
	}
}

func TestRunsMeterSourceErrorPropagation(t *testing.T) {
	boom := errors.New("ledger unavailable")

	runsAgg := &stubRunsAggregator{err: boom}
	src := NewRunsMeterSource(runsAgg, &stubStepCounter{})
	if _, err := src.MetersForPeriod(context.Background(), "org-1", base.Add(-time.Hour), base); !errors.Is(err, boom) {
		t.Fatalf("expected runs aggregator error, got %v", err)
	}

	steps := &stubStepCounter{err: boom}
	src = NewRunsMeterSource(&stubRunsAggregator{}, steps)
	if _, err := src.MetersForPeriod(context.Background(), "org-1", base.Add(-time.Hour), base); !errors.Is(err, boom) {
		t.Fatalf("expected step counter error, got %v", err)
	}
}
