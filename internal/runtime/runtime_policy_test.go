package runtime

// Issue #75: governance enforcement at the tool seam. These tests pin the
// contract that a policy-denied tool is NEVER invoked, that the denial is
// visible on the recorded step (policy id + decision + reason + step error),
// that a failing decision source fails closed, and that an absent enforcer
// keeps byte-identical legacy behavior.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/models"
	"agentos/internal/policies"
	"agentos/internal/tools"
)

// recordingRecorder collects recorded steps for assertions.
type recordingRecorder struct {
	mu    sync.Mutex
	steps []Step
}

func (r *recordingRecorder) RecordStep(_ context.Context, _ string, step Step) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
	return nil
}

func (r *recordingRecorder) all() []Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Step(nil), r.steps...)
}

// stubEnforcer wraps an explicit-flag enforcer over a canned decision/error.
type stubEnforcer struct {
	decision  policies.Decision
	err       error
	mu        sync.Mutex
	evaluated []string
}

func (s *stubEnforcer) enforcer() *policies.Enforcer {
	return policies.NewEnforcerWithFlag(policies.EvaluatorFunc(func(ctx context.Context, orgID string, req policies.EvaluateRequest) (policies.Decision, error) {
		s.mu.Lock()
		s.evaluated = append(s.evaluated, req.Action+"/"+req.Resource.ID)
		s.mu.Unlock()
		if s.err != nil {
			return policies.Decision{}, s.err
		}
		return s.decision, nil
	}), true)
}

func (s *stubEnforcer) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.evaluated...)
}

// newPolicyTestRunner builds an offline runner with a calculator registry and
// the given enforcer.
func newPolicyTestRunner(t *testing.T, enforcer *policies.Enforcer) (*Runner, *recordingRecorder, string) {
	t.Helper()
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())
	rec := &recordingRecorder{}
	runner := NewRunnerWithOptions(agentService, registry,
		WithStepRecorder(rec),
		WithPolicyEnforcer(enforcer),
	)
	return runner, rec, agent.ID
}

func newPolicyTestRunnerProvider(t *testing.T, provider models.Provider, enforcer *policies.Enforcer) (*Runner, *recordingRecorder, string) {
	t.Helper()
	runner, rec, agentID := newPolicyTestRunner(t, enforcer)
	runner.provider = provider
	return runner, rec, agentID
}

func TestRunOfflineToolPolicyDeniedCalculatorNotInvoked(t *testing.T) {
	deny := &stubEnforcer{decision: policies.Decision{
		Decision:        policies.EffectDeny,
		MatchedPolicyID: "pol-1",
		Reason:          `deny by policy "block calculator" (priority 10)`,
	}}
	runner, rec, agentID := newPolicyTestRunner(t, deny.enforcer())

	run, err := runner.RunWithID(context.Background(), "run-pol", agentID, "What is 21+21?")
	if err != nil {
		t.Fatalf("RunWithID returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		// Skip semantics: the run completes through the offline fallback,
		// it does not fail because a governed tool was withheld.
		t.Fatalf("status = %q, want COMPLETED (denied tool is skipped)", run.Status)
	}
	if !strings.Contains(run.Output, "Completed Math Agent") {
		t.Fatalf("output = %q, want the offline-fallback completion", run.Output)
	}
	// The calculator never executed: no computed result anywhere.
	if strings.Contains(run.Output, "42") {
		t.Fatalf("calculator result leaked into output: %q", run.Output)
	}

	var toolStep *Step
	for i, step := range rec.all() {
		if step.Type == StepTypeTool {
			s := rec.all()[i]
			toolStep = &s
		}
	}
	if toolStep == nil {
		t.Fatalf("no tool step recorded for the denied calculator")
	}
	if toolStep.Status != StepFailed {
		t.Fatalf("tool step status = %q, want failed", toolStep.Status)
	}
	if toolStep.Error == "" || !strings.HasPrefix(toolStep.Error, ToolPolicyDeniedCode) {
		t.Fatalf("tool step error = %q, want %q prefix", toolStep.Error, ToolPolicyDeniedCode)
	}
	if toolStep.Policy == nil || toolStep.Policy.PolicyID != "pol-1" {
		t.Fatalf("tool step policy = %+v, want matched policy pol-1", toolStep.Policy)
	}
	if toolStep.Output == "" || !strings.Contains(toolStep.Output, "tool denied by policy") {
		t.Fatalf("tool step output = %q, want the denial observation", toolStep.Output)
	}
	if got := deny.calls(); len(got) != 1 || got[0] != policies.ActionToolCall+"/calculator" {
		t.Fatalf("evaluated = %v, want one %s/calculator call", got, policies.ActionToolCall)
	}
}

func TestRunOfflineToolPolicyAllowedCalculatorRuns(t *testing.T) {
	allow := &stubEnforcer{decision: policies.Decision{Decision: policies.EffectAllow, Reason: "no matching policy; default allow"}}
	runner, rec, agentID := newPolicyTestRunner(t, allow.enforcer())

	run, err := runner.RunWithID(context.Background(), "run-pol", agentID, "What is 21+21?")
	if err != nil {
		t.Fatalf("RunWithID returned error: %v", err)
	}
	if run.Output != "42" {
		t.Fatalf("output = %q, want the calculator result", run.Output)
	}
	var toolStep *Step
	for i, step := range rec.all() {
		if step.Type == StepTypeTool {
			s := rec.all()[i]
			toolStep = &s
		}
	}
	if toolStep == nil || toolStep.Status != StepSucceeded || toolStep.Policy != nil {
		t.Fatalf("allowed tool step = %+v, want success without policy block", toolStep)
	}
}

func TestRunOfflineNoEnforcerUnchanged(t *testing.T) {
	runner, rec, agentID := newPolicyTestRunner(t, nil)
	run, err := runner.RunWithID(context.Background(), "run-pol", agentID, "What is 21+21?")
	if err != nil || run.Output != "42" {
		t.Fatalf("nil enforcer changed legacy behavior: %v %q", err, run.Output)
	}
	// Disabled flag over a wired enforcer is equally inert.
	disabled := policies.NewEnforcerWithFlag(&errEvaluator{}, false)
	runner2, _, agentID2 := newPolicyTestRunner(t, disabled)
	run2, err := runner2.RunWithID(context.Background(), "run-pol-4", agentID2, "What is 2+2?")
	if err != nil || run2.Output != "4" {
		t.Fatalf("disabled enforcer changed legacy behavior: %v %q", err, run2.Output)
	}
	if n := len(rec.all()); n == 0 {
		t.Fatalf("expected recorded steps from the first run")
	}
}

// errEvaluator always fails: the seam must fail closed (deny, never invoke).
type errEvaluator struct{}

func (errEvaluator) EvaluateCtx(context.Context, string, policies.EvaluateRequest) (policies.Decision, error) {
	return policies.Decision{}, errors.New("policy store unavailable")
}

func TestRunProviderToolPolicyUnavailableFailsClosed(t *testing.T) {
	runner, rec, agentID := newPolicyTestRunnerProvider(t, &scriptedProvider{
		name: "scripted",
		resps: []*models.CompletionResponse{
			{Text: `{"tool":"calculator","arguments":{"expression":"1+1"}}`, Model: "scripted"},
		},
	}, (&stubEnforcer{err: errors.New("policy store unavailable")}).enforcer())

	run, err := runner.RunWithID(context.Background(), "run-pol", agentID, "compute")
	if err != nil {
		t.Fatalf("RunWithID returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("status = %q, want COMPLETED (fail-closed denial feeds the model)", run.Status)
	}
	var toolStep *Step
	for i, step := range rec.all() {
		if step.Type == StepTypeTool {
			s := rec.all()[i]
			toolStep = &s
		}
	}
	if toolStep == nil {
		t.Fatalf("no tool step recorded for the unavailable-policy block")
	}
	if !strings.HasPrefix(toolStep.Error, ToolPolicyUnavailableCode) {
		t.Fatalf("tool step error = %q, want %q prefix", toolStep.Error, ToolPolicyUnavailableCode)
	}
}

func TestRunProviderToolPolicyDeniedObservationFedToModel(t *testing.T) {
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: `{"tool":"calculator","arguments":{"expression":"2+2"}}`, Model: "scripted"},
		{Text: "final answer without the tool", Model: "scripted"},
	}}
	runner, rec, agentID := newPolicyTestRunnerProvider(t, provider, (&stubEnforcer{decision: policies.Decision{
		Decision:        policies.EffectDeny,
		MatchedPolicyID: "pol-9",
		Reason:          "tool not on allowlist",
	}}).enforcer())

	run, err := runner.RunWithID(context.Background(), "run-pol", agentID, "do math")
	if err != nil {
		t.Fatalf("RunWithID returned error: %v", err)
	}
	if run.Output != "final answer without the tool" {
		t.Fatalf("output = %q, want the model's post-denial answer", run.Output)
	}
	// The second model call received the policy denial as the tool message.
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) < 2 {
		t.Fatalf("expected a second model call after the denial, got %d", len(provider.requests))
	}
	second := provider.requests[1]
	var toolMsg string
	for _, m := range second.Messages {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "tool denied by policy") || !strings.Contains(toolMsg, "tool not on allowlist") {
		t.Fatalf("tool observation = %q, want the denial reason", toolMsg)
	}
	var toolStep *Step
	for i, step := range rec.all() {
		if step.Type == StepTypeTool {
			s := rec.all()[i]
			toolStep = &s
		}
	}
	if toolStep == nil || toolStep.Policy == nil || toolStep.Policy.PolicyID != "pol-9" {
		t.Fatalf("denied tool step policy record missing: %+v", toolStep)
	}
}

func TestRunProviderToolPolicyRequireApprovalFailsClosed(t *testing.T) {
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: `{"tool":"http_request","arguments":{"url":"https://example.invalid"}}`, Model: "scripted"},
	}}
	runner, rec, agentID := newPolicyTestRunnerProvider(t, provider, (&stubEnforcer{decision: policies.Decision{
		Decision:        policies.EffectAllow,
		MatchedPolicyID: "pol-appr",
		Reason:          "allow by policy \"gate\" (priority 3); approval required before execution",
		RequireApproval: true,
	}}).enforcer())

	registryHasHTTP := runner.toolRegistry != nil
	if registryHasHTTP {
		runner.toolRegistry.Register(&staticTool{name: "http_request", result: map[string]any{"ok": true}})
	}
	run, err := runner.RunWithID(context.Background(), "run-pol", agentID, "call out")
	if err != nil {
		t.Fatalf("RunWithID returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("status = %q, want COMPLETED", run.Status)
	}
	for _, step := range rec.all() {
		if step.Type == StepTypeTool && step.Status == StepSucceeded {
			t.Fatalf("require_approval tool must not execute, step = %+v", step)
		}
	}
	var toolStep *Step
	for i, step := range rec.all() {
		if step.Type == StepTypeTool {
			s := rec.all()[i]
			toolStep = &s
		}
	}
	if toolStep == nil || !strings.HasPrefix(toolStep.Error, ToolPolicyDeniedCode) {
		t.Fatalf("approval-gated tool step = %+v, want a policy_denied record", toolStep)
	}
}

// staticTool is a minimal Tool for registry seeding in policy tests.
type staticTool struct {
	name   string
	result map[string]any
}

func (t *staticTool) Name() string { return t.name }
func (t *staticTool) Execute(map[string]any) (map[string]any, error) {
	return t.result, nil
}
