package policies

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeEvaluator is a decision source returning a canned decision/error so
// Enforcer tests never depend on the store-backed service.
type fakeEvaluator struct {
	decision Decision
	err      error
	lastOrg  string
	lastReq  EvaluateRequest
}

func (f *fakeEvaluator) EvaluateCtx(ctx context.Context, orgID string, req EvaluateRequest) (Decision, error) {
	f.lastOrg = orgID
	f.lastReq = req
	if f.err != nil {
		return Decision{}, f.err
	}
	return f.decision, nil
}

func TestEnforcementFromEnvDefaultsOn(t *testing.T) {
	t.Setenv(EnforcementEnvVar, "")
	if !EnforcementFromEnv() {
		t.Fatalf("unset flag must default to enforcement ON")
	}
	for _, on := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(EnforcementEnvVar, on)
		if !EnforcementFromEnv() {
			t.Fatalf("flag %q must enable enforcement", on)
		}
	}
	// Garbage never silently disables the governance gate.
	for _, garbage := range []string{"maybe", "off ", "2"} {
		t.Setenv(EnforcementEnvVar, garbage)
		if !EnforcementFromEnv() {
			t.Fatalf("garbage flag %q must be treated as ON", garbage)
		}
	}
	for _, off := range []string{"0", "false", "FALSE"} {
		t.Setenv(EnforcementEnvVar, off)
		if EnforcementFromEnv() {
			t.Fatalf("flag %q must disable enforcement", off)
		}
	}
}

func TestEnforcerNilAndDisabledAreDefaultAllow(t *testing.T) {
	var nilEnforcer *Enforcer
	if nilEnforcer.Enabled() {
		t.Fatalf("nil enforcer must be disabled")
	}
	d, err := nilEnforcer.AuthorizeRunCreation(context.Background(), "org-1", "agent-1", "production", 0)
	if err != nil || !d.Allowed() {
		t.Fatalf("nil enforcer run creation = %v, %v; want default allow", d, err)
	}
	d, err = nilEnforcer.AuthorizeToolCall(context.Background(), "org-1", "calculator", "")
	if err != nil || !d.Allowed() {
		t.Fatalf("nil enforcer tool call = %v, %v; want default allow", d, err)
	}

	// Flag off over a live evaluator: allowed without consulting the source.
	fake := &fakeEvaluator{decision: Decision{Decision: EffectDeny, Reason: "blocked"}}
	off := NewEnforcerWithFlag(fake, false)
	if off.Enabled() {
		t.Fatalf("flag-off enforcer must report disabled")
	}
	d, err = off.AuthorizeRunCreation(context.Background(), "org-1", "agent-1", "", 0)
	if err != nil || !d.Allowed() {
		t.Fatalf("flag-off run creation = %v, %v; want default allow", d, err)
	}
	if fake.lastReq.Action != "" {
		t.Fatalf("flag-off enforcer must not consult the evaluator")
	}

	// Enabled flag but nil decision source: disabled, default allow.
	noSource := NewEnforcerWithFlag(nil, true)
	if noSource.Enabled() {
		t.Fatalf("nil-source enforcer must be disabled")
	}
}

func TestEnforcerAuthorizeRunCreationWiresRequest(t *testing.T) {
	fake := &fakeEvaluator{decision: Decision{Decision: EffectDeny, MatchedPolicyID: "p1", Reason: "over budget"}}
	env := NewEnforcerWithFlag(fake, true)

	d, err := env.AuthorizeRunCreation(context.Background(), "org-1", "agent-7", "", 500)
	if err != nil {
		t.Fatalf("AuthorizeRunCreation returned error: %v", err)
	}
	if d.Allowed() {
		t.Fatalf("expected deny decision to propagate")
	}
	if fake.lastOrg != "org-1" {
		t.Fatalf("org id not forwarded: %q", fake.lastOrg)
	}
	req := fake.lastReq
	if req.Action != ActionRunExecute {
		t.Fatalf("action = %q, want %q", req.Action, ActionRunExecute)
	}
	if req.Resource.Type != ResourceAgent || req.Resource.ID != "agent-7" || req.Resource.TenantID != "org-1" {
		t.Fatalf("resource = %+v, want agent/agent-7/org-1", req.Resource)
	}
	// Blank environment normalizes to the platform default.
	if req.Context.Environment != DefaultEnvironment {
		t.Fatalf("environment = %q, want default %q", req.Context.Environment, DefaultEnvironment)
	}
	if req.Context.EstimatedCostCents != 500 {
		t.Fatalf("estimated cost = %d, want 500", req.Context.EstimatedCostCents)
	}
}

func TestEnforcerAuthorizeToolCallWiresRequest(t *testing.T) {
	fake := &fakeEvaluator{decision: Decision{Decision: EffectAllow}}
	env := NewEnforcerWithFlag(fake, true)

	d, err := env.AuthorizeToolCall(context.Background(), "org-2", "http_request", "staging")
	if err != nil || !d.Allowed() {
		t.Fatalf("AuthorizeToolCall = %v, %v; want allow", d, err)
	}
	req := fake.lastReq
	if req.Action != ActionToolCall {
		t.Fatalf("action = %q, want %q", req.Action, ActionToolCall)
	}
	if req.Resource.Type != ResourceTool || req.Resource.ID != "http_request" {
		t.Fatalf("resource = %+v, want tool/http_request", req.Resource)
	}
	if req.Context.Tool != "http_request" {
		t.Fatalf("context.tool = %q, want http_request (tool_allowlist matching)", req.Context.Tool)
	}
	if req.Context.Environment != "staging" {
		t.Fatalf("environment = %q, want staging", req.Context.Environment)
	}
}

func TestEnforcerPropagatesEvaluatorError(t *testing.T) {
	fake := &fakeEvaluator{err: errors.New("store down")}
	env := NewEnforcerWithFlag(fake, true)
	if _, err := env.AuthorizeRunCreation(context.Background(), "org-1", "agent-1", "", 0); err == nil {
		t.Fatalf("evaluation failure must surface as error (caller fails closed)")
	}
	if _, err := env.AuthorizeToolCall(context.Background(), "org-1", "calculator", ""); err == nil {
		t.Fatalf("evaluation failure must surface as error (caller fails closed)")
	}
}

func TestEnforcerOverServiceDenyAndRequireApproval(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, err := svc.CreatePolicyCtx(ctx, "org-1", &Policy{
		Name:         "block http tool",
		Effect:       EffectDeny,
		ResourceType: ResourceTool,
		Actions:      []string{ActionToolCall},
		Conditions:   Conditions{ToolAllowlist: []string{"http_request"}, Environments: []string{"production"}},
		Priority:     10,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreatePolicyCtx returned error: %v", err)
	}
	// Production: denied. Staging: default allow.
	d, err := NewEnforcerWithFlag(svc, true).AuthorizeToolCall(ctx, "org-1", "http_request", "production")
	if err != nil || d.Allowed() {
		t.Fatalf("production http_request = %v, %v; want deny", d, err)
	}
	d, err = NewEnforcerWithFlag(svc, true).AuthorizeToolCall(ctx, "org-1", "http_request", "staging")
	if err != nil || !d.Allowed() {
		t.Fatalf("staging http_request = %v, %v; want default allow", d, err)
	}

	// require_approval winning policy: allow + RequireApproval annotation.
	if _, err := svc.CreatePolicyCtx(ctx, "org-2", &Policy{
		Name:         "approve every run",
		Effect:       EffectAllow,
		ResourceType: ResourceAgent,
		Actions:      []string{ActionRunExecute},
		Conditions:   Conditions{RequireApproval: true},
		Priority:     5,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreatePolicyCtx returned error: %v", err)
	}
	d, err = NewEnforcerWithFlag(svc, true).AuthorizeRunCreation(ctx, "org-2", "agent-1", "", 0)
	if err != nil {
		t.Fatalf("AuthorizeRunCreation returned error: %v", err)
	}
	if !d.Allowed() || !d.RequireApproval {
		t.Fatalf("decision = %+v; want allow with require_approval", d)
	}
}

func TestRunScopeContext(t *testing.T) {
	ctx := context.Background()
	scope := RunScopeFromContext(ctx)
	if scope != (RunScope{Environment: DefaultEnvironment}) {
		t.Fatalf("absent scope = %+v, want zero org + default environment", scope)
	}
	ctx = WithRunScope(ctx, RunScope{OrganizationID: "org-9", Environment: "staging"})
	scope = RunScopeFromContext(ctx)
	if scope.OrganizationID != "org-9" || scope.Environment != "staging" {
		t.Fatalf("stamped scope = %+v", scope)
	}
	// Blank environment normalizes so env-scoped policies match the default.
	ctx = WithRunScope(ctx, RunScope{OrganizationID: "org-9"})
	if got := RunScopeFromContext(ctx).Environment; got != DefaultEnvironment {
		t.Fatalf("blank environment = %q, want %q", got, DefaultEnvironment)
	}
	// Foreign context values are ignored.
	type otherKey struct{}
	ctx = context.WithValue(context.Background(), otherKey{}, RunScope{OrganizationID: "evil"})
	if got := RunScopeFromContext(ctx).OrganizationID; got != "" {
		t.Fatalf("foreign context value leaked: %q", got)
	}
}

func TestHTTPEvaluatorRoundTrip(t *testing.T) {
	var gotPath, gotKey string
	var gotBody EvaluateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Decision{Decision: EffectDeny, MatchedPolicyID: "p9", Reason: "denied remotely"})
	}))
	defer srv.Close()

	ev := NewHTTPEvaluator(srv.URL, "key-1")
	d, err := ev.EvaluateCtx(context.Background(), "org-1", EvaluateRequest{Action: ActionToolCall})
	if err != nil {
		t.Fatalf("EvaluateCtx returned error: %v", err)
	}
	if d.Allowed() || d.MatchedPolicyID != "p9" {
		t.Fatalf("decision = %+v, want remote deny", d)
	}
	if gotPath != "/v1/policies/evaluate" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotKey != "key-1" {
		t.Fatalf("api key header = %q", gotKey)
	}
	if gotBody.Action != ActionToolCall {
		t.Fatalf("forwarded action = %q", gotBody.Action)
	}
}

func TestHTTPEvaluatorErrorPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ev := NewHTTPEvaluator(srv.URL, "")
	if _, err := ev.EvaluateCtx(context.Background(), "org-1", EvaluateRequest{}); err == nil {
		t.Fatalf("5xx must surface as error (fail closed)")
	}
	// Unreachable endpoint.
	down := NewHTTPEvaluator("http://127.0.0.1:1", "")
	if _, err := down.EvaluateCtx(context.Background(), "org-1", EvaluateRequest{}); err == nil {
		t.Fatalf("unreachable endpoint must surface as error")
	}
	// Nil/blank evaluator.
	var nilEv *HTTPEvaluator
	if _, err := nilEv.EvaluateCtx(context.Background(), "org-1", EvaluateRequest{}); err == nil {
		t.Fatalf("nil evaluator must error, not fail open")
	}
	if _, err := (&HTTPEvaluator{}).EvaluateCtx(context.Background(), "org-1", EvaluateRequest{}); err == nil {
		t.Fatalf("blank base url must error, not fail open")
	}
}
