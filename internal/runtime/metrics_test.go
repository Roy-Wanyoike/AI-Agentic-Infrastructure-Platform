package runtime

import (
	"context"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/models"
	"agentos/internal/observability"
	"agentos/internal/tools"
)

// toolsCount reads the agentos_tools_total counter from the registry snapshot
// (0 when the family has never been incremented).
func toolsCount(t *testing.T, m *observability.Metrics) int64 {
	t.Helper()
	counts, _ := m.Snapshot()
	return counts[observability.MetricToolsTotal]
}

// TestIncToolsOfflineCalculatorPath asserts the offline deterministic path
// (math input routed to the calculator tool) feeds agentos_tools_total
// (issue #12).
func TestIncToolsOfflineCalculatorPath(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())

	m := observability.NewMetrics()
	runner := NewRunnerWithOptions(agentService, registry, WithMetrics(m))

	if _, err := runner.Run(context.Background(), agent.ID, "What is 2 + 2?"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := toolsCount(t, m); got != 1 {
		t.Fatalf("after one tool execution got %d, want 1", got)
	}

	// A second run executes the tool again: the counter accumulates.
	if _, err := runner.Run(context.Background(), agent.ID, "What is 3 * 4?"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := toolsCount(t, m); got != 2 {
		t.Fatalf("after two tool executions got %d, want 2", got)
	}
}

// TestIncToolsSkipsModelOnlyRuns asserts plain model completions (no tool
// step) never increment the counter.
func TestIncToolsSkipsModelOnlyRuns(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)

	m := observability.NewMetrics()
	// Offline fallback for non-math input records a model step only.
	runner := NewRunnerWithOptions(agentService, tools.NewRegistry(), WithMetrics(m))
	if _, err := runner.Run(context.Background(), agent.ID, "hello agent"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Provider mode with a direct final answer records a model step only.
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: "the answer is 42"},
	}}
	runner2 := NewRunnerWithOptions(agentService, tools.NewRegistry(), WithProvider(provider), WithMetrics(m))
	if _, err := runner2.Run(context.Background(), agent.ID, "hello agent"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := toolsCount(t, m); got != 0 {
		t.Fatalf("model-only runs incremented tools counter: got %d, want 0", got)
	}
}

// TestIncToolsProviderLoopAndFailures asserts the provider-driven loop counts
// every executed tool step, including failed tool calls (unknown tool), while
// model steps stay uncounted.
func TestIncToolsProviderLoopAndFailures(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())

	m := observability.NewMetrics()

	// Round trip: one tool call, then the final answer.
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: `{"tool": "calculator", "arguments": {"expression": "6 * 7"}}`},
		{Text: "the result is 42"},
	}}
	runner := NewRunnerWithOptions(agentService, registry, WithProvider(provider), WithMetrics(m))
	run, err := runner.Run(context.Background(), agent.ID, "compute 6*7 for me")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Steps != 2 {
		t.Fatalf("expected 2 steps (model+tool), got %d", run.Steps)
	}
	if got := toolsCount(t, m); got != 1 {
		t.Fatalf("after one provider-loop tool call got %d, want 1", got)
	}

	// Failed tool execution (unknown tool) is still a tool execution.
	provider2 := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: `{"tool": "missing_tool", "arguments": {}}`},
		{Text: "recovered answer"},
	}}
	runner2 := NewRunnerWithOptions(agentService, registry, WithProvider(provider2), WithMetrics(m))
	if _, err := runner2.Run(context.Background(), agent.ID, "call the missing tool"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	// Both runners shared one registry: 1 (successful round trip) + 1
	// (failed unknown-tool call) tool executions accumulated.
	counts, _ := m.Snapshot()
	if total := counts[observability.MetricToolsTotal]; total != 2 {
		t.Fatalf("across both runners got %d tool executions, want 2", total)
	}
}

// TestIncToolsNilMetricsSafe asserts the nil-safe dependency-injection
// contract: runners without metrics keep working unchanged (issue #12).
func TestIncToolsNilMetricsSafe(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())

	// No metrics wired at all.
	runner := NewRunner(agentService, registry)
	if _, err := runner.Run(context.Background(), agent.ID, "What is 2 + 2?"); err != nil {
		t.Fatalf("Run without metrics returned error: %v", err)
	}

	// Explicit nil registry via the option and the setter.
	runner2 := NewRunnerWithOptions(agentService, registry, WithMetrics(nil))
	runner2.SetMetrics(nil)
	if _, err := runner2.Run(context.Background(), agent.ID, "What is 5 + 5?"); err != nil {
		t.Fatalf("Run with nil metrics returned error: %v", err)
	}

	// SetMetrics on a nil runner must not panic.
	var nilRunner *Runner
	nilRunner.SetMetrics(observability.NewMetrics())
}
