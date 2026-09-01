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

func postEventWithRetries(apiBase, runID string, payload map[string]any) error {
	b, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/v1/runs/%s/events", apiBase, runID)
	apiKey := os.Getenv("AGENTOS_API_KEY")
	var lastErr error
	backoff := 200 * time.Millisecond
	for attempt := 0; attempt < 5; attempt++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(b))
		if err != nil {
			lastErr = err
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("X-API-Key", apiKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("bad status: %s", resp.Status)
		time.Sleep(backoff)
		backoff *= 2
	}
	return lastErr
}

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
	processTask := func(task *queue.Task) error {
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
			payload := map[string]any{"type": "status", "name": "status.changed", "payload": map[string]any{"status": string(runs.StatusRunning), "ts": time.Now().UTC().Format(time.RFC3339)}}
			_ = postEventWithRetries(apiBase, runID, payload)
		}()
		run, err := runner.Run(context.Background(), agentID, input)
		if err != nil {
			_ = runsService.UpdateStatus(runID, runs.StatusFailed, "")
			go func() {
				apiBase := os.Getenv("AGENTOS_API")
				if apiBase == "" {
					apiBase = "http://localhost:8080"
				}
				payload := map[string]any{"type": "status", "name": "status.changed", "payload": map[string]any{"status": string(runs.StatusFailed), "ts": time.Now().UTC().Format(time.RFC3339)}}
				_ = postEventWithRetries(apiBase, runID, payload)
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
			payload := map[string]any{"type": "status", "name": "status.changed", "payload": map[string]any{"status": string(runs.StatusCompleted), "output": run.Output, "ts": time.Now().UTC().Format(time.RFC3339)}}
			_ = postEventWithRetries(apiBase, runID, payload)
		}()
		return nil
	}

	worker := queue.NewWorker(workQueue, processTask)

	logr.Info("agentos worker starting", "port", cfg.Worker.Port, "env", cfg.Env)

	// If AGENTOS_API_PULL=true, poll the API for tasks (development pull model)
	if os.Getenv("AGENTOS_API_PULL") == "true" {
		apiBase := os.Getenv("AGENTOS_API")
		if apiBase == "" {
			apiBase = "http://localhost:8080"
		}
		apiKey := os.Getenv("AGENTOS_API_KEY")
		for {
			// call pull endpoint
			req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/queue/pull", apiBase), nil)
			if apiKey != "" {
				req.Header.Set("X-API-Key", apiKey)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp == nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			// no content -> nothing to do
			if resp.StatusCode == http.StatusNoContent {
				resp.Body.Close()
				time.Sleep(500 * time.Millisecond)
				continue
			}
			// only accept 200 OK with a valid task payload
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				logr.Warn("pull returned non-OK status", "status", resp.StatusCode)
				time.Sleep(500 * time.Millisecond)
				continue
			}
			var t queue.Task
			if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
				resp.Body.Close()
				logr.Warn("failed to decode task from pull", "error", err)
				time.Sleep(500 * time.Millisecond)
				continue
			}
			resp.Body.Close()
			// convert to pointer task expected by runner
			task := &queue.Task{ID: t.ID, Type: t.Type, Payload: t.Payload}
			if err := processTask(task); err != nil {
				logr.Warn("worker process failed", "error", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	for {
		if err := worker.ProcessNext(); err != nil {
			logr.Warn("worker process failed", "error", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
