package main

// Billing handler tests (issue #24, wave-5 track 5-c): auth (401), the RBAC
// matrix through the registered middleware chain (OWNER-only subscribe via
// organization.manage, OWNER/ADMIN cancel via users.manage, MEMBER+ reads via
// runs.execute), contract JSON envelopes, tenant isolation and lifecycle
// behavior. All in-memory, no infrastructure. Mirrors versions_test.go style.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/billing"
)

// billStubUsage feeds deterministic usage rows into the billing service
// (implements billing.UsageSource).
type billStubUsage struct {
	rows []billing.UsageRow
}

func (s *billStubUsage) UsageForPeriod(_ context.Context, _ string, _, _ time.Time) ([]billing.UsageRow, error) {
	return s.rows, nil
}

type billingHandlerEnv struct {
	mux         *http.ServeMux
	svc         *billing.Service
	orgID       string
	planID      string
	ownerToken  string
	adminToken  string
	memberToken string
	viewerToken string
	otherToken  string // authenticated OWNER of a DIFFERENT tenant
}

func newBillingHandlerEnv(t *testing.T) *billingHandlerEnv {
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
	// ADMIN/MEMBER/VIEWER exist only as claims (the claims.Role fallback path
	// in RequirePermission covers roles not registered in the user map).
	adminToken, err := authSvc.GenerateToken(&auth.User{
		ID: "user-admin", Organization: owner.Organization, Email: "admin@acme.test", Role: "ADMIN",
	})
	if err != nil {
		t.Fatalf("GenerateToken(admin) returned error: %v", err)
	}
	memberToken, err := authSvc.GenerateToken(&auth.User{
		ID: "user-member", Organization: owner.Organization, Email: "member@acme.test", Role: "MEMBER",
	})
	if err != nil {
		t.Fatalf("GenerateToken(member) returned error: %v", err)
	}
	viewerToken, err := authSvc.GenerateToken(&auth.User{
		ID: "user-viewer", Organization: owner.Organization, Email: "viewer@acme.test", Role: "VIEWER",
	})
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
	}})
	plan, err := svc.CreatePlanCtx(context.Background(), billing.PlanInput{
		Name: "starter", PriceCents: 1900, IncludedQuota: 10,
		Metadata: map[string]any{"overage_run_rate_cents": 2},
	})
	if err != nil {
		t.Fatalf("seed plan failed: %v", err)
	}

	mux := http.NewServeMux()
	registerBillingRoutes(mux, svc, authSvc, apiKeysSvc)

	return &billingHandlerEnv{
		mux: mux, svc: svc, orgID: owner.Organization, planID: plan.ID,
		ownerToken: ownerToken, adminToken: adminToken,
		memberToken: memberToken, viewerToken: viewerToken, otherToken: otherToken,
	}
}

func (e *billingHandlerEnv) do(t *testing.T, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
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

func (e *billingHandlerEnv) subscribe(t *testing.T, token, planID string) map[string]any {
	t.Helper()
	rr, body := e.do(t, http.MethodPost, "/billing/subscriptions", token, `{"plan_id":"`+planID+`"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("subscribe: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	sub, _ := body["subscription"].(map[string]any)
	if sub == nil {
		t.Fatalf("subscribe: expected subscription envelope, got %v", body)
	}
	return sub
}

func TestBillingEndpointsRequireAuth(t *testing.T) {
	env := newBillingHandlerEnv(t)
	paths := []struct{ method, path string }{
		{http.MethodGet, "/billing/plans"},
		{http.MethodPost, "/billing/subscriptions"},
		{http.MethodGet, "/billing/subscription"},
		{http.MethodPost, "/billing/subscription/cancel"},
		{http.MethodGet, "/billing/invoices"},
		{http.MethodGet, "/billing/invoices/inv-1"},
	}
	for _, p := range paths {
		rr, _ := env.do(t, p.method, p.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without credentials: expected %d, got %d body=%s", p.method, p.path, http.StatusUnauthorized, rr.Code, rr.Body.String())
		}
	}
}

func TestBillingRBACMatrix(t *testing.T) {
	env := newBillingHandlerEnv(t)

	// Subscribe: OWNER only (organization.manage) — also seeds the live
	// subscription the read endpoints below report on.
	rr, _ := env.do(t, http.MethodPost, "/billing/subscriptions", env.ownerToken, `{"plan_id":"`+env.planID+`"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("owner subscribe: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	for _, tc := range []struct{ name, token string }{
		{"admin", env.adminToken}, {"member", env.memberToken}, {"viewer", env.viewerToken},
	} {
		rr, _ = env.do(t, http.MethodPost, "/billing/subscriptions", tc.token, `{"plan_id":"`+env.planID+`"}`)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s subscribe: expected %d, got %d body=%s", tc.name, http.StatusForbidden, rr.Code, rr.Body.String())
		}
	}

	// Reads: MEMBER+ (runs.execute) — OWNER/ADMIN/MEMBER pass, VIEWER is 403.
	for _, path := range []string{"/billing/plans", "/billing/subscription", "/billing/invoices"} {
		for _, tc := range []struct {
			token string
			want  int
		}{
			{env.ownerToken, http.StatusOK},
			{env.adminToken, http.StatusOK},
			{env.memberToken, http.StatusOK},
			{env.viewerToken, http.StatusForbidden},
		} {
			rr, _ := env.do(t, http.MethodGet, path, tc.token, "")
			if rr.Code != tc.want {
				t.Fatalf("GET %s: expected %d, got %d body=%s", path, tc.want, rr.Code, rr.Body.String())
			}
		}
	}

	// Cancel: OWNER/ADMIN (users.manage), MEMBER/VIEWER 403.
	rr, _ = env.do(t, http.MethodPost, "/billing/subscription/cancel", env.memberToken, `{"immediate":true}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member cancel: expected %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
	rr, _ = env.do(t, http.MethodPost, "/billing/subscription/cancel", env.adminToken, `{"immediate":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin cancel: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
}

func TestBillingPlansEnvelope(t *testing.T) {
	env := newBillingHandlerEnv(t)
	rr, body := env.do(t, http.MethodGet, "/billing/plans", env.memberToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	plans, ok := body["plans"].([]any)
	if !ok || len(plans) != 1 {
		t.Fatalf("expected one plan, got %v", body)
	}
	plan := plans[0].(map[string]any)
	if plan["name"] != "starter" || plan["price_cents"] != float64(1900) || plan["currency"] != "usd" || plan["included_quota"] != float64(10) {
		t.Fatalf("unexpected plan view: %v", plan)
	}
}

func TestBillingSubscribeLifecycleOverHTTP(t *testing.T) {
	env := newBillingHandlerEnv(t)

	// No subscription yet: GET reports 404 NO_SUBSCRIPTION.
	rr, body := env.do(t, http.MethodGet, "/billing/subscription", env.ownerToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "NO_SUBSCRIPTION" {
		t.Fatalf("expected 404 NO_SUBSCRIPTION, got %d body=%s", rr.Code, rr.Body.String())
	}

	sub := env.subscribe(t, env.ownerToken, env.planID)
	if sub["status"] != billing.StatusTrial {
		t.Fatalf("expected trial, got %v", sub["status"])
	}

	// Double subscribe: 409 SUBSCRIPTION_EXISTS.
	rr, body = env.do(t, http.MethodPost, "/billing/subscriptions", env.ownerToken, `{"plan_id":"`+env.planID+`"}`)
	if rr.Code != http.StatusConflict || errCode(t, body) != "CONFLICT" {
		t.Fatalf("expected 409 CONFLICT, got %d body=%s", rr.Code, rr.Body.String())
	}

	// GET returns subscription + quota (12 metered runs from the stub exceed
	// the plan's included quota of 10).
	rr, body = env.do(t, http.MethodGet, "/billing/subscription", env.memberToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	sub, ok := body["subscription"].(map[string]any)
	if !ok || sub["status"] != billing.StatusTrial {
		t.Fatalf("unexpected subscription view: %v", body)
	}
	quota, ok := body["quota"].(map[string]any)
	if !ok || quota["consumed_runs"] != float64(12) || quota["exceeded"] != true || quota["included_runs"] != float64(10) {
		t.Fatalf("unexpected quota view: %v", body)
	}

	// Deferred cancel (empty body -> immediate=false): flag set, still live.
	rr, body = env.do(t, http.MethodPost, "/billing/subscription/cancel", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("deferred cancel: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	sub = body["subscription"].(map[string]any)
	if sub["cancel_at_period_end"] != true || sub["status"] != billing.StatusActive && sub["status"] != billing.StatusTrial {
		t.Fatalf("unexpected deferred cancel result: %v", sub)
	}
}

func TestBillingCancelImmediate(t *testing.T) {
	env := newBillingHandlerEnv(t)
	env.subscribe(t, env.ownerToken, env.planID)

	rr, body := env.do(t, http.MethodPost, "/billing/subscription/cancel", env.ownerToken, `{"immediate":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	sub := body["subscription"].(map[string]any)
	if sub["status"] != billing.StatusCanceled || sub["cancel_at_period_end"] == true {
		t.Fatalf("unexpected immediate cancel result: %v", sub)
	}
	if _, ok := sub["canceled_at"]; !ok {
		t.Fatal("immediate cancel should carry canceled_at")
	}

	// Canceling again: no live subscription left -> 404 NO_SUBSCRIPTION.
	rr, body = env.do(t, http.MethodPost, "/billing/subscription/cancel", env.ownerToken, `{"immediate":true}`)
	if rr.Code != http.StatusNotFound || errCode(t, body) != "NO_SUBSCRIPTION" {
		t.Fatalf("expected 404 NO_SUBSCRIPTION, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBillingSubscribeValidation(t *testing.T) {
	env := newBillingHandlerEnv(t)
	cases := []struct {
		name string
		body string
		want int
		code string
	}{
		{"missing plan_id", `{}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"blank plan_id", `{"plan_id":"  "}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"unknown plan", `{"plan_id":"plan-nope"}`, http.StatusNotFound, "PLAN_NOT_FOUND"},
		{"malformed json", `{"plan_id":`, http.StatusBadRequest, "INVALID_REQUEST"},
	}
	for _, tc := range cases {
		rr, body := env.do(t, http.MethodPost, "/billing/subscriptions", env.ownerToken, tc.body)
		if rr.Code != tc.want || errCode(t, body) != tc.code {
			t.Fatalf("%s: expected %d %s, got %d body=%s", tc.name, tc.want, tc.code, rr.Code, rr.Body.String())
		}
	}
}

func TestBillingInvoicesOverHTTP(t *testing.T) {
	env := newBillingHandlerEnv(t)
	env.subscribe(t, env.ownerToken, env.planID)

	// Seed an invoice for the current period via the service (generation is a
	// service/worker capability; the HTTP surface is read-only for invoices).
	from := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	to := from.Add(24 * time.Hour)
	inv, created, err := env.svc.GenerateInvoiceCtx(context.Background(), env.orgID, from, to)
	if err != nil || !created {
		t.Fatalf("seed invoice failed: created=%v err=%v", created, err)
	}

	// List: newest first, lines omitted.
	rr, body := env.do(t, http.MethodGet, "/billing/invoices", env.memberToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	invoices, ok := body["invoices"].([]any)
	if !ok || len(invoices) != 1 {
		t.Fatalf("expected one invoice, got %v", body)
	}
	view := invoices[0].(map[string]any)
	if view["id"] != inv.ID || view["subtotal_cents"] != float64(inv.SubtotalCents) || view["status"] != billing.InvoiceOpen {
		t.Fatalf("unexpected invoice view: %v", view)
	}
	if _, hasLines := view["lines"]; hasLines {
		t.Fatal("list entries should omit lines")
	}

	// Detail with lines.
	rr, body = env.do(t, http.MethodGet, "/billing/invoices/"+inv.ID, env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	detail := body["invoice"].(map[string]any)
	lines, ok := detail["lines"].([]any)
	if !ok || len(lines) == 0 {
		t.Fatalf("expected lines on the detail view, got %v", detail)
	}
	line := lines[0].(map[string]any)
	if line["source"] != billing.LineSourceRun || line["quantity"] != float64(12) || line["amount_cents"] != float64(4) {
		t.Fatalf("unexpected line view: %v", line)
	}

	// Unknown invoice -> 404 INVOICE_NOT_FOUND.
	rr, body = env.do(t, http.MethodGet, "/billing/invoices/inv-nope", env.ownerToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "INVOICE_NOT_FOUND" {
		t.Fatalf("expected 404 INVOICE_NOT_FOUND, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Tenant isolation BOTH directions: the foreign OWNER sees neither the
	// invoice nor any other Acme billing data.
	rr, body = env.do(t, http.MethodGet, "/billing/invoices/"+inv.ID, env.otherToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "INVOICE_NOT_FOUND" {
		t.Fatalf("foreign detail: expected 404 INVOICE_NOT_FOUND, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr, _ = env.do(t, http.MethodGet, "/billing/invoices", env.otherToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign list: expected %d, got %d", http.StatusOK, rr.Code)
	}
	rr, body = env.do(t, http.MethodGet, "/billing/subscription", env.otherToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "NO_SUBSCRIPTION" {
		t.Fatalf("foreign subscription: expected 404 NO_SUBSCRIPTION, got %d body=%s", rr.Code, rr.Body.String())
	}
	// The foreign owner cannot cancel Acme's subscription either (the tenant
	// comes from the claims, so the mutation targets the WRONG org and finds
	// no live subscription there).
	rr, body = env.do(t, http.MethodPost, "/billing/subscription/cancel", env.otherToken, `{"immediate":true}`)
	if rr.Code != http.StatusNotFound || errCode(t, body) != "NO_SUBSCRIPTION" {
		t.Fatalf("foreign cancel: expected 404 NO_SUBSCRIPTION, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBillingEmptyCatalogStillLists(t *testing.T) {
	// A fresh service with no plans must render "plans": [] (never null).
	svc := billing.NewService()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()
	_, owner, err := authSvc.Register("EmptyCo", "owner@empty.test", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	mux := http.NewServeMux()
	registerBillingRoutes(mux, svc, authSvc, apiKeysSvc)
	req := httptest.NewRequest(http.MethodGet, "/billing/plans", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"plans":[]`) {
		t.Fatalf("expected empty plans list, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBillingNilServiceUnavailable(t *testing.T) {
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()
	_, owner, err := authSvc.Register("NilCo", "owner@nil.test", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	mux := http.NewServeMux()
	registerBillingRoutes(mux, nil, authSvc, apiKeysSvc)
	req := httptest.NewRequest(http.MethodGet, "/billing/plans", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for nil billing service, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBillingMethodNotAllowed(t *testing.T) {
	env := newBillingHandlerEnv(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/billing/plans"},
		{http.MethodGet, "/billing/subscriptions"},
		{http.MethodDelete, "/billing/subscription/cancel"},
		{http.MethodPost, "/billing/invoices"},
	} {
		rr, _ := env.do(t, tc.method, tc.path, env.ownerToken, "")
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: expected 405, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}
