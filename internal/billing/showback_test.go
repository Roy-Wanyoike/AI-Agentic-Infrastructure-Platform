package billing

// Showback service tests (wave-8 issue C): aggregation correctness over a
// multi-agent / multi-day window, validation (window + group_by), the team
// fallback limitation, the zero-activity omission and the runs adapter.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agentos/internal/runs"
)

type stubShowbackSource struct {
	usage []ShowbackUsage
	err   error
}

func (s *stubShowbackSource) ShowbackForPeriod(_ context.Context, _ string, _, _ time.Time) ([]ShowbackUsage, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.usage, nil
}

func TestShowbackReportAggregatesPerAgent(t *testing.T) {
	// Multi-agent source; a real multi-DAY window is exercised through the
	// runs adapter below (the ledger already bucketed the days away).
	src := &stubShowbackSource{usage: []ShowbackUsage{
		{AgentID: "agent-b", Runs: 7, CostCents: 42.5},
		{AgentID: "agent-a", Runs: 3, CostCents: 10},
		{AgentID: "agent-c", Runs: 1, CostCents: 2.25},
	}}
	svc := NewShowbackService(src)

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	report, err := svc.ShowbackReport(context.Background(), "org-1", from, to, ShowbackGroupByAgent)
	if err != nil {
		t.Fatalf("ShowbackReport returned error: %v", err)
	}
	if report.From != from || report.To != to || report.GroupBy != ShowbackGroupByAgent {
		t.Fatalf("report header mismatch: %+v", report)
	}
	// Rows are sorted by ScopeRef for deterministic output.
	if len(report.Rows) != 3 ||
		report.Rows[0].ScopeRef != "agent-a" || report.Rows[1].ScopeRef != "agent-b" || report.Rows[2].ScopeRef != "agent-c" {
		t.Fatalf("rows not sorted/scoped as documented: %+v", report.Rows)
	}
	if report.Rows[0].Runs != 3 || report.Rows[0].CostCents != 10 {
		t.Fatalf("row aggregation mismatch: %+v", report.Rows[0])
	}
	var runs int64
	var cost float64
	for _, row := range report.Rows {
		runs += row.Runs
		cost += row.CostCents
	}
	if runs != 11 || cost != 54.75 {
		t.Fatalf("totals mismatch: runs=%d cost=%f", runs, cost)
	}
	if len(report.Notes) != 0 {
		t.Fatalf("agent grouping needs no notes: %+v", report.Notes)
	}
}

func TestShowbackReportMultiDayWindowValidation(t *testing.T) {
	svc := NewShowbackService(&stubShowbackSource{})
	ctx := context.Background()

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	sameDay := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// from == to and from > to are invalid...
	if _, err := svc.ShowbackReport(ctx, "org-1", from, from, ShowbackGroupByAgent); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("zero-length window: want ErrInvalidPeriod, got %v", err)
	}
	if _, err := svc.ShowbackReport(ctx, "org-1", sameDay, from, ShowbackGroupByAgent); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("inverted window: want ErrInvalidPeriod, got %v", err)
	}
	if _, err := svc.ShowbackReport(ctx, "org-1", time.Time{}, from, ShowbackGroupByAgent); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("zero from: want ErrInvalidPeriod, got %v", err)
	}
	// ...and the window is capped at 92 days (93 fails, 92 passes).
	to93 := from.AddDate(0, 0, 93)
	to92 := from.AddDate(0, 0, 92)
	if _, err := svc.ShowbackReport(ctx, "org-1", from, to93, ShowbackGroupByAgent); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("93-day window: want ErrInvalidPeriod, got %v", err)
	}
	if _, err := svc.ShowbackReport(ctx, "org-1", from, to92, ShowbackGroupByAgent); err != nil {
		t.Fatalf("92-day window should pass: %v", err)
	}
	// Unknown group_by is refused (400 INVALID_GROUP_BY on the wire).
	if _, err := svc.ShowbackReport(ctx, "org-1", from, to92, "model"); !errors.Is(err, ErrInvalidShowbackGroupBy) {
		t.Fatalf("unknown group_by: want ErrInvalidShowbackGroupBy, got %v", err)
	}
	// Empty org is refused before any ledger read.
	if _, err := svc.ShowbackReport(ctx, "", from, to92, ShowbackGroupByAgent); err == nil {
		t.Fatal("empty org must be refused")
	}
}

func TestShowbackReportTeamFallbackDocumentsLimitation(t *testing.T) {
	src := &stubShowbackSource{usage: []ShowbackUsage{
		{AgentID: "agent-a", Runs: 3, CostCents: 10},
	}}
	svc := NewShowbackService(src)
	report, err := svc.ShowbackReport(context.Background(), "org-1",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		ShowbackGroupByTeam)
	if err != nil {
		t.Fatalf("ShowbackReport(team) returned error: %v", err)
	}
	// The echo stays "team" but rows are agent-grouped...
	if report.GroupBy != ShowbackGroupByTeam || len(report.Rows) != 1 || report.Rows[0].ScopeRef != "agent-a" {
		t.Fatalf("team fallback must keep agent rows: %+v", report)
	}
	// ...and the limitation is recorded, never silently papered over.
	if len(report.Notes) == 0 {
		t.Fatal("team fallback must document the limitation in Notes")
	}
}

func TestShowbackReportZeroActivityAgentOmitted(t *testing.T) {
	// The ledger (GROUP BY agent_id) only reports agents WITH activity; the
	// documented choice is omission — a showback is a statement of metered
	// activity, not an agent inventory.
	src := &stubShowbackSource{usage: []ShowbackUsage{
		{AgentID: "agent-a", Runs: 3, CostCents: 10},
		// agent-idle never appears in ledger output.
	}}
	svc := NewShowbackService(src)
	report, err := svc.ShowbackReport(context.Background(), "org-1",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		ShowbackGroupByAgent)
	if err != nil {
		t.Fatalf("ShowbackReport returned error: %v", err)
	}
	for _, row := range report.Rows {
		if row.ScopeRef == "agent-idle" {
			t.Fatal("zero-activity agents must be omitted (documented choice)")
		}
	}
	if len(report.Rows) != 1 {
		t.Fatalf("expected exactly one row, got %+v", report.Rows)
	}
}

func TestShowbackReportCountersAreSourceProvided(t *testing.T) {
	// ToolCalls/TokensIn/TokensOut are a schema-supported seam: the shipped
	// runs adapter leaves them 0 (documented), but a source that CAN attribute
	// them has the row fields carried through untouched.
	src := &stubShowbackSource{usage: []ShowbackUsage{
		{AgentID: "agent-a", Runs: 2, ToolCalls: 9, TokensIn: 1200, TokensOut: 340, CostCents: 5},
	}}
	svc := NewShowbackService(src)
	report, err := svc.ShowbackReport(context.Background(), "org-1",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		ShowbackGroupByAgent)
	if err != nil {
		t.Fatalf("ShowbackReport returned error: %v", err)
	}
	row := report.Rows[0]
	if row.ToolCalls != 9 || row.TokensIn != 1200 || row.TokensOut != 340 {
		t.Fatalf("source-provided counters not carried through: %+v", row)
	}
}

func TestShowbackReportSourceFailurePropagates(t *testing.T) {
	svc := NewShowbackService(&stubShowbackSource{err: fmt.Errorf("aggregate unavailable")})
	_, err := svc.ShowbackReport(context.Background(), "org-1",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		ShowbackGroupByAgent)
	if err == nil {
		t.Fatal("source failure must propagate (showback must not fake zeros)")
	}
}

func TestRunsShowbackSourceMapsAgentBuckets(t *testing.T) {
	agg := &stubCostAggregator{buckets: []runs.CostBucket{
		{AgentID: "agent-a", Runs: 3, CostCents: 12.5},
		{AgentID: "agent-b", Runs: 1, CostCents: 2},
	}}
	source := NewRunsShowbackSource(agg)
	usage, err := source.ShowbackForPeriod(context.Background(), "org-1", tsA, tsB)
	if err != nil {
		t.Fatalf("ShowbackForPeriod returned error: %v", err)
	}
	if len(usage) != 2 || usage[0].AgentID != "agent-a" || usage[0].Runs != 3 || usage[0].CostCents != 12.5 {
		t.Fatalf("unexpected usage rows: %+v", usage)
	}
	if agg.groupBy != runs.CostGroupByAgent || agg.orgID != "org-1" {
		t.Fatalf("adapter must aggregate by agent for the caller's org: %+v", agg)
	}
	// The shipped adapter leaves tool-call/token counters 0 (documented
	// limitation — no per-agent aggregate exists).
	for _, u := range usage {
		if u.ToolCalls != 0 || u.TokensIn != 0 || u.TokensOut != 0 {
			t.Fatalf("shipped adapter must not invent counters: %+v", u)
		}
	}
	// Nil-dependency contract.
	var nilSource *RunsShowbackSource
	if rows, err := nilSource.ShowbackForPeriod(context.Background(), "org-1", tsA, tsB); err != nil || rows != nil {
		t.Fatalf("nil source must be zero-contribution: %+v err %v", rows, err)
	}
	if rows, err := NewRunsShowbackSource(nil).ShowbackForPeriod(context.Background(), "org-1", tsA, tsB); err != nil || rows != nil {
		t.Fatalf("nil aggregator must be zero-contribution: %+v err %v", rows, err)
	}
}
