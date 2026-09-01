package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"agentos/internal/agents"
	"agentos/internal/config"
	"agentos/internal/logger"
	"agentos/internal/queue"
	"agentos/internal/runs"
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
	runsService := runs.NewService()
	worker := queue.NewWorker(workQueue, func(task *queue.Task) error {
		if task == nil || task.Payload == nil {
			return fmt.Errorf("task payload is required")
		}
		runID, _ := task.Payload["run_id"].(string)
		agentID, _ := task.Payload["agent_id"].(string)
		input, _ := task.Payload["input"].(string)
		if runID == "" || agentID == "" || input == "" {
			return fmt.Errorf("task payload missing run_id, agent_id or input")
		}
		// mark run running
		_ = runsService.UpdateStatus(runID, runs.StatusRunning, "")
		// notify API about status change (so streaming service can pick it up)
		go func() {
			apiBase := os.Getenv("AGENTOS_API")
			if apiBase == "" {
				apiBase = "http://localhost:8080"
			}
			payload := map[string]any{"type": "status", "name": "status.changed", "payload": map[string]any{"status": string(runs.StatusRunning)}}
			b, _ := json.Marshal(payload)
			_, _ = http.Post(fmt.Sprintf("%s/v1/runs/%s/events", apiBase, runID), "application/json", bytes.NewReader(b))
		}()
		run, err := runner.Run(context.Background(), agentID, input)
		if err != nil {
			_ = runsService.UpdateStatus(runID, runs.StatusFailed, "")
			go func() {
				apiBase := os.Getenv("AGENTOS_API")
				if apiBase == "" {
					apiBase = "http://localhost:8080"
				}
				payload := map[string]any{"type": "status", "name": "status.changed", "payload": map[string]any{"status": string(runs.StatusFailed)}}
				b, _ := json.Marshal(payload)
				_, _ = http.Post(fmt.Sprintf("%s/v1/runs/%s/events", apiBase, runID), "application/json", bytes.NewReader(b))
			}()
			return err
		}
		task.Payload["result"] = run.Output
		task.Payload["status"] = string(run.Status)
		_ = runsService.UpdateStatus(runID, runs.StatusCompleted, run.Output)
		go func() {
			apiBase := os.Getenv("AGENTOS_API")
			if apiBase == "" {
				apiBase = "http://localhost:8080"
			}
			payload := map[string]any{"type": "status", "name": "status.changed", "payload": map[string]any{"status": string(runs.StatusCompleted), "output": run.Output}}
			b, _ := json.Marshal(payload)
			_, _ = http.Post(fmt.Sprintf("%s/v1/runs/%s/events", apiBase, runID), "application/json", bytes.NewReader(b))
		}()
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
