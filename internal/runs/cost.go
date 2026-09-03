package runs

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// CostGroupBy selects the aggregation dimension of the tenant usage-costs
// report (GET /v1/usage/costs?group_by=...).
type CostGroupBy string

const (
	// CostGroupByDay buckets by UTC day (bucket = "YYYY-MM-DD").
	CostGroupByDay CostGroupBy = "day"
	// CostGroupByAgent buckets by the run's agent_id.
	CostGroupByAgent CostGroupBy = "agent"
	// CostGroupByModel buckets by the serving model id, resolved at query
	// time from the agents catalog (runs do not denormalize the model today).
	CostGroupByModel CostGroupBy = "model"
)

// ErrInvalidGroupBy is returned when group_by is outside day|agent|model;
// the HTTP handler maps it to 400 INVALID_GROUP_BY.
var ErrInvalidGroupBy = errors.New("runs: group_by must be one of day, agent, model")

// CostBucket is one aggregated row of the tenant cost report. Exactly one of
// Bucket / AgentID / Model is populated depending on the grouping; the other
// identity fields stay empty and are omitted from JSON (omitempty).
type CostBucket struct {
	// Bucket is the UTC day ("YYYY-MM-DD"); set only for group_by=day.
	Bucket string `json:"bucket,omitempty"`
	// AgentID is set only for group_by=agent.
	AgentID string `json:"agent_id,omitempty"`
	// Model is set only for group_by=model.
	Model string `json:"model,omitempty"`
	// CostCents is the summed cost of the bucket (USD cents; 0 = unpriced).
	CostCents float64 `json:"cost_cents"`
	// Runs is the number of runs in the bucket.
	Runs int64 `json:"runs"`
}

const (
	// Tenant guard: every aggregation filters on organization_id and a
	// half-open [from, to) created_at window. Day buckets are formatted in
	// UTC so the report is stable regardless of the database session
	// timezone. Index: idx_runs_org_created_cost (migration 012).
	sqlAggregateCostsByDay = `SELECT to_char(r.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS bucket, COALESCE(SUM(r.cost_cents), 0), COUNT(*) FROM runs r WHERE r.organization_id = $1 AND r.created_at >= $2 AND r.created_at < $3 GROUP BY 1 ORDER BY 1`
	// Tenant guard: aggregation scoped to one organization_id.
	sqlAggregateCostsByAgent = `SELECT r.agent_id, COALESCE(SUM(r.cost_cents), 0), COUNT(*) FROM runs r WHERE r.organization_id = $1 AND r.created_at >= $2 AND r.created_at < $3 GROUP BY r.agent_id ORDER BY r.agent_id`
	// Tenant guard: aggregation scoped to one organization_id; the model is
	// resolved from the agents catalog (LEFT JOIN so deleted agents still
	// aggregate under the empty model label).
	sqlAggregateCostsByModel = `SELECT COALESCE(a.model, ''), COALESCE(SUM(r.cost_cents), 0), COUNT(*) FROM runs r LEFT JOIN agents a ON a.id = r.agent_id WHERE r.organization_id = $1 AND r.created_at >= $2 AND r.created_at < $3 GROUP BY 1 ORDER BY 1`
)

// aggregateSQLFor returns the tenant-guarded aggregate query for one grouping.
func aggregateSQLFor(groupBy CostGroupBy) (string, error) {
	switch groupBy {
	case CostGroupByDay:
		return sqlAggregateCostsByDay, nil
	case CostGroupByAgent:
		return sqlAggregateCostsByAgent, nil
	case CostGroupByModel:
		return sqlAggregateCostsByModel, nil
	default:
		return "", ErrInvalidGroupBy
	}
}

// AggregateCosts implements the Store-side aggregation: one tenant-scoped
// query per grouping over the half-open [from, to) window (see cost.go for
// the SQL constants and their tenant-guard comments).
func (s *pgStore) AggregateCosts(ctx context.Context, orgID string, from, to time.Time, groupBy CostGroupBy) ([]CostBucket, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	query, err := aggregateSQLFor(groupBy)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, query, orgID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CostBucket, 0)
	for rows.Next() {
		var bucket CostBucket
		var label string
		if err := rows.Scan(&label, &bucket.CostCents, &bucket.Runs); err != nil {
			return nil, err
		}
		switch groupBy {
		case CostGroupByDay:
			bucket.Bucket = label
		case CostGroupByAgent:
			bucket.AgentID = label
		case CostGroupByModel:
			bucket.Model = label
		}
		out = append(out, bucket)
	}
	return out, rows.Err()
}

// AggregateCosts aggregates runs.cost_cents for one tenant over the half-open
// [from, to) window, grouped by day, agent or model. The store implementation
// is the Postgres path; without a store the in-memory runs are aggregated
// (model labels resolve to the empty string there — the in-memory runs
// service has no agents catalog; documented wiring-doc limitation).
func (s *Service) AggregateCostsCtx(ctx context.Context, orgID string, from, to time.Time, groupBy CostGroupBy) ([]CostBucket, float64, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, 0, errors.New("organization id is required")
	}
	if _, err := aggregateSQLFor(groupBy); err != nil {
		return nil, 0, err
	}
	if from.IsZero() || to.IsZero() {
		return nil, 0, errors.New("from and to are required")
	}
	if !to.After(from) {
		return nil, 0, errors.New("to must be after from")
	}

	if s.store != nil {
		series, err := s.store.AggregateCosts(ctx, orgID, from, to, groupBy)
		if err != nil {
			return nil, 0, err
		}
		var total float64
		for _, b := range series {
			total += b.CostCents
		}
		return series, total, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	buckets := make(map[string]*CostBucket)
	for _, run := range s.runs {
		if run.OrganizationID != orgID {
			continue
		}
		if run.CreatedAt.Before(from) || !run.CreatedAt.Before(to) {
			continue
		}
		var label string
		switch groupBy {
		case CostGroupByDay:
			label = run.CreatedAt.UTC().Format("2006-01-02")
		case CostGroupByAgent:
			label = run.AgentID
		case CostGroupByModel:
			// In-memory mode has no agents catalog: model labels stay empty
			// (zero-infrastructure limitation, documented).
			label = ""
		}
		b := buckets[label]
		if b == nil {
			b = &CostBucket{}
			switch groupBy {
			case CostGroupByDay:
				b.Bucket = label
			case CostGroupByAgent:
				b.AgentID = label
			case CostGroupByModel:
				b.Model = label
			}
			buckets[label] = b
		}
		b.Runs++
		b.CostCents += run.TotalCostCents
	}
	series := make([]CostBucket, 0, len(buckets))
	var total float64
	for _, b := range buckets {
		series = append(series, *b)
		total += b.CostCents
	}
	sort.Slice(series, func(i, j int) bool {
		li, lj := series[i].Bucket+series[i].AgentID+series[i].Model, series[j].Bucket+series[j].AgentID+series[j].Model
		return li < lj
	})
	return series, total, nil
}
