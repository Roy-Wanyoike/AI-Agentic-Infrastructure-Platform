package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentos/internal/agents"
	"agentos/internal/evaluations"
	"agentos/internal/models"
	"agentos/internal/runtime"
)

// TestEvalRunnerWithEnvProviderReportsUsageAndCost pins the issue #15
// API-process wiring end to end against a stub OpenAI-compatible server:
// models.ProviderFromEnv builds the provider from env, the runner executes an
// eval case against it (real token usage on runtime.Run.Tokens), and the
// evaluations service prices that usage into per-case cost_cents through the
// models.ComputeCostCents hook — exactly the wiring newApp performs.
//
// Without OPENAI_API_KEY the same wiring stays deterministic-offline; that
// contract is covered by TestProviderFromEnv* in internal/models.
func TestEvalRunnerWithEnvProviderReportsUsageAndCost(t *testing.T) {
	// Deterministic pricing for the stub model (USD per 1M tokens):
	// (1000*1 + 500*3)/1e6*100 = 0.25 cents for the usage below.
	t.Setenv("AGENTOS_PRICING_JSON", `[{"model":"gpt-4o-mini","input_per_m_tokens":1,"output_per_m_tokens":3}]`)

	var sawAuth, sawModel string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		sawAuth = r.Header.Get("Authorization")
		sawModel = payload.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"2"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`))
	}))
	defer stub.Close()

	t.Setenv(models.ProviderAPIKeyEnvVar, "test-key-123")
	t.Setenv(models.ProviderBaseURLEnvVar, stub.URL)
	t.Setenv(models.ProviderModelEnvVar, "") // the model comes from the agent config

	provider, ok := models.ProviderFromEnv(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !ok {
		t.Fatal("with OPENAI_API_KEY set the API process must wire a real provider")
	}

	ctx := context.Background()
	agentSvc := agents.NewService()
	agent, err := agentSvc.CreateAgentCtx(ctx, "org-eval", "Eval Agent", "d", "answer concisely", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx returned error: %v", err)
	}

	// Same wiring as newApp: provider-driven runner behind the eval service.
	runner := runtime.NewRunnerWithOptions(agentSvc, nil, runtime.WithProvider(provider))

	// 1) The runner itself reports the provider's real token usage.
	run, err := runner.Run(ctx, agent.ID, "What is 1+1?")
	if err != nil {
		t.Fatalf("runner.Run returned error: %v", err)
	}
	if run.Output != "2" {
		t.Fatalf("runner output = %q, want the stub completion", run.Output)
	}
	if run.Tokens.PromptTokens != 1000 || run.Tokens.CompletionTokens != 500 || run.Tokens.TotalTokens != 1500 {
		t.Fatalf("runner must surface provider usage, got %+v", run.Tokens)
	}
	if sawAuth != "Bearer test-key-123" {
		t.Fatalf("stub saw Authorization %q, want the env-configured bearer key", sawAuth)
	}
	if sawModel != "gpt-4o-mini" {
		t.Fatalf("stub saw model %q, want the agent's configured model", sawModel)
	}

	// 2) The eval flow prices the reported usage through the pricing hook.
	evalSvc := evaluations.NewService(evaluations.Deps{
		Agents:      agentSvc,
		Runner:      runner,
		CaseTimeout: 5 * time.Second,
	})
	evalSvc.AttachUsageSource(evaluations.UsageSourceFunc(func(orgID, agentID string) (string, bool) {
		ag, err := agentSvc.GetAgentCtx(context.Background(), orgID, agentID)
		if err != nil {
			return "", false
		}
		return ag.Model, true
	}))

	dataset, err := evalSvc.CreateDataset(ctx, "org-eval", "stub suite", "issue #15",
		[]evaluations.Case{{ID: "c1", Input: "What is 1+1?", Expected: "2", Scorer: evaluations.ScorerExact}})
	if err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}

	evalRun, err := evalSvc.RunDataset(ctx, "org-eval", dataset.ID, agent.ID)
	if err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	if evalRun.Status != evaluations.StatusCompleted {
		t.Fatalf("eval run status = %q, want completed", evalRun.Status)
	}
	if len(evalRun.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(evalRun.Results))
	}
	res := evalRun.Results[0]
	if !res.Passed || res.Output != "2" {
		t.Fatalf("case should pass on the stub completion, got passed=%v output=%q err=%q", res.Passed, res.Output, res.Error)
	}
	// Non-zero, correctly priced cost: (1000*1 + 500*3)/1e6 * 100 = 0.25 cents.
	if !almostEqualCents(res.CostCents, 0.25) {
		t.Fatalf("result cost_cents = %v, want 0.25 (pricing hook must see real usage)", res.CostCents)
	}
	if evalRun.Summary == nil || !almostEqualCents(evalRun.Summary.TotalCostCents, 0.25) {
		t.Fatalf("summary total_cost_cents should aggregate the priced usage, got %+v", evalRun.Summary)
	}
}

func almostEqualCents(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
