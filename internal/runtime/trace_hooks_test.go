package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"

	"agentos/internal/agents"
	"agentos/internal/models"
	"agentos/internal/observability"
	"agentos/internal/policies"
	"agentos/internal/tools"
)

// traceTestAgent creates the agent used by the tracing tests. The model id is
// a priced builtin (models.ComputeCostCents) so the cost-attribution contract
// is assertable end to end.
func traceTestAgent(t *testing.T, agentService *agents.Service) *agents.Agent {
	t.Helper()
	agent, err := agentService.Create("org-1", "Trace Agent", "Traced calculations", "Solve math problems", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	return agent
}

// spanIndex indexes recorded spans by name (duplicates keep the LAST).
func spanIndex(spans []observability.RecordedSpan) map[string]observability.RecordedSpan {
	idx := make(map[string]observability.RecordedSpan, len(spans))
	for _, s := range spans {
		idx[s.Name] = s
	}
	return idx
}

// TestTraceHooksHierarchy proves the full run → agent → step → model/tool
// span tree of one provider-mode run, the agentos.* attribute contract on
// every execution span, and the model-call cost attribution (issue #114).
func TestTraceHooksHierarchy(t *testing.T) {
	spans, restore := observability.InstallTestTracing()
	defer restore()

	agentService := agents.NewService()
	agent := traceTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())

	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		// Token counts sized so gpt-4o-mini pricing yields 4.5 cents exactly
		// (4500 millicents): (100000*0.15 + 50000*0.6)/1e6 * 100.
		{Text: `{"tool": "calculator", "arguments": {"expression": "6 * 7"}}`,
			Model: "gpt-4o-mini", Usage: models.Usage{PromptTokens: 100000, CompletionTokens: 50000, TotalTokens: 150000}},
		{Text: "the result is 42",
			Model: "gpt-4o-mini", Usage: models.Usage{PromptTokens: 200, CompletionTokens: 10, TotalTokens: 210}},
	}}
	runner := NewRunnerWithOptions(agentService, registry, WithProvider(provider))

	// The worker stamps tenant + workflow identity + queue attempt on the
	// execution context; the run spans must mirror all three.
	ctx := policies.WithRunScope(context.Background(), policies.RunScope{OrganizationID: "org-1", Environment: "production"})
	ctx = observability.ContextWithWorkflow(ctx, "wfrun-9", "node-2")
	ctx = observability.ContextWithAttempt(ctx, 3)

	run, err := runner.RunWithID(ctx, "run-trace-1", agent.ID, "compute 6*7 for me")
	if err != nil {
		t.Fatalf("RunWithID returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s (%s)", run.Status, run.Error)
	}

	recorded := spans()
	// 1 run + 1 agent + 2 steps + 2 models + 1 tool = 7 spans.
	if len(recorded) != 7 {
		t.Fatalf("expected 7 spans, got %d: %+v", len(recorded), names(recorded))
	}
	idx := spanIndex(recorded)
	runSpan, ok := idx[observability.SpanRunExecute]
	if !ok {
		t.Fatalf("run span missing: %+v", names(recorded))
	}
	agentSpan := idx[observability.SpanAgentExecute]
	steps := 0
	var firstStep, secondStep observability.RecordedSpan
	var firstModel, secondModel observability.RecordedSpan
	var toolSpan observability.RecordedSpan
	for _, s := range recorded {
		switch s.Name {
		case observability.SpanStepExecute:
			steps++
			if firstStep.Name == "" {
				firstStep = s
			} else {
				secondStep = s
			}
		case observability.SpanModelCall:
			if firstModel.Name == "" {
				firstModel = s
			} else {
				secondModel = s
			}
		case observability.SpanToolCall:
			toolSpan = s
		}
	}
	if steps != 2 {
		t.Fatalf("expected 2 step spans, got %d", steps)
	}

	// Trace identity: every span shares the run's trace.
	for _, s := range recorded {
		if s.TraceID != runSpan.TraceID {
			t.Errorf("span %s trace %v != run trace %v", s.Name, s.TraceID, runSpan.TraceID)
		}
	}
	// Parent chain: agent→run, step→agent, model/tool→step.
	if agentSpan.ParentSpanID != runSpan.SpanID {
		t.Error("agent span must be a child of the run span")
	}
	if firstStep.ParentSpanID != agentSpan.SpanID || secondStep.ParentSpanID != agentSpan.SpanID {
		t.Error("step spans must be children of the agent span")
	}
	if firstModel.ParentSpanID != firstStep.SpanID || secondModel.ParentSpanID != secondStep.SpanID {
		t.Error("model spans must be children of their step spans")
	}
	if toolSpan.ParentSpanID != firstStep.SpanID {
		t.Error("tool span must be a child of the first step span")
	}

	// Run span: identity + workflow + attempt attributes and trace registry.
	if got := runSpan.Attr(observability.AttrOrgID); got != "org-1" {
		t.Errorf("run span %s = %q", observability.AttrOrgID, got)
	}
	if got := runSpan.Attr(observability.AttrAgentID); got != agent.ID {
		t.Errorf("run span %s = %q", observability.AttrAgentID, got)
	}
	if got := runSpan.Attr(observability.AttrRunID); got != "run-trace-1" {
		t.Errorf("run span %s = %q", observability.AttrRunID, got)
	}
	if got := runSpan.Attr(observability.AttrModelID); got != "gpt-4o-mini" {
		t.Errorf("run span %s = %q", observability.AttrModelID, got)
	}
	if got := runSpan.Attr(observability.AttrWorkflowID); got != "wfrun-9" {
		t.Errorf("run span %s = %q", observability.AttrWorkflowID, got)
	}
	if got := runSpan.Attr(observability.AttrWorkflowNodeID); got != "node-2" {
		t.Errorf("run span %s = %q", observability.AttrWorkflowNodeID, got)
	}
	if got := runSpan.Attr(observability.AttrAttempt); got != "3" {
		t.Errorf("run span %s = %q", observability.AttrAttempt, got)
	}
	if got := runSpan.Attr(observability.AttrRunStatus); got != string(StatusCompleted) {
		t.Errorf("run span %s = %q", observability.AttrRunStatus, got)
	}
	if runSpan.StatusCode != codes.Unset {
		t.Errorf("completed run span status = %v, want Unset", runSpan.StatusCode)
	}
	tid, ok := observability.LookupRunTraceID("run-trace-1")
	if !ok || tid != runSpan.TraceID.String() {
		t.Fatalf("trace registry = %q,%v want %q", tid, ok, runSpan.TraceID.String())
	}

	// Step spans carry the step index.
	if got := firstStep.Attr(observability.AttrStepIndex); got != "1" {
		t.Errorf("first step %s = %q", observability.AttrStepIndex, got)
	}
	if got := secondStep.Attr(observability.AttrStepIndex); got != "2" {
		t.Errorf("second step %s = %q", observability.AttrStepIndex, got)
	}

	// Model spans: model id + usage + cost attribution (4500 millicents on
	// the first call per gpt-4o-mini pricing).
	if got := firstModel.Attr(observability.AttrModelID); got != "gpt-4o-mini" {
		t.Errorf("model span %s = %q", observability.AttrModelID, got)
	}
	if got := firstModel.Attr(observability.AttrTokensTotal); got != "150000" {
		t.Errorf("model span %s = %q", observability.AttrTokensTotal, got)
	}
	if got := firstModel.Attr(observability.AttrCostMillicents); got != "4500" {
		t.Errorf("model span %s = %q, want 4500", observability.AttrCostMillicents, got)
	}

	// Tool span: tool name + identity block.
	if got := toolSpan.Attr(observability.AttrToolName); got != "calculator" {
		t.Errorf("tool span %s = %q", observability.AttrToolName, got)
	}
	if got := toolSpan.Attr(observability.AttrRunID); got != "run-trace-1" {
		t.Errorf("tool span %s = %q", observability.AttrRunID, got)
	}
}

// TestTraceHooksFailedRunMarksSpanError proves a failed run stamps the run
// span with agentos.run.status=FAILED and span status Error carrying the cause.
func TestTraceHooksFailedRunMarksSpanError(t *testing.T) {
	spans, restore := observability.InstallTestTracing()
	defer restore()

	agentService := agents.NewService()
	agent := traceTestAgent(t, agentService)
	provider := &scriptedProvider{name: "scripted", errs: []error{errors.New("provider exploded")}}
	runner := NewRunnerWithOptions(agentService, tools.NewRegistry(), WithProvider(provider))

	ctx := policies.WithRunScope(context.Background(), policies.RunScope{OrganizationID: "org-1"})
	run, rerr := runner.RunWithID(ctx, "run-trace-2", agent.ID, "hello")
	if rerr == nil {
		t.Fatal("expected the run to fail")
	}
	if run.Status != StatusFailed {
		t.Fatalf("expected failed, got %s", run.Status)
	}

	var runSpan observability.RecordedSpan
	for _, s := range spans() {
		if s.Name == observability.SpanRunExecute {
			runSpan = s
		}
	}
	if runSpan.Name == "" {
		t.Fatal("run span missing")
	}
	if got := runSpan.Attr(observability.AttrRunStatus); got != string(StatusFailed) {
		t.Errorf("run span %s = %q", observability.AttrRunStatus, got)
	}
	if runSpan.StatusCode != codes.Error {
		t.Errorf("run span status = %v, want Error", runSpan.StatusCode)
	}
	if !strings.Contains(runSpan.StatusDescription, "provider exploded") {
		t.Errorf("run span status description must carry the cause: %q", runSpan.StatusDescription)
	}
}

// TestTraceHooksDisabledKeepsFastPath pins the disabled contract at the
// runtime boundary: no spans can exist (no provider installed), the run
// outcome is unchanged, and the trace registry stays empty.
func TestTraceHooksDisabledKeepsFastPath(t *testing.T) {
	if observability.TracingEnabled() {
		t.Fatal("precondition: tracing must be disabled")
	}
	agentService := agents.NewService()
	agent := traceTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())

	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: "the result is 42", Model: "gpt-4o-mini", Usage: models.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}},
	}}
	runner := NewRunnerWithOptions(agentService, registry, WithProvider(provider))

	ctx := policies.WithRunScope(context.Background(), policies.RunScope{OrganizationID: "org-1"})
	ctx = observability.ContextWithWorkflow(ctx, "wfrun-1", "node-1")
	run, err := runner.RunWithID(ctx, "run-off", agent.ID, "compute")
	if err != nil {
		t.Fatalf("RunWithID returned error: %v", err)
	}
	if run.Status != StatusCompleted || run.Output != "the result is 42" {
		t.Fatalf("disabled run outcome changed: %+v", run)
	}
	if _, ok := observability.LookupRunTraceID("run-off"); ok {
		t.Fatal("disabled runs must not register a trace id")
	}
}

func names(spans []observability.RecordedSpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name)
	}
	return out
}
