package sdk

import (
	"context"
	"fmt"
	"time"
)

// Node/Edge/DSL mirror internal/workflows' DSL JSON shape (snake_case tags on
// the server side).
type Node struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Name   string         `json:"name,omitempty"`
	Config map[string]any `json:"config,omitempty"`
}

// Edge is one directed connection in the workflow DAG.
type Edge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

// DSL is the workflow definition ({"nodes":[…],"edges":[…]}).
type DSL struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// CreateWorkflowRequest is the POST /v1/workflows/create body.
type CreateWorkflowRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	DSL         DSL    `json:"dsl"`
}

// WorkflowSummary is the entry shape of GET /v1/workflows ({"workflows":[…]})
// and the "workflow" member of the create/publish responses.
type WorkflowSummary struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description,omitempty"`
	Status         string    `json:"status"`
	CurrentVersion int       `json:"current_version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// WorkflowVersion is one immutable published DSL snapshot.
type WorkflowVersion struct {
	Version   int       `json:"version"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	DSLSnap   DSL       `json:"dsl_snapshot"`
}

// WorkflowDetail is GET /v1/workflows/{id} and the create response payload.
type WorkflowDetail struct {
	WorkflowSummary
	DSL      DSL               `json:"dsl"`
	Versions []WorkflowVersion `json:"versions,omitempty"`
}

// WorkflowList is the wrapped shape of GET /v1/workflows.
type WorkflowList struct {
	Workflows []WorkflowSummary `json:"workflows"`
}

// CreateWorkflowResponse is the 201 body of POST /v1/workflows/create.
type CreateWorkflowResponse struct {
	Workflow WorkflowDetail `json:"workflow"`
}

// ExecutionResult is the 200 body of POST /v1/workflows/{id}/execute.
type ExecutionResult struct {
	WorkflowRunID string   `json:"workflow_run_id"`
	RunIDs        []string `json:"run_ids"`
	Status        string   `json:"status"`
}

// ListWorkflows lists the tenant's workflows (GET /v1/workflows).
func (c *Client) ListWorkflows(ctx context.Context) (*WorkflowList, error) {
	var out WorkflowList
	if err := c.do(ctx, httpMethodGet, "/workflows", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Workflows == nil {
		out.Workflows = []WorkflowSummary{}
	}
	return &out, nil
}

// CreateWorkflow creates a draft workflow from a DSL
// (POST /v1/workflows/create; 422 validation arrays surface as *APIError).
func (c *Client) CreateWorkflow(ctx context.Context, req CreateWorkflowRequest) (*CreateWorkflowResponse, error) {
	var out CreateWorkflowResponse
	if err := c.do(ctx, httpMethodPost, "/workflows/create", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWorkflow fetches one workflow with its versions (GET /v1/workflows/{id}).
func (c *Client) GetWorkflow(ctx context.Context, id string) (*WorkflowDetail, error) {
	var out struct {
		Workflow WorkflowDetail `json:"workflow"`
	}
	if err := c.do(ctx, httpMethodGet, "/workflows/"+urlPathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Workflow, nil
}

// ExecuteWorkflow expands the workflow DAG into agent runs
// (POST /v1/workflows/{id}/execute).
func (c *Client) ExecuteWorkflow(ctx context.Context, id, input string) (*ExecutionResult, error) {
	var out ExecutionResult
	body := map[string]string{"input": input}
	path := "/workflows/" + urlPathEscape(id) + "/execute"
	if err := c.do(ctx, httpMethodPost, path, nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWorkflowRun fetches one workflow run with its node timeline
// (GET /v1/workflow-runs/{id}).
func (c *Client) GetWorkflowRun(ctx context.Context, id string) (*WorkflowRunView, error) {
	var out WorkflowRunView
	path := "/workflow-runs/" + urlPathEscape(id)
	if err := c.do(ctx, httpMethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WorkflowRunView is the GET /v1/workflow-runs/{id} response shape.
type WorkflowRunView struct {
	ID         string    `json:"id"`
	WorkflowID string    `json:"workflow_id"`
	Status     string    `json:"status"`
	NodeRuns   []NodeRun `json:"node_runs"`
}

// NodeRun mirrors one checkpointed node attempt of a workflow run.
type NodeRun struct {
	ID            string     `json:"id"`
	WorkflowRunID string     `json:"workflow_run_id"`
	NodeID        string     `json:"node_id"`
	RunID         string     `json:"run_id,omitempty"`
	Status        string     `json:"status"`
	Error         string     `json:"error,omitempty"`
	Attempt       int        `json:"attempt"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// String renders a compact summary used by CLI table rows.
func (r ExecutionResult) String() string {
	return fmt.Sprintf("workflow_run %s status=%s runs=%d", r.WorkflowRunID, r.Status, len(r.RunIDs))
}
