package billing

import (
	"context"
	"time"

	"agentos/internal/runs"
)

// ---------------------------------------------------------------------------
// Org-scoped usage-meter aggregation (issue #57).
//
// MetersForPeriod aggregates the metered activity of one organization over a
// half-open [from, to) window into the meter set that the usage-meter HTTP
// surface (GET /v1/usage/meters) reports and the optional Stripe sync posts
// to Stripe's /v1/usage_records:
//
//	runs_count        — metered runs in the window. Source: the SAME
//	                    runs.cost_cents aggregation that powers
//	                    GET /v1/usage/costs and invoice run lines
//	                    (RunsCostAggregator, see usage.go); only the run
//	                    COUNTS are taken here, costs are never re-read.
//	tool_calls_count  — metered tool executions in the window. Source:
//	                    run_steps rows with step_type='tool' (runtime writes
//	                    them on every tool step; see internal/runs/metering.go
//	                    for the additive aggregate).
//
// SANDBOX SECONDS — documented omission (never invent data): the platform
// does not record sandbox execution duration anywhere durable today.
// internal/sandbox executes tools in a process-isolated runner but meters
// nothing; run_steps carries no aggregate duration; the usage_records table
// (internal/usage) has NO production writer. There is therefore no honest
// source for a sandbox_seconds meter, so the meter is intentionally absent
// from MetersForPeriod and from the HTTP response. When a durable sandbox
// duration ledger exists, add MeterSandboxSeconds here and to the fragment
// (api/fragments/usage-meters.yaml) — nothing else needs to change.
// ---------------------------------------------------------------------------

// Meter names as they appear on the wire (snake_case).
const (
	MeterRunsCount      = "runs_count"
	MeterToolCallsCount = "tool_calls_count"
)

// maxMeterWindowDays caps one meter window (defense in depth; mirrors
// maxInvoiceWindowDays and the usage-costs report cap).
const maxMeterWindowDays = 366

// Meters is the aggregated meter set for one org over one window. Values are
// counts of real recorded events; a meter with no backing source stays 0
// (see the nil-dependency contract on RunsMeterSource).
type Meters struct {
	RunsCount      int64
	ToolCallsCount int64
}

// MeterSource is the metering-facing view over recorded activity. Implement
// it to feed the usage-meter endpoint (and, when enabled, the Stripe usage
// sync) from any ledger; the shipped implementation is RunsMeterSource.
type MeterSource interface {
	MetersForPeriod(ctx context.Context, orgID string, from, to time.Time) (*Meters, error)
}

// StepCountAggregator is the slice of the runs service the meter source
// consumes for tool-call counts; satisfied by *runs.Service.AggregateStepCountsCtx.
type StepCountAggregator interface {
	AggregateStepCountsCtx(ctx context.Context, orgID, stepType string, from, to time.Time) (int64, error)
}

// RunsMeterSource aggregates meters from the runs service: run counts from
// the existing cost aggregation (group_by=day run totals) and tool-call
// counts from the step-type aggregate.
type RunsMeterSource struct {
	runCounts  RunsCostAggregator
	stepCounts StepCountAggregator
}

// NewRunsMeterSource wires the meter source to the runs ledger. Production
// wiring (cmd/api/main.go, orchestrator-owned):
//
//	billing.NewRunsMeterSource(runsSvc, runsSvc)
//
// Both arguments are satisfied by *runs.Service. NIL-DEPENDENCY CONTRACT: a
// nil aggregator contributes 0 for its meter (mirroring RunsUsageSource,
// which yields an empty ledger when unwired) instead of failing the read —
// meters are observability data and must not turn a reporting window into a
// 500. Window validation errors still fail loudly (below).
func NewRunsMeterSource(runCounts RunsCostAggregator, stepCounts StepCountAggregator) *RunsMeterSource {
	return &RunsMeterSource{runCounts: runCounts, stepCounts: stepCounts}
}

// MetersForPeriod implements MeterSource. The window must be a non-empty
// half-open interval of at most maxMeterWindowDays (ErrInvalidPeriod
// otherwise, mapped to 400 VALIDATION_ERROR by the HTTP layer).
func (r *RunsMeterSource) MetersForPeriod(ctx context.Context, orgID string, from, to time.Time) (*Meters, error) {
	if err := validateMeterPeriod(from, to); err != nil {
		return nil, err
	}
	meters := &Meters{}
	if r.runCounts != nil {
		// group_by=day: one bucket per UTC day; the SUM of bucket run counts
		// is exactly the window's metered-run total (each run lands in
		// exactly one day bucket — no double counting, same numbers as the
		// usage-cost report).
		buckets, _, err := r.runCounts.AggregateCostsCtx(ctx, orgID, from, to, runs.CostGroupByDay)
		if err != nil {
			return nil, err
		}
		for _, bucket := range buckets {
			meters.RunsCount += bucket.Runs
		}
	}
	if r.stepCounts != nil {
		n, err := r.stepCounts.AggregateStepCountsCtx(ctx, orgID, runs.StepTypeTool, from, to)
		if err != nil {
			return nil, err
		}
		meters.ToolCallsCount = n
	}
	return meters, nil
}

// validateMeterPeriod enforces the window contract shared by the meter and
// margin reads.
func validateMeterPeriod(from, to time.Time) error {
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return ErrInvalidPeriod
	}
	if to.Sub(from).Hours()/24 > maxMeterWindowDays {
		return ErrInvalidPeriod
	}
	return nil
}
