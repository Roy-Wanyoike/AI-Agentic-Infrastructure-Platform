package main

// Usage-meter + margin handler tests (issue #57): auth, the RBAC matrix
// through the registered middleware chain (MEMBER+ meters via runs.execute,
// OWNER-only margin via organization.manage), real in-memory meter
// aggregation over runs+steps, window validation, the env-driven async Stripe
// sync trigger and the margin envelope math. Mirrors billing_test.go style.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/billing"
	"agentos/internal/runs"
)

// discardLogger keeps async-sync log noise out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingSyncer implements billing.StripeSyncer and records calls.
type recordingSyncer struct {
	mu      sync.Mutex
	calls   []syncCall
	enabled bool
	err     error
}

type syncCall struct {
	orgID    string
	from, to time.Time
	meters   *billing.Meters
}

func (r *recordingSyncer) Enabled() bool { return r.enabled }

func (r *recordingSyncer) SyncUsage(_ context.Context, orgID string, from, to time.Time, meters *billing.Meters) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, syncCall{orgID: orgID, from: from, to: to, meters: meters})
	return r.err
}

func (r *recordingSyncer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingSyncer) last() syncCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

type metersHandlerEnv struct {
	mux         *http.ServeMux
	svc         *billing.Service
	runsSvc     *runs.Service
	syncer      *recordingSyncer
	orgID       string
	planID      string
	ownerToken  string
	adminToken  string
	memberToken string
	viewerToken string
	otherToken  string // authenticated OWNER of a DIFFERENT tenant
}

func newMetersHandlerEnv(t *testing.T) *metersHandlerEnv {
	t.Helper()
	runsSvc := runs.NewService()
	// Default wiring mirrors production: both meter dependencies are the runs
	// service (run counts from the cost ledger, tool calls from the steps).
	return newMetersHandlerEnvFull(t, billing.NewRunsMeterSource(runsSvc, runsSvc), runsSvc)
}

func newMetersHandlerEnvWithSource(t *testing.T, meterSrc billing.MeterSource) *metersHandlerEnv {
	t.Helper()
	return newMetersHandlerEnvFull(t, meterSrc, runs.NewService())
}

func newMetersHandlerEnvFull(t *testing.T, meterSrc billing.MeterSource, runsSvc *runs.Service) *metersHandlerEnv {
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

	svc := billing.NewService()
	svc.SetUsageSource(&billStubUsage{rows: []billing.UsageRow{
		{Source: billing.LineSourceRun, Model: "gpt-4o-mini", Runs: 12, CostCents: 3.5},
		{Source: billing.LineSourceRun, Model: "gpt-4o", Runs: 5, CostCents: 12.25},
	}})
	plan, err := svc.CreatePlanCtx(context.Background(), billing.PlanInput{
		Name: "starter", PriceCents: 1900, IncludedQuota: 10,
	})
	if err != nil {
		t.Fatalf("seed plan failed: %v", err)
	}

	syncer := &recordingSyncer{enabled: true}

	mux := http.NewServeMux()
	// billing routes are registered too: subscribing over HTTP seeds the
	// subscription the margin endpoint reports on.
	registerBillingRoutes(mux, svc, authSvc, apiKeysSvc)
	registerUsageMetersRoutes(mux, svc, meterSrc, syncer, authSvc, apiKeysSvc, discardLogger())

	return &metersHandlerEnv{
		mux: mux, svc: svc, runsSvc: runsSvc, syncer: syncer,
		orgID: owner.Organization, planID: plan.ID,
		ownerToken: ownerToken, adminToken: adminToken,
		memberToken: memberToken, viewerToken: viewerToken, otherToken: otherToken,
	}
}

func (e *metersHandlerEnv) do(t *testing.T, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	out := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

func (e *metersHandlerEnv) subscribe(t *testing.T, token string) map[string]any {
	t.Helper()
	rr, body := e.do(t, http.MethodPost, "/billing/subscriptions", token, `{"plan_id":"`+e.planID+`"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("subscribe: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	sub, _ := body["subscription"].(map[string]any)
	if sub == nil {
		t.Fatalf("subscribe: expected subscription envelope, got %v", body)
	}
	return sub
}

// waitForSync polls until the fake syncer recorded n calls.
func waitForSync(t *testing.T, s *recordingSyncer, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.count() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("expected %d sync call(s), got %d", n, s.count())
}

func TestUsageMetersAndMarginRequireAuth(t *testing.T) {
	env := newMetersHandlerEnvWithSource(t, billing.NewRunsMeterSource(nil, nil))
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, "/usage/meters"},
		{http.MethodGet, "/billing/margin"},
	} {
		rr, _ := env.do(t, p.method, p.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without credentials: expected %d, got %d body=%s", p.method, p.path, http.StatusUnauthorized, rr.Code, rr.Body.String())
		}
	}
}

func TestUsageMetersRBACMatrix(t *testing.T) {
	env := newMetersHandlerEnvWithSource(t, billing.NewRunsMeterSource(nil, nil))

	// Meters: MEMBER+ (runs.execute) — VIEWER is 403.
	for _, tc := range []struct {
		token string
		want  int
	}{
		{env.ownerToken, http.StatusOK},
		{env.adminToken, http.StatusOK},
		{env.memberToken, http.StatusOK},
		{env.viewerToken, http.StatusForbidden},
	} {
		rr, _ := env.do(t, http.MethodGet, "/usage/meters", tc.token, "")
		if rr.Code != tc.want {
			t.Fatalf("GET /usage/meters: expected %d, got %d body=%s", tc.want, rr.Code, rr.Body.String())
		}
	}

	// Margin: OWNER only (organization.manage) — ADMIN/MEMBER/VIEWER are 403.
	for _, tc := range []struct {
		token string
		want  int
	}{
		{env.ownerToken, http.StatusNotFound}, // OWNER passes RBAC; no subscription yet -> 404
		{env.adminToken, http.StatusForbidden},
		{env.memberToken, http.StatusForbidden},
		{env.viewerToken, http.StatusForbidden},
	} {
		rr, _ := env.do(t, http.MethodGet, "/billing/margin", tc.token, "")
		if rr.Code != tc.want {
			t.Fatalf("GET /billing/margin: expected %d, got %d body=%s", tc.want, rr.Code, rr.Body.String())
		}
	}
}

func TestUsageMetersAggregationOverHTTP(t *testing.T) {
	env := newMetersHandlerEnv(t)
	// Seed runs + steps for the caller's org and a foreign one.
	seedMeterRuns(t, env.runsSvc, env.orgID, 3, 4)     // 3 runs, 4 tool steps
	seedMeterRuns(t, env.runsSvc, "org-foreign", 2, 6) // must be invisible

	from := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	to := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	rr, body := env.do(t, http.MethodGet, fmt.Sprintf("/usage/meters?from=%s&to=%s", from, to), env.memberToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	meters, ok := body["meters"].(map[string]any)
	if !ok {
		t.Fatalf("expected meters envelope, got %v", body)
	}
	if meters[billing.MeterRunsCount] != float64(3) {
		t.Fatalf("runs_count: expected 3, got %v", meters[billing.MeterRunsCount])
	}
	if meters[billing.MeterToolCallsCount] != float64(4) {
		t.Fatalf("tool_calls_count: expected 4, got %v", meters[billing.MeterToolCallsCount])
	}
	if _, hasSandbox := meters["sandbox_seconds"]; hasSandbox {
		t.Fatal("sandbox_seconds must be absent (not metered anywhere — never invented)")
	}
	if body["from"] == nil || body["to"] == nil {
		t.Fatalf("window echo missing: %v", body)
	}

	// Tenant isolation: the foreign OWNER's org has no runs at all — it must
	// see honest zeros, never Acme's 3/4 (nor the third org's seeded data).
	rr, body = env.do(t, http.MethodGet, fmt.Sprintf("/usage/meters?from=%s&to=%s", from, to), env.otherToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign meters: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	meters = body["meters"].(map[string]any)
	if meters[billing.MeterRunsCount] != float64(0) || meters[billing.MeterToolCallsCount] != float64(0) {
		t.Fatalf("foreign tenant saw cross-tenant data: %v", meters)
	}
}

func TestUsageMetersWindowValidation(t *testing.T) {
	env := newMetersHandlerEnvWithSource(t, billing.NewRunsMeterSource(nil, nil))
	now := time.Now().UTC()
	cases := []struct {
		name string
		q    string
	}{
		{"bad from", "?from=nope&to=" + now.Format(time.RFC3339)},
		{"bad to", "?from=" + now.Add(-time.Hour).Format(time.RFC3339) + "&to=nope"},
		{"inverted", "?from=" + now.Format(time.RFC3339) + "&to=" + now.Add(-time.Hour).Format(time.RFC3339)},
		{"over 366 days", "?from=" + now.Add(-400*24*time.Hour).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)},
	}
	for _, tc := range cases {
		rr, body := env.do(t, http.MethodGet, "/usage/meters"+tc.q, env.memberToken, "")
		if rr.Code != http.StatusBadRequest || errCode(t, body) != "INVALID_TIME_RANGE" {
			t.Fatalf("%s: expected 400 INVALID_TIME_RANGE, got %d body=%s", tc.name, rr.Code, rr.Body.String())
		}
		// A failed read must not trigger a sync.
		if env.syncer.count() != 0 {
			t.Fatalf("%s: sync must not fire for failed reads", tc.name)
		}
	}

	// GET-only: POST is 405.
	rr, _ := env.do(t, http.MethodPost, "/usage/meters", env.memberToken, "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /usage/meters: expected 405, got %d", rr.Code)
	}
}

func TestUsageMetersMeterSourceUnavailable(t *testing.T) {
	env := newMetersHandlerEnvWithSource(t, nil) // nil meter source: honest 503
	rr, body := env.do(t, http.MethodGet, "/usage/meters", env.ownerToken, "")
	if rr.Code != http.StatusServiceUnavailable || errCode(t, body) != "METER_SOURCE_UNAVAILABLE" {
		t.Fatalf("expected 503 METER_SOURCE_UNAVAILABLE, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUsageMetersTriggersStripeSync(t *testing.T) {
	env := newMetersHandlerEnv(t)
	seedMeterRuns(t, env.runsSvc, env.orgID, 2, 3)

	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	to := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	rr, _ := env.do(t, http.MethodGet, fmt.Sprintf("/usage/meters?from=%s&to=%s", from.Format(time.RFC3339), to.Format(time.RFC3339)), env.memberToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}

	// The sync is async: wait for the goroutine, then assert the contract.
	waitForSync(t, env.syncer, 1)
	call := env.syncer.last()
	if call.orgID != env.orgID {
		t.Fatalf("sync org: expected %s, got %s", env.orgID, call.orgID)
	}
	if !call.from.Equal(from) || !call.to.Equal(to) {
		t.Fatalf("sync window: expected [%v,%v), got [%v,%v)", from, to, call.from, call.to)
	}
	if call.meters == nil || call.meters.RunsCount != 2 || call.meters.ToolCallsCount != 3 {
		t.Fatalf("sync meters: expected runs=2 tool_calls=3, got %+v", call.meters)
	}
}

func TestUsageMetersDisabledSyncerNeverFires(t *testing.T) {
	env := newMetersHandlerEnv(t)
	env.syncer.enabled = false // NoopSyncer semantics: no goroutine at all

	rr, _ := env.do(t, http.MethodGet, "/usage/meters", env.memberToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rr.Code)
	}
	// Give the (never started) goroutine a beat, then assert nothing fired.
	time.Sleep(20 * time.Millisecond)
	if env.syncer.count() != 0 {
		t.Fatal("disabled syncer must not be invoked")
	}
}

func TestBillingMarginOverHTTP(t *testing.T) {
	env := newMetersHandlerEnv(t)

	// No subscription: 404 NO_SUBSCRIPTION.
	rr, body := env.do(t, http.MethodGet, "/billing/margin", env.ownerToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "NO_SUBSCRIPTION" {
		t.Fatalf("expected 404 NO_SUBSCRIPTION, got %d body=%s", rr.Code, rr.Body.String())
	}

	sub := env.subscribe(t, env.ownerToken)

	rr, body = env.do(t, http.MethodGet, "/billing/margin", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	margin, ok := body["margin"].(map[string]any)
	if !ok {
		t.Fatalf("expected margin envelope, got %v", body)
	}
	// Formula: 1900 − round(3.5 + 12.25) = 1884.
	if margin["usage_cost_cents"] != 15.75 {
		t.Fatalf("usage_cost_cents: expected 15.75, got %v", margin["usage_cost_cents"])
	}
	if margin["margin_cents"] != float64(1884) {
		t.Fatalf("margin_cents: expected 1884, got %v", margin["margin_cents"])
	}
	if pct, ok := margin["margin_percent"].(float64); !ok || pct < 99.17 || pct > 99.18 {
		t.Fatalf("margin_percent: expected ≈99.17, got %v", margin["margin_percent"])
	}
	if margin["price_cents"] != float64(1900) || margin["currency"] != "usd" {
		t.Fatalf("unexpected price/currency echo: %v", margin)
	}
	// The reported period is the subscription's own current period.
	if margin["period_start"] != sub["period_start"] || margin["period_end"] != sub["period_end"] {
		t.Fatalf("margin period %v/%v does not match subscription period %v/%v",
			margin["period_start"], margin["period_end"], sub["period_start"], sub["period_end"])
	}
	plan, ok := margin["plan"].(map[string]any)
	if !ok || plan["name"] != "starter" || plan["included_quota"] != float64(10) || plan["unlimited"] != false {
		t.Fatalf("unexpected plan echo: %v", margin["plan"])
	}

	// Tenant isolation: the foreign OWNER reads their OWN org's margin —
	// never Acme's (they have no subscription: 404, no data leak).
	rr, body = env.do(t, http.MethodGet, "/billing/margin", env.otherToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "NO_SUBSCRIPTION" {
		t.Fatalf("foreign margin: expected 404 NO_SUBSCRIPTION, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBillingMarginZeroUsage(t *testing.T) {
	env := newMetersHandlerEnv(t)
	// Empty the stub ledger: zero usage -> full margin, 100%.
	env.svc.SetUsageSource(&billStubUsage{})
	env.subscribe(t, env.ownerToken)

	rr, body := env.do(t, http.MethodGet, "/billing/margin", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	margin := body["margin"].(map[string]any)
	if margin["usage_cost_cents"] != float64(0) || margin["margin_cents"] != float64(1900) {
		t.Fatalf("expected full margin at zero usage, got %v", margin)
	}
	if margin["margin_percent"] != float64(100) {
		t.Fatalf("expected 100%% margin, got %v", margin["margin_percent"])
	}
}

// seedMeterRuns creates n runs for the given org with m tool steps in TOTAL
// (all on the first run, plus one model step per run) in the in-memory runs
// service.
func seedMeterRuns(t *testing.T, svc *runs.Service, orgID string, n, m int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		run, err := svc.CreateRunCtx(ctx, orgID, "agent-meter", "input")
		if err != nil {
			t.Fatalf("CreateRunCtx returned error: %v", err)
		}
		if i == 0 {
			for j := 0; j < m; j++ {
				if err := svc.RecordStep(ctx, orgID, run.ID, &runs.Step{StepType: runs.StepTypeTool, Status: "COMPLETED"}); err != nil {
					t.Fatalf("RecordStep returned error: %v", err)
				}
			}
		}
		if err := svc.RecordStep(ctx, orgID, run.ID, &runs.Step{StepType: runs.StepTypeModel, Status: "COMPLETED"}); err != nil {
			t.Fatalf("RecordStep(model) returned error: %v", err)
		}
	}
}
