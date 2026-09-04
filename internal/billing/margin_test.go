package billing

// Margin computation tests (issue #57): the exact formula
// margin_cents = plan.price_cents − round(Σ usage.cost_cents) over the
// subscription's current period, plus every documented edge case (no
// subscription, vanished plan, unlimited plan, zero usage, negative margin,
// zero price, unwired usage source, usage-source failure propagation).

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// periodStubUsage records the window it was asked about.
type periodStubUsage struct {
	rows             []UsageRow
	err              error
	lastOrg          string
	lastFrom, lastTo time.Time
	calls            int
}

func (s *periodStubUsage) UsageForPeriod(_ context.Context, orgID string, from, to time.Time) ([]UsageRow, error) {
	s.calls++
	s.lastOrg, s.lastFrom, s.lastTo = orgID, from, to
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func newMarginService(t *testing.T, at *time.Time, usage UsageSource) *Service {
	t.Helper()
	s := newTestService(at)
	if usage != nil {
		s.SetUsageSource(usage)
	}
	return s
}

func TestComputeMarginNoSubscription(t *testing.T) {
	s := newMarginService(t, &base, nil)
	if _, err := s.ComputeMarginCtx(context.Background(), "org-empty"); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("expected ErrNoSubscription, got %v", err)
	}
}

func TestComputeMarginFormula(t *testing.T) {
	usage := &periodStubUsage{rows: []UsageRow{
		{Source: LineSourceRun, Model: "gpt-4o-mini", Runs: 12, CostCents: 3.5},
		{Source: LineSourceRun, Model: "gpt-4o", Runs: 5, CostCents: 12.25},
	}}
	s := newMarginService(t, &base, usage)
	plan := mustPlan(t, s, "starter-margin", 1900, 10, nil)
	sub := mustSubscribe(t, s, "org-margin", plan.ID)

	report, err := s.ComputeMarginCtx(context.Background(), "org-margin")
	if err != nil {
		t.Fatalf("ComputeMarginCtx returned error: %v", err)
	}
	// Formula: 1900 − round(3.5 + 12.25) = 1900 − 16 = 1884.
	if report.UsageCostCents != 15.75 {
		t.Fatalf("UsageCostCents: expected 15.75, got %v", report.UsageCostCents)
	}
	if report.MarginCents != 1884 {
		t.Fatalf("MarginCents: expected 1884, got %d", report.MarginCents)
	}
	if report.MarginPercent == nil {
		t.Fatal("MarginPercent must be set for a priced plan")
	}
	// (1 − 15.75/1900) × 100 ≈ 99.171052631578947
	if math.Abs(*report.MarginPercent-99.171052631578947) > 1e-9 {
		t.Fatalf("MarginPercent: expected ≈99.171052631578947, got %v", *report.MarginPercent)
	}
	// Context fields.
	if report.PlanID != plan.ID || report.PlanName != "starter-margin" || report.Currency != "usd" {
		t.Fatalf("unexpected plan echo: %+v", report)
	}
	if report.SubscriptionID != sub.ID || report.Status != StatusTrial {
		t.Fatalf("unexpected subscription echo: %+v", report)
	}
	if report.IncludedQuota != 10 || report.Unlimited {
		t.Fatalf("unexpected quota echo: quota=%d unlimited=%v", report.IncludedQuota, report.Unlimited)
	}
	// The cost basis MUST be computed over the subscription's own period
	// window (the same window the quota check re-counts).
	if usage.calls != 1 || usage.lastOrg != "org-margin" {
		t.Fatalf("unexpected usage source call: calls=%d org=%s", usage.calls, usage.lastOrg)
	}
	if !usage.lastFrom.Equal(sub.PeriodStart) || !usage.lastTo.Equal(sub.PeriodEnd) {
		t.Fatalf("usage source received window [%v,%v), expected [%v,%v)", usage.lastFrom, usage.lastTo, sub.PeriodStart, sub.PeriodEnd)
	}
}

func TestComputeMarginZeroUsage(t *testing.T) {
	s := newMarginService(t, &base, &periodStubUsage{})
	plan := mustPlan(t, s, "starter-zero", 1900, 10, nil)
	mustSubscribe(t, s, "org-zero", plan.ID)

	report, err := s.ComputeMarginCtx(context.Background(), "org-zero")
	if err != nil {
		t.Fatalf("ComputeMarginCtx returned error: %v", err)
	}
	if report.UsageCostCents != 0 || report.MarginCents != 1900 {
		t.Fatalf("expected full margin (cost 0), got cost=%v margin=%d", report.UsageCostCents, report.MarginCents)
	}
	if report.MarginPercent == nil || *report.MarginPercent != 100 {
		t.Fatalf("expected 100%% margin, got %v", report.MarginPercent)
	}
}

func TestComputeMarginUnlimitedPlan(t *testing.T) {
	// Unlimited (IncludedQuota == 0) changes no margin math: quota shapes
	// overage billing, not price-vs-cost.
	s := newMarginService(t, &base, &periodStubUsage{rows: []UsageRow{
		{Source: LineSourceRun, Runs: 999, CostCents: 500},
	}})
	plan := mustPlan(t, s, "unlimited-margin", 4900, 0, nil)
	mustSubscribe(t, s, "org-unl", plan.ID)

	report, err := s.ComputeMarginCtx(context.Background(), "org-unl")
	if err != nil {
		t.Fatalf("ComputeMarginCtx returned error: %v", err)
	}
	if !report.Unlimited || report.IncludedQuota != 0 {
		t.Fatalf("expected unlimited plan echo, got %+v", report)
	}
	if report.MarginCents != 4400 {
		t.Fatalf("MarginCents: expected 4400 (4900−500), got %d", report.MarginCents)
	}
}

func TestComputeMarginNegativeMargin(t *testing.T) {
	// Cost above price is an honest loss, never clamped.
	s := newMarginService(t, &base, &periodStubUsage{rows: []UsageRow{
		{Source: LineSourceRun, Runs: 400, CostCents: 2500.4},
	}})
	plan := mustPlan(t, s, "starter-loss", 1900, 10, nil)
	mustSubscribe(t, s, "org-loss", plan.ID)

	report, err := s.ComputeMarginCtx(context.Background(), "org-loss")
	if err != nil {
		t.Fatalf("ComputeMarginCtx returned error: %v", err)
	}
	if report.MarginCents != -600 { // 1900 − round(2500.4)=1900−2500
		t.Fatalf("MarginCents: expected -600, got %d", report.MarginCents)
	}
	if report.MarginPercent == nil || *report.MarginPercent >= 0 {
		t.Fatalf("expected negative margin percent, got %v", report.MarginPercent)
	}
}

func TestComputeMarginZeroPriceOmitsPercent(t *testing.T) {
	// A zero-price plan has no meaningful percentage: MarginPercent is nil.
	s := newMarginService(t, &base, &periodStubUsage{rows: []UsageRow{
		{Source: LineSourceRun, Runs: 3, CostCents: 12.4},
	}})
	plan := mustPlan(t, s, "free-tier-margin", 0, 5, nil)
	mustSubscribe(t, s, "org-free", plan.ID)

	report, err := s.ComputeMarginCtx(context.Background(), "org-free")
	if err != nil {
		t.Fatalf("ComputeMarginCtx returned error: %v", err)
	}
	if report.MarginCents != -12 { // 0 − round(12.4)
		t.Fatalf("MarginCents: expected -12, got %d", report.MarginCents)
	}
	if report.MarginPercent != nil {
		t.Fatalf("MarginPercent must be nil for a zero-price plan, got %v", *report.MarginPercent)
	}
}

func TestComputeMarginWithoutUsageSource(t *testing.T) {
	// Zero-infrastructure mode: no UsageSource wired — cost basis 0 (the same
	// provenance as invoice generation there), margin equals the full price.
	s := newMarginService(t, &base, nil)
	plan := mustPlan(t, s, "starter-nosrc", 1900, 10, nil)
	mustSubscribe(t, s, "org-nosrc", plan.ID)

	report, err := s.ComputeMarginCtx(context.Background(), "org-nosrc")
	if err != nil {
		t.Fatalf("ComputeMarginCtx returned error: %v", err)
	}
	if report.UsageCostCents != 0 || report.MarginCents != 1900 {
		t.Fatalf("expected full margin, got cost=%v margin=%d", report.UsageCostCents, report.MarginCents)
	}
}

func TestComputeMarginUsageErrorPropagates(t *testing.T) {
	// A failed ledger must NOT be reported as margin 100%: errors propagate
	// (same rule as quotaFor).
	boom := errors.New("cost ledger unavailable")
	s := newMarginService(t, &base, &periodStubUsage{err: boom})
	plan := mustPlan(t, s, "starter-boom", 1900, 10, nil)
	mustSubscribe(t, s, "org-boom", plan.ID)

	if _, err := s.ComputeMarginCtx(context.Background(), "org-boom"); !errors.Is(err, boom) {
		t.Fatalf("expected usage source error, got %v", err)
	}
}

func TestComputeMarginPlanVanished(t *testing.T) {
	// The plan row disappeared behind the FK guard: margin cannot exist
	// without a price -> ErrPlanNotFound (NOT the quota unlimited fallback).
	s := newMarginService(t, &base, &periodStubUsage{})
	plan := mustPlan(t, s, "ghost-plan", 1900, 10, nil)
	mustSubscribe(t, s, "org-ghost", plan.ID)
	s.store.(*memoryStore).plans = nil // white-box: simulate the vanished row

	if _, err := s.ComputeMarginCtx(context.Background(), "org-ghost"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("expected ErrPlanNotFound, got %v", err)
	}
}
