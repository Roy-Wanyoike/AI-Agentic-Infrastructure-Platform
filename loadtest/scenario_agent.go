package loadtest

import (
	"context"
	"sync/atomic"
	"time"
)

// scenarioAgent churns the full agent lifecycle: create -> transition
// (draft -> building -> active) -> run -> delete, measured per operation.
//
// Deletion targets a second, run-free agent created in the same iteration:
// the platform guard (cmd/api/agents.go, issue #49 semantics) refuses to
// delete an agent that runs still reference (409 AGENT_HAS_RUNS), so the
// run-bearing agent intentionally stays and each iteration deletes a fresh
// disposable agent instead — measuring the real delete path without turning
// the documented 409 guard into a fake error.
type scenarioAgent struct{}

func (scenarioAgent) Name() string { return "lifecycle" }

func (s scenarioAgent) Run(ctx context.Context, env *Env) (*Report, error) {
	cfg := env.Config()
	b := newReportBuilder(s.Name(), cfg)

	setup := b.setupOp("provision")
	tenant, err := env.RegisterTenant(ctx, setup)
	if err != nil {
		return b.build(0, nil, nil), err
	}
	sampler := env.startSampling(ctx, tenant.Token)

	createC := b.op("create_agent")
	transitionC := b.op("transition")
	runC := b.op("create_run")
	deleteC := b.op("delete_agent")

	var seq atomic.Int64
	started := time.Now()
	env.closedLoopPhase(ctx, cfg.Workers, cfg.Duration, func() {
		n := seq.Add(1)
		// create (the run-bearing agent of this iteration)
		agentID, err := env.CreateAgent(ctx, createC, tenant, "churn-agent")
		if err != nil {
			return
		}
		// transition draft -> building -> active (both measured)
		if err := env.TransitionAgent(ctx, transitionC, tenant, agentID, "building"); err != nil {
			return
		}
		if err := env.TransitionAgent(ctx, transitionC, tenant, agentID, "active"); err != nil {
			return
		}
		// run: one run creation on the now-active agent
		_, _ = env.CreateRun(ctx, runC, tenant, agentID, "load")
		// create a disposable run-free agent and delete it (see doc comment)
		disposable, err := env.CreateAgent(ctx, createC, tenant, "churn-disposable")
		if err != nil {
			return
		}
		_ = n
		if err := env.DeleteAgent(ctx, deleteC, tenant, disposable); err != nil {
			return
		}
	})
	elapsed := time.Since(started)

	return b.build(elapsed, sampler.Report(cfg.SampleInterval), map[string]any{
		"iterations": seq.Load(),
		"workers":    cfg.Workers,
		"note":       "delete measures a run-free agent (AGENT_HAS_RUNS guard blocks deleting run-bearing agents by design)",
	}), nil
}

// DeleteAgent issues DELETE /agents/{id} (200 on success).
func (e *Env) DeleteAgent(ctx context.Context, c *collector, t *Tenant, agentID string) error {
	req := e.newRequest(ctx, "DELETE", e.apiURL("/agents/"+agentID), t.Authz, "")
	var deleted struct {
		Deleted bool   `json:"deleted"`
		ID      string `json:"id"`
	}
	if _, err := e.measureJSON(ctx, c, 200, req, &deleted); err != nil {
		return err
	}
	return nil
}
