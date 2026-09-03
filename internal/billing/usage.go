package billing

import (
	"context"
	"time"

	"agentos/internal/runs"
)

// UsageRow is one metered billing row injected into invoice generation and
// quota counting. Billing owns this shape so the metering side never has to
// know about invoice lines.
type UsageRow struct {
	// Source is the invoice-line source: "run" or "eval" (empty -> "run";
	// anything else is rejected with ErrInvalidUsageRow). Overage lines are
	// derived, never injected.
	Source string
	// Model is the serving model label of the bucket (optional; recorded in
	// the line refs for auditability).
	Model string
	// Runs is the metered run count of the row (drives the quota budget).
	Runs int64
	// CostCents is the fractional token cost of the row (rounded to whole
	// cents on the invoice line, half away from zero).
	CostCents float64
}

// UsageSource is the billing-facing view of the metering layer. Implement it
// to feed invoices/quota from any cost ledger; the shipped implementation is
// RunsUsageSource (runs.cost_cents aggregates — the SAME numbers behind
// GET /v1/usage/costs, so an invoice always reconciles with the usage report
// of the same window and billing never duplicates cost storage).
type UsageSource interface {
	UsageForPeriod(ctx context.Context, orgID string, from, to time.Time) ([]UsageRow, error)
}

// RunsCostAggregator is the slice of the runs service billing consumes; it is
// satisfied by *runs.Service.AggregateCostsCtx.
type RunsCostAggregator interface {
	AggregateCostsCtx(ctx context.Context, orgID string, from, to time.Time, groupBy runs.CostGroupBy) ([]runs.CostBucket, float64, error)
}

// RunsUsageSource adapts the runs service cost aggregation into billing usage
// rows: one row per model bucket (group_by=model), source="run". The window
// is passed through untouched (half-open [from, to), UTC) so invoice windows
// and quota windows aggregate exactly the runs the usage-cost report shows.
type RunsUsageSource struct {
	aggregator RunsCostAggregator
}

// NewRunsUsageSource wires billing to the runs cost ledger.
// Production wiring: billing.NewServiceWithStore(billing.NewPostgresStore(db),
// billing.NewRunsUsageSource(runsSvc)).
func NewRunsUsageSource(aggregator RunsCostAggregator) *RunsUsageSource {
	return &RunsUsageSource{aggregator: aggregator}
}

func (r *RunsUsageSource) UsageForPeriod(ctx context.Context, orgID string, from, to time.Time) ([]UsageRow, error) {
	if r == nil || r.aggregator == nil {
		return nil, nil
	}
	// group_by=model gives one bucket per model with its run count and token
	// cost — exactly the run-line granularity of an invoice.
	buckets, _, err := r.aggregator.AggregateCostsCtx(ctx, orgID, from, to, runs.CostGroupByModel)
	if err != nil {
		return nil, err
	}
	out := make([]UsageRow, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, UsageRow{
			Source:    LineSourceRun,
			Model:     bucket.Model,
			Runs:      bucket.Runs,
			CostCents: bucket.CostCents,
		})
	}
	return out, nil
}
