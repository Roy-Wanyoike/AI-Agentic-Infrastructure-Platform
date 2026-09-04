package main

// Tests for the create-run quota enforcement gate (issue #47). The handler is
// dual-mode like the rest of the platform: the table-driven suite below runs
// the gate over the in-memory billing service, and the sqlmock-backed test
// pins the same 402 decision over the Postgres store (subscription + plan
// queries) with a usage source — the shape the production wiring uses.

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

	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/billing"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// gateUsageSource feeds deterministic quota consumption through the billing
// UsageSource seam (production: RunsUsageSource over the cost aggregates).
type gateUsageSource struct {
	rows []billing.UsageRow
	err  error
}

func (s gateUsageSource) UsageForPeriod(_ context.Context, _ string, _, _ time.Time) ([]billing.UsageRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

// quotaGateEnv is the per-case test fixture: everything the gate touches is
// saved/restored so cases stay isolated.
type quotaGateEnv struct {
	authSvc  *auth.Service
	token    string
	orgID    string
	billing  *billing.Service
	runsSvc  *runs.Service
	queue    *queue.Queue
	auditSvc *audit.Service
}

func newQuotaGateEnv(t *testing.T, includedQuota int64, usage billing.UsageSource, consume int64) *quotaGateEnv {
	t.Helper()
	env := &quotaGateEnv{authSvc: auth.NewService("dev-secret")}
	_, user, err := env.authSvc.Register("Acme", "quota@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	env.token, err = env.authSvc.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	env.orgID = user.Organization

	env.billing = billing.NewService()
	if usage != nil {
		env.billing.SetUsageSource(usage)
	}
	if includedQuota >= 0 {
		plan, err := env.billing.CreatePlanCtx(context.Background(), billing.PlanInput{
			Name: "starter", PriceCents: 1900, IncludedQuota: includedQuota,
		})
		if err != nil {
			t.Fatalf("CreatePlanCtx returned error: %v", err)
		}
		if _, err := env.billing.SubscribeCtx(context.Background(), env.orgID, plan.ID); err != nil {
			t.Fatalf("SubscribeCtx returned error: %v", err)
		}
		if consume > 0 {
			if err := env.billing.RecordQuotaConsumptionCtx(context.Background(), env.orgID, consume); err != nil {
				t.Fatalf("RecordQuotaConsumptionCtx returned error: %v", err)
			}
		}
	}
	env.runsSvc = runs.NewService()
	env.queue = queue.NewQueue()
	env.auditSvc = audit.NewService()
	return env
}

func (env *quotaGateEnv) postRun(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"agent_id":"agent-1","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.token)
	rr := httptest.NewRecorder()
	// The org id is omitted from the body so the claims' organization is used
	// (the same tenant resolution the middleware-protected route performs).
	auth.RequireAuth(env.authSvc)(createRunHandler(env.queue, env.auditSvc)).ServeHTTP(rr, req)
	return rr
}

// TestCreateRunQuotaEnforcement is the decision matrix of the create-run gate.
func TestCreateRunQuotaEnforcement(t *testing.T) {
	cases := []struct {
		name          string
		flag          string // AGENTOS_BILLING_ENFORCEMENT
		includedQuota int64  // -1 = org never subscribes
		consume       int64
		usage         billing.UsageSource
		wantStatus    int
		wantErrorCode string // empty -> expect a 201 creation, not an error envelope
		wantRunCount  int
		wantQueueLen  int
		wantAuditDeny int
	}{
		{
			name:          "enforcement off + exceeded passes (today's behavior)",
			flag:          "",
			includedQuota: 10,
			consume:       11,
			wantStatus:    http.StatusCreated,
			wantRunCount:  1,
			wantQueueLen:  1,
		},
		{
			name:          "capped org over quota blocked with 402, nothing enqueued",
			flag:          "true",
			includedQuota: 10,
			consume:       11,
			wantStatus:    http.StatusPaymentRequired,
			wantErrorCode: billing.ReasonQuotaExceeded,
			wantRunCount:  0,
			wantQueueLen:  0,
			wantAuditDeny: 1,
		},
		{
			name:          "capped org within quota passes",
			flag:          "true",
			includedQuota: 10,
			consume:       4,
			wantStatus:    http.StatusCreated,
			wantRunCount:  1,
			wantQueueLen:  1,
		},
		{
			name:          "unlimited org passes even far over",
			flag:          "true",
			includedQuota: 0,
			consume:       9999,
			wantStatus:    http.StatusCreated,
			wantRunCount:  1,
			wantQueueLen:  1,
		},
		{
			name:          "org without subscription passes (no quota to exceed)",
			flag:          "true",
			includedQuota: -1,
			wantStatus:    http.StatusCreated,
			wantRunCount:  1,
			wantQueueLen:  1,
		},
		{
			name:          "usage-source exceeded blocked (PG-mode parity, memory service)",
			flag:          "true",
			includedQuota: 10,
			usage: gateUsageSource{rows: []billing.UsageRow{
				{Source: billing.LineSourceRun, Model: "gpt-4o-mini", Runs: 12},
			}},
			wantStatus:    http.StatusPaymentRequired,
			wantErrorCode: billing.ReasonQuotaExceeded,
			wantRunCount:  0,
			wantQueueLen:  0,
			wantAuditDeny: 1,
		},
		{
			name:          "usage-source failure refuses to fake availability",
			flag:          "true",
			includedQuota: 10,
			usage:         gateUsageSource{err: errors.New("aggregate unavailable")},
			wantStatus:    http.StatusInternalServerError,
			wantErrorCode: "internal_error",
			wantRunCount:  0,
			wantQueueLen:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(billing.EnforcementEnvVar, tc.flag)
			savedBilling, savedRuns := billingServiceVar, runsServiceVar
			t.Cleanup(func() { billingServiceVar, runsServiceVar = savedBilling, savedRuns })

			env := newQuotaGateEnv(t, tc.includedQuota, tc.usage, tc.consume)
			billingServiceVar = env.billing
			runsServiceVar = env.runsSvc

			rr := env.postRun(t)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if rr.Code == http.StatusCreated {
				var body map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatalf("response should be valid JSON: %v", err)
				}
				if body["status"] != "queued" {
					t.Fatalf("expected status queued, got %#v", body["status"])
				}
			} else {
				// Structured error envelope contract.
				if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Fatalf("error responses must be application/json, got %q", ct)
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
				if e.Action == "run.quota_denied" {
					denials++
					if e.OrganizationID != env.orgID || e.Metadata["reason"] != billing.ReasonQuotaExceeded {
						t.Fatalf("unexpected denial audit entry: %+v", e)
					}
				}
			}
			if denials != tc.wantAuditDeny {
				t.Fatalf("run.quota_denied audit rows = %d, want %d", denials, tc.wantAuditDeny)
			}
		})
	}
}

// TestCreateRunQuotaEnforcementNotWired pins the fail-closed wiring contract:
// with the flag ON but no billing service assigned, run creation returns 503
// billing_unavailable instead of silently bypassing the quota.
func TestCreateRunQuotaEnforcementNotWired(t *testing.T) {
	t.Setenv(billing.EnforcementEnvVar, "true")
	savedBilling, savedRuns := billingServiceVar, runsServiceVar
	t.Cleanup(func() { billingServiceVar, runsServiceVar = savedBilling, savedRuns })
	billingServiceVar = nil
	runsServiceVar = nil

	env := newQuotaGateEnv(t, -1, nil, 0)
	rr := env.postRun(t)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d, body=%s", rr.Code, http.StatusServiceUnavailable, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("error envelope should be valid JSON: %v", err)
	}
	if body.Error.Code != "billing_unavailable" {
		t.Fatalf("error.code = %q, want billing_unavailable", body.Error.Code)
	}
	if env.queue.Length() != 0 {
		t.Fatal("nothing may be enqueued when the gate refuses")
	}
}

// TestCreateRunQuotaGatePostgresMode exercises the gate against the Postgres
// billing store (sqlmock): the same handler decision (402 quota_exceeded) is
// made from subscription+plan rows plus a usage source, i.e. the production
// wiring shape (migration 016 tables; no new migration involved).
func TestCreateRunQuotaGatePostgresMode(t *testing.T) {
	t.Setenv(billing.EnforcementEnvVar, "true")
	savedBilling, savedRuns := billingServiceVar, runsServiceVar
	t.Cleanup(func() { billingServiceVar, runsServiceVar = savedBilling, savedRuns })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	env := newQuotaGateEnv(t, -1, nil, 0) // no in-memory billing; PG store below
	runsServiceVar = env.runsSvc

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	subRows := sqlmock.NewRows([]string{
		"id", "organization_id", "plan_id", "status", "period_start", "period_end",
		"cancel_at_period_end", "canceled_at", "created_at", "updated_at",
	}).AddRow("sub-1", env.orgID, "plan-1", "active", now, now.Add(30*24*time.Hour), false, nil, now, now)
	planRows := sqlmock.NewRows([]string{
		"id", "name", "price_cents", "currency", "included_quota",
		"COALESCE(metadata::text, '')", "created_at", "updated_at",
	}).AddRow("plan-1", "starter", 1900, "usd", int64(10), "", now, now)

	mock.ExpectQuery(`SELECT .* FROM subscriptions WHERE organization_id = \$1 ORDER BY created_at DESC, id DESC`).
		WithArgs(env.orgID).
		WillReturnRows(subRows)
	mock.ExpectQuery(`SELECT id, name, price_cents, currency, included_quota, COALESCE\(metadata::text, ''\), created_at, updated_at FROM plans WHERE id = \$1`).
		WithArgs("plan-1").
		WillReturnRows(planRows)

	billingServiceVar = billing.NewServiceWithStore(billing.NewPostgresStore(db), gateUsageSource{
		rows: []billing.UsageRow{{Source: billing.LineSourceRun, Model: "gpt-4o-mini", Runs: 11}},
	})

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
	if body.Error.Code != billing.ReasonQuotaExceeded {
		t.Fatalf("error.code = %q, want quota_exceeded", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "included 10 runs, consumed 11 runs") {
		t.Fatalf("message should carry the quota numbers, got %q", body.Error.Message)
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
	// The denial was audited against the caller's org.
	if denials := len(env.auditSvc.List()); denials != 1 {
		t.Fatalf("denial audit rows = %d, want 1", denials)
	}
}
