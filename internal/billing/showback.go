package billing

// ---------------------------------------------------------------------------
// Showback / chargeback reports (wave-8 issue C).
//
// ShowbackReport answers the Finance question "what did each agent cost us
// over this window?" from the SAME usage ledger the usage-cost report and
// the usage meters read (runs.cost_cents / run counts per agent). Nothing
// here re-computes or re-stores cost: every number is an existing, auditable
// aggregate, so a showback row for an agent always reconciles with
// GET /v1/usage/costs?group_by=agent of the same window.
//
// Documented limitations (the "never invent data" convention of meters.go):
//
//   - ToolCalls: the platform records tool steps durably, but the ONLY
//     exposed aggregate (AggregateStepCountsCtx) is org-wide, not per-agent.
//     Per-row ToolCalls is therefore a schema-supported seam that stays 0 in
//     the shipped adapter until a per-agent step aggregate exists (mirrors
//     the sandbox-seconds omission and the "eval" invoice-line seam). Tests
//     may inject sources that report it.
//   - TokensIn/TokensOut: token usage lives in free-form per-step TokenUsage
//     metadata with NO durable aggregate anywhere, so there is no honest
//     per-agent token source; the fields stay 0 in the shipped adapter for
//     the same reason.
//   - Team grouping: runs carry organization_id + agent_id only — the ledger
//     has NO team attribute. Per the feature contract, group_by=team falls
//     back to agent grouping and records the limitation in Report.Notes;
//     team rows are only ever emitted when a source actually reports them.
//   - Zero-activity agents produce NO row (ledger GROUP BY semantics): a
//     showback is a statement of metered activity, not an agent inventory.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"agentos/internal/runs"
)

// Showback groupings.
const (
	ShowbackGroupByAgent = "agent"
	ShowbackGroupByTeam  = "team"
)

// ShowbackMaxWindowDays caps one report window (a fiscal quarter at most —
// showback is a Finance surface, not an audit export). Exported so the HTTP
// layer validates against the same constant the service enforces.
const ShowbackMaxWindowDays = 92

// ErrInvalidShowbackGroupBy is returned when group_by is outside agent|team;
// the HTTP handler maps it to 400 INVALID_GROUP_BY (the usage-costs code).
var ErrInvalidShowbackGroupBy = errors.New("billing: group_by must be one of agent, team")

// ShowbackRow is one aggregated scope of a showback report. ScopeRef is the
// agent id (team rows would carry the team id — none exist today, see the
// limitations note above).
type ShowbackRow struct {
	ScopeRef  string
	Runs      int64
	ToolCalls int64
	TokensIn  int64
	TokensOut int64
	CostCents float64
}

// ShowbackReport is the computed report. Rows are sorted by ScopeRef so the
// wire output is deterministic.
type ShowbackReport struct {
	From    time.Time
	To      time.Time
	GroupBy string // as requested ("team" stays "team" even in fallback)
	Rows    []*ShowbackRow
	// Notes carries the honest limitations of this report (team fallback,
	// unattributable counters) so a consumer never mistakes an absent number
	// for a zero.
	Notes []string
}

// ShowbackUsage is one per-agent slice of metered usage injected by a
// ShowbackSource.
type ShowbackUsage struct {
	AgentID   string
	Runs      int64
	ToolCalls int64
	TokensIn  int64
	TokensOut int64
	CostCents float64
}

// ShowbackSource is the showback-facing view of the metering layer.
// Implement it to feed reports from any ledger; the shipped implementation
// is RunsShowbackSource over the per-agent cost aggregation.
type ShowbackSource interface {
	ShowbackForPeriod(ctx context.Context, orgID string, from, to time.Time) ([]ShowbackUsage, error)
}

// RunsShowbackSource adapts the runs service per-agent cost aggregation into
// showback usage rows. Production wiring (cmd/api):
//
//	billing.NewRunsShowbackSource(runsSvc)
//
// NIL-DEPENDENCY CONTRACT (mirrors RunsUsageSource / RunsConsumptionSource):
// a nil adapter or nil aggregator contributes no rows instead of failing the
// read — showback is observability data and must not turn a reporting window
// into a 500. Window validation errors still fail loudly (ShowbackReport).
type RunsShowbackSource struct {
	aggregator RunsCostAggregator
}

// NewRunsShowbackSource wires the showback service to the runs cost ledger
// (group_by=agent: one row per agent with run count and token cost).
func NewRunsShowbackSource(aggregator RunsCostAggregator) *RunsShowbackSource {
	return &RunsShowbackSource{aggregator: aggregator}
}

// ShowbackForPeriod implements ShowbackSource.
func (r *RunsShowbackSource) ShowbackForPeriod(ctx context.Context, orgID string, from, to time.Time) ([]ShowbackUsage, error) {
	if r == nil || r.aggregator == nil {
		return nil, nil
	}
	buckets, _, err := r.aggregator.AggregateCostsCtx(ctx, orgID, from, to, runs.CostGroupByAgent)
	if err != nil {
		return nil, err
	}
	out := make([]ShowbackUsage, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, ShowbackUsage{
			AgentID:   bucket.AgentID,
			Runs:      bucket.Runs,
			CostCents: bucket.CostCents,
		})
	}
	return out, nil
}

// ShowbackService builds showback reports from an injected ShowbackSource.
type ShowbackService struct {
	source ShowbackSource
}

// NewShowbackService returns a showback service over the given source
// (production: NewRunsShowbackSource(runsSvc); tests: fakes).
func NewShowbackService(source ShowbackSource) *ShowbackService {
	return &ShowbackService{source: source}
}

// ShowbackReport aggregates the org's metered usage over the half-open
// [from, to) window. Validation: from < to (ErrInvalidPeriod otherwise), the
// window is capped at showbackMaxWindowDays (92 days) and group_by must be
// agent|team (ErrInvalidShowbackGroupBy).
//
// Grouping: "agent" reports one row per agent with activity. "team" would
// group by team, but the ledger carries no team attribute, so it falls back
// to agent grouping and records the limitation in Notes (team rows are added
// only when a source actually reports them — never fabricated here).
func (s *ShowbackService) ShowbackReport(ctx context.Context, orgID string, from, to time.Time, groupBy string) (*ShowbackReport, error) {
	if orgID == "" {
		return nil, errors.New("billing: organization id is required")
	}
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, fmt.Errorf("%w: from must be before to", ErrInvalidPeriod)
	}
	if to.Sub(from).Hours()/24 > ShowbackMaxWindowDays {
		return nil, fmt.Errorf("%w: window exceeds the maximum of %d days", ErrInvalidPeriod, ShowbackMaxWindowDays)
	}
	switch groupBy {
	case ShowbackGroupByAgent, ShowbackGroupByTeam:
	default:
		return nil, ErrInvalidShowbackGroupBy
	}

	report := &ShowbackReport{From: from, To: to, GroupBy: groupBy}
	var usage []ShowbackUsage
	if s.source != nil {
		var err error
		if usage, err = s.source.ShowbackForPeriod(ctx, orgID, from, to); err != nil {
			return nil, err
		}
	}
	for _, u := range usage {
		report.Rows = append(report.Rows, &ShowbackRow{
			ScopeRef:  u.AgentID,
			Runs:      u.Runs,
			ToolCalls: u.ToolCalls,
			TokensIn:  u.TokensIn,
			TokensOut: u.TokensOut,
			CostCents: u.CostCents,
		})
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		return report.Rows[i].ScopeRef < report.Rows[j].ScopeRef
	})
	if groupBy == ShowbackGroupByTeam {
		report.Notes = append(report.Notes,
			"no team attribute exists in the usage ledger; rows are grouped by agent (team attribution unavailable)")
	}
	return report, nil
}
