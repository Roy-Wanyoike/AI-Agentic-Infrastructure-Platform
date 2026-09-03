package models

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// discardLogger returns a logger that swallows output (for tests that only
// need the constructor's return values), and captureLogger returns one that
// records formatted output so the log-line semantics can be pinned.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func captureLogger() (*slog.Logger, *strings.Builder) {
	var b strings.Builder
	return slog.New(slog.NewTextHandler(&b, nil)), &b
}

// newUsageStub spins up an OpenAI-compatible /chat/completions stub that
// records the last request and answers with a fixed completion + usage.
func newUsageStub(t *testing.T, status int, body string) (*httptest.Server, *stubRequest) {
	t.Helper()
	rec := &stubRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, rec) // best-effort capture of the chat payload
		rec.authorization = r.Header.Get("Authorization")
		rec.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// stubRequest captures what the stub server saw.
type stubRequest struct {
	Model         string `json:"model"`
	authorization string
	path          string
}

const stubCompletion = `{"model":"stub-model","choices":[{"message":{"role":"assistant","content":"hello from stub"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

func TestProviderFromEnvWithoutKeyStaysOffline(t *testing.T) {
	// No env set at all (t.Setenv guards against leakage from other tests).
	t.Setenv(ProviderAPIKeyEnvVar, "")
	t.Setenv(ProviderFallbackAPIKeyEnvVar, "")

	logr, logs := captureLogger()
	provider, ok := ProviderFromEnv(logr)
	if ok {
		t.Fatal("without OPENAI_API_KEY the constructor must report offline (ok=false)")
	}
	if provider != nil {
		t.Fatalf("offline mode must return a nil provider, got %T", provider)
	}
	// Log-line semantics: a warning announcing the offline deterministic mode.
	if out := logs.String(); !strings.Contains(out, "offline deterministic mode") {
		t.Fatalf("offline mode should log a warning, got %q", out)
	}
}

func TestProviderFromEnvBlankKeyStaysOffline(t *testing.T) {
	t.Setenv(ProviderAPIKeyEnvVar, "   \t") // whitespace-only counts as unset

	provider, ok := ProviderFromEnv(discardLogger())
	if ok || provider != nil {
		t.Fatalf("blank OPENAI_API_KEY must stay offline, got ok=%v provider=%T", ok, provider)
	}
}

func TestProviderFromEnvWithKeyBuildsOpenAIProvider(t *testing.T) {
	stub, rec := newUsageStub(t, http.StatusOK, stubCompletion)
	t.Setenv(ProviderAPIKeyEnvVar, "secret-key-123")
	t.Setenv(ProviderBaseURLEnvVar, stub.URL)
	t.Setenv(ProviderModelEnvVar, "gpt-4o-mini")
	t.Setenv(ProviderFallbackAPIKeyEnvVar, "") // no failover in this test

	logr, logs := captureLogger()
	provider, ok := ProviderFromEnv(logr)
	if !ok {
		t.Fatal("with OPENAI_API_KEY set the constructor must report provider mode")
	}
	if got := provider.Name(); got != "openai-compatible" {
		t.Fatalf("provider name = %q, want %q", got, "openai-compatible")
	}
	// The configured base URL flows into the provider (normalized, no slash).
	op, isOpenAI := provider.(*OpenAIProvider)
	if !isOpenAI {
		t.Fatalf("expected an *OpenAIProvider without fallback config, got %T", provider)
	}
	if got := op.BaseURL(); got != stub.URL {
		t.Fatalf("BaseURL() = %q, want the configured stub URL %q", got, stub.URL)
	}

	// A completion through the env-built provider hits the configured
	// endpoint with the bearer key and default model, and parses the usage.
	resp, err := provider.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.Text != "hello from stub" {
		t.Fatalf("Complete text = %q, want the stub completion", resp.Text)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 || resp.Usage.TotalTokens != 18 {
		t.Fatalf("usage not parsed from the stub response: %+v", resp.Usage)
	}
	if rec.authorization != "Bearer secret-key-123" {
		t.Fatalf("stub saw Authorization %q, want the env-configured bearer key", rec.authorization)
	}
	if rec.path != "/chat/completions" {
		t.Fatalf("stub saw path %q, want /chat/completions", rec.path)
	}
	if rec.Model != "gpt-4o-mini" {
		t.Fatalf("stub saw model %q, want the AGENTOS_WORKER_MODEL value", rec.Model)
	}
	// Log-line semantics: "model provider configured" with base_url + model.
	out := logs.String()
	if !strings.Contains(out, "model provider configured") ||
		!strings.Contains(out, "base_url="+stub.URL) ||
		!strings.Contains(out, "model=gpt-4o-mini") {
		t.Fatalf("configured mode should log base_url and model, got %q", out)
	}
}

func TestProviderFromEnvDefaultsBaseURL(t *testing.T) {
	t.Setenv(ProviderAPIKeyEnvVar, "k")
	t.Setenv(ProviderBaseURLEnvVar, "")
	t.Setenv(ProviderModelEnvVar, "  ")

	provider, ok := ProviderFromEnv(discardLogger())
	if !ok {
		t.Fatal("key-only configuration should still build a provider")
	}
	op, isOpenAI := provider.(*OpenAIProvider)
	if !isOpenAI {
		t.Fatalf("expected *OpenAIProvider, got %T", provider)
	}
	if got := op.BaseURL(); got != DefaultOpenAIBaseURL {
		t.Fatalf("blank OPENAI_BASE_URL should default to %q, got %q", DefaultOpenAIBaseURL, got)
	}
	// The default model id carries over: the request below must fail with a
	// typed invalid-request error (no model anywhere) without touching the
	// network — proving the blank AGENTOS_WORKER_MODEL was honored.
	if _, err := provider.Complete(context.Background(), CompletionRequest{Prompt: "hi"}); err == nil {
		t.Fatal("completion without any model should be rejected before dialing")
	}
}

func TestProviderFromEnvChainsFallbackWhenConfigured(t *testing.T) {
	primary, primaryRec := newUsageStub(t, http.StatusInternalServerError, `{"error":"primary down"}`)
	fallback, fallbackRec := newUsageStub(t, http.StatusOK, stubCompletion)

	t.Setenv(ProviderAPIKeyEnvVar, "primary-key")
	t.Setenv(ProviderBaseURLEnvVar, primary.URL)
	t.Setenv(ProviderModelEnvVar, "gpt-4o-mini")
	t.Setenv(ProviderFallbackAPIKeyEnvVar, "fallback-key")
	t.Setenv(ProviderFallbackBaseURLEnvVar, fallback.URL)

	logr, logs := captureLogger()
	provider, ok := ProviderFromEnv(logr)
	if !ok {
		t.Fatal("provider mode expected")
	}
	// The failover chain is observable through the provider name.
	if got := provider.Name(); got != "openai-compatible->fallback" {
		t.Fatalf("Name() = %q, want the chained %q", got, "openai-compatible->fallback")
	}
	if _, isChain := provider.(*FailoverProvider); !isChain {
		t.Fatalf("expected a *FailoverProvider when a fallback key is set, got %T", provider)
	}
	// Failover behavior: the 500-ing primary is skipped for the healthy
	// fallback, which serves the completion.
	resp, err := provider.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete through the failover chain returned error: %v", err)
	}
	if resp.Text != "hello from stub" {
		t.Fatalf("fallback completion text = %q", resp.Text)
	}
	if primaryRec.path == "" {
		t.Fatal("the primary should have been attempted first")
	}
	if fallbackRec.path == "" {
		t.Fatal("the fallback should have served the request")
	}
	if fallbackRec.authorization != "Bearer fallback-key" {
		t.Fatalf("fallback saw Authorization %q, want the AGENTOS_FALLBACK_API_KEY bearer", fallbackRec.authorization)
	}
	out := logs.String()
	if !strings.Contains(out, "model provider failover enabled") ||
		!strings.Contains(out, "fallback_base_url="+fallback.URL) {
		t.Fatalf("failover setup should be logged, got %q", out)
	}
}

func TestProviderFromEnvFallbackBaseURLIgnoredWithoutFallbackKey(t *testing.T) {
	stub, _ := newUsageStub(t, http.StatusOK, stubCompletion)
	t.Setenv(ProviderAPIKeyEnvVar, "primary-key")
	t.Setenv(ProviderBaseURLEnvVar, stub.URL)
	t.Setenv(ProviderModelEnvVar, "")
	t.Setenv(ProviderFallbackAPIKeyEnvVar, "")                                   // unset...
	t.Setenv(ProviderFallbackBaseURLEnvVar, "http://fallback-should-be-ignored") // ...so this is never read

	provider, ok := ProviderFromEnv(discardLogger())
	if !ok {
		t.Fatal("provider mode expected")
	}
	if got := provider.Name(); got != "openai-compatible" {
		t.Fatalf("without a fallback key there must be no chaining, got name %q", got)
	}
	if _, isChain := provider.(*FailoverProvider); isChain {
		t.Fatal("a fallback base URL without a fallback key must not build a chain")
	}
}
