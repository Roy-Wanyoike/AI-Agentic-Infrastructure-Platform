package main

// Tests for the create-run governance enforcement gate (issue #75). The
// suite proves enforcement in the DEFAULT IN-MEMORY mode (no Postgres): the
// seam is wired exactly the way newApp wires it, over an in-memory policies
// service, and every case asserts the response envelope, the run/queue side
// effects and the audit trail. The decision matrix:
//
//   - deny policy        -> 403 policy_denied + audit, no run row, no task
//   - max_cost_cents cap -> over-budget run denied, within-budget allowed
//   - require_approval   -> 202 WAITING_APPROVAL, approval filed, NO task;
//     operator approval resumes the run and re-enqueues it
//   - tenant guards      -> policies never leak across organizations
//   - no policies        -> default allow, run accepted with a policy step
//   - failing source     -> 503 policy_unavailable (fail closed)
//   - flag off / unwired -> legacy always-accept behavior

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/billing"
	"agentos/internal/policies"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// policyGateEnv is the per-case fixture: everything the gate touches is
// saved/restored so cases stay isolated.
type policyGateEnv struct {
	authSvc  *auth.Service
	token    string
	orgID    string
	policies *policies.Service
	runsSvc  *runs.Service
	queue    *queue.Queue
	auditSvc *audit.Service
	apSvc    *approvals.Service
}

// newPolicyGateEnv wires the enforcement seam over in-memory services (the
// default zero-infrastructure mode) and installs the package globals the
// create-run handler reads. Every global is restored on cleanup.
func newPolicyGateEnv(t *testing.T, enabled bool) *policyGateEnv {
	t.Helper()
	env := &policyGateEnv{authSvc: auth.NewService("dev-secret")}
	_, user, err := env.authSvc.Register("Acme", "policy@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	env.token, err = env.authSvc.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	env.orgID = user.Organization

	env.policies = policies.NewService()
	env.runsSvc = runs.NewService()
	env.queue = queue.NewQueue()
	env.auditSvc = audit.NewService()
	env.apSvc = approvals.NewService()
	env.apSvc.SetRunController(newApprovalRunController(env.runsSvc, env.queue))

	prevRuns, prevBilling, prevSeam := runsServiceVar, billingServiceVar, policyEnforcementVar
	runsServiceVar = env.runsSvc
	billingServiceVar = billing.NewService() // no subscription -> quota gate allows
	policyEnforcementVar = &runPolicyEnforcement{
		Policies:  env.policies,
		Enforcer:  policies.NewEnforcerWithFlag(env.policies, enabled),
		Approvals: env.apSvc,
	}
	t.Cleanup(func() {
		runsServiceVar, billingServiceVar, policyEnforcementVar = prevRuns, prevBilling, prevSeam
	})
	return env
}

// createPolicy files a policy for the fixture org through the real service
// (the same records POST /policies/create would persist).
func (env *policyGateEnv) createPolicy(t *testing.T, p *policies.Policy) *policies.Policy {
	t.Helper()
	policy, err := env.policies.CreatePolicyCtx(context.Background(), env.orgID, p)
	if err != nil {
		t.Fatalf("CreatePolicyCtx returned error: %v", err)
	}
	return policy
}

// postRun sends an authenticated create-run request with the given JSON body.
func (env *policyGateEnv) postRun(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.token)
	rr := httptest.NewRecorder()
	// The org id is omitted from the body so the claims' organization is used
	// (the same tenant resolution the middleware-protected route performs).
	auth.RequireAuth(env.authSvc)(createRunHandler(env.queue, env.auditSvc)).ServeHTTP(rr, req)
	return rr
}

// runCount returns the number of run rows the org currently has.
func (env *policyGateEnv) runCount(t *testing.T, orgID string) int {
	t.Helper()
	runsList, err := env.runsSvc.ListRunsCtx(context.Background(), orgID)
	if err != nil {
		t.Fatalf("ListRunsCtx returned error: %v", err)
	}
	return len(runsList)
}

func (env *policyGateEnv) auditCount(t *testing.T, action string) int {
	t.Helper()
	n := 0
	for _, entry := range env.auditSvc.List() {
		if entry.Action == action {
			n++
		}
	}
	return n
}

// decodeErrorEnvelope reads the structured {"error":{...}} body.
func decodeErrorEnvelope(t *testing.T, rr *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not an error envelope: %v (%q)", err, rr.Body.String())
	}
	return body.Error.Code, body.Error.Message
}

func TestCreateRunPolicyDeny(t *testing.T) {
	env := newPolicyGateEnv(t, true)
	env.createPolicy(t, &policies.Policy{
		Name:         "block production runs",
		Effect:       policies.EffectDeny,
		ResourceType: policies.ResourceAgent,
		Actions:      []string{policies.ActionRunExecute},
		Conditions:   policies.Conditions{Environments: []string{"production"}},
		Priority:     10,
		Enabled:      true,
	})

	// Production (explicit): denied with the decision reason.
	rr := env.postRun(t, `{"agent_id":"agent-1","input":"hello","environment":"production"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%q)", rr.Code, rr.Body.String())
	}
	code, message := decodeErrorEnvelope(t, rr)
	if code != "policy_denied" {
		t.Fatalf("error code = %q, want policy_denied", code)
	}
	if !strings.Contains(message, `deny by policy "block production runs"`) {
		t.Fatalf("message = %q, want the decision reason", message)
	}
	if got := env.runCount(t, env.orgID); got != 0 {
		t.Fatalf("denied run left %d run rows behind", got)
	}
	if env.queue.Length() != 0 {
		t.Fatalf("denied run enqueued %d tasks", env.queue.Length())
	}
	if n := env.auditCount(t, "run.policy_denied"); n != 1 {
		t.Fatalf("run.policy_denied audit entries = %d, want 1", n)
	}

	// Staging is not listed by the policy: default allow, run accepted.
	rr = env.postRun(t, `{"agent_id":"agent-1","input":"hello","environment":"staging"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("staging status = %d, want 201 (%q)", rr.Code, rr.Body.String())
	}
	if env.queue.Length() != 1 {
		t.Fatalf("staging run queue length = %d, want 1", env.queue.Length())
	}
}

func TestCreateRunMaxCostGateBlocksOverBudgetRun(t *testing.T) {
	env := newPolicyGateEnv(t, true)
	limit := int64(100)
	env.createPolicy(t, &policies.Policy{
		Name:         "over-budget guard",
		Effect:       policies.EffectDeny,
		ResourceType: policies.ResourceAgent,
		Actions:      []string{policies.ActionRunExecute},
		Conditions:   policies.Conditions{MaxCostCents: &limit},
		Priority:     5,
		Enabled:      true,
	})

	// Over budget (estimated 500 > cap 100): denied.
	rr := env.postRun(t, `{"agent_id":"agent-1","input":"expensive","estimated_cost_cents":500}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%q)", rr.Code, rr.Body.String())
	}
	code, message := decodeErrorEnvelope(t, rr)
	if code != "policy_denied" {
		t.Fatalf("error code = %q, want policy_denied", code)
	}
	if !strings.Contains(message, "exceeds cap 100") {
		t.Fatalf("message = %q, want the budget reason", message)
	}
	if got := env.runCount(t, env.orgID); got != 0 {
		t.Fatalf("over-budget run left %d run rows behind", got)
	}

	// Within budget (estimated 50 <= cap 100): the condition does not match,
	// default allow applies.
	rr = env.postRun(t, `{"agent_id":"agent-1","input":"cheap","estimated_cost_cents":50}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%q)", rr.Code, rr.Body.String())
	}
	if env.queue.Length() != 1 {
		t.Fatalf("within-budget run queue length = %d, want 1", env.queue.Length())
	}
}

func TestCreateRunRequireApprovalPauses(t *testing.T) {
	env := newPolicyGateEnv(t, true)
	env.createPolicy(t, &policies.Policy{
		Name:         "approve production runs",
		Effect:       policies.EffectAllow,
		ResourceType: policies.ResourceAgent,
		Actions:      []string{policies.ActionRunExecute},
		Conditions:   policies.Conditions{Environments: []string{"production"}, RequireApproval: true},
		Priority:     3,
		Enabled:      true,
	})

	rr := env.postRun(t, `{"agent_id":"agent-1","input":"hello","environment":"production"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%q)", rr.Code, rr.Body.String())
	}
	var body struct {
		RunID      string `json:"run_id"`
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
		Policy     struct {
			PolicyID        string `json:"policy_id"`
			Decision        string `json:"decision"`
			Reason          string `json:"reason"`
			RequireApproval bool   `json:"require_approval"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 202 body: %v (%q)", err, rr.Body.String())
	}
	if body.Status != string(runs.StatusWaitingApproval) {
		t.Fatalf("status = %q, want waiting_approval", body.Status)
	}
	if body.ApprovalID == "" {
		t.Fatalf("approval_id missing from the 202 body")
	}
	if !body.Policy.RequireApproval || body.Policy.Decision != policies.EffectAllow {
		t.Fatalf("policy block = %+v, want an allow-with-approval verdict", body.Policy)
	}

	// The run is parked in WAITING_APPROVAL with the verdict on its timeline,
	// and execution was NOT enqueued.
	run, err := env.runsSvc.GetRunCtx(context.Background(), env.orgID, body.RunID)
	if err != nil {
		t.Fatalf("GetRunCtx returned error: %v", err)
	}
	if run.Status != runs.StatusWaitingApproval {
		t.Fatalf("run status = %q, want waiting_approval", run.Status)
	}
	steps, err := env.runsSvc.Steps(context.Background(), env.orgID, body.RunID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("policy steps = %v err=%v, want exactly one", steps, err)
	}
	if steps[0].StepType != "policy" || steps[0].OutputMeta["policy_id"] != body.Policy.PolicyID {
		t.Fatalf("policy step = %+v, want the recorded verdict", steps[0])
	}
	if env.queue.Length() != 0 {
		t.Fatalf("approval-gated run enqueued %d tasks, want 0", env.queue.Length())
	}
	if n := env.auditCount(t, "run.approval_required"); n != 1 {
		t.Fatalf("run.approval_required audit entries = %d, want 1", n)
	}

	// Operator approval resumes the run (waiting_approval -> pending) and
	// re-enqueues execution through the approvalRunController.
	if _, err := env.apSvc.Decide(context.Background(), env.orgID, body.ApprovalID, approvals.StatusApproved, "ship it", "admin-1"); err != nil {
		t.Fatalf("Approvals.Decide returned error: %v", err)
	}
	resumed, err := env.runsSvc.GetRunCtx(context.Background(), env.orgID, body.RunID)
	if err != nil {
		t.Fatalf("GetRunCtx after approval returned error: %v", err)
	}
	if resumed.Status != runs.StatusPending {
		t.Fatalf("run status after approval = %q, want pending", resumed.Status)
	}
	if env.queue.Length() != 1 {
		t.Fatalf("queue length after approval = %d, want 1 (execution re-enqueued)", env.queue.Length())
	}
	if task := env.queue.Peek(); task == nil || task.Payload["run_id"] != body.RunID {
		t.Fatalf("re-enqueued task = %+v, want the resumed run", task)
	}
}

func TestCreateRunRequireApprovalRejectionKeepsRunParked(t *testing.T) {
	env := newPolicyGateEnv(t, true)
	env.createPolicy(t, &policies.Policy{
		Name:         "approve staging runs",
		Effect:       policies.EffectAllow,
		ResourceType: policies.ResourceAgent,
		Actions:      []string{policies.ActionRunExecute},
		Conditions:   policies.Conditions{Environments: []string{"staging"}, RequireApproval: true},
		Priority:     3,
		Enabled:      true,
	})

	rr := env.postRun(t, `{"agent_id":"agent-1","input":"hello","environment":"staging"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%q)", rr.Code, rr.Body.String())
	}
	var body struct {
		RunID      string `json:"run_id"`
		ApprovalID string `json:"approval_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}
	if _, err := env.apSvc.Decide(context.Background(), env.orgID, body.ApprovalID, approvals.StatusRejected, "not safe", "admin-1"); err != nil {
		t.Fatalf("Approvals.Decide returned error: %v", err)
	}
	// A rejection never resumes the run: it stays parked and nothing executes.
	run, err := env.runsSvc.GetRunCtx(context.Background(), env.orgID, body.RunID)
	if err != nil {
		t.Fatalf("GetRunCtx returned error: %v", err)
	}
	if run.Status != runs.StatusWaitingApproval {
		t.Fatalf("run status after rejection = %q, want waiting_approval", run.Status)
	}
	if env.queue.Length() != 0 {
		t.Fatalf("queue length after rejection = %d, want 0", env.queue.Length())
	}
}

func TestCreateRunPolicyTenantGuards(t *testing.T) {
	env := newPolicyGateEnv(t, true)
	limit := int64(0) // deny everything over zero estimated cents
	env.createPolicy(t, &policies.Policy{
		Name:         "acme lockdown",
		Effect:       policies.EffectDeny,
		ResourceType: policies.ResourceAgent,
		Actions:      []string{policies.ActionRunExecute},
		Conditions:   policies.Conditions{MaxCostCents: &limit},
		Priority:     10,
		Enabled:      true,
	})

	// A second organization with NO policies is unaffected by Acme's rule.
	_, other, err := env.authSvc.Register("Beta", "beta@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register (second org) returned error: %v", err)
	}
	otherToken, err := env.authSvc.GenerateToken(other)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}

	// Acme: denied.
	if rr := env.postRun(t, `{"agent_id":"agent-1","input":"hello","estimated_cost_cents":1}`); rr.Code != http.StatusForbidden {
		t.Fatalf("acme status = %d, want 403 (%q)", rr.Code, rr.Body.String())
	}
	// Beta: the tenant-scoped policy engine never sees Acme's rule.
	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"agent_id":"agent-1","input":"hello","estimated_cost_cents":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+otherToken)
	rr := httptest.NewRecorder()
	auth.RequireAuth(env.authSvc)(createRunHandler(env.queue, env.auditSvc)).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("beta status = %d, want 201 (%q)", rr.Code, rr.Body.String())
	}
	// Beta's run rows are invisible to Acme's tenant scope and vice versa.
	acmeRuns := env.runCount(t, env.orgID)
	if acmeRuns != 0 {
		t.Fatalf("acme run rows = %d, want 0", acmeRuns)
	}
	if n := env.auditCount(t, "run.policy_denied"); n != 1 {
		t.Fatalf("run.policy_denied audit entries = %d, want 1 (acme only)", n)
	}
}

func TestCreateRunNoPolicyOrgUnaffected(t *testing.T) {
	env := newPolicyGateEnv(t, true)

	rr := env.postRun(t, `{"agent_id":"agent-1","input":"hello"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%q)", rr.Code, rr.Body.String())
	}
	var body struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 201 body: %v", err)
	}
	if env.queue.Length() != 1 {
		t.Fatalf("queue length = %d, want 1", env.queue.Length())
	}
	// The default-allow verdict is still recorded on the run timeline so the
	// dashboard can show WHY the run was accepted.
	steps, err := env.runsSvc.Steps(context.Background(), env.orgID, body.RunID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("policy steps = %v err=%v, want exactly one", steps, err)
	}
	if steps[0].StepType != "policy" {
		t.Fatalf("step type = %q, want policy", steps[0].StepType)
	}
	if steps[0].OutputMeta["decision"] != policies.EffectAllow {
		t.Fatalf("step decision = %v, want allow", steps[0].OutputMeta["decision"])
	}
	if steps[0].OutputMeta["reason"] != "no matching policy; default allow" {
		t.Fatalf("step reason = %v, want the default-allow reason", steps[0].OutputMeta["reason"])
	}
	if got, ok := steps[0].OutputMeta["environment"].(string); ok && got != policies.DefaultEnvironment {
		t.Fatalf("step environment = %q, want default %q", got, policies.DefaultEnvironment)
	}
}

func TestCreateRunPolicyUnavailableFailsClosed(t *testing.T) {
	env := newPolicyGateEnv(t, true)
	// A failing decision source must surface as 503, never bypass the gate.
	policyEnforcementVar.Enforcer = policies.NewEnforcerWithEvaluator(failingEvaluator{})

	rr := env.postRun(t, `{"agent_id":"agent-1","input":"hello"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%q)", rr.Code, rr.Body.String())
	}
	code, _ := decodeErrorEnvelope(t, rr)
	if code != "policy_unavailable" {
		t.Fatalf("error code = %q, want policy_unavailable", code)
	}
	if got := env.runCount(t, env.orgID); got != 0 {
		t.Fatalf("fail-closed run left %d run rows behind", got)
	}
	if n := env.auditCount(t, "run.policy_unavailable"); n != 1 {
		t.Fatalf("run.policy_unavailable audit entries = %d, want 1", n)
	}
}

// failingEvaluator always errors: the fail-closed contract of the gate.
type failingEvaluator struct{}

func (failingEvaluator) EvaluateCtx(context.Context, string, policies.EvaluateRequest) (policies.Decision, error) {
	return policies.Decision{}, errors.New("policy store unavailable")
}

func TestCreateRunPolicyEnforcementDisabledKeepsLegacyBehavior(t *testing.T) {
	env := newPolicyGateEnv(t, false)
	limit := int64(0)
	env.createPolicy(t, &policies.Policy{
		Name:         "would-be lockdown",
		Effect:       policies.EffectDeny,
		ResourceType: policies.ResourceAgent,
		Actions:      []string{policies.ActionRunExecute},
		Conditions:   policies.Conditions{MaxCostCents: &limit},
		Priority:     10,
		Enabled:      true,
	})

	// Flag off: the deny policy is inert, the run is accepted, and no policy
	// trace is written (legacy behavior byte-identical).
	rr := env.postRun(t, `{"agent_id":"agent-1","input":"hello","estimated_cost_cents":5}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%q)", rr.Code, rr.Body.String())
	}
	var body struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 201 body: %v", err)
	}
	if env.queue.Length() != 1 {
		t.Fatalf("queue length = %d, want 1", env.queue.Length())
	}
	steps, err := env.runsSvc.Steps(context.Background(), env.orgID, body.RunID)
	if err != nil || len(steps) != 0 {
		t.Fatalf("policy steps = %v err=%v, want none (disabled seam writes nothing)", steps, err)
	}
	if n := env.auditCount(t, "run.policy_denied"); n != 0 {
		t.Fatalf("run.policy_denied audit entries = %d, want 0", n)
	}
}
