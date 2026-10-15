package runtime

// trace_hooks.go — issue #114: execution trace hierarchy hooks for the agent
// runtime loop.
//
// Every helper here is additive and disabled-safe: when tracing is OFF (the
// AGENTOS_TRACING_ENABLED default) they cost one atomic load, return the
// caller's context UNCHANGED and a nil handle, so the bounded loop's fast
// path stays byte-identical — no allocations, no spans, no attribute work.
// When tracing is ON they build the run → agent → step → tool/model span
// tree required by issue #114:
//
//      run.execute   (root, RunWithID)      agentos.run.id / agentos.agent.id / org
//      agent.execute (child of run)        one agent's bounded loop
//      step.execute  (child of agent)      one loop iteration (agentos.step.index)
//      model.call    (child of step)       provider completion (usage + cost)
//      tool.call     (child of step)       tool invocation
//
// Cost attribution mirrors the runs ledger: model-call spans carry
// agentos.cost.millicents computed with models.ComputeCostCents — the same
// pricing the worker's step recorder uses — so traces and billing agree.
//
// All span creation goes through internal/observability, keeping this file
// free of OTel provider concerns (the tracer is installed once per process).

import (
	"context"
	"math"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"agentos/internal/agents"
	"agentos/internal/models"
	"agentos/internal/observability"
	"agentos/internal/policies"
)

// runScopeOrgID resolves the tenant id stamped on the execution context
// (policies.WithRunScope — the worker stamps it before handing a task to the
// runner) so execution spans carry agentos.org.id. Empty when unstamped.
func runScopeOrgID(ctx context.Context) string {
	return policies.RunScopeFromContext(ctx).OrganizationID
}

// centsToMillicents converts a USD-cents cost (models.ComputeCostCents) into
// the integer millicents recorded on model-call spans (rounded half away from
// zero; a 0-cent unpriced model stays 0).
func centsToMillicents(cents float64) int64 {
	return int64(math.Round(cents * 1000))
}

// runTrace holds the open run and agent spans of one execution. Nil whenever
// tracing is disabled — every method is nil-safe for that reason.
type runTrace struct {
	run   trace.Span
	agent trace.Span
}

// startRunTrace opens the run root span ("run.execute") and the agent span
// ("agent.execute") for one RunWithID execution and registers the run's trace
// id in the observability registry (trace id exposure). Disabled →
// (ctx unchanged, nil). The caller MUST defer (*runTrace).end.
func (r *Runner) startRunTrace(ctx context.Context, run *Run, agent *agents.Agent) (context.Context, *runTrace) {
	if !observability.TracingEnabled() {
		return ctx, nil
	}
	orgID := ""
	if scope := policies.RunScopeFromContext(ctx); scope != (policies.RunScope{}) {
		orgID = scope.OrganizationID
	}
	wf := observability.WorkflowFromContext(ctx)
	attempt := observability.AttemptFromContext(ctx)
	runCtx, runSpan := observability.StartRunExecuteSpan(ctx, orgID, run.AgentID, run.ID, agent.Model,
		wf.WorkflowRunID, wf.NodeID, attempt)
	agentCtx, agentSpan := observability.StartAgentSpan(runCtx, orgID, agent.ID, run.ID, agent.Model)
	return agentCtx, &runTrace{run: runSpan, agent: agentSpan}
}

// end closes the agent and run spans (agent first: children end before
// parents) and stamps the terminal run status plus the failure cause onto the
// run span. Nil-safe: a nil trace is the disabled fast path.
func (t *runTrace) end(run *Run) {
	if t == nil {
		return
	}
	if run != nil {
		t.run.SetAttributes(attribute.String(observability.AttrRunStatus, string(run.Status)))
		if run.Status == StatusFailed && run.Error != "" {
			t.run.SetStatus(codes.Error, run.Error)
		}
	}
	t.agent.End()
	t.run.End()
}

// startStepTrace opens the per-step span ("step.execute") for one loop
// iteration. Disabled → (ctx unchanged, nil).
func (r *Runner) startStepTrace(ctx context.Context, orgID, agentID, runID string, index int) (context.Context, *stepTrace) {
	if !observability.TracingEnabled() {
		return ctx, nil
	}
	stepCtx, span := observability.StartStepSpan(ctx, orgID, agentID, runID, index)
	return stepCtx, &stepTrace{span: span}
}

// stepTrace holds the open span of one bounded-loop step. Nil-safe on end.
type stepTrace struct {
	span trace.Span
}

// end closes the step span, marking err (typed loop errors included) on the
// span status when non-nil.
func (s *stepTrace) end(err error) {
	if s == nil {
		return
	}
	observability.RecordSpanError(s.span, err)
	s.span.End()
}

// startModelTrace opens the model-call span ("model.call") for one provider
// completion. Disabled → (ctx unchanged, nil span handle).
func (r *Runner) startModelTrace(ctx context.Context, orgID, agentID, runID, modelID string) (context.Context, trace.Span) {
	if !observability.TracingEnabled() {
		return ctx, nil
	}
	return observability.StartModelSpan(ctx, orgID, agentID, runID, modelID)
}

// endModelTrace closes a model-call span: it mirrors the provider-reported
// token usage and the priced cost (agentos.cost.millicents, priced with
// agent.Model through models.ComputeCostCents — the same pricing the runs
// ledger uses) onto the span, records the call error and ends the span. All
// handles (nil span, nil response, nil error) are safe.
func (r *Runner) endModelTrace(span trace.Span, modelID string, resp *models.CompletionResponse, err error) {
	if span == nil {
		return
	}
	if resp != nil {
		cost := models.ComputeCostCents(modelID, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		observability.SetModelSpanUsage(span, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens, centsToMillicents(cost))
	}
	observability.RecordSpanError(span, err)
	span.End()
}

// startToolTrace opens the tool-invocation span ("tool.call") for one tool
// call. Disabled → (ctx unchanged, nil span handle).
func (r *Runner) startToolTrace(ctx context.Context, orgID, agentID, runID, tool string) (context.Context, trace.Span) {
	if !observability.TracingEnabled() {
		return ctx, nil
	}
	return observability.StartToolInvocationSpan(ctx, orgID, agentID, runID, tool)
}

// endToolTrace closes a tool-invocation span, recording err (policy denials
// included) on the span status when non-nil. Nil-safe.
func (r *Runner) endToolTrace(span trace.Span, err error) {
	if span == nil {
		return
	}
	observability.RecordSpanError(span, err)
	span.End()
}
