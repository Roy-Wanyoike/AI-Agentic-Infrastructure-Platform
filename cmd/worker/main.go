package main

import (
	"context"
	"fmt"
	"time"

	"agentos/internal/agents"
	"agentos/internal/config"
	"agentos/internal/logger"
	"agentos/internal/queue"
	"agentos/internal/runtime"
	"agentos/internal/tools"
)

func main() {
	cfg := config.Load()
	logr := logger.New(cfg.Env)

	agentService := agents.NewService()
	_, err := agentService.Create("org-demo", "Support Agent", "Demo customer support agent", "Answer simple questions", "gpt-4o-mini")
	if err != nil {
		logr.Warn("seed agent setup failed", "error", err)
	}

	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())
	runner := runtime.NewRunner(agentService, registry)
	workQueue := queue.NewQueue()
	worker := queue.NewWorker(workQueue, func(task *queue.Task) error {
		if task == nil || task.Payload == nil {
			return fmt.Errorf("task payload is required")
		}
		agentID, _ := task.Payload["agent_id"].(string)
		input, _ := task.Payload["input"].(string)
		if agentID == "" || input == "" {
			return fmt.Errorf("task payload missing agent_id or input")
		}
		run, err := runner.Run(context.Background(), agentID, input)
		if err != nil {
			return err
		}
		task.Payload["result"] = run.Output
		task.Payload["status"] = string(run.Status)
		return nil
	})

	logr.Info("agentos worker starting", "port", cfg.Worker.Port, "env", cfg.Env)
	for {
		if err := worker.ProcessNext(); err != nil {
			logr.Warn("worker process failed", "error", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
