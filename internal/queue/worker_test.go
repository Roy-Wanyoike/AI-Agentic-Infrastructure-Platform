package queue

import (
	"context"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/runtime"
	"agentos/internal/tools"
)

func TestWorkerProcessesQueuedRun(t *testing.T) {
	agentService := agents.NewService()
	agent, err := agentService.Create("org-1", "Support Agent", "desc", "Answer requests", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Create agent returned error: %v", err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())
	runner := runtime.NewRunner(agentService, registry)
	q := NewQueue()
	q.Enqueue("agent.run", map[string]any{"agent_id": agent.ID, "input": "What is 2 + 2?"})

	worker := NewWorker(q, func(task *Task) error {
		if task == nil || task.Payload == nil {
			return context.DeadlineExceeded
		}
		run, err := runner.Run(context.Background(), task.Payload["agent_id"].(string), task.Payload["input"].(string))
		if err != nil {
			return err
		}
		task.Payload["output"] = run.Output
		task.Payload["status"] = string(run.Status)
		return nil
	})

	if err := worker.ProcessNext(); err != nil {
		t.Fatalf("ProcessNext returned error: %v", err)
	}
	if q.Length() != 0 {
		t.Fatalf("queue should be empty after processing, got %d", q.Length())
	}
	if got := q.Peek(); got != nil {
		t.Fatalf("Peek should be nil after queue drain, got %#v", got)
	}
}
