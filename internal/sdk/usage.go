package sdk

import (
	"context"
	"net/url"
)

// CostBucket is one aggregation row of GET /v1/usage/costs. Bucket is set for
// group_by=day; AgentID/Model for the other groupings (omitted otherwise) —
// mirroring the handler's map-literal response exactly.
type CostBucket struct {
	Bucket    string  `json:"bucket,omitempty"`
	AgentID   string  `json:"agent_id,omitempty"`
	Model     string  `json:"model,omitempty"`
	CostCents float64 `json:"cost_cents"`
	Runs      int     `json:"runs"`
}

// UsageCosts is the GET /v1/usage/costs response.
type UsageCosts struct {
	TotalCostCents float64      `json:"total_cost_cents"`
	Series         []CostBucket `json:"series"`
}

// CostsQuery selects the report window and grouping. From/To accept the same
// formats the handler parses (RFC3339 timestamps or YYYY-MM-DD dates); empty
// values let the server default to the trailing 30 days.
type CostsQuery struct {
	From    string
	To      string
	GroupBy string // "day" (default) | "agent" | "model"
}

// Costs fetches the tenant usage-cost report (GET /v1/usage/costs).
func (c *Client) Costs(ctx context.Context, q CostsQuery) (*UsageCosts, error) {
	query := make(url.Values)
	if q.From != "" {
		query.Set("from", q.From)
	}
	if q.To != "" {
		query.Set("to", q.To)
	}
	if q.GroupBy != "" {
		query.Set("group_by", q.GroupBy)
	}
	var out UsageCosts
	if err := c.do(ctx, httpMethodGet, "/usage/costs", query, nil, &out); err != nil {
		return nil, err
	}
	if out.Series == nil {
		out.Series = []CostBucket{}
	}
	return &out, nil
}
