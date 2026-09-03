package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/auth"
	"agentos/internal/runs"
)

// usgTestEnv bundles the route under test with a real in-memory auth service
// and a sqlmock-backed runs service (the endpoint is store-driven).
type usgTestEnv struct {
	mux     *http.ServeMux
	authSvc *auth.Service
	runsSvc *runs.Service
	mock    sqlmock.Sqlmock
	orgID   string
	token   string
	closeDB func()
}

func newUsgTestEnv(t *testing.T) *usgTestEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	authSvc := auth.NewService("test-secret")
	org, user, err := authSvc.RegisterCtx(context.Background(), "Acme", "owner@acme.test", "password123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err := authSvc.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	runsSvc := runs.NewServiceWithStore(runs.NewPostgresStore(db))
	mux := http.NewServeMux()
	registerUsageCostsRoutes(mux, runsSvc, authSvc, nil)
	return &usgTestEnv{
		mux: mux, authSvc: authSvc, runsSvc: runsSvc, mock: mock,
		orgID: org.ID, token: token, closeDB: func() { _ = db.Close() },
	}
}

func (e *usgTestEnv) get(t *testing.T, query string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	url := "/usage/costs"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	body := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response body is not JSON: %v (%q)", err, rec.Body.String())
		}
	}
	return rec, body
}

func usgErrCode(t *testing.T, body map[string]any) string {
	t.Helper()
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error envelope, got %v", body)
	}
	code, _ := errObj["code"].(string)
	return code
}

func TestUsageCostsGroupByDay(t *testing.T) {
	env := newUsgTestEnv(t)
	defer env.closeDB()

	from := "2026-09-01T00:00:00Z"
	to := "2026-09-04T00:00:00Z"
	// Tenant guard + window: the aggregate query must run with the caller's
	// organization_id and the half-open [from, to) window.
	env.mock.ExpectQuery("to_char\\(r\\.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'\\)").
		WithArgs(env.orgID, fromTime(t, from), fromTime(t, to)).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "sum", "count"}).
			AddRow("2026-09-02", 1.5, 3).
			AddRow("2026-09-03", 0.25, 1))

	rec, body := env.get(t, "from="+from+"&to="+to+"&group_by=day")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", rec.Code, body)
	}
	if got, ok := body["total_cost_cents"].(float64); !ok || got != 1.75 {
		t.Fatalf("total_cost_cents should be the series sum (1.75), got %v", body["total_cost_cents"])
	}
	series, ok := body["series"].([]any)
	if !ok || len(series) != 2 {
		t.Fatalf("expected 2 series entries, got %v", body["series"])
	}
	first, _ := series[0].(map[string]any)
	if first["bucket"] != "2026-09-02" || first["cost_cents"] != 1.5 || first["runs"] != 3.0 {
		t.Fatalf("unexpected first bucket: %v", first)
	}
	if _, present := first["agent_id"]; present {
		t.Fatalf("day buckets must not carry agent_id: %v", first)
	}
	if _, present := first["model"]; present {
		t.Fatalf("day buckets must not carry model: %v", first)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestUsageCostsGroupByAgent(t *testing.T) {
	env := newUsgTestEnv(t)
	defer env.closeDB()

	env.mock.ExpectQuery("GROUP BY r\\.agent_id").
		WithArgs(env.orgID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"agent_id", "sum", "count"}).
			AddRow("agent-1", 2.0, 4).
			AddRow("agent-2", 0.0, 2))

	rec, body := env.get(t, "group_by=agent")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", rec.Code, body)
	}
	series, _ := body["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("expected 2 agent buckets, got %v", body["series"])
	}
	first, _ := series[0].(map[string]any)
	if first["agent_id"] != "agent-1" || first["cost_cents"] != 2.0 || first["runs"] != 4.0 {
		t.Fatalf("unexpected agent bucket: %v", first)
	}
	if _, present := first["bucket"]; present {
		t.Fatalf("agent buckets must not carry bucket: %v", first)
	}
	if got, ok := body["total_cost_cents"].(float64); !ok || got != 2.0 {
		t.Fatalf("total should be 2.0, got %v", body["total_cost_cents"])
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestUsageCostsGroupByModel(t *testing.T) {
	env := newUsgTestEnv(t)
	defer env.closeDB()

	env.mock.ExpectQuery("LEFT JOIN agents a ON a\\.id = r\\.agent_id").
		WithArgs(env.orgID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"model", "sum", "count"}).
			AddRow("gpt-4o", 3.75, 5))

	rec, body := env.get(t, "group_by=model")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", rec.Code, body)
	}
	series, _ := body["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("expected 1 model bucket, got %v", body["series"])
	}
	first, _ := series[0].(map[string]any)
	if first["model"] != "gpt-4o" || first["cost_cents"] != 3.75 || first["runs"] != 5.0 {
		t.Fatalf("unexpected model bucket: %v", first)
	}
	if _, present := first["bucket"]; present {
		t.Fatalf("model buckets must not carry bucket: %v", first)
	}
	if _, present := first["agent_id"]; present {
		t.Fatalf("model buckets must not carry agent_id: %v", first)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestUsageCostsEmptySeries(t *testing.T) {
	env := newUsgTestEnv(t)
	defer env.closeDB()

	env.mock.ExpectQuery("to_char\\(r\\.created_at").
		WithArgs(env.orgID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "sum", "count"}))

	rec, body := env.get(t, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", rec.Code, body)
	}
	// series must serialize as an empty array, never null.
	series, ok := body["series"].([]any)
	if !ok || len(series) != 0 {
		t.Fatalf("series should be an empty array, got %v", body["series"])
	}
	if got, ok := body["total_cost_cents"].(float64); !ok || got != 0 {
		t.Fatalf("total_cost_cents should be 0, got %v", body["total_cost_cents"])
	}
}

func TestUsageCostsInvalidGroupBy(t *testing.T) {
	env := newUsgTestEnv(t)
	defer env.closeDB()

	rec, body := env.get(t, "group_by=week")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %v", rec.Code, body)
	}
	if code := usgErrCode(t, body); code != "INVALID_GROUP_BY" {
		t.Fatalf("expected INVALID_GROUP_BY, got %q", code)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no query should run for an invalid grouping: %v", err)
	}
}

func TestUsageCostsInvalidTimeRange(t *testing.T) {
	env := newUsgTestEnv(t)
	defer env.closeDB()

	cases := []struct {
		name  string
		query string
	}{
		{"bad from", "from=not-a-date"},
		{"bad to", "to=13/45/2026"},
		{"from after to", "from=2026-09-04T00:00:00Z&to=2026-09-01T00:00:00Z"},
		{"from equals to", "from=2026-09-01T00:00:00Z&to=2026-09-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := env.get(t, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %v", rec.Code, body)
			}
			if code := usgErrCode(t, body); code != "INVALID_TIME_RANGE" {
				t.Fatalf("expected INVALID_TIME_RANGE, got %q", code)
			}
		})
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no query should run for invalid windows: %v", err)
	}
}

func TestUsageCostsAcceptsBareDates(t *testing.T) {
	env := newUsgTestEnv(t)
	defer env.closeDB()

	env.mock.ExpectQuery("to_char\\(r\\.created_at").
		WithArgs(env.orgID, fromTime(t, "2026-09-01T00:00:00Z"), fromTime(t, "2026-09-04T00:00:00Z")).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "sum", "count"}).AddRow("2026-09-02", 1.0, 1))

	rec, body := env.get(t, "from=2026-09-01&to=2026-09-04")
	if rec.Code != http.StatusOK {
		t.Fatalf("bare YYYY-MM-DD window should be accepted, got %d: %v", rec.Code, body)
	}
	if got, ok := body["total_cost_cents"].(float64); !ok || got != 1.0 {
		t.Fatalf("unexpected total: %v", body["total_cost_cents"])
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestUsageCostsRequiresAuth(t *testing.T) {
	env := newUsgTestEnv(t)
	defer env.closeDB()

	// No credentials: 401 before any store interaction. The auth
	// middleware answers 401 with a plain-text body (platform-wide
	// behavior), so this path skips the JSON envelope parsing.
	req := httptest.NewRequest(http.MethodGet, "/usage/costs", nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request should be 401, got %d", rec.Code)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no query should run without credentials: %v", err)
	}
}

func TestUsageCostsAuthenticatedRequestReachesStore(t *testing.T) {
	env := newUsgTestEnv(t)
	defer env.closeDB()

	// The registered OWNER holds usage.read; the request must reach the
	// tenant-scoped aggregate query (org id from the claims, never the client).
	env.mock.ExpectQuery("to_char\\(r\\.created_at").
		WithArgs(env.orgID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "sum", "count"}))
	rec, body := env.get(t, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated request should reach the aggregation, got %d: %v", rec.Code, body)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

// fromTime parses an RFC3339 string the way the handler does; used to assert
// exact window args in sqlmock expectations.
func fromTime(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		if parsed2, err2 := time.Parse("2006-01-02", raw); err2 == nil {
			return parsed2.UTC()
		}
		t.Fatalf("fromTime(%q) failed: %v", raw, err)
	}
	return parsed.UTC()
}
