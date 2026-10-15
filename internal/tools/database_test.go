package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/connectors"
	"agentos/internal/policies"
)

// newMockedDB builds a sqlmock handle plus a provider that always returns it.
func newMockedDB(t *testing.T) (*DatabaseTool, sqlmock.Sqlmock, *MemoryAuditSink, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sink := NewMemoryAuditSink(10)
	tool := NewDatabaseTool(DBConnectionProviderFunc(func(_ context.Context, orgID, name string) (*sql.DB, error) {
		if orgID != "org-1" || name != "warehouse" {
			return nil, fmt.Errorf("unexpected provider lookup org=%q name=%q", orgID, name)
		}
		return db, nil
	})).WithAuditSink(sink)
	return tool, mock, sink, db
}

// dbRows builds a two-column result set (id bigint as string, name text).
func dbRows(values ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "name"})
	for i := 0; i+1 < len(values); i += 2 {
		rows.AddRow(values[i], values[i+1])
	}
	return rows
}

func dbCtx() context.Context {
	return policies.WithRunScope(context.Background(), policies.RunScope{
		OrganizationID: "org-1",
		Environment:    "production",
	})
}

func TestDatabaseToolSuccessAndShape(t *testing.T) {
	tool, mock, sink, _ := newMockedDB(t)
	mock.ExpectPrepare(regexp.QuoteMeta("SELECT id, name FROM customers WHERE region = $1")).
		ExpectQuery().
		WithArgs("emea").
		WillReturnRows(dbRows("1", "Acme", "2", "Globex"))

	result, err := tool.ExecuteContext(dbCtx(), map[string]any{
		"connection": "warehouse",
		"sql":        "SELECT id, name FROM customers WHERE region = $1",
		"params":     []any{"emea"},
	})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	cols, _ := result["columns"].([]string)
	if len(cols) != 2 || cols[0] != "id" || cols[1] != "name" {
		t.Fatalf("unexpected columns: %#v", result["columns"])
	}
	rows, _ := result["rows"].([]map[string]any)
	if len(rows) != 2 || rows[0]["name"] != "Acme" || rows[1]["id"] != "2" {
		t.Fatalf("unexpected rows: %#v", result["rows"])
	}
	if result["row_count"] != 2 || result["truncated"] != false {
		t.Fatalf("unexpected counts: %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}

	// Audit: every query is recorded with the statement hash and the row
	// count; raw parameter values and raw statement text never appear.
	events := sink.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(events))
	}
	meta := events[0].Metadata
	if meta["sql_sha256"] == nil || len(meta["sql_sha256"].(string)) != 64 {
		t.Fatalf("audit metadata must carry the statement sha256, got %#v", meta)
	}
	if meta["row_count"] != 2 || meta["status"] != "ok" {
		t.Fatalf("audit metadata should record row_count/status, got %#v", meta)
	}
	blob, _ := json.Marshal(meta)
	if strings.Contains(string(blob), "emea") || strings.Contains(string(blob), "SELECT id") {
		t.Fatalf("audit metadata leaked query content: %s", blob)
	}
	if events[0].OrganizationID != "org-1" || events[0].Action != "tools."+DatabaseToolName {
		t.Fatalf("audit event not scoped correctly: %#v", events[0])
	}
}

func TestDatabaseToolReadOnlyEnforcement(t *testing.T) {
	tool, mock, _, _ := newMockedDB(t)
	cases := []struct {
		name      string
		sql       string
		readonly  any
		wantError bool
	}{
		{"select default", "SELECT 1", nil, false},
		{"cte", "WITH x AS (SELECT 1) SELECT * FROM x", nil, false},
		{"explain", "EXPLAIN ANALYZE SELECT 1", nil, false},
		{"show", "SHOW timezone", nil, false},
		{"insert blocked by default", "INSERT INTO t VALUES (1)", nil, true},
		{"update blocked by default", "UPDATE t SET a = 1", nil, true},
		{"delete blocked by default", "DELETE FROM t", nil, true},
		{"ddl blocked by default", "CREATE TABLE t (id int)", nil, true},
		{"insert allowed when readonly=false", "INSERT INTO t VALUES ($1)", false, false},
		{"leading comment then select", "-- report\nSELECT 1", nil, false},
		{"leading comment then drop", "-- setup\nDROP TABLE t", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{"connection": "warehouse", "sql": tc.sql}
			if tc.readonly != nil {
				input["readonly"] = tc.readonly
			}
			// Allowed statements reach the database (empty result set);
			// rejected ones must produce NO database traffic at all.
			if !tc.wantError {
				mock.ExpectPrepare(regexp.QuoteMeta(tc.sql)).ExpectQuery().WillReturnRows(dbRows())
			}
			_, err := tool.ExecuteContext(dbCtx(), input)
			if tc.wantError && err == nil {
				t.Fatalf("expected rejection for %q", tc.sql)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.sql, err)
			}
		})
	}
	// The rejected statements must never have reached the database: only the
	// allowed statements above were prepared and queried.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database traffic: %v", err)
	}
}

func TestDatabaseToolBlocksMultipleStatementsAndInjection(t *testing.T) {
	tool, mock, _, _ := newMockedDB(t)
	cases := []struct {
		name string
		sql  string
	}{
		{"stacked dml", "SELECT 1; DROP TABLE customers"},
		{"stacked with trailing semicolon", "SELECT 1; DELETE FROM customers;"},
		{"semicolon inside string does not split, but second stmt does", "SELECT ';' ; DROP TABLE x"},
		{"comment smuggled second statement", "SELECT 1 /*;*/ ; DELETE FROM t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.ExecuteContext(dbCtx(), map[string]any{
				"connection": "warehouse", "sql": tc.sql,
			})
			if err == nil {
				t.Fatalf("expected multiple-statement rejection for %q", tc.sql)
			}
			if !errors.Is(err, ErrStatementInvalid) {
				t.Fatalf("expected ErrStatementInvalid, got %v", err)
			}
		})
	}
	// Nothing reached the database.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database traffic: %v", err)
	}
}

func TestDatabaseToolSemicolonsInsideLiteralsAreFine(t *testing.T) {
	tool, mock, _, _ := newMockedDB(t)
	stmt := "SELECT * FROM notes WHERE body = 'a;b;c' AND tag = \"x;y\" AND body <> $$dollar;semicolon$$"
	mock.ExpectPrepare(regexp.QuoteMeta(stmt)).ExpectQuery().WillReturnRows(dbRows())
	if _, err := tool.ExecuteContext(dbCtx(), map[string]any{
		"connection": "warehouse", "sql": stmt,
	}); err != nil {
		t.Fatalf("literals containing semicolons must not be split: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestDatabaseToolRowLimits(t *testing.T) {
	tool, mock, _, _ := newMockedDB(t)

	// Explicit cap of 2 over 3 available rows -> truncated.
	mock.ExpectPrepare(regexp.QuoteMeta("SELECT 1")).ExpectQuery().
		WillReturnRows(dbRows("1", "a", "2", "b", "3", "c"))
	result, err := tool.ExecuteContext(dbCtx(), map[string]any{
		"connection": "warehouse", "sql": "SELECT 1", "row_limit": float64(2),
	})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	if got := len(result["rows"].([]map[string]any)); got != 2 {
		t.Fatalf("expected 2 rows, got %d", got)
	}
	if result["truncated"] != true {
		t.Fatalf("expected truncated=true, got %#v", result["truncated"])
	}

	// Above the hard max -> clamped to MaxDatabaseRowLimit.
	if got := dbRowLimitArg(float64(5000)); got != MaxDatabaseRowLimit {
		t.Fatalf("row_limit must clamp to %d, got %d", MaxDatabaseRowLimit, got)
	}
	// Missing -> default.
	if got := dbRowLimitArg(nil); got != DefaultDatabaseRowLimit {
		t.Fatalf("row_limit default should be %d, got %d", DefaultDatabaseRowLimit, got)
	}
	// Garbage -> default (never 0, which would return nothing).
	if got := dbRowLimitArg("abc"); got != DefaultDatabaseRowLimit {
		t.Fatalf("garbage row_limit should fall back to default, got %d", got)
	}
}

func TestDatabaseToolDuplicateColumnsSuffixed(t *testing.T) {
	tool, mock, _, _ := newMockedDB(t)
	rows := sqlmock.NewRows([]string{"id", "id"}).AddRow("1", "2")
	mock.ExpectPrepare(regexp.QuoteMeta("SELECT a.id, b.id FROM a JOIN b")).ExpectQuery().WillReturnRows(rows)
	result, err := tool.ExecuteContext(dbCtx(), map[string]any{
		"connection": "warehouse", "sql": "SELECT a.id, b.id FROM a JOIN b",
	})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	row := result["rows"].([]map[string]any)[0]
	if _, ok := row["id"]; !ok {
		t.Fatalf("first duplicate column should keep the base name: %#v", row)
	}
	if _, ok := row["id_2"]; !ok {
		t.Fatalf("second duplicate column should be suffixed _2: %#v", row)
	}
}

func TestDatabaseToolTimeout(t *testing.T) {
	tool, mock, _, _ := newMockedDB(t)
	mock.ExpectPrepare(regexp.QuoteMeta("SELECT pg_sleep(30)")).
		ExpectQuery().
		WillDelayFor(time.Second).
		WillReturnRows(sqlmock.NewRows([]string{"pg_sleep"}).AddRow(""))

	start := time.Now()
	_, err := tool.ExecuteContext(dbCtx(), map[string]any{
		"connection": "warehouse", "sql": "SELECT pg_sleep(30)", "timeout_ms": float64(80),
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout did not bound the call (took %s)", elapsed)
	}
	msg := err.Error()
	if !strings.Contains(msg, "deadline") && !strings.Contains(msg, "cancel") {
		t.Fatalf("expected a context deadline/cancel error, got: %v", err)
	}
}

func TestDatabaseToolContextPropagation(t *testing.T) {
	tool, mock, _, _ := newMockedDB(t)
	ctx, cancel := context.WithCancel(dbCtx())
	cancel() // caller cancellation must be honored before any database work
	if _, err := tool.ExecuteContext(ctx, map[string]any{
		"connection": "warehouse", "sql": "SELECT 1",
	}); err == nil {
		t.Fatal("expected error from a canceled context")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database must not be touched after cancellation: %v", err)
	}
}

func TestDatabaseToolNamedParamsAndValidation(t *testing.T) {
	tool, mock, _, _ := newMockedDB(t)

	// Object params bind in sorted key order, without touching the text.
	mock.ExpectPrepare(regexp.QuoteMeta("SELECT * FROM events WHERE org = $1 AND kind = $2")).
		ExpectQuery().WithArgs("org-1", "login").WillReturnRows(dbRows())
	if _, err := tool.ExecuteContext(dbCtx(), map[string]any{
		"connection": "warehouse",
		"sql":        "SELECT * FROM events WHERE org = $1 AND kind = $2",
		"params":     map[string]any{"2": "login", "1": "org-1"},
	}); err != nil {
		t.Fatalf("object params should bind in sorted key order: %v", err)
	}

	// Scalars are rejected: silent coercion would hide a broken binding.
	if _, err := tool.ExecuteContext(dbCtx(), map[string]any{
		"connection": "warehouse", "sql": "SELECT 1", "params": "42",
	}); !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("expected ErrParamsInvalid for scalar params, got %v", err)
	}

	// Arity mismatch surfaces from the strict prepare (driver-side), which is
	// exactly the no-interpolation guarantee: values can never be pasted in.
	mock.ExpectPrepare(regexp.QuoteMeta("SELECT * FROM t WHERE id = $1")).
		ExpectQuery().WithArgs().
		WillReturnError(fmt.Errorf("expected 1 arguments, got 0"))
	_, err := tool.ExecuteContext(dbCtx(), map[string]any{
		"connection": "warehouse", "sql": "SELECT * FROM t WHERE id = $1",
	})
	if err == nil {
		t.Fatal("expected prepare-time arity error")
	}
}

func TestDatabaseToolFailClosedWithoutScopeOrProvider(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer func() { _ = db.Close() }()

	// No run scope in context -> refused before touching anything.
	tool := NewDatabaseTool(DBConnectionProviderFunc(func(_ context.Context, orgID, name string) (*sql.DB, error) {
		return db, nil
	}))
	if _, err := tool.Execute(map[string]any{"connection": "warehouse", "sql": "SELECT 1"}); err == nil {
		t.Fatal("expected organization-scope refusal without run scope")
	}

	// No provider wired -> registered but fails closed.
	unwired := NewDatabaseTool(nil)
	ctx := dbCtx()
	if _, err := unwired.ExecuteContext(ctx, map[string]any{"connection": "warehouse", "sql": "SELECT 1"}); !errors.Is(err, ErrConnectionRequired) {
		t.Fatalf("expected ErrConnectionRequired, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database traffic: %v", err)
	}
}

func TestDatabaseToolMalformedInput(t *testing.T) {
	tool, _, _, _ := newMockedDB(t)
	cases := []struct {
		name  string
		input map[string]any
	}{
		{"missing connection", map[string]any{"sql": "SELECT 1"}},
		{"missing sql", map[string]any{"connection": "warehouse"}},
		{"empty sql", map[string]any{"connection": "warehouse", "sql": "  "}},
		{"oversized statement", map[string]any{"connection": "warehouse", "sql": strings.Repeat("1", MaxDatabaseStatementBytes+1)}},
		{"unterminated string", map[string]any{"connection": "warehouse", "sql": "SELECT 'oops"}},
		{"unterminated comment", map[string]any{"connection": "warehouse", "sql": "SELECT 1 /* oops"}},
		{"statement is only a comment", map[string]any{"connection": "warehouse", "sql": "-- nothing to see"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.ExecuteContext(dbCtx(), tc.input); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

// connectorForDSN builds an in-memory connectors service with one active
// "warehouse" connector carrying the given secret_ref.
func connectorForDSN(t *testing.T, secretRef, status string) *connectors.Service {
	t.Helper()
	svc := connectors.NewService()
	_, err := svc.Create(context.Background(), "org-1", connectors.CreateInput{
		Name:      "warehouse",
		Type:      connectors.TypeHTTP,
		BaseURL:   "https://db.example.internal",
		SecretRef: secretRef,
		Status:    status,
	}, "tester")
	if err != nil {
		t.Fatalf("connector create failed: %v", err)
	}
	return svc
}

func TestConnectorDBProviderLifecycle(t *testing.T) {
	svc := connectorForDSN(t, "db-dsn", connectors.StatusActive)
	var opens int
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer func() { _ = db.Close() }()

	provider, err := NewConnectorDBProvider(svc,
		connectors.SecretResolverFunc(func(_ context.Context, orgID, name string) (string, error) {
			if orgID != "org-1" || name != "db-dsn" {
				t.Fatalf("unexpected secret resolve org=%q name=%q", orgID, name)
			}
			return "postgres://redacted", nil
		}),
		func(dsn string) (*sql.DB, error) {
			opens++
			if dsn != "postgres://redacted" {
				t.Fatalf("opener received unexpected dsn %q", dsn)
			}
			return db, nil
		})
	if err != nil {
		t.Fatalf("NewConnectorDBProvider failed: %v", err)
	}

	// First call opens; second call reuses the pool (opener not re-invoked).
	for i := 0; i < 2; i++ {
		got, err := provider.Connection(dbCtx(), "org-1", "warehouse")
		if err != nil {
			t.Fatalf("Connection failed (round %d): %v", i, err)
		}
		if got != db {
			t.Fatalf("unexpected handle round %d", i)
		}
	}
	if opens != 1 {
		t.Fatalf("expected the opener to run once (pool caching), ran %d times", opens)
	}

	// Tenant isolation: another org cannot see the connector.
	if _, err := provider.Connection(dbCtx(), "org-2", "warehouse"); !errors.Is(err, ErrConnectionRequired) {
		t.Fatalf("cross-tenant lookup must fail with ErrConnectionRequired, got %v", err)
	}
	_ = mock
}

func TestConnectorDBProviderGating(t *testing.T) {
	disabled := connectorForDSN(t, "db-dsn", connectors.StatusDisabled)
	provider, err := NewConnectorDBProvider(disabled,
		connectors.SecretResolverFunc(func(_ context.Context, _, _ string) (string, error) {
			t.Fatal("secret must not be resolved for a disabled connector")
			return "", nil
		}), nil)
	if err != nil {
		t.Fatalf("NewConnectorDBProvider failed: %v", err)
	}
	if _, err := provider.Connection(dbCtx(), "org-1", "warehouse"); !errors.Is(err, ErrConnectionRequired) {
		t.Fatalf("disabled connector must be refused, got %v", err)
	}

	// Resolver failure surfaces the secret NAME, never a value.
	failing := connectorForDSN(t, "db-dsn", connectors.StatusActive)
	p2, err := NewConnectorDBProvider(failing,
		connectors.SecretResolverFunc(func(_ context.Context, _, _ string) (string, error) {
			return "", errors.New("boom")
		}), nil)
	if err != nil {
		t.Fatalf("NewConnectorDBProvider failed: %v", err)
	}
	_, err = p2.Connection(dbCtx(), "org-1", "warehouse")
	if err == nil || !errors.Is(err, ErrConnectionRequired) {
		t.Fatalf("expected ErrConnectionRequired on resolver failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "db-dsn") || strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("error should name the secret ref but no value: %v", err)
	}

	// No secret_ref on the connector -> refused without resolution.
	plain := connectorForDSN(t, "", connectors.StatusActive)
	p3, err := NewConnectorDBProvider(plain,
		connectors.SecretResolverFunc(func(_ context.Context, _, _ string) (string, error) {
			t.Fatal("secret must not be resolved when secret_ref is empty")
			return "", nil
		}), nil)
	if err != nil {
		t.Fatalf("NewConnectorDBProvider failed: %v", err)
	}
	if _, err := p3.Connection(dbCtx(), "org-1", "warehouse"); !errors.Is(err, ErrConnectionRequired) {
		t.Fatalf("missing secret_ref must be refused, got %v", err)
	}
}

func TestDatabaseToolPolicyGateSurfacesCost(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer func() { _ = db.Close() }()

	var captured policies.EvaluateRequest
	gate := NewToolPolicyGate(policies.EvaluatorFunc(func(_ context.Context, orgID string, req policies.EvaluateRequest) (policies.Decision, error) {
		captured = req
		return policies.Decision{Decision: policies.EffectDeny, Reason: "budget exceeded"}, nil
	}))
	tool := NewDatabaseTool(DBConnectionProviderFunc(func(_ context.Context, _, _ string) (*sql.DB, error) {
		t.Fatal("denied tool must never reach the database")
		return nil, nil
	})).WithPolicyGate(gate)

	_, err = tool.ExecuteContext(dbCtx(), map[string]any{
		"connection": "warehouse", "sql": "SELECT 1", "estimated_cost_cents": float64(77),
	})
	if err == nil || !errors.Is(err, ToolPolicyDenied) {
		t.Fatalf("expected ToolPolicyDenied, got %v", err)
	}
	if captured.Context.EstimatedCostCents != 77 {
		t.Fatalf("gate must surface estimated_cost_cents, got %#v", captured.Context)
	}
	if captured.Action != policies.ActionToolCall || captured.Context.Tool != DatabaseToolName ||
		captured.Resource.Type != policies.ResourceTool || captured.Resource.TenantID != "org-1" {
		t.Fatalf("gate built an unexpected request: %#v", captured)
	}
}
