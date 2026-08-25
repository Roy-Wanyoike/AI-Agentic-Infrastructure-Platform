package runtime

import (
	"context"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/tools"
)

func TestRunAgentUsesToolWhenPromptRequiresIt(t *testing.T) {
	agentService := agents.NewService()
	agent, err := agentService.Create("org-1", "Math Agent", "Performs calculations", "Solve math problems", "calculator")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	toolRegistry := tools.NewRegistry()
	toolRegistry.Register(tools.NewCalculatorTool())
	runner := NewRunner(agentService, toolRegistry)

	run, err := runner.Run(context.Background(), agent.ID, "What is 2 + 2?")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s", run.Status)
	}
	if run.Output == "" {
		t.Fatal("run output should not be empty")
	}
}

func TestRunAgentUsesToolForGenericArithmeticInput(t *testing.T) {
	agentService := agents.NewService()
	agent, err := agentService.Create("org-1", "Math Agent", "Performs calculations", "Solve math problems", "calculator")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	toolRegistry := tools.NewRegistry()
	toolRegistry.Register(tools.NewCalculatorTool())
	runner := NewRunner(agentService, toolRegistry)

	run, err := runner.Run(context.Background(), agent.ID, "What is 3 * 4?")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Output != "12" {
		t.Fatalf("expected arithmetic tool result 12, got %q", run.Output)
	}
}

func TestRunAgentRejectsUnknownAgent(t *testing.T) {
	runner := NewRunner(agents.NewService(), tools.NewRegistry())
	if _, err := runner.Run(context.Background(), "missing-agent", "hello"); err == nil {
		t.Fatal("Run should reject unknown agent IDs")
	}
}
