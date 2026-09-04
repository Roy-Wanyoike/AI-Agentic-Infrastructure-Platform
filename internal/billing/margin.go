package billing

import (
	"context"
	"math"
	"time"
)

// ---------------------------------------------------------------------------
// Per-tenant margin (issue #57).
//
// MarginReport answers, for the org's CURRENT subscription period, the one
// revenue question the invoice engine deliberately does not: how much of the
// recurring plan price is left after the metered model spend.
//
// EXACT FORMULA (documented; every input is an existing, auditable number):
//
//      revenue_cents   = plan.price_cents                       (Plan.PriceCents;
//                        migration 016 plans.price_cents — the recurring
//                        monthly catalog price of the subscription's plan)
//      cost_cents      = Σ usage.cost_cents over the half-open
//                        [period_start, period_end) of the current subscription
//                        (the UsageSource ledger — production: RunsUsageSource
//                        over runs.cost_cents, the EXACT aggregation behind
//                        GET /v1/usage/costs and invoice run lines; see usage.go)
//      margin_cents    = revenue_cents − round(cost_cents)      (half away from
//                        zero — the same rounding invoice lines use)
//      margin_percent  = (1 − cost_cents / revenue_cents) × 100  when
//                        revenue_cents > 0; UNDEFINED (omitted on the wire)
//                        when the plan price is 0 — a zero-price plan has no
//                        meaningful percentage.
//
// Deliberate scopes and edge cases:
//
//   - The window is the subscription's own live period (half-open), the SAME
//     window the quota check re-counts (quotaFor) — margin and quota always
//     describe the same slice of time. For a canceled subscription the latest
//     row's last period is reported (GetCurrentSubscriptionCtx fallback).
//   - Overage is NOT folded into revenue. plan.metadata
//     {"overage_run_rate_cents": N} prices runs beyond included_quota on
//     INVOICES, but it is contingent billing, not committed revenue; margin
//     is price-vs-cost of the committed subscription.
//   - Unlimited plans (IncludedQuota == 0) compute identically: quota shapes
//     overage billing, not the price/cost margin.
//   - Zero usage → cost 0 → margin equals the full plan price (100%).
//   - cost > price → NEGATIVE margin, reported honestly (no clamping).
//   - No UsageSource wired (zero-infrastructure mode) → cost basis 0 — the
//     same provenance as invoice generation in that mode (GenerateInvoiceCtx
//     with a nil UsageSource produces no run lines).
//   - UsageSource failures are PROPAGATED, never swallowed (same rule as
//     quotaFor: a faked 0 cost would overstate margin, the one error margin
//     must never make).
//   - No subscription → ErrNoSubscription; the plan row vanished behind the
//     FK guard → ErrPlanNotFound (margin cannot exist without a price —
//     unlike quotaFor's unlimited fallback there is nothing to compute).
// ---------------------------------------------------------------------------

// MarginReport is the computed margin view for one organization's current
// subscription period. cmd/api renders it snake_case; this struct carries
// the exact numbers so tests can pin the formula.
type MarginReport struct {
	OrganizationID string
	SubscriptionID string
	// Subscription status (trial|active|past_due|canceled) of the row whose
	// period the margin describes.
	Status string
	// Period is the half-open [PeriodStart, PeriodEnd) the cost basis was
	// computed over (the subscription's current period).
	PeriodStart time.Time
	PeriodEnd   time.Time
	PlanID      string
	PlanName    string
	Currency    string
	PriceCents  int64 // revenue side: plan.price_cents
	// IncludedQuota/Unlimited are echoed for context; they never change the
	// margin math (see the documented scopes above).
	IncludedQuota int64
	Unlimited     bool
	// UsageCostCents is the exact (unrounded) cost basis. NOTE: invoice lines
	// round PER ROW before summing; margin rounds the window total once, so
	// margin_cents and (price − invoice_run_subtotal) can differ by fractions
	// of a cent. The exact aggregate is the honest basis; the invoice
	// rounding difference is bounded by half a cent per line.
	UsageCostCents float64
	// MarginCents = PriceCents − round(UsageCostCents).
	MarginCents int64
	// MarginPercent = (1 − cost/price) × 100, nil when PriceCents == 0
	// (percentage of a zero-price plan is undefined, never fabricated).
	MarginPercent *float64
}

// ComputeMarginCtx computes the margin report for the org's current
// subscription period (see the formula documentation above).
func (s *Service) ComputeMarginCtx(ctx context.Context, orgID string) (*MarginReport, error) {
	sub, err := s.GetCurrentSubscriptionCtx(ctx, orgID)
	if err != nil {
		return nil, err
	}
	plan, err := s.store.GetPlan(ctx, sub.PlanID)
	if err != nil {
		return nil, err
	}

	var cost float64
	if s.usage != nil {
		rows, err := s.usage.UsageForPeriod(ctx, sub.OrganizationID, sub.PeriodStart, sub.PeriodEnd)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			cost += row.CostCents
		}
	}

	report := &MarginReport{
		OrganizationID: sub.OrganizationID,
		SubscriptionID: sub.ID,
		Status:         sub.Status,
		PeriodStart:    sub.PeriodStart,
		PeriodEnd:      sub.PeriodEnd,
		PlanID:         plan.ID,
		PlanName:       plan.Name,
		Currency:       plan.Currency,
		PriceCents:     plan.PriceCents,
		IncludedQuota:  plan.IncludedQuota,
		Unlimited:      plan.IncludedQuota == 0,
		UsageCostCents: cost,
		MarginCents:    plan.PriceCents - int64(math.Round(cost)),
	}
	if plan.PriceCents > 0 {
		pct := (1 - cost/float64(plan.PriceCents)) * 100
		report.MarginPercent = &pct
	}
	return report, nil
}
