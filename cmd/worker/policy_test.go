package main

// Tests for the worker-side policy enforcement wiring (issue #75): the
// decision-source selection matrix and the run-scope context stamping. The
// enforcement behavior itself (denied tool never invoked, fail-closed on a
// failing source) is pinned by internal/runtime and internal/policies.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentos/internal/policies"
)

// discardLogger keeps test output clean while exercising the real log lines.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWorkerPolicyEvaluatorSelection(t *testing.T) {
	logr := discardLogger()

	// Pull mode with an API base: the remote evaluate endpoint.
	t.Setenv("AGENTOS_API_PULL", "true")
	t.Setenv("AGENTOS_API", "http://api.internal:8080")
	t.Setenv("AGENTOS_API_KEY", "k-1")
	ev := newWorkerPolicyEvaluator(logr, nil)
	if _, ok := ev.(*policies.HTTPEvaluator); !ok {
		t.Fatalf("pull-mode evaluator = %T, want *policies.HTTPEvaluator", ev)
	}

	// Pull mode without AGENTOS_API falls back to local state (in-memory).
	t.Setenv("AGENTOS_API", "")
	ev = newWorkerPolicyEvaluator(logr, nil)
	if _, ok := ev.(*policies.Service); !ok {
		t.Fatalf("fallback evaluator = %T, want *policies.Service", ev)
	}

	// Default (no pull mode, no DB): process-local in-memory service whose
	// decisions default to allow for organizations with no policies.
	t.Setenv("AGENTOS_API_PULL", "")
	ev = newWorkerPolicyEvaluator(logr, nil)
	svc, ok := ev.(*policies.Service)
	if !ok {
		t.Fatalf("default evaluator = %T, want *policies.Service", ev)
	}
	d, err := svc.EvaluateCtx(context.Background(), "org-1", policies.EvaluateRequest{
		Action:   policies.ActionToolCall,
		Resource: policies.Resource{Type: policies.ResourceTool, ID: "calculator"},
		Context:  policies.EvalContext{Environment: policies.DefaultEnvironment, Tool: "calculator"},
	})
	if err != nil || !d.Allowed() {
		t.Fatalf("no-policy evaluation = %v, %v; want default allow", d, err)
	}
}

func TestWorkerPolicyEvaluatorPullModeRoundTrip(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision":"deny","matched_policy_id":"p1","reason":"tool not on allowlist"}`))
	}))
	defer srv.Close()

	t.Setenv("AGENTOS_API_PULL", "true")
	t.Setenv("AGENTOS_API", srv.URL)
	t.Setenv("AGENTOS_API_KEY", "key-9")

	ev := newWorkerPolicyEvaluator(discardLogger(), nil)
	d, err := ev.EvaluateCtx(context.Background(), "org-1", policies.EvaluateRequest{Action: policies.ActionToolCall})
	if err != nil {
		t.Fatalf("EvaluateCtx returned error: %v", err)
	}
	if d.Allowed() || d.MatchedPolicyID != "p1" {
		t.Fatalf("decision = %+v, want the remote deny", d)
	}
	if gotPath != "/v1/policies/evaluate" || gotKey != "key-9" {
		t.Fatalf("request = %q key %q, want evaluate endpoint with the API key", gotPath, gotKey)
	}
}

func TestRunScopeContextStamping(t *testing.T) {
	ctx := runScopeContext(context.Background(), "org-7", "staging")
	scope := policies.RunScopeFromContext(ctx)
	if scope.OrganizationID != "org-7" || scope.Environment != "staging" {
		t.Fatalf("scope = %+v, want org-7/staging", scope)
	}
	// Blank environment keeps the platform default (resolved at evaluation
	// time), so legacy payloads without an environment stay governed.
	ctx = runScopeContext(context.Background(), "org-7", "")
	if got := policies.RunScopeFromContext(ctx).Environment; got != policies.DefaultEnvironment {
		t.Fatalf("blank environment = %q, want default %q", got, policies.DefaultEnvironment)
	}
}
