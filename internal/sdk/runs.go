package sdk

import (
	"context"
	"net/url"
	"time"
)

// Run status values as defined by internal/runs (wire strings).
const (
	RunStatusQueued    = "QUEUED"
	RunStatusRunning   = "RUNNING"
	RunStatusCompleted = "COMPLETED"
	RunStatusFailed    = "FAILED"
)

// TerminalRunStatuses are the run states that end the CLI's watch loop.
var TerminalRunStatuses = map[string]bool{
	RunStatusCompleted: true,
	RunStatusFailed:    true,
}

// CreateRunRequest is the POST /v1/runs body. organization_id may be empty —
// the handler defaults it from the token claims.
type CreateRunRequest struct {
	OrganizationID string `json:"organization_id,omitempty"`
	AgentID        string `json:"agent_id"`
	Input          string `json:"input"`
}

// CreateRunResponse is the 201 body of POST /v1/runs.
type CreateRunResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

// Run mirrors the wire shape of GET /v1/runs/{id} and the entries of the
// {"runs":[…]} envelope. The handler marshals internal/runs.Run untagged
// (PascalCase keys) with the additive snake_case total_cost_cents field.
type Run struct {
	ID             string    `json:"ID"`
	OrganizationID string    `json:"OrganizationID"`
	AgentID        string    `json:"AgentID"`
	Input          string    `json:"Input"`
	Output         string    `json:"Output"`
	Status         string    `json:"Status"`
	TotalCostCents float64   `json:"total_cost_cents,omitempty"`
	CreatedAt      time.Time `json:"CreatedAt"`
	UpdatedAt      time.Time `json:"UpdatedAt"`
}

// RunList is the wrapped shape of GET /v1/runs.
type RunList struct {
	Runs []Run `json:"runs"`
}

// Step mirrors one entry of GET /v1/runs/{id}/steps. internal/runs.Step has
// no JSON tags, so the wire keys are the Go field names.
type Step struct {
	ID          string         `json:"ID"`
	RunID       string         `json:"RunID"`
	StepType    string         `json:"StepType"`
	Status      string         `json:"Status"`
	InputMeta   map[string]any `json:"InputMeta"`
	OutputMeta  map[string]any `json:"OutputMeta"`
	Error       string         `json:"Error"`
	TokenUsage  map[string]any `json:"TokenUsage"`
	Cost        float64        `json:"Cost"`
	StartedAt   time.Time      `json:"StartedAt"`
	CompletedAt time.Time      `json:"CompletedAt"`
	CreatedAt   time.Time      `json:"CreatedAt"`
}

// RunSteps is the wrapped shape of GET /v1/runs/{id}/steps.
type RunSteps struct {
	RunID string `json:"run_id"`
	Steps []Step `json:"steps"`
}

// CreateRun enqueues a run (POST /v1/runs, 201).
func (c *Client) CreateRun(ctx context.Context, req CreateRunRequest) (*CreateRunResponse, error) {
	var out CreateRunResponse
	if err := c.do(ctx, httpMethodPost, "/runs", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRun fetches one run by ID (GET /v1/runs/{id}, bare object).
func (c *Client) GetRun(ctx context.Context, id string) (*Run, error) {
	var out Run
	if err := c.do(ctx, httpMethodGet, "/runs/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRuns returns the caller's runs (GET /v1/runs, {"runs":[…]} envelope).
func (c *Client) ListRuns(ctx context.Context) (*RunList, error) {
	var out RunList
	if err := c.do(ctx, httpMethodGet, "/runs", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Runs == nil {
		out.Runs = []Run{}
	}
	return &out, nil
}

// Steps returns the execution trace of one run
// (GET /v1/runs/{id}/steps, {"run_id":…,"steps":[…]} envelope).
func (c *Client) Steps(ctx context.Context, runID string) (*RunSteps, error) {
	var out RunSteps
	path := "/runs/" + url.PathEscape(runID) + "/steps"
	if err := c.do(ctx, httpMethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Steps == nil {
		out.Steps = []Step{}
	}
	return &out, nil
}

// StreamEvents opens the SSE event stream of one run
// (GET /v1/runs/{id}/events with Accept: text/event-stream). The returned
// EventStream yields the run's event history first and then live events; call
// Close when done. The connection stays open until the server closes it, the
// context is cancelled or Close is called.
func (c *Client) StreamEvents(ctx context.Context, runID string) (*EventStream, error) {
	full := c.endpoint("/runs/"+url.PathEscape(runID)+"/events", nil)
	req, err := c.newRequest(ctx, httpMethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		raw := readAllLimited(resp.Body)
		return nil, newAPIError(resp, raw)
	}
	return newEventStream(resp), nil
}
