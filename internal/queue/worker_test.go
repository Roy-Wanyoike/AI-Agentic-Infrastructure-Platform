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

func TestQueueScaleCheckReportsThroughputAndRecovery(t *testing.T) {
	q := NewQueue()
	for i := 0; i < 20; i++ {
		q.Enqueue("agent.run", map[string]any{"agent_id": "agent-1", "input": "hello"})
	}

	report, err := ScaleCheck(q, 4)
	if err != nil {
		t.Fatalf("ScaleCheck returned error: %v", err)
	}
	if report.TotalProcessed == 0 {
		t.Fatal("expected at least one processed item in the scale report")
	}
	if report.ThroughputPerSecond <= 0 {
		t.Fatal("expected throughput to be greater than zero")
	}
	if report.RecoveryRate <= 0 {
		t.Fatal("expected a positive recovery rate for scale validation")
	}
}
