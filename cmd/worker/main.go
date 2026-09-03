package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agentos/internal/agents"
	"agentos/internal/config"
	"agentos/internal/database"
	"agentos/internal/logger"
	"agentos/internal/models"
	"agentos/internal/observability"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/runtime"
	"agentos/internal/tools"
	"agentos/internal/workflows"
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
func stepRecorderAdapter(runsService *runs.Service, agentsvc *agents.Service) runtime.StepRecorder {
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
			// wave-3 3-b: price model steps through the pricing hook
			// (unknown model -> 0 cents, never an error).
			if step.Type == runtime.StepTypeModel {
				if agent, aerr := agentsvc.GetAgentCtx(context.Background(), run.OrganizationID, run.AgentID); aerr == nil {
					rs.Cost = models.ComputeCostCents(agent.Model,
						step.TokenUsage.PromptTokens, step.TokenUsage.CompletionTokens)
				}
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

	// Provider wiring (issue #15): the env-driven construction moved into
	// models.ProviderFromEnv so the worker and the API's evaluation runner
	// share one configuration path — same env vars (OPENAI_API_KEY,
	// OPENAI_BASE_URL, AGENTOS_WORKER_MODEL), same defaults, same failover
	// chaining (AGENTOS_FALLBACK_API_KEY/AGENTOS_FALLBACK_BASE_URL) and the
	// same log-line semantics. Without OPENAI_API_KEY the runner keeps the
	// deterministic offline mode so local development needs no credentials.
	provider, _ := models.ProviderFromEnv(logr)

	// Task queue (wave-3 3-a): same AGENTOS_QUEUE selection as the API so both
	// processes cooperate on one task flow (redis mode) or stay self-contained
	// (memory mode, the default).
	workQueue, wqerr := queue.NewFromConfig(cfg)
	if wqerr != nil {
		logr.Error("queue backend init failed", "error", wqerr)
		os.Exit(1)
	}
	defer func() { _ = workQueue.Close() }()
	runsService := runs.NewService()
	// Issue #12: the worker owns the point of truth for worker-executed
	// runs. Its own Metrics registry counts terminal run transitions
	// (agentos_runs_total, via runs.Service.SetMetrics) and every executed
	// tool step (agentos_tools_total, via runtime.WithMetrics). The API's
	// /metrics endpoint additionally mirrors terminal outcomes through the
	// worker status callback (cmd/api/handlers.go); this registry stays
	// process-local and is unit-tested, not exposed over HTTP today.
	metricsSvc := observability.NewMetrics()
	runsService.SetMetrics(metricsSvc)
	runner := runtime.NewRunnerWithOptions(agentsvc, registry,
		runtime.WithProvider(provider),
		runtime.WithStepRecorder(stepRecorderAdapter(runsService, agentsvc)),
		runtime.WithMetrics(metricsSvc),
	)
	// wave-3 3-c: durable workflow recovery. One startup pass, then a sweep
	// every DefaultRecoveryInterval (1m). The pass times out runs past their
	// deadline_at (status timeout / WORKFLOW_RUN_TIMEOUT), orphans the
	// pending/running node checkpoints of stale runs (NODE_ORPHANED) and
	// re-enqueues their next pending node through workQueue. Safe to run in
	// several workers: Postgres candidates are selected FOR UPDATE SKIP LOCKED
	// and every transition is a guarded conditional UPDATE.
	var recStore workflows.Store
	if dsn := database.DSNFromEnv(); dsn != "" {
		if recDB, derr := database.Connect(dsn); derr == nil {
			defer func() { _ = recDB.Close() }()
			recStore = workflows.NewPostgresStore(recDB)
		} else {
			logr.Warn("workflow recovery: database unavailable, recovery disabled", "error", derr.Error())
		}
	}
	wfSvc := workflows.NewServiceWithOptions(recStore, workflows.WithStaleAfter(workflows.StaleAfterFromEnv()))
	recoveryCtx, recoveryStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer recoveryStop()
	go func() {
		if err := workflows.NewRecoveryWorker(wfSvc, workflows.DefaultRecoveryInterval).Run(recoveryCtx); err != nil && !errors.Is(err, context.Canceled) {
			logr.Warn("workflow recovery loop stopped", "error", err)
		}
	}()

	processTask := func(task *queue.Task) error {
		ctx := context.Background()
		if task == nil || task.Payload == nil {
			return fmt.Errorf("task payload is required")
		}
		runID, _ := task.Payload["run_id"].(string)
		agentID, _ := task.Payload["agent_id"].(string)
		input, _ := task.Payload["input"].(string)
		if runID == "" || agentID == "" || input == "" {
			return fmt.Errorf("task payload missing run_id, agent_id or input")
		}
		// wave-3 3-c: durable checkpointing of workflow node execution.
		workflowRunID, _ := task.Payload["workflow_run_id"].(string)
		nodeID, _ := task.Payload["node_id"].(string)
		var checkpoint *workflows.NodeRun
		if workflowRunID != "" && nodeID != "" {
			orgID, _ := task.Payload["organization_id"].(string)
			nr, nerr := wfSvc.BeginNodeRun(ctx, orgID, workflowRunID, nodeID, runID)
			switch {
			case errors.Is(nerr, workflows.ErrNodeRunTerminal):
				return nil // replayed task: this attempt is already finished
			case nerr != nil:
				return nerr
			}
			checkpoint = nr
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
			// Fix (found while wiring issue #12 metrics): this branch
			// used to return the stale outer `err` (nil after the
			// successful seed), so failed runs looked successful to
			// the queue loop. Return the real runner error.
			return rerr
		}
		task.Payload["result"] = run.Output
		task.Payload["status"] = string(run.Status)
		if checkpoint != nil {
			fin, code := workflows.RunStatusCompleted, ""
			if string(run.Status) == string(runs.StatusFailed) {
				fin, code = workflows.RunStatusFailed, "NODE_FAILED"
			}
			_ = wfSvc.FinishNodeRun(ctx, checkpoint.OrganizationID, checkpoint.ID, fin, code)
		}
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
