package main

// Budget handler + gate tests (wave-8 issue C): auth, the RBAC matrix through
// the registered middleware chain (writes OWNER/ADMIN via users.manage, reads
// MEMBER+ via runs.execute — the billing grants), CRUD envelopes, validation,
// conflict, tenant isolation, the dry-run evaluate endpoint, and the
// create-run budget gate integration (mirroring runs_billing_test.go style,
// in-memory and sqlmock-backed Postgres modes).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/billing"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// stubBudgetConsumption feeds deterministic consumption (implements
// billing.ConsumptionSource); records the windows it was asked about.
type stubBudgetConsumption struct {
	rows []billing.ConsumptionRow
	err  error
}

func (s *stubBudgetConsumption) ConsumptionForPeriod(_ context.Context, _ string, _, _ time.Time) ([]billing.ConsumptionRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

type budgetHandlerEnv struct {
	mux         *http.ServeMux
	svc         *billing.BudgetService
	agentsSvc   *agents.Service
	orgID       string
	agentID     string
	ownerToken  string
	adminToken  string
	memberToken string
	viewerToken string
	otherToken  string
}

func newBudgetHandlerEnv(t *testing.T, rows []billing.ConsumptionRow, srcErr error) *budgetHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	ownerToken, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken(owner) returned error: %v", err)
	}
	adminToken, err := authSvc.GenerateToken(&auth.User{ID: "user-admin", Organization: owner.Organization, Email: "admin@acme.test", Role: "ADMIN"})
	if err != nil {
		t.Fatalf("GenerateToken(admin) returned error: %v", err)
	}
	memberToken, err := authSvc.GenerateToken(&auth.User{ID: "user-member", Organization: owner.Organization, Email: "member@acme.test", Role: "MEMBER"})
	if err != nil {
		t.Fatalf("GenerateToken(member) returned error: %v", err)
	}
	viewerToken, err := authSvc.GenerateToken(&auth.User{ID: "user-viewer", Organization: owner.Organization, Email: "viewer@acme.test", Role: "VIEWER"})
	if err != nil {
		t.Fatalf("GenerateToken(viewer) returned error: %v", err)
	}
	_, foreign, err := authSvc.Register("OtherCo", "owner@other.test", "secret123")
	if err != nil {
		t.Fatalf("Register(foreign) returned error: %v", err)
	}
	otherToken, err := authSvc.GenerateToken(foreign)
	if err != nil {
		t.Fatalf("GenerateToken(foreign) returned error: %v", err)
	}

	agentsSvc := agents.NewService()
	agent, err := agentsSvc.CreateAgentCtx(context.Background(), owner.Organization, "worker", "", "test instructions", "m1")
	if err != nil {
		t.Fatalf("CreateAgentCtx returned error: %v", err)
	}

	svc := newBudgetService(nil, &stubBudgetConsumption{rows: rows, err: srcErr})
	// registerBudgetRoutes assigns budgetServiceVar as a side effect; the
	// route registrations below exercise the exact production wiring.
	mux := http.NewServeMux()
	registerBudgetRoutes(mux, nil, &stubBudgetConsumption{rows: rows, err: srcErr}, agentsSvc, authSvc, apiKeysSvc)
	t.Cleanup(func() { budgetServiceVar = nil }) // tests below override it directly
	svc = budgetServiceVar                       // the instance the routes serve

	return &budgetHandlerEnv{
		mux: mux, svc: svc, agentsSvc: agentsSvc, orgID: owner.Organization, agentID: agent.ID,
		ownerToken: ownerToken, adminToken: adminToken,
		memberToken: memberToken, viewerToken: viewerToken, otherToken: otherToken,
	}
}

func (e *budgetHandlerEnv) do(t *testing.T, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	out := map[string]any{}
	if strings.Contains(rr.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
	}
	return rr, out
}

func TestBudgetEndpointsRequireAuth(t *testing.T) {
	env := newBudgetHandlerEnv(t, nil, nil)
	cases := []struct{ method, path string }{
		{http.MethodPost, "/budgets"},
		{http.MethodGet, "/budgets"},
		{http.MethodGet, "/budgets/evaluate"},
		{http.MethodGet, "/budgets/bud-1"},
		{http.MethodDelete, "/budgets/bud-1"},
	}
	for _, tc := range cases {
		rr, _ := env.do(t, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token = %d, want 401", tc.method, tc.path, rr.Code)
		}
	}
}

func TestBudgetRBACMatrix(t *testing.T) {
	env := newBudgetHandlerEnv(t, nil, nil)
	body := `{"scope":"organization","limit_runs":10}`

	// Writes: OWNER/ADMIN (users.manage); MEMBER/VIEWER refused.
	// Each accepted create is deleted before the next iteration: an active
	// org-scope budget is unique, and the loop exercises WHO may write, not
	// the conflict rule (covered by TestBudgetConflictOverHTTP).
	for _, tc := range []struct {
		token  string
		status int
	}{{env.ownerToken, http.StatusCreated}, {env.adminToken, http.StatusCreated},
		{env.memberToken, http.StatusForbidden}, {env.viewerToken, http.StatusForbidden}} {
		rr, resp := env.do(t, http.MethodPost, "/budgets", tc.token, body)
		if rr.Code != tc.status {
			t.Fatalf("POST /budgets with %q = %d, want %d", tc.token, rr.Code, tc.status)
		}
		if rr.Code == http.StatusCreated {
			budget, _ := resp["budget"].(map[string]any)
			id, _ := budget["id"].(string)
			rr2, _ := env.do(t, http.MethodDelete, "/budgets/"+id, env.ownerToken, "")
			if rr2.Code != http.StatusNoContent {
				t.Fatalf("cleanup delete of %s = %d, want 204", id, rr2.Code)
			}
		}
	}
	// DELETE carries the write tier too.
	rr, _ := env.do(t, http.MethodPost, "/budgets", env.ownerToken, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed create failed: %d", rr.Code)
	}
	rr, _ = env.do(t, http.MethodDelete, "/budgets/bud-any", env.memberToken, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("DELETE as member = %d, want 403", rr.Code)
	}

	// Reads: MEMBER+ (runs.execute); VIEWER refused.
	rr, _ = env.do(t, http.MethodGet, "/budgets", env.memberToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /budgets as member = %d, want 200", rr.Code)
	}
	rr, _ = env.do(t, http.MethodGet, "/budgets", env.viewerToken, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("GET /budgets as viewer = %d, want 403", rr.Code)
	}
	rr, _ = env.do(t, http.MethodGet, "/budgets/evaluate", env.viewerToken, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("GET evaluate as viewer = %d, want 403", rr.Code)
	}
	rr, _ = env.do(t, http.MethodGet, "/budgets/evaluate", env.memberToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET evaluate as member = %d, want 200", rr.Code)
	}
}

func TestBudgetCRUDOverHTTP(t *testing.T) {
	env := newBudgetHandlerEnv(t, nil, nil)

	rr, body := env.do(t, http.MethodPost, "/budgets", env.ownerToken,
		`{"scope":"organization","limit_runs":100,"limit_cost_cents":5000}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", rr.Code, rr.Body.String())
	}
	budget, _ := body["budget"].(map[string]any)
	if budget == nil || budget["scope"] != "organization" || budget["limit_runs"] != float64(100) ||
		budget["is_active"] != true || budget["period"] != "monthly" || budget["ref_id"] != "" {
		t.Fatalf("unexpected budget envelope: %v", body)
	}
	id, _ := budget["id"].(string)

	// List + active filter.
	rr, body = env.do(t, http.MethodGet, "/budgets?active=true", env.ownerToken, "")
	list, _ := body["budgets"].([]any)
	if rr.Code != http.StatusOK || len(list) != 1 {
		t.Fatalf("list = %d with %d rows", rr.Code, len(list))
	}
	rr, body = env.do(t, http.MethodGet, "/budgets?active=bananas", env.ownerToken, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid active filter = %d, want 400", rr.Code)
	}

	// Detail.
	rr, body = env.do(t, http.MethodGet, "/budgets/"+id, env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("detail = %d", rr.Code)
	}

	// Full PUT update (deactivate).
	rr, body = env.do(t, http.MethodPut, "/budgets/"+id, env.ownerToken,
		`{"scope":"organization","limit_runs":200,"limit_cost_cents":0,"is_active":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("update = %d body=%s", rr.Code, rr.Body.String())
	}
	if updated, _ := body["budget"].(map[string]any); updated["limit_runs"] != float64(200) || updated["is_active"] != false {
		t.Fatalf("update not applied: %v", body)
	}

	// Delete -> 204, then 404.
	rr, _ = env.do(t, http.MethodDelete, "/budgets/"+id, env.ownerToken, "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", rr.Code)
	}
	rr, _ = env.do(t, http.MethodGet, "/budgets/"+id, env.ownerToken, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("deleted detail = %d, want 404", rr.Code)
	}
}

func TestBudgetCreateValidationOverHTTP(t *testing.T) {
	env := newBudgetHandlerEnv(t, nil, nil)
	cases := []struct {
		name        string
		body        string
		wantCode    int
		wantErrCode string
	}{
		{"missing scope", `{"limit_runs":10}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"unknown scope", `{"scope":"team","limit_runs":10}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"agent scope without ref", `{"scope":"agent","limit_runs":10}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"negative limit", `{"scope":"organization","limit_runs":-1}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"both limits unlimited", `{"scope":"organization"}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"bad period", `{"scope":"organization","limit_runs":1,"period":"weekly"}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		// The referenced agent must exist INSIDE the caller's tenant: a
		// foreign/unknown agent id is a 404, never a 403.
		{"agent scope with unknown agent", `{"scope":"agent","ref_id":"nope","limit_runs":10}`, http.StatusNotFound, "AGENT_NOT_FOUND"},
	}
	for _, tc := range cases {
		rr, body := env.do(t, http.MethodPost, "/budgets", env.ownerToken, tc.body)
		if rr.Code != tc.wantCode {
			t.Fatalf("%s: status = %d body=%s", tc.name, rr.Code, rr.Body.String())
		}
		if got, _ := body["error"].(map[string]any)["code"]; got != tc.wantErrCode {
			t.Fatalf("%s: error.code = %v, want %s", tc.name, got, tc.wantErrCode)
		}
	}

	// Agent-scoped create with a REAL agent id succeeds and stores the ref.
	rr, body := env.do(t, http.MethodPost, "/budgets", env.ownerToken,
		`{"scope":"agent","ref_id":"`+env.agentID+`","limit_runs":10}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("agent-scope create = %d body=%s", rr.Code, rr.Body.String())
	}
	if b, _ := body["budget"].(map[string]any); b["ref_id"] != env.agentID {
		t.Fatalf("ref_id not stored: %v", body)
	}
}

func TestBudgetConflictOverHTTP(t *testing.T) {
	env := newBudgetHandlerEnv(t, nil, nil)
	body := `{"scope":"organization","limit_runs":10}`
	if rr, _ := env.do(t, http.MethodPost, "/budgets", env.ownerToken, body); rr.Code != http.StatusCreated {
		t.Fatalf("first create = %d", rr.Code)
	}
	rr, resp := env.do(t, http.MethodPost, "/budgets", env.ownerToken, body)
	if rr.Code != http.StatusConflict {
		t.Fatalf("second active create = %d, want 409", rr.Code)
	}
	if got, _ := resp["error"].(map[string]any)["code"]; got != "BUDGET_CONFLICT" {
		t.Fatalf("error.code = %v, want BUDGET_CONFLICT", got)
	}
}

func TestBudgetCrossTenantIsolation(t *testing.T) {
	env := newBudgetHandlerEnv(t, nil, nil)
	rr, body := env.do(t, http.MethodPost, "/budgets", env.ownerToken, `{"scope":"organization","limit_runs":10}`)
	id, _ := body["budget"].(map[string]any)["id"].(string)

	// The foreign OWNER gets 404 (cross-tenant 404, never 403) and an empty list.
	rr, _ = env.do(t, http.MethodGet, "/budgets/"+id, env.otherToken, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant detail = %d, want 404", rr.Code)
	}
	rr, _ = env.do(t, http.MethodDelete, "/budgets/"+id, env.otherToken, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete = %d, want 404", rr.Code)
	}
	rr, body = env.do(t, http.MethodGet, "/budgets", env.otherToken, "")
	if rows, _ := body["budgets"].([]any); rr.Code != http.StatusOK || len(rows) != 0 {
		t.Fatalf("foreign list must be empty: %d %v", rr.Code, body)
	}
}

func TestBudgetEvaluateEndpoint(t *testing.T) {
	env := newBudgetHandlerEnv(t, []billing.ConsumptionRow{
		{AgentID: "agent-busy", Runs: 10, CostCents: 500},
	}, nil)

	// Seed an org runs-budget of 10.
	rr, body := env.do(t, http.MethodPost, "/budgets", env.ownerToken, `{"scope":"organization","limit_runs":10}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed budget = %d body=%s", rr.Code, rr.Body.String())
	}

	// Dry-run: one more run breaches the budget.
	rr, body = env.do(t, http.MethodGet, "/budgets/evaluate?runs=1&cost=0", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("evaluate = %d body=%s", rr.Code, rr.Body.String())
	}
	decision, _ := body["decision"].(map[string]any)
	if decision == nil || decision["allowed"] != false || decision["reason"] != billing.ReasonBudgetExceeded ||
		decision["denied_scope"] != "organization" {
		t.Fatalf("unexpected decision: %v", body)
	}
	if decision["message"] == "" || decision["denied_budget_id"] == "" {
		t.Fatalf("decision must carry message + budget id: %v", decision)
	}

	// Zero projection stays allowed (boundary: consumed == limit).
	rr, body = env.do(t, http.MethodGet, "/budgets/evaluate?runs=0", env.ownerToken, "")
	if decision, _ = body["decision"].(map[string]any); rr.Code != http.StatusOK || decision["allowed"] != true {
		t.Fatalf("zero projection must be allowed: %d %v", rr.Code, body)
	}

	// Cost budget dry-run in cost cents.
	rr, _ = env.do(t, http.MethodPost, "/budgets", env.ownerToken, `{"scope":"organization","limit_cost_cents":100,"is_active":false}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("inactive cost budget = %d", rr.Code)
	}
	rr, body = env.do(t, http.MethodGet, "/budgets/evaluate?runs=0&cost=50.6", env.ownerToken, "")
	if decision, _ = body["decision"].(map[string]any); rr.Code != http.StatusOK || decision["allowed"] != true {
		t.Fatalf("inactive budget must not deny: %d %v", rr.Code, body)
	}

	// Validation errors.
	for _, q := range []string{"?runs=bananas", "?cost=-3", "?runs=-1", "?cost=nan"} {
		if rr, _ := env.do(t, http.MethodGet, "/budgets/evaluate"+q, env.ownerToken, ""); rr.Code != http.StatusBadRequest {
			t.Fatalf("evaluate %s = %d, want 400", q, rr.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Create-run budget gate (mirrors runs_billing_test.go)
// ---------------------------------------------------------------------------

type budgetGateEnv struct {
	authSvc  *auth.Service
	token    string
	orgID    string
	runsSvc  *runs.Service
	queue    *queue.Queue
	auditSvc *audit.Service
}

func newBudgetGateEnv(t *testing.T) *budgetGateEnv {
	t.Helper()
	env := &budgetGateEnv{authSvc: auth.NewService("dev-secret")}
	_, user, err := env.authSvc.Register("Acme", "budgetgate@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	env.token, err = env.authSvc.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	env.orgID = user.Organization
	env.runsSvc = runs.NewService()
	env.queue = queue.NewQueue()
	env.auditSvc = audit.NewService()
	return env
}

func (env *budgetGateEnv) postRun(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"agent_id":"agent-1","input":"hello","estimated_cost_cents":100}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.token)
	rr := httptest.NewRecorder()
	auth.RequireAuth(env.authSvc)(createRunHandler(env.queue, env.auditSvc)).ServeHTTP(rr, req)
	return rr
}

// TestCreateRunBudgetEnforcement is the decision matrix of the create-run
// budget gate. The quota gate is neutralized by NOT subscribing the org
// (no subscription = no quota to exceed), so every outcome below is the
// budget gate's alone.
func TestCreateRunBudgetEnforcement(t *testing.T) {
	cases := []struct {
		name          string
		flag          string // AGENTOS_BILLING_ENFORCEMENT
		wireBudgets   bool   // assign budgetServiceVar
		budgets       []billing.BudgetInput
		consumption   *stubBudgetConsumption
		wantStatus    int
		wantErrorCode string
		wantRunCount  int
		wantQueueLen  int
		wantAuditDeny int
	}{
		{
			name:         "enforcement off + over budget passes (today's behavior)",
			flag:         "",
			wireBudgets:  true,
			budgets:      []billing.BudgetInput{{Scope: billing.BudgetScopeOrganization, LimitRuns: 1, IsActive: true}},
			consumption:  &stubBudgetConsumption{rows: []billing.ConsumptionRow{{AgentID: "agent-1", Runs: 99}}},
			wantStatus:   http.StatusCreated,
			wantRunCount: 1,
			wantQueueLen: 1,
		},
		{
			name:         "enforcement on + budget surface unwired allows (additive seam)",
			flag:         "true",
			wireBudgets:  false,
			wantStatus:   http.StatusCreated,
			wantRunCount: 1,
			wantQueueLen: 1,
		},
		{
			name:         "enforcement on + within budget passes",
			flag:         "true",
			wireBudgets:  true,
			budgets:      []billing.BudgetInput{{Scope: billing.BudgetScopeOrganization, LimitRuns: 10, IsActive: true}},
			consumption:  &stubBudgetConsumption{rows: []billing.ConsumptionRow{{AgentID: "agent-1", Runs: 4}}},
			wantStatus:   http.StatusCreated,
			wantRunCount: 1,
			wantQueueLen: 1,
		},
		{
			name:          "org budget over runs denied with 402 budget_exceeded",
			flag:          "true",
			wireBudgets:   true,
			budgets:       []billing.BudgetInput{{Scope: billing.BudgetScopeOrganization, LimitRuns: 10, IsActive: true}},
			consumption:   &stubBudgetConsumption{rows: []billing.ConsumptionRow{{AgentID: "agent-1", Runs: 10}}},
			wantStatus:    http.StatusPaymentRequired,
			wantErrorCode: billing.ReasonBudgetExceeded,
			wantRunCount:  0,
			wantQueueLen:  0,
			wantAuditDeny: 1,
		},
		{
			name:          "agent budget denies only the budgeted agent",
			flag:          "true",
			wireBudgets:   true,
			budgets:       []billing.BudgetInput{{Scope: billing.BudgetScopeAgent, RefID: "agent-1", LimitCostCents: 100, IsActive: true}},
			consumption:   &stubBudgetConsumption{rows: []billing.ConsumptionRow{{AgentID: "agent-1", Runs: 1, CostCents: 100.4}}},
			wantStatus:    http.StatusPaymentRequired,
			wantErrorCode: billing.ReasonBudgetExceeded,
			wantRunCount:  0,
			wantQueueLen:  0,
			wantAuditDeny: 1,
		},
		{
			name:         "other agent unaffected by agent-scope budget",
			flag:         "true",
			wireBudgets:  true,
			budgets:      []billing.BudgetInput{{Scope: billing.BudgetScopeAgent, RefID: "agent-other", LimitRuns: 1, IsActive: true}},
			consumption:  &stubBudgetConsumption{rows: []billing.ConsumptionRow{{AgentID: "agent-other", Runs: 999}}},
			wantStatus:   http.StatusCreated,
			wantRunCount: 1,
			wantQueueLen: 1,
		},
		{
			name:          "consumption failure refuses to fake availability",
			flag:          "true",
			wireBudgets:   true,
			budgets:       []billing.BudgetInput{{Scope: billing.BudgetScopeOrganization, LimitRuns: 10, IsActive: true}},
			consumption:   &stubBudgetConsumption{err: errors.New("aggregate unavailable")},
			wantStatus:    http.StatusInternalServerError,
			wantErrorCode: "internal_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(billing.EnforcementEnvVar, tc.flag)
			savedBudgets, savedBilling, savedRuns := budgetServiceVar, billingServiceVar, runsServiceVar
			t.Cleanup(func() { budgetServiceVar, billingServiceVar, runsServiceVar = savedBudgets, savedBilling, savedRuns })

			env := newBudgetGateEnv(t)
			svc := billing.NewBudgetService()
			if tc.consumption != nil {
				svc.SetConsumptionSource(tc.consumption)
			}
			for _, in := range tc.budgets {
				if _, err := svc.CreateBudgetCtx(context.Background(), env.orgID, in); err != nil {
					t.Fatalf("seed budget failed: %v", err)
				}
			}
			billingServiceVar = billing.NewService() // quota gate: org has no subscription -> allow
			runsServiceVar = env.runsSvc
			if tc.wireBudgets {
				budgetServiceVar = svc
			} else {
				budgetServiceVar = nil
			}

			rr := env.postRun(t)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus == http.StatusPaymentRequired || tc.wantStatus == http.StatusInternalServerError {
				var body struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatalf("error envelope should be valid JSON: %v", err)
				}
				if body.Error.Code != tc.wantErrorCode {
					t.Fatalf("error.code = %q, want %q", body.Error.Code, tc.wantErrorCode)
				}
				if strings.TrimSpace(body.Error.Message) == "" {
					t.Fatal("error.message must not be empty")
				}
			}
			if env.queue.Length() != tc.wantQueueLen {
				t.Fatalf("queue length = %d, want %d", env.queue.Length(), tc.wantQueueLen)
			}
			created, err := env.runsSvc.ListRunsCtx(context.Background(), env.orgID)
			if err != nil {
				t.Fatalf("ListRunsCtx returned error: %v", err)
			}
			if len(created) != tc.wantRunCount {
				t.Fatalf("run rows = %d, want %d", len(created), tc.wantRunCount)
			}
			denials := 0
			for _, e := range env.auditSvc.List() {
				if e.Action == "run.budget_denied" {
					denials++
					if e.OrganizationID != env.orgID || e.Metadata["reason"] != billing.ReasonBudgetExceeded {
						t.Fatalf("unexpected budget denial audit entry: %+v", e)
					}
				}
			}
			if denials != tc.wantAuditDeny {
				t.Fatalf("run.budget_denied audit rows = %d, want %d", denials, tc.wantAuditDeny)
			}
		})
	}
}

// TestCreateRunBudgetGatePostgresMode pins the same 402 decision over the
// Postgres budget store + per-agent cost aggregation (the production wiring
// shape; migration 026 tables).
func TestCreateRunBudgetGatePostgresMode(t *testing.T) {
	t.Setenv(billing.EnforcementEnvVar, "true")
	savedBudgets, savedBilling, savedRuns := budgetServiceVar, billingServiceVar, runsServiceVar
	t.Cleanup(func() { budgetServiceVar, billingServiceVar, runsServiceVar = savedBudgets, savedBilling, savedRuns })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	env := newBudgetGateEnv(t)
	billingServiceVar = billing.NewService()
	runsServiceVar = env.runsSvc

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	budgetRows := sqlmock.NewRows([]string{
		"id", "organization_id", "scope", "ref_id", "period", "limit_runs",
		"limit_cost_cents", "is_active", "created_at", "updated_at",
	}).AddRow("bud-1", env.orgID, "organization", "", "monthly", int64(10), int64(0), true, now, now)
	costRows := sqlmock.NewRows([]string{"agent_id", "cost", "runs"}).AddRow("agent-1", float64(12.5), int64(11))

	// EvaluateBudget: budget list, then the month-to-date per-agent aggregate.
	mock.ExpectQuery(`SELECT .* FROM budgets WHERE organization_id = \$1 ORDER BY created_at ASC, id ASC`).
		WithArgs(env.orgID).
		WillReturnRows(budgetRows)
	mock.ExpectQuery(`SELECT r\.agent_id, COALESCE\(SUM\(r\.cost_cents\), 0\), COUNT\(\*\) FROM runs r`).
		WithArgs(env.orgID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(costRows)

	budgetServiceVar = billing.NewBudgetServiceWithStore(billing.NewBudgetPostgresStore(db),
		billing.NewRunsConsumptionSource(runs.NewServiceWithStore(runs.NewPostgresStore(db))))

	rr := env.postRun(t)
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want %d, body=%s", rr.Code, http.StatusPaymentRequired, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("error envelope should be valid JSON: %v", err)
	}
	if body.Error.Code != billing.ReasonBudgetExceeded {
		t.Fatalf("error.code = %q, want budget_exceeded", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "allows 10 runs, projected 12 runs") {
		t.Fatalf("message should carry the budget numbers, got %q", body.Error.Message)
	}
	if env.queue.Length() != 0 {
		t.Fatal("nothing may be enqueued when the gate refuses")
	}
	if created, lerr := env.runsSvc.ListRunsCtx(context.Background(), env.orgID); lerr != nil || len(created) != 0 {
		t.Fatalf("no run row may exist after a 402; got %d rows err=%v", len(created), lerr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
	if denials := len(env.auditSvc.List()); denials != 1 {
		t.Fatalf("denial audit rows = %d, want 1", denials)
	}
}
