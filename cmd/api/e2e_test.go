package main

// Issue #55 (production hardening): the full-stack end-to-end test.
//
// Unlike the handler-level tests in this package (which wire individual
// register*Routes stacks), this file boots the REAL application constructor —
// newApp, the exact function main() runs — in zero-infrastructure mode and
// drives it through the complete a.routes() stack (CORS, rate limiting and
// metrics middleware included), exactly as an operator would:
//
//      register -> login -> create agent -> create run (QUEUED)
//        -> drive the worker loop in-process against the app's memory queue
//        -> run COMPLETED with steps + usage + cost asserted via the HTTP surface
//        -> audit trail spot-check + canary status 404 for a deployment-less agent
//
// The in-process worker loop mirrors cmd/worker/main.go's processTask
// semantics (RUNNING transition, runtime.Runner execution, terminal
// COMPLETED/FAILED transition, terminal status mirrored to the API through
// POST /runs/{id}/events exactly like postEventWithRetries) with the
// stepRecorderAdapter that turns runtime steps into run_steps rows and prices
// model steps through models.ComputeCostCents.
//
// Two execution modes are pinned, both fully deterministic and offline (no
// network, no credentials):
//
//   - TestFullStackRunLifecycleE2E runs the runner in its offline mode (nil
//     provider — what production does without OPENAI_API_KEY): the runs
//     complete through the calculator tool path and the canned offline-fallback
//     model step, and the usage/cost surfaces report the documented offline
//     contract (unpriced, zero tokens).
//   - TestFullStackWorkerUsageCostE2E attaches models.NewStaticProvider (the
//     repo's deterministic, dependency-free test provider — it reports token
//     usage like a real completion) so the usage -> cost -> billing ledger
//     carries a non-zero signal through the whole HTTP surface.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/config"
	"agentos/internal/logger"
	"agentos/internal/models"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/runtime"
	"agentos/internal/tools"
)

// e2eEnv is one booted application plus the live identity of its first tenant.
type e2eEnv struct {
	app     *app
	handler http.Handler // the full routes() stack, untruncated
	orgID   string
	userID  string
	token   string
}

// newE2EEnv boots the app exactly like main() would in zero-infrastructure
// mode: newApp(nil db) over the in-memory service graph, then routes() for the
// real middleware chain. Environment knobs that could change behavior
// (Redis-backed rate limiting, NATS, a live model provider) are pinned empty.
func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("REDIS_URL", "")
	t.Setenv("AGENTOS_NATS_URL", "")
	t.Setenv("OPENAI_API_KEY", "")

	a := newApp(config.Config{}, logger.New("production"), nil)
	return &e2eEnv{app: a, handler: a.routes()}
}

// do sends one request through the full stack and decodes the JSON object
// response (if any). Authentication uses the env's bearer token once set.
func (e *e2eEnv) do(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	rr := httptest.NewRecorder()
	e.handler.ServeHTTP(rr, req)
	resp := map[string]any{}
	// Some legacy error paths answer plain text via http.Error; only decode
	// when the body actually looks like a JSON object.
	if body := rr.Body.String(); strings.HasPrefix(body, "{") {
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s %s: response is not a JSON object: %v — body=%s", method, path, err, rr.Body.String())
		}
	}
	return rr, resp
}

// registerAndLogin walks the identity flow through the real handlers and
// records the tenant for subsequent requests.
func (e *e2eEnv) registerAndLogin(t *testing.T) {
	t.Helper()
	rr, resp := e.do(t, http.MethodPost, "/api/v1/auth/register",
		`{"organization":"Acme Corp","email":"owner@acme.test","password":"secret123"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register = %d, want 201 — body=%s", rr.Code, rr.Body.String())
	}
	org, ok := resp["organization"].(map[string]any)
	if !ok {
		t.Fatalf("register response missing organization: %s", rr.Body.String())
	}
	user, ok := resp["user"].(map[string]any)
	if !ok {
		t.Fatalf("register response missing user: %s", rr.Body.String())
	}
	e.orgID, _ = org["ID"].(string)
	e.userID, _ = user["ID"].(string)
	if e.orgID == "" || e.userID == "" {
		t.Fatalf("register response missing org/user ids: %s", rr.Body.String())
	}

	rr, resp = e.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"owner@acme.test","password":"secret123"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200 — body=%s", rr.Code, rr.Body.String())
	}
	token, _ := resp["token"].(string)
	if token == "" {
		t.Fatalf("login response missing token: %s", rr.Body.String())
	}
	e.token = token
}

// createAgent walks POST /agents/create and returns the new agent id.
func (e *e2eEnv) createAgent(t *testing.T, name string) string {
	t.Helper()
	rr, resp := e.do(t, http.MethodPost, "/api/v1/agents/create",
		`{"name":"`+name+`","description":"e2e agent","instructions":"answer helpfully","model":"gpt-4o-mini"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create agent = %d, want 201 — body=%s", rr.Code, rr.Body.String())
	}
	agentID, _ := resp["ID"].(string)
	if agentID == "" {
		t.Fatalf("create agent response missing ID: %s", rr.Body.String())
	}
	return agentID
}

// createRun walks POST /runs and returns the queued run id.
func (e *e2eEnv) createRun(t *testing.T, agentID, input string) string {
	t.Helper()
	rr, resp := e.do(t, http.MethodPost, "/api/v1/runs",
		`{"agent_id":"`+agentID+`","input":`+jsonQuote(input)+`}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create run = %d, want 201 — body=%s", rr.Code, rr.Body.String())
	}
	if status, _ := resp["status"].(string); status != "queued" {
		t.Fatalf("create run status = %q, want queued — body=%s", status, rr.Body.String())
	}
	runID, _ := resp["run_id"].(string)
	if runID == "" {
		t.Fatalf("create run response missing run_id: %s", rr.Body.String())
	}
	return runID
}

// workerRunner builds the execution engine for the in-process worker loop:
// the same construction cmd/worker/main.go uses (registry + step recorder +
// optional provider), with the step recorder adapted onto the app's runs
// service so worker-executed steps are observable through the API surface.
// (In the two-process deployment the worker keeps its own in-memory runs
// service and mirrors only terminal statuses to the API; sharing the service
// here makes the steps part of the asserted surface. The events mirror below
// still exercises the production relay path.)
func (e *e2eEnv) workerRunner(provider models.Provider) *runtime.Runner {
	agentSvc := e.app.agentsSvc
	runsSvc := e.app.runsSvc
	// Same tool seed as cmd/worker/main.go.
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())
	recorder := runtime.StepRecorderFunc(func(ctx context.Context, runID string, step runtime.Step) error {
		if step.Input == "" && step.Output == "" && step.Error == "" {
			return nil
		}
		run, ok := runsSvc.Get(runID)
		if !ok {
			return nil // run already pruned; nothing to attach the step to
		}
		rs := &runs.Step{
			StepType:    step.Type,
			Status:      step.Status,
			InputMeta:   map[string]any{"input": step.Input, "name": step.Name, "index": step.Index},
			OutputMeta:  map[string]any{"output": step.Output},
			Error:       step.Error,
			StartedAt:   time.Now().UTC().Add(-time.Duration(step.DurationMS) * time.Millisecond),
			CompletedAt: time.Now().UTC(),
		}
		if step.TokenUsage.TotalTokens > 0 {
			rs.TokenUsage = map[string]any{
				"prompt_tokens":     step.TokenUsage.PromptTokens,
				"completion_tokens": step.TokenUsage.CompletionTokens,
				"total_tokens":      step.TokenUsage.TotalTokens,
			}
			// Wave-3 3-b pricing hook: model steps are priced from the
			// agent's configured model (unknown model -> 0 cents).
			if step.Type == runtime.StepTypeModel {
				if agent, aerr := agentSvc.GetAgentCtx(ctx, run.OrganizationID, run.AgentID); aerr == nil {
					rs.Cost = models.ComputeCostCents(agent.Model,
						step.TokenUsage.PromptTokens, step.TokenUsage.CompletionTokens)
				}
			}
		}
		return runsSvc.RecordStep(ctx, run.OrganizationID, runID, rs)
	})
	return runtime.NewRunnerWithOptions(e.app.agentsSvc, registry,
		runtime.WithProvider(provider),
		runtime.WithStepRecorder(recorder),
	)
}

// driveWorkerLoop runs ONE pass of the production worker loop over the app's
// memory queue: queue.Worker.ProcessNext -> processTask, mirroring
// cmd/worker/main.go (RUNNING transition, runner execution, terminal
// transition, terminal status mirrored to the API via POST /runs/{id}/events
// exactly like postEventWithRetries — the events relay in runEventsHandler is
// what lands the durable outcome in the API process in the two-process
// deployment).
func (e *e2eEnv) driveWorkerLoop(t *testing.T, runner *runtime.Runner) {
	t.Helper()
	worker := queue.NewWorker(e.app.queueSvc, func(task *queue.Task) error {
		if task == nil || task.Payload == nil {
			return fmt.Errorf("task payload is required")
		}
		runID, _ := task.Payload["run_id"].(string)
		agentID, _ := task.Payload["agent_id"].(string)
		input, _ := task.Payload["input"].(string)
		if runID == "" || agentID == "" || input == "" {
			return fmt.Errorf("task payload missing run_id, agent_id or input")
		}
		// Mark running on the worker's runs service (trusted internal path).
		if err := e.app.runsSvc.UpdateStatus(runID, runs.StatusRunning, ""); err != nil {
			return err
		}
		run, rerr := runner.RunWithID(context.Background(), runID, agentID, input)
		if rerr != nil {
			_ = e.app.runsSvc.UpdateStatus(runID, runs.StatusFailed, "")
			return rerr
		}
		if err := e.app.runsSvc.UpdateStatus(runID, runs.StatusCompleted, run.Output); err != nil {
			return err
		}
		// Mirror the terminal status to the API through the real events
		// endpoint (postEventWithRetries equivalent; retry loop elided —
		// the handler is in-process here).
		rr, _ := e.do(t, http.MethodPost, "/api/v1/runs/"+runID+"/events",
			`{"type":"status","name":"status.changed","payload":{"status":"COMPLETED","output":`+jsonQuote(run.Output)+`}}`)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status mirror for run %s = %d, want 204 — body=%s", runID, rr.Code, rr.Body.String())
		}
		return nil
	})
	if err := worker.ProcessNext(); err != nil {
		t.Fatalf("worker ProcessNext returned error: %v", err)
	}
}

// jsonQuote renders s as a JSON string literal (keeps the request bodies
// readable without hand-escaping).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestFullStackRunLifecycleE2E is the issue #55 headline flow: register ->
// login -> create agent -> create two runs -> drive the worker loop in-process
// (memory queue, offline runner) -> both runs COMPLETED with steps asserted
// through the API, offline usage/cost contract pinned, audit trail spot-checked
// and canary status 404 for a deployment-less agent.
func TestFullStackRunLifecycleE2E(t *testing.T) {
	e := newE2EEnv(t)

	// Unauthenticated surface stays closed through the full stack.
	rr, _ := e.do(t, http.MethodGet, "/api/v1/runs", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /runs = %d, want 401", rr.Code)
	}

	// 1) register -> login (bad credentials rejected along the way).
	rr, _ = e.do(t, http.MethodPost, "/api/v1/auth/login", `{"email":"owner@acme.test","password":"wrong"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("login with wrong password = %d, want 401", rr.Code)
	}
	e.registerAndLogin(t)

	// 2) create agent.
	agentID := e.createAgent(t, "Support Agent")

	// 3) create runs: math input routes to the offline calculator tool step,
	// plain input to the canned offline-fallback model step.
	runTool := e.createRun(t, agentID, "What is 21+21?")
	runModel := e.createRun(t, agentID, "say hi")
	if got := e.app.queueSvc.Length(); got != 2 {
		t.Fatalf("queue length after two creates = %d, want 2", got)
	}

	// 4) drive the worker loop in-process (memory queue, offline runner —
	// nil provider is exactly what production runs on without OPENAI_API_KEY).
	runner := e.workerRunner(nil)
	e.driveWorkerLoop(t, runner) // consumes runTool
	e.driveWorkerLoop(t, runner) // consumes runModel
	if got := e.app.queueSvc.Length(); got != 0 {
		t.Fatalf("queue length after worker passes = %d, want 0 (tasks must be ACKed)", got)
	}

	// 5) runTool: COMPLETED with the calculator tool step.
	rr, run := e.do(t, http.MethodGet, "/api/v1/runs/"+runTool, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get run = %d, want 200 — body=%s", rr.Code, rr.Body.String())
	}
	if status, _ := run["Status"].(string); status != "COMPLETED" {
		t.Fatalf("tool run status = %q, want COMPLETED — body=%s", status, rr.Body.String())
	}
	if output, _ := run["Output"].(string); output != "42" {
		t.Fatalf("tool run output = %q, want the calculator result 42", output)
	}
	if cost, _ := run["total_cost_cents"].(float64); cost != 0 {
		t.Fatalf("offline run total_cost_cents = %v, want 0 (offline mode is unpriced)", cost)
	}
	step := e2eSingleStep(t, e, runTool)
	if got, _ := step["StepType"].(string); got != "tool" {
		t.Fatalf("tool run step type = %q, want tool", got)
	}
	if got, _ := step["Status"].(string); got != "succeeded" {
		t.Fatalf("tool run step status = %q, want succeeded", got)
	}
	inputMeta, _ := step["InputMeta"].(map[string]any)
	if got, _ := inputMeta["name"].(string); got != "calculator" {
		t.Fatalf("tool run step name = %q, want calculator", got)
	}
	if got, _ := inputMeta["input"].(string); got != "21+21" {
		t.Fatalf("tool run step input = %q, want the extracted expression", got)
	}
	outputMeta, _ := step["OutputMeta"].(map[string]any)
	if got, _ := outputMeta["output"].(string); got != "42" {
		t.Fatalf("tool run step output = %q, want 42", got)
	}
	if step["TokenUsage"] != nil {
		t.Fatalf("offline tool step must carry no token usage, got %v", step["TokenUsage"])
	}
	if cost, _ := step["Cost"].(float64); cost != 0 {
		t.Fatalf("offline tool step cost = %v, want 0", cost)
	}

	// 6) runModel: COMPLETED with the deterministic offline-fallback model step.
	rr, run = e.do(t, http.MethodGet, "/api/v1/runs/"+runModel, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get model run = %d, want 200 — body=%s", rr.Code, rr.Body.String())
	}
	if status, _ := run["Status"].(string); status != "COMPLETED" {
		t.Fatalf("model run status = %q, want COMPLETED", status)
	}
	wantOutput := "Completed Support Agent in response to: say hi"
	if output, _ := run["Output"].(string); output != wantOutput {
		t.Fatalf("model run output = %q, want %q", output, wantOutput)
	}
	step = e2eSingleStep(t, e, runModel)
	if got, _ := step["StepType"].(string); got != "model" {
		t.Fatalf("model run step type = %q, want model", got)
	}
	if got, _ := step["Status"].(string); got != "succeeded" {
		t.Fatalf("model run step status = %q, want succeeded", got)
	}
	inputMeta, _ = step["InputMeta"].(map[string]any)
	if got, _ := inputMeta["name"].(string); got != "offline-fallback" {
		t.Fatalf("model run step name = %q, want offline-fallback", got)
	}
	outputMeta, _ = step["OutputMeta"].(map[string]any)
	if got, _ := outputMeta["output"].(string); got != wantOutput {
		t.Fatalf("model run step output = %q, want %q", got, wantOutput)
	}
	if step["TokenUsage"] != nil {
		t.Fatalf("offline model step must carry no token usage, got %v", step["TokenUsage"])
	}

	// 7) usage + cost surfaces: both offline runs aggregate unpriced.
	rr, report := e.do(t, http.MethodGet, "/api/v1/usage/costs?group_by=agent", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("usage costs = %d, want 200 — body=%s", rr.Code, rr.Body.String())
	}
	if total, _ := report["total_cost_cents"].(float64); total != 0 {
		t.Fatalf("usage report total_cost_cents = %v, want 0 for offline runs", total)
	}
	series, _ := report["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("usage report series = %d buckets, want 1 (both runs on one agent)", len(series))
	}
	bucket, _ := series[0].(map[string]any)
	if got, _ := bucket["agent_id"].(string); got != agentID {
		t.Fatalf("usage bucket agent_id = %q, want %q", got, agentID)
	}
	if got, _ := bucket["runs"].(float64); got != 2 {
		t.Fatalf("usage bucket runs = %v, want 2", got)
	}

	// 8) audit spot-check: the mutations are on the trail with the right
	// actor and resources.
	rr, aud := e.do(t, http.MethodGet, "/api/v1/audit-events", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("audit events = %d, want 200 — body=%s", rr.Code, rr.Body.String())
	}
	events, _ := aud["events"].([]any)
	actions := map[string]string{} // action -> resource
	for _, raw := range events {
		entry, _ := raw.(map[string]any)
		action, _ := entry["action"].(string)
		resource, _ := entry["resource"].(string)
		actions[action] = resource
	}
	if res := actions["agent.created"]; res != "agents/"+agentID {
		t.Fatalf("audit agent.created resource = %q, want agents/%s (all=%v)", res, agentID, actions)
	}
	if res := actions["run.created"]; res != "runs/"+runTool && res != "runs/"+runModel {
		t.Fatalf("audit run.created resource = %q, want one of runs/%s|runs/%s", res, runTool, runModel)
	}
	for _, raw := range events {
		entry, _ := raw.(map[string]any)
		if action, _ := entry["action"].(string); action == "run.created" {
			if actor, _ := entry["actor"].(string); actor != e.userID {
				t.Fatalf("audit run.created actor = %q, want the registering user %q", actor, e.userID)
			}
			break
		}
	}

	// 9) canary status for a deployment-less agent: 404, structured envelope.
	rr, errBody := e.do(t, http.MethodGet, "/api/v1/agents/"+agentID+"/canary/status", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("canary status with no deployment = %d, want 404 — body=%s", rr.Code, rr.Body.String())
	}
	errObj, _ := errBody["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "NOT_FOUND" {
		t.Fatalf("canary status error code = %v, want NOT_FOUND", errObj["code"])
	}
}

// TestFullStackWorkerUsageCostE2E pins the non-zero usage -> cost -> billing
// ledger through the same full-stack flow: the deterministic StaticProvider
// reports token usage, the worker loop prices the model step through
// models.ComputeCostCents, and every surface (run row, run step, usage-costs
// report) agrees on the number.
func TestFullStackWorkerUsageCostE2E(t *testing.T) {
	// Deterministic pricing for the agent's model (USD per 1M tokens),
	// mirroring cmd/api/eval_provider_test.go.
	t.Setenv("AGENTOS_PRICING_JSON", `[{"model":"gpt-4o-mini","input_per_m_tokens":1,"output_per_m_tokens":3}]`)

	e := newE2EEnv(t)
	e.registerAndLogin(t)
	agentID := e.createAgent(t, "Support Agent")
	runID := e.createRun(t, agentID, "tell me a joke")

	// StaticProvider (echo mode) is the repo's deterministic offline test
	// provider: same in-process execution, but the completion reports real
	// token usage so the cost pipeline carries a non-zero signal.
	runner := e.workerRunner(models.NewStaticProvider("static-e2e", ""))
	e.driveWorkerLoop(t, runner)

	// Run row: COMPLETED with the echoed completion and the priced total.
	rr, run := e.do(t, http.MethodGet, "/api/v1/runs/"+runID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get run = %d, want 200 — body=%s", rr.Code, rr.Body.String())
	}
	if status, _ := run["Status"].(string); status != "COMPLETED" {
		t.Fatalf("run status = %q, want COMPLETED — body=%s", status, rr.Body.String())
	}
	wantOutput := "echo: tell me a joke"
	if output, _ := run["Output"].(string); output != wantOutput {
		t.Fatalf("run output = %q, want the StaticProvider echo %q", output, wantOutput)
	}

	// Run step: one model step carrying the provider usage and priced cost.
	step := e2eSingleStep(t, e, runID)
	if got, _ := step["StepType"].(string); got != "model" {
		t.Fatalf("step type = %q, want model", got)
	}
	usage, _ := step["TokenUsage"].(map[string]any)
	if usage == nil {
		t.Fatalf("provider-mode step must carry token usage, got nil — step=%v", step)
	}
	promptTokens, _ := usage["prompt_tokens"].(float64)
	completionTokens, _ := usage["completion_tokens"].(float64)
	totalTokens, _ := usage["total_tokens"].(float64)
	if promptTokens <= 0 || completionTokens <= 0 || totalTokens != promptTokens+completionTokens {
		t.Fatalf("step token usage = %v, want positive prompt+completion and total = sum", usage)
	}
	stepCost, _ := step["Cost"].(float64)
	wantCost := models.ComputeCostCents("gpt-4o-mini", int(promptTokens), int(completionTokens))
	if wantCost <= 0 {
		t.Fatalf("recomputed cost = %v, want > 0 (deterministic pricing table must apply)", wantCost)
	}
	if !almostEqualCents(stepCost, wantCost) {
		t.Fatalf("step Cost = %v, want the pricing-hook value %v", stepCost, wantCost)
	}

	// The run total and the usage report must agree with the step cost.
	runTotal, _ := run["total_cost_cents"].(float64)
	if !almostEqualCents(runTotal, wantCost) {
		t.Fatalf("run total_cost_cents = %v, want the single step cost %v", runTotal, wantCost)
	}
	rr, report := e.do(t, http.MethodGet, "/api/v1/usage/costs?group_by=agent", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("usage costs = %d, want 200 — body=%s", rr.Code, rr.Body.String())
	}
	if total, _ := report["total_cost_cents"].(float64); !almostEqualCents(total, wantCost) {
		t.Fatalf("usage report total_cost_cents = %v, want %v", total, wantCost)
	}
	series, _ := report["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("usage report series = %d buckets, want 1", len(series))
	}
	bucket, _ := series[0].(map[string]any)
	if got, _ := bucket["agent_id"].(string); got != agentID {
		t.Fatalf("usage bucket agent_id = %q, want %q", got, agentID)
	}
	if got, _ := bucket["runs"].(float64); got != 1 {
		t.Fatalf("usage bucket runs = %v, want 1", got)
	}
	if got, _ := bucket["cost_cents"].(float64); !almostEqualCents(got, wantCost) {
		t.Fatalf("usage bucket cost_cents = %v, want %v", got, wantCost)
	}
}

// e2eSingleStep fetches GET /runs/{id}/steps through the full stack and
// returns the single recorded step (every e2e run records exactly one).
func e2eSingleStep(t *testing.T, e *e2eEnv, runID string) map[string]any {
	t.Helper()
	rr, resp := e.do(t, http.MethodGet, "/api/v1/runs/"+runID+"/steps", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get steps for run %s = %d, want 200 — body=%s", runID, rr.Code, rr.Body.String())
	}
	steps, _ := resp["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("run %s recorded %d steps, want exactly 1 — body=%s", runID, len(steps), rr.Body.String())
	}
	step, _ := steps[0].(map[string]any)
	if step == nil {
		t.Fatalf("run %s step is not an object: %s", runID, rr.Body.String())
	}
	return step
}
