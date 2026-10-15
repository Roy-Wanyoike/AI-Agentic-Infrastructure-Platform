package main

// limiter_test.go — issue #124 HTTP surface + create-run enforcement, all
// exercised through the REAL middleware chain (RequireAuthOrAPIKey ->
// RequirePermission; auth.RequireAuth -> createRunHandler). Companion test
// of cmd/api/limiter.go (residency_test.go harness precedent):
//
//   - management CRUD over /rate-limits: auth (401), the limiter.read /
//     limiter.write RBAC matrix (403), validation (400/422), absent-row
//     deletes (404) and the happy lifecycle (upsert org default +
//     subject row, list with the org_default flag, delete, read-back);
//   - the REQUIRED integration test: an agent exceeding its RPM limit gets
//     429 {"error":{"code":"rate_limited","scope":...,"retry_after":...}}
//     with a Retry-After header and NO run row for the denied request —
//     in BOTH engine modes (in-memory fallback and Redis over miniredis);
//   - subject isolation (a denied agent does not throttle its siblings) and
//     the unwired-gate no-op default (legacy always-allow).
//
// Both engine variants wire the seam exactly the way newApp wires it: ONE
// resolver shared by the engine and the Enforcer, over an in-memory store
// (the zero-infrastructure mode).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/billing"
	"agentos/internal/limiter"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// newLimiterTestRouter mounts the management surface over the in-memory
// store with the real auth middleware (the newApp wiring, in-memory mode).
func newLimiterTestRouter(t *testing.T) (http.Handler, *auth.Service, *limiter.Enforcer) {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	keysSvc := apikeys.NewService()
	store := limiter.NewMemoryStore()
	resolver := limiter.NewResolver(store)
	svc := limiter.NewEnforcer(limiter.NewLimiter(nil, resolver), resolver)
	apiMux := http.NewServeMux()
	registerLimiterRoutes(apiMux, svc, authSvc, keysSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, svc
}

func TestRateLimitsAuthRequired(t *testing.T) {
	h, _, _ := newLimiterTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/rate-limits"},
		{http.MethodPut, "/api/v1/rate-limits/agent"},
		{http.MethodPut, "/api/v1/rate-limits/agent/a1"},
		{http.MethodDelete, "/api/v1/rate-limits/agent"},
		{http.MethodDelete, "/api/v1/rate-limits/agent/a1"},
	} {
		if rr := resReq(t, h, tc.method, tc.path, "", `{"rpm":5}`); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d", tc.method, tc.path, rr.Code)
		}
	}
}

func TestRateLimitsRBACMatrix(t *testing.T) {
	h, authSvc, _ := newLimiterTestRouter(t)
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = resTokenFor(t, authSvc, strings.ToLower(role)+"@x.io", "org-a", role)
	}
	// limiter.read: EVERY role may list (limits are configuration, not
	// credentials — dashboards and runbooks render them).
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		if rr := resReq(t, h, http.MethodGet, "/api/v1/rate-limits", tokens[role], ""); rr.Code != http.StatusOK {
			t.Errorf("GET as %s: want 200, got %d", role, rr.Code)
		}
	}
	// limiter.write: OWNER/ADMIN only — a change re-throttles the org.
	for _, role := range []string{"MEMBER", "VIEWER"} {
		if rr := resReq(t, h, http.MethodPut, "/api/v1/rate-limits/agent", tokens[role], `{"rpm":5}`); rr.Code != http.StatusForbidden {
			t.Errorf("PUT as %s: want 403, got %d", role, rr.Code)
		}
		if rr := resReq(t, h, http.MethodPut, "/api/v1/rate-limits/agent/a1", tokens[role], `{"rpm":5}`); rr.Code != http.StatusForbidden {
			t.Errorf("PUT subject as %s: want 403, got %d", role, rr.Code)
		}
		if rr := resReq(t, h, http.MethodDelete, "/api/v1/rate-limits/agent", tokens[role], ""); rr.Code != http.StatusForbidden {
			t.Errorf("DELETE as %s: want 403, got %d", role, rr.Code)
		}
	}
	for _, role := range []string{"OWNER", "ADMIN"} {
		if rr := resReq(t, h, http.MethodPut, "/api/v1/rate-limits/agent", tokens[role], `{"rpm":5}`); rr.Code != http.StatusOK {
			t.Errorf("PUT as %s: want 200, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}
}

func TestRateLimitsUpsertValidation(t *testing.T) {
	h, authSvc, _ := newLimiterTestRouter(t)
	token := resTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")
	for _, tc := range []struct {
		name, path, body string
		wantFragment     string
	}{
		{"unknown scope", "/api/v1/rate-limits/tenant", `{"rpm":5}`, "scope must be one of"},
		{"org scope with subject", "/api/v1/rate-limits/org/org-1", `{"rpm":5}`, "org scope has no subject"},
		{"negative rpm", "/api/v1/rate-limits/agent", `{"rpm":-1}`, "must be >= 0"},
		{"negative tpm", "/api/v1/rate-limits/model/m1", `{"tpm":-3}`, "must be >= 0"},
		{"all-zero enabled row", "/api/v1/rate-limits/agent", `{"rpm":0,"rpd":0,"tpm":0}`, "at least one of rpm"},
	} {
		rr := resReq(t, h, http.MethodPut, tc.path, token, tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: want 422, got %d body=%s", tc.name, rr.Code, rr.Body.String())
			continue
		}
		body := resBody(t, rr)
		errObj, _ := body["error"].(map[string]any)
		if errObj == nil || errObj["code"] != "VALIDATION_ERROR" {
			t.Errorf("%s: want VALIDATION_ERROR, got %v", tc.name, body)
		}
		if msg, _ := errObj["message"].(string); !strings.Contains(msg, tc.wantFragment) {
			t.Errorf("%s: message %q missing %q", tc.name, msg, tc.wantFragment)
		}
	}
	// Malformed JSON is a 400 before any validation runs.
	if rr := resReq(t, h, http.MethodPut, "/api/v1/rate-limits/agent", token, `{"rpm":`); rr.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: want 400, got %d", rr.Code)
	}
	// Validation on DELETE paths (unknown scope / org scope with subject).
	for _, path := range []string{"/api/v1/rate-limits/tenant", "/api/v1/rate-limits/org/org-1"} {
		if rr := resReq(t, h, http.MethodDelete, path, token, ""); rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("DELETE %s: want 422, got %d", path, rr.Code)
		}
	}
}

func TestRateLimitsCRUDLifecycle(t *testing.T) {
	h, authSvc, svc := newLimiterTestRouter(t)
	token := resTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	// Org default for the agent scope.
	rr := resReq(t, h, http.MethodPut, "/api/v1/rate-limits/agent", token, `{"rpm":5,"rpd":100}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT org default: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	putBody := resBody(t, rr)
	limitObj, _ := putBody["limit"].(map[string]any)
	if limitObj["subject_id"] != nil || limitObj["scope"] != "agent" {
		t.Errorf("org default row: want subject_id null + scope agent, got %v", limitObj)
	}

	// Subject-specific row overrides the default for agent-1.
	rr = resReq(t, h, http.MethodPut, "/api/v1/rate-limits/agent/agent-1", token, `{"rpm":3,"tpm":1000}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT subject row: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	// List reflects both rows and the org_default flag.
	rr = resReq(t, h, http.MethodGet, "/api/v1/rate-limits", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET list: want 200, got %d", rr.Code)
	}
	listBody := resBody(t, rr)
	items, _ := listBody["limits"].([]any)
	if len(items) != 2 {
		t.Fatalf("list: want 2 rows, got %d (%v)", len(items), listBody)
	}
	seen := map[string]bool{}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		subject, _ := item["subject_id"].(string)
		if subject == "" && item["org_default"] != true {
			t.Errorf("org default row missing org_default flag: %v", item)
		}
		if subject == "agent-1" && item["org_default"] != false {
			t.Errorf("subject row must not be org_default: %v", item)
		}
		seen[subject] = true
	}
	if !seen[""] || !seen["agent-1"] {
		t.Errorf("list rows missing expected subjects: %v", seen)
	}

	// Resolution: subject-specific over org default over unlimited.
	limit, found, err := svc.Resolver().Resolve(context.Background(), "org-a", limiter.ScopeAgent, "agent-1")
	if err != nil || !found || limit.RPM != 3 || limit.TPM != 1000 {
		t.Errorf("subject resolution: want rpm=3 tpm=1000 found=true, got %+v found=%v err=%v", limit, found, err)
	}
	limit, found, err = svc.Resolver().Resolve(context.Background(), "org-a", limiter.ScopeAgent, "agent-2")
	if err != nil || !found || limit.RPM != 5 || limit.RPD != 100 {
		t.Errorf("org default resolution: want rpm=5 rpd=100, got %+v found=%v err=%v", limit, found, err)
	}
	limit, found, err = svc.Resolver().Resolve(context.Background(), "org-a", limiter.ScopeTool, "t1")
	if err != nil || found {
		t.Errorf("unlimited scope: want found=false, got %+v found=%v err=%v", limit, found, err)
	}

	// Delete the subject row; the org default stays.
	rr = resReq(t, h, http.MethodDelete, "/api/v1/rate-limits/agent/agent-1", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE subject: want 200, got %d", rr.Code)
	}
	// Second delete of the absent row: 404 not_found (no existence leak).
	rr = resReq(t, h, http.MethodDelete, "/api/v1/rate-limits/agent/agent-1", token, "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("re-DELETE: want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if body := resBody(t, rr); body["error"].(map[string]any)["code"] != "NOT_FOUND" {
		t.Errorf("re-DELETE: want NOT_FOUND code, got %v", body)
	}
	// Deleting the org default of an untouched scope is also 404.
	rr = resReq(t, h, http.MethodDelete, "/api/v1/rate-limits/tool", token, "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("DELETE absent org default: want 404, got %d", rr.Code)
	}
	// The surviving row is the org default.
	rr = resReq(t, h, http.MethodGet, "/api/v1/rate-limits", token, "")
	items, _ = resBody(t, rr)["limits"].([]any)
	if len(items) != 1 {
		t.Errorf("post-delete list: want 1 row, got %d", len(items))
	}
}

// rateGateEnv is the create-run enforcement fixture: everything the gate
// touches is saved/restored so cases stay isolated (policyGateEnv pattern).
type rateGateEnv struct {
	authSvc  *auth.Service
	token    string
	orgID    string
	runsSvc  *runs.Service
	limStore *limiter.MemoryStore
	svc      *limiter.Enforcer
}

// newRateGateEnv wires the create-run gate seam the way newApp does: ONE
// resolver shared by the engine and the Enforcer, over an in-memory store.
// buildEngine may replace the engine (the miniredis Redis variant); nil
// keeps the in-memory fallback engine.
func newRateGateEnv(t *testing.T, buildEngine func(res *limiter.Resolver) *limiter.Limiter) *rateGateEnv {
	t.Helper()
	env := &rateGateEnv{authSvc: auth.NewService("dev-secret"), limStore: limiter.NewMemoryStore()}
	_, user, err := env.authSvc.Register("Acme", "limiter@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	env.token, err = env.authSvc.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	env.orgID = user.Organization
	env.runsSvc = runs.NewService()
	res := limiter.NewResolver(env.limStore)
	engine := limiter.NewLimiter(nil, res)
	if buildEngine != nil {
		engine = buildEngine(res)
	}
	env.svc = limiter.NewEnforcer(engine, res)

	prevRuns, prevBilling, prevGate := runsServiceVar, billingServiceVar, limiterGateVar
	runsServiceVar = env.runsSvc
	billingServiceVar = billing.NewService() // no subscription -> quota gate allows
	limiterGateVar = env.svc
	t.Cleanup(func() {
		runsServiceVar, billingServiceVar, limiterGateVar = prevRuns, prevBilling, prevGate
	})
	return env
}

func (env *rateGateEnv) postRun(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.token)
	rr := httptest.NewRecorder()
	auth.RequireAuth(env.authSvc)(createRunHandler(queue.NewQueue(), nil)).ServeHTTP(rr, req)
	return rr
}

func (env *rateGateEnv) runCount(t *testing.T) int {
	t.Helper()
	runsList, err := env.runsSvc.ListRunsCtx(context.Background(), env.orgID)
	if err != nil {
		t.Fatalf("ListRunsCtx returned error: %v", err)
	}
	return len(runsList)
}

// seedAgentLimit files the agent-scope limit through the Enforcer (the same
// path PUT /rate-limits uses), so the resolver cache is coherent from the
// very first check.
func (env *rateGateEnv) seedAgentLimit(t *testing.T, agentID string, rpm int) {
	t.Helper()
	err := env.svc.UpsertConfig(context.Background(), env.orgID, limiter.Config{
		Scope: limiter.ScopeAgent, Subject: agentID, RPM: rpm, Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertConfig returned error: %v", err)
	}
}

// assertRateLimited checks the 429 envelope + Retry-After header contract.
func assertRateLimited(t *testing.T, rr *httptest.ResponseRecorder, wantScope string) {
	t.Helper()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d body=%s", rr.Code, rr.Body.String())
	}
	retryAfter, err := strconv.Atoi(rr.Header().Get("Retry-After"))
	if err != nil || retryAfter < 1 {
		t.Errorf("Retry-After header: want integer >= 1, got %q", rr.Header().Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Code       string `json:"code"`
			Scope      string `json:"scope"`
			RetryAfter int    `json:"retry_after"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not an error envelope: %v (%q)", err, rr.Body.String())
	}
	if body.Error.Code != "rate_limited" {
		t.Errorf("error.code: want rate_limited, got %q", body.Error.Code)
	}
	if body.Error.Scope != wantScope {
		t.Errorf("error.scope: want %q, got %q", wantScope, body.Error.Scope)
	}
	if body.Error.RetryAfter != retryAfter {
		t.Errorf("error.retry_after (%d) must equal the Retry-After header (%d)", body.Error.RetryAfter, retryAfter)
	}
}

// TestCreateRunRateLimitedMemory is the REQUIRED integration test,
// in-memory engine variant: an agent exceeding its RPM limit gets 429 on
// the subsequent request, with no run row left behind for the denial and
// sibling agents unaffected.
func TestCreateRunRateLimitedMemory(t *testing.T) {
	env := newRateGateEnv(t, nil)
	env.seedAgentLimit(t, "agent-throttled", 2)

	for i := 0; i < 2; i++ {
		rr := env.postRun(t, `{"agent_id":"agent-throttled","input":"hi"}`)
		if rr.Code != http.StatusCreated {
			t.Fatalf("request %d: want 201, got %d body=%s", i+1, rr.Code, rr.Body.String())
		}
	}
	// Third request within the same minute window: denied, no run row.
	rr := env.postRun(t, `{"agent_id":"agent-throttled","input":"hi"}`)
	assertRateLimited(t, rr, "agent")
	if got := env.runCount(t); got != 2 {
		t.Errorf("denied request must leave no run behind: want 2 runs, got %d", got)
	}

	// Subject isolation: a sibling agent on the same org still runs.
	rr = env.postRun(t, `{"agent_id":"agent-sibling","input":"hi"}`)
	if rr.Code != http.StatusCreated {
		t.Errorf("sibling agent: want 201, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestCreateRunRateLimitedRedis is the same integration contract over the
// Redis engine (miniredis): the sliding-window Lua script denies the third
// request within the window and the seam answers 429 identically.
func TestCreateRunRateLimitedRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	env := newRateGateEnv(t, func(res *limiter.Resolver) *limiter.Limiter {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		return limiter.NewLimiter(rdb, res)
	})
	env.seedAgentLimit(t, "agent-throttled", 2)

	for i := 0; i < 2; i++ {
		rr := env.postRun(t, `{"agent_id":"agent-throttled","input":"hi"}`)
		if rr.Code != http.StatusCreated {
			t.Fatalf("request %d: want 201, got %d body=%s", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := env.postRun(t, `{"agent_id":"agent-throttled","input":"hi"}`)
	assertRateLimited(t, rr, "agent")
	if got := env.runCount(t); got != 2 {
		t.Errorf("denied request must leave no run behind: want 2 runs, got %d", got)
	}
}

// TestCreateRunRateLimitUnwiredIsNoop documents the no-op default: an
// unwired gate (limiterGateVar nil) keeps the legacy always-allow behavior
// even with limits configured.
func TestCreateRunRateLimitUnwiredIsNoop(t *testing.T) {
	env := newRateGateEnv(t, nil)
	prevGate := limiterGateVar
	limiterGateVar = nil
	t.Cleanup(func() { limiterGateVar = prevGate })
	env.seedAgentLimit(t, "agent-throttled", 1)

	for i := 0; i < 3; i++ {
		rr := env.postRun(t, `{"agent_id":"agent-throttled","input":"hi"}`)
		if rr.Code != http.StatusCreated {
			t.Fatalf("request %d: want 201 (gate unwired), got %d body=%s", i+1, rr.Code, rr.Body.String())
		}
	}
}
