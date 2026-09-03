package sdk

import (
	"context"
	"net/url"
	"time"
)

// HTTP method names shared by the resource files.
const (
	httpMethodGet    = "GET"
	httpMethodPost   = "POST"
	httpMethodDelete = "DELETE"
)

// CreateAgentRequest is the POST /v1/agents/create body. organization_id may
// be empty — the handler defaults it to the caller's tenant from the token.
type CreateAgentRequest struct {
	OrganizationID string `json:"organization_id,omitempty"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Instructions   string `json:"instructions"`
	Model          string `json:"model"`
}

// Agent mirrors the wire shape of GET /v1/agents (bare JSON array),
// POST /v1/agents/create (bare object, 201) and GET /v1/agents/{id}. The
// handlers marshal internal/agents.Service's struct without JSON tags, so the
// keys are the Go field names (PascalCase) — see the openapi.yaml Agent
// schema examples.
type Agent struct {
	ID               string    `json:"ID"`
	OrganizationID   string    `json:"OrganizationID"`
	Name             string    `json:"Name"`
	Description      string    `json:"Description"`
	Instructions     string    `json:"Instructions"`
	Model            string    `json:"Model"`
	Status           string    `json:"Status"`
	CurrentVersionID string    `json:"CurrentVersionID"`
	CreatedAt        time.Time `json:"CreatedAt"`
	UpdatedAt        time.Time `json:"UpdatedAt"`
}

// ListAgents returns the caller's agents (GET /v1/agents). The response is a
// bare JSON array — not an envelope. organizationID optionally overrides the
// ?organization_id query parameter (cross-org access is forbidden server-side).
func (c *Client) ListAgents(ctx context.Context, organizationID string) ([]Agent, error) {
	var out []Agent
	if err := c.do(ctx, httpMethodGet, "/agents", orgQuery(organizationID), nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Agent{}
	}
	return out, nil
}

// CreateAgent creates an agent in DRAFT status (POST /v1/agents/create).
func (c *Client) CreateAgent(ctx context.Context, req CreateAgentRequest) (*Agent, error) {
	var out Agent
	if err := c.do(ctx, httpMethodPost, "/agents/create", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAgent fetches one agent by ID (GET /v1/agents/{id}, bare object).
func (c *Client) GetAgent(ctx context.Context, id string) (*Agent, error) {
	var out Agent
	if err := c.do(ctx, httpMethodGet, "/agents/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// orgQuery builds the shared ?organization_id query value.
func orgQuery(organizationID string) url.Values {
	if organizationID == "" {
		return nil
	}
	q := make(url.Values)
	q.Set("organization_id", organizationID)
	return q
}
