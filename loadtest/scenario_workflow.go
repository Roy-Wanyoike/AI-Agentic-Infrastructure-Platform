package loadtest

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// FanoutDSL builds the workflow-stress definition: an entry agent node that
// fans out to fanout parallel agent branches and joins back into a final
// agent node — a multi-node DAG with fan-out/fan-in (issue #125 contract).
// Every executable node carries config.agent_id so run-expansion dispatch
// creates a run per node.
func FanoutDSL(agentID string, fanout int) map[string]any {
	nodes := []map[string]any{{
		"id": "entry", "type": "agent",
		"config": map[string]any{"agent_id": agentID},
	}}
	var edges []map[string]any
	for i := 0; i < fanout; i++ {
		id := fmt.Sprintf("fan_%d", i)
		nodes = append(nodes, map[string]any{
			"id": id, "type": "agent",
			"config": map[string]any{"agent_id": agentID},
		})
		edges = append(edges,
			map[string]any{"from": "entry", "to": id, "condition": "on_success"},
			map[string]any{"from": id, "to": "join", "condition": "on_success"},
		)
	}
	nodes = append(nodes, map[string]any{
		"id": "join", "type": "agent",
		"config": map[string]any{"agent_id": agentID},
	})
	return map[string]any{"nodes": nodes, "edges": edges}
}

// scenarioWorkflow stresses the DAG execution path: it publishes one fan-out
// workflow (entry -> N parallel branches -> join, all backed by an ACTIVE
// agent) and then drives concurrent POST /workflows/{id}/execute at a steady
// RPS. Each execute synchronously expands the DAG (creates one run per node,
// enqueues the worker tasks) and returns the workflow run — the measured
// latency is the end-to-end engine duration.
type scenarioWorkflow struct{}

func (scenarioWorkflow) Name() string { return "workflow" }

func (s scenarioWorkflow) Run(ctx context.Context, env *Env) (*Report, error) {
	cfg := env.Config()
	b := newReportBuilder(s.Name(), cfg)

	tenant, agentID, err := provisionAgent(ctx, env, b, "workflow")
	if err != nil {
		return b.build(0, nil, nil), err
	}

	// One-time: create + publish the fan-out workflow for this tenant.
	setup := b.setupOp("workflow")
	wfID, err := env.CreateWorkflow(ctx, setup, tenant, "stress-fanout", FanoutDSL(agentID, cfg.FanOut))
	if err != nil {
		return b.build(0, nil, nil), err
	}
	if err := env.PublishWorkflow(ctx, setup, tenant, wfID); err != nil {
		return b.build(0, nil, nil), err
	}

	sampler := env.startSampling(ctx, tenant.Token)

	if cfg.Warmup > 0 {
		warm := newCollector()
		env.pacedPhaseRPS(ctx, warm, cfg.Warmup, cfg.RPS, func() {
			_, _ = env.ExecuteWorkflow(ctx, warm, tenant, wfID)
		})
	}

	primary := b.op("workflow_execute")
	control := b.op("healthz")
	var seq atomic.Int64
	started := time.Now()
	env.pacedPhase(ctx, primary, cfg.Duration, func() {
		n := seq.Add(1)
		_, _ = env.ExecuteWorkflow(ctx, primary, tenant, wfID)
		if n%10 == 0 {
			env.Healthz(ctx, control)
		}
	})
	elapsed := time.Since(started)

	return b.build(elapsed, sampler.Report(cfg.SampleInterval), map[string]any{
		"agent_id":    agentID,
		"workflow_id": wfID,
		"fan_out":     cfg.FanOut,
		"dag_nodes":   cfg.FanOut + 2,
		"note":        "without a worker the expanded runs stay QUEUED; execute measures the synchronous DAG expansion+dispatch",
	}), nil
}

// CreateWorkflow creates + validates a workflow via POST /workflows/create
// and returns its id (setup op latencies land in c).
func (e *Env) CreateWorkflow(ctx context.Context, c *collector, t *Tenant, name string, dsl map[string]any) (string, error) {
	body, err := jsonMarshal(map[string]any{
		"name": name, "description": "loadtest fan-out DAG", "dsl": dsl,
	})
	if err != nil {
		return "", err
	}
	req := e.newRequest(ctx, "POST", e.apiURL("/workflows/create"), t.Authz, body)
	var created struct {
		Workflow struct {
			ID string `json:"id"`
		} `json:"workflow"`
	}
	if _, err := e.measureJSON(ctx, c, 201, req, &created); err != nil {
		return "", fmt.Errorf("create workflow: %w", err)
	}
	if created.Workflow.ID == "" {
		return "", fmt.Errorf("create workflow returned an empty id")
	}
	return created.Workflow.ID, nil
}

// PublishWorkflow publishes a draft workflow (POST /workflows/{id}/publish).
func (e *Env) PublishWorkflow(ctx context.Context, c *collector, t *Tenant, wfID string) error {
	req := e.newRequest(ctx, "POST", e.apiURL("/workflows/"+wfID+"/publish"), t.Authz, "")
	var published struct {
		Workflow struct {
			ID string `json:"id"`
		} `json:"workflow"`
	}
	if _, err := e.measureJSON(ctx, c, 200, req, &published); err != nil {
		return fmt.Errorf("publish workflow: %w", err)
	}
	return nil
}

// ExecuteWorkflow runs one DAG execution (POST /workflows/{id}/execute) and
// returns the workflow_run_id.
func (e *Env) ExecuteWorkflow(ctx context.Context, c *collector, t *Tenant, wfID string) (string, error) {
	req := e.newRequest(ctx, "POST", e.apiURL("/workflows/"+wfID+"/execute"), t.Authz, `{"input":"load"}`)
	var result struct {
		WorkflowRunID string `json:"workflow_run_id"`
		Status        string `json:"status"`
	}
	if _, err := e.measureJSON(ctx, c, 200, req, &result); err != nil {
		return "", fmt.Errorf("execute workflow: %w", err)
	}
	return result.WorkflowRunID, nil
}
