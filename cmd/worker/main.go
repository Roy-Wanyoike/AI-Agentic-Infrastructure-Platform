package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/config"
	"agentos/internal/logger"
	"agentos/internal/models"
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

// agentSvcCreate seeds the demo agent used by local development.
func agentSvcCreate(svc *agents.Service) (*agents.Agent, error) {
	return svc.Create("org-demo", "Support Agent", "Demo customer support agent", "Answer simple questions", "gpt-4o-mini")
}

// stepRecorderAdapter adapts the runtime step stream to the runs service
// recorder so model/tool steps become queryable run_steps rows. The tenant
// scope is resolved from the run itself (trusted internal worker path).
func stepRecorderAdapter(runsService *runs.Service) runtime.StepRecorder {
	return runtime.StepRecorderFunc(func(ctx context.Context, runID string, step runtime.Step) error {
		if step.Input == "" && step.Output == "" && step.Error == "" {
			return nil
		}
		run, ok := runsService.Get(runID)
		if !ok {
			return nil // run already pruned; nothing to attach the step to
		}
		started := time.Now().UTC().Add(-time.Duration(step.DurationMS) * time.Millisecond)
		rs := &runs.Step{
			RunID:       runID,
			StepType:    step.Type,
			Status:      step.Status,
			InputMeta:   map[string]any{"input": step.Input, "name": step.Name, "index": step.Index},
			OutputMeta:  map[string]any{"output": step.Output},
			Error:       step.Error,
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
		if step.TokenUsage.TotalTokens > 0 {
			rs.TokenUsage = map[string]any{
				"prompt_tokens":     step.TokenUsage.PromptTokens,
				"completion_tokens": step.TokenUsage.CompletionTokens,
				"total_tokens":      step.TokenUsage.TotalTokens,
			}
		}
		return runsService.RecordStep(ctx, run.OrganizationID, runID, rs)
	})
}

func main() {
	cfg := config.Load()
	logr := logger.New(cfg.Env)

	agentsvc := agents.NewService()
	_, err := agentSvcCreate(agentsvc)
	if err != nil {
		logr.Warn("seed agent setup failed", "error", err)
	}

	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())
	registry.Register(tools.NewHTTPRequestTool())

	// Provider wiring: when OPENAI_API_KEY is set the worker runs real model
	// calls against any OpenAI-compatible endpoint (OPENAI_BASE_URL to point
	// at OpenRouter/Groq/Ollama/vLLM). Without a key the runner stays in its
	// deterministic offline mode so local development needs no credentials.
	var provider models.Provider
	if apiKey := os.Getenv("OPENAI_API_KEY"); strings.TrimSpace(apiKey) != "" {
		baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		model := strings.TrimSpace(os.Getenv("AGENTOS_WORKER_MODEL"))
		primary := models.NewOpenAIProvider("openai-compatible", apiKey, baseURL, model, &http.Client{Timeout: 120 * time.Second})
		if fbKey := os.Getenv("AGENTOS_FALLBACK_API_KEY"); strings.TrimSpace(fbKey) != "" {
			fbBase := strings.TrimSpace(os.Getenv("AGENTOS_FALLBACK_BASE_URL"))
			if fbBase == "" {
				fbBase = "https://api.openai.com/v1"
			}
			fallback := models.NewOpenAIProvider("fallback", fbKey, fbBase, model, &http.Client{Timeout: 120 * time.Second})
			if chained, cerr := models.NewFailoverProvider(primary, fallback); cerr == nil {
				provider = chained
			} else {
				provider = primary
			}
		} else {
			provider = primary
		}
		logr.Info("model provider configured", "base_url", baseURL, "model", model)
	} else {
		logr.Warn("no OPENAI_API_KEY set; worker runs in offline deterministic mode")
	}

	workQueue := queue.NewQueue()
	runsService := runs.NewService()
	runner := runtime.NewRunnerWithOptions(agentsvc, registry,
		runtime.WithProvider(provider),
		runtime.WithStepRecorder(stepRecorderAdapter(runsService)),
	)
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
		run, rerr := runner.RunWithID(context.Background(), runID, agentID, input)
		if rerr != nil {
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
