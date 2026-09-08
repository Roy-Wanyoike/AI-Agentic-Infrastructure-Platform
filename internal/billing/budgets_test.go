package billing

// Budget service tests (wave-8 issue C): CRUD + the one-active-budget-per-
// scope conflict, evaluation per scope (organization + agent), the calendar-
// month period boundary, error propagation and the Postgres store (sqlmock,
// mirroring store_test.go conventions).

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/runs"
)

// stubConsumption feeds deterministic consumption rows (implements
// ConsumptionSource); rows are returned only when the requested window
// matches monthOf (period-boundary tests key on this).
type stubConsumption struct {
	rows    []ConsumptionRow
	err     error
	monthOf time.Time // nil-window behavior: return rows for ANY window when zero
	seenWin [2]time.Time
}

func (s *stubConsumption) ConsumptionForPeriod(_ context.Context, _ string, from, to time.Time) ([]ConsumptionRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.seenWin = [2]time.Time{from, to}
	if !s.monthOf.IsZero() {
		monthFrom, monthTo := monthWindow(s.monthOf)
		if !from.Equal(monthFrom) || !to.Equal(monthTo) {
			return nil, nil // consumption from another month is not visible
		}
	}
	return s.rows, nil
}

func newBudgetTestService(source ConsumptionSource) *BudgetService {
	svc := NewBudgetService()
	if source != nil {
		svc.SetConsumptionSource(source)
	}
	return svc
}

func TestBudgetCRUDAndConflict(t *testing.T) {
	ctx := context.Background()
	svc := newBudgetTestService(nil)

	created, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{
		Scope: BudgetScopeOrganization, LimitRuns: 100, LimitCostCents: 5000, IsActive: true,
	})
	if err != nil {
		t.Fatalf("CreateBudgetCtx returned error: %v", err)
	}
	if created.ID == "" || created.Period != BudgetPeriodMonthly || !created.IsActive {
		t.Fatalf("unexpected created budget: %+v", created)
	}
	if created.Scope != BudgetScopeOrganization || created.RefID != "" {
		t.Fatalf("org budget must keep scope/ref normalized: %+v", created)
	}

	// Second ACTIVE create for the same (scope, ref) is a conflict — the
	// documented "second Create without Update" 409.
	if _, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 1, IsActive: true}); !errors.Is(err, ErrBudgetConflict) {
		t.Fatalf("second active create: want ErrBudgetConflict, got %v", err)
	}
	// An INACTIVE create for the same scope is fine (no active-slot clash).
	if _, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 1, IsActive: false}); err != nil {
		t.Fatalf("inactive create returned error: %v", err)
	}

	// Deactivate via Update, then a fresh create succeeds (slot freed).
	if _, err := svc.UpdateBudgetCtx(ctx, "org-1", created.ID, BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 100, IsActive: false}); err != nil {
		t.Fatalf("UpdateBudgetCtx(deactivate) returned error: %v", err)
	}
	second, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 5, IsActive: true})
	if err != nil {
		t.Fatalf("create after deactivate returned error: %v", err)
	}
	// Re-activating the first while the second is active conflicts...
	if _, err := svc.UpdateBudgetCtx(ctx, "org-1", created.ID, BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 100, IsActive: true}); !errors.Is(err, ErrBudgetConflict) {
		t.Fatalf("cross-budget activation: want ErrBudgetConflict, got %v", err)
	}
	// ...but re-saving an already-active budget never conflicts with itself.
	if _, err := svc.UpdateBudgetCtx(ctx, "org-1", second.ID, BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 7, IsActive: true}); err != nil {
		t.Fatalf("self re-activation returned error: %v", err)
	}

	// List + active filter.
	all, err := svc.ListBudgetsCtx(ctx, "org-1", false)
	if err != nil || len(all) != 3 {
		t.Fatalf("ListBudgetsCtx = %d budgets, err %v", len(all), err)
	}
	active, err := svc.ListBudgetsCtx(ctx, "org-1", true)
	if err != nil || len(active) != 1 || active[0].ID != second.ID {
		t.Fatalf("active filter = %d budgets, want 1 (second)", len(active))
	}

	// Get is tenant-guarded.
	if _, err := svc.GetBudgetCtx(ctx, "org-other", second.ID); !errors.Is(err, ErrBudgetNotFound) {
		t.Fatalf("cross-tenant get: want ErrBudgetNotFound, got %v", err)
	}
	if got, err := svc.GetBudgetCtx(ctx, "org-1", second.ID); err != nil || got.LimitRuns != 7 {
		t.Fatalf("GetBudgetCtx = %+v err %v", got, err)
	}

	// Delete is tenant-guarded and removes the row.
	if err := svc.DeleteBudgetCtx(ctx, "org-other", second.ID); !errors.Is(err, ErrBudgetNotFound) {
		t.Fatalf("cross-tenant delete: want ErrBudgetNotFound, got %v", err)
	}
	if err := svc.DeleteBudgetCtx(ctx, "org-1", second.ID); err != nil {
		t.Fatalf("DeleteBudgetCtx returned error: %v", err)
	}
	if _, err := svc.GetBudgetCtx(ctx, "org-1", second.ID); !errors.Is(err, ErrBudgetNotFound) {
		t.Fatalf("deleted budget still readable: %v", err)
	}
}

func TestBudgetAgentScopeRequiresRefAndExistingAgentIsHTTPLayer(t *testing.T) {
	ctx := context.Background()
	svc := newBudgetTestService(nil)

	if _, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeAgent, RefID: "  ", LimitRuns: 1}); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("agent scope without ref: want ErrInvalidBudget, got %v", err)
	}
	budget, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeAgent, RefID: "agent-1", LimitRuns: 1})
	if err != nil {
		t.Fatalf("agent-scope create returned error: %v", err)
	}
	if budget.RefID != "agent-1" {
		t.Fatalf("ref_id not stored: %+v", budget)
	}
	// The service is pure: agent EXISTENCE is validated at the HTTP layer
	// (billing cannot import agents); here the ref is opaque.
}

func TestBudgetValidation(t *testing.T) {
	ctx := context.Background()
	svc := newBudgetTestService(nil)

	cases := []struct {
		name string
		in   BudgetInput
	}{
		{"unknown scope", BudgetInput{Scope: "team", LimitRuns: 1}},
		{"negative runs", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: -1}},
		{"negative cost", BudgetInput{Scope: BudgetScopeOrganization, LimitCostCents: -5}},
		{"both limits unlimited", BudgetInput{Scope: BudgetScopeOrganization}},
		{"bad period", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 1, Period: "weekly"}},
	}
	for _, tc := range cases {
		if _, err := svc.CreateBudgetCtx(ctx, "org-1", tc.in); !errors.Is(err, ErrInvalidBudget) {
			t.Fatalf("%s: want ErrInvalidBudget, got %v", tc.name, err)
		}
	}
	// Empty period normalizes to monthly.
	b, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 1})
	if err != nil || b.Period != BudgetPeriodMonthly {
		t.Fatalf("empty period should default to monthly: %+v err %v", b, err)
	}
}

func TestEvaluateBudgetOrgScope(t *testing.T) {
	ctx := context.Background()
	source := &stubConsumption{rows: []ConsumptionRow{
		{AgentID: "agent-a", Runs: 8, CostCents: 300.4},
		{AgentID: "agent-b", Runs: 4, CostCents: 199.7},
	}}
	svc := newBudgetTestService(source)

	// Runs budget: consumed 12, projected 1 -> 13 > 12 denies.
	if _, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 12, IsActive: true}); err != nil {
		t.Fatalf("seed budget: %v", err)
	}
	decision, err := svc.EvaluateBudget(ctx, "org-1", "agent-a", 1, 0)
	if err != nil {
		t.Fatalf("EvaluateBudget returned error: %v", err)
	}
	if decision.Allowed || decision.Reason != ReasonBudgetExceeded || decision.DeniedScope != BudgetScopeOrganization {
		t.Fatalf("expected org-scope denial, got %+v", decision)
	}
	if decision.DeniedBudgetID == "" || decision.Message == "" {
		t.Fatalf("denial must carry budget id + message: %+v", decision)
	}

	// Cost budget: consumed 500.1 -> rounds to 500 (margin convention: round
	// the window total once); projected 1 -> 501 > 500 denies.
	svc2 := newBudgetTestService(source)
	if _, err := svc2.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitCostCents: 500, IsActive: true}); err != nil {
		t.Fatalf("seed cost budget: %v", err)
	}
	decision, err = svc2.EvaluateBudget(ctx, "org-1", "", 0, 1)
	if err != nil {
		t.Fatalf("EvaluateBudget(cost) returned error: %v", err)
	}
	if decision.Allowed {
		t.Fatalf("expected cost denial (500 + 1 > 500), got %+v", decision)
	}
	// Boundary: consumed == limit is still allowed (mirrors quotaFor).
	decision, err = svc2.EvaluateBudget(ctx, "org-1", "", 0, 0)
	if err != nil || !decision.Allowed {
		t.Fatalf("consumed == limit must stay allowed: %+v err %v", decision, err)
	}

	// Exactly at the run limit with no projection -> allowed.
	svc3 := newBudgetTestService(source)
	if _, err := svc3.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 12, IsActive: true}); err != nil {
		t.Fatalf("seed budget: %v", err)
	}
	if decision, err = svc3.EvaluateBudget(ctx, "org-1", "", 0, 0); err != nil || !decision.Allowed {
		t.Fatalf("zero projection at limit must be allowed: %+v err %v", decision, err)
	}
}

func TestEvaluateBudgetAgentScope(t *testing.T) {
	ctx := context.Background()
	source := &stubConsumption{rows: []ConsumptionRow{
		{AgentID: "agent-a", Runs: 10, CostCents: 100},
		{AgentID: "agent-b", Runs: 2, CostCents: 20},
	}}
	svc := newBudgetTestService(source)
	if _, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeAgent, RefID: "agent-a", LimitRuns: 10, IsActive: true}); err != nil {
		t.Fatalf("seed agent budget: %v", err)
	}

	// agent-a at its limit: one more run denies.
	decision, err := svc.EvaluateBudget(ctx, "org-1", "agent-a", 1, 0)
	if err != nil {
		t.Fatalf("EvaluateBudget returned error: %v", err)
	}
	if decision.Allowed || decision.DeniedScope != BudgetScopeAgent {
		t.Fatalf("expected agent-scope denial, got %+v", decision)
	}
	// agent-b is unaffected (its consumption is 2 << 10 and no budget covers it).
	if decision, err = svc.EvaluateBudget(ctx, "org-1", "agent-b", 5, 0); err != nil || !decision.Allowed {
		t.Fatalf("agent-b must be unaffected: %+v err %v", decision, err)
	}
	// Empty agentID matches only org budgets — none here.
	if decision, err = svc.EvaluateBudget(ctx, "org-1", "", 1, 0); err != nil || !decision.Allowed {
		t.Fatalf("empty agentID must skip agent budgets: %+v err %v", decision, err)
	}
	// A foreign budget's agent scope never matches another agent.
	if decision, err = svc.EvaluateBudget(ctx, "org-1", "agent-z", 1, 0); err != nil || !decision.Allowed {
		t.Fatalf("unbudgeted agent must pass: %+v err %v", decision, err)
	}
}

func TestEvaluateBudgetDecisionOrderIsDeterministic(t *testing.T) {
	ctx := context.Background()
	source := &stubConsumption{rows: []ConsumptionRow{
		{AgentID: "agent-a", Runs: 50, CostCents: 0},
	}}
	svc := newBudgetTestService(source)
	orgBudget, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 10, IsActive: true})
	if err != nil {
		t.Fatalf("seed org budget: %v", err)
	}
	if _, err = svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeAgent, RefID: "agent-a", LimitRuns: 5, IsActive: true}); err != nil {
		t.Fatalf("seed agent budget: %v", err)
	}
	// Both would deny; the ORG budget wins (organization evaluated first).
	decision, err := svc.EvaluateBudget(ctx, "org-1", "agent-a", 1, 0)
	if err != nil {
		t.Fatalf("EvaluateBudget returned error: %v", err)
	}
	if decision.DeniedBudgetID != orgBudget.ID || decision.DeniedScope != BudgetScopeOrganization {
		t.Fatalf("org budget must win the tie: %+v", decision)
	}
}

func TestEvaluateBudgetPeriodBoundary(t *testing.T) {
	ctx := context.Background()
	// The metered month is February 2026.
	source := &stubConsumption{monthOf: time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC), rows: []ConsumptionRow{
		{AgentID: "agent-a", Runs: 10, CostCents: 0},
	}}
	svc := newBudgetTestService(source)
	if _, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 10, IsActive: true}); err != nil {
		t.Fatalf("seed budget: %v", err)
	}

	// Feb 28 23:59 UTC — same calendar month: denial (10 + 1 > 10).
	svc.SetClock(func() time.Time { return time.Date(2026, 2, 28, 23, 59, 0, 0, time.UTC) })
	decision, err := svc.EvaluateBudget(ctx, "org-1", "", 1, 0)
	if err != nil || decision.Allowed {
		t.Fatalf("end-of-month denial expected: %+v err %v", decision, err)
	}

	// Mar 1 00:00 UTC — the month rolled over: February's consumption is out
	// of the window and the SAME budget allows the run again.
	svc.SetClock(func() time.Time { return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) })
	decision, err = svc.EvaluateBudget(ctx, "org-1", "", 1, 0)
	if err != nil || !decision.Allowed {
		t.Fatalf("month rollover must reset consumption: %+v err %v", decision, err)
	}
	// The lookup window was the March calendar month.
	monthFrom, monthTo := monthWindow(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if !source.seenWin[0].Equal(monthFrom) || !source.seenWin[1].Equal(monthTo) {
		t.Fatalf("consumption window = %v..%v, want %v..%v", source.seenWin[0], source.seenWin[1], monthFrom, monthTo)
	}
}

func TestEvaluateBudgetConsumptionFailurePropagates(t *testing.T) {
	ctx := context.Background()
	svc := newBudgetTestService(&stubConsumption{err: errors.New("ledger unavailable")})
	if _, err := svc.CreateBudgetCtx(ctx, "org-1", BudgetInput{Scope: BudgetScopeOrganization, LimitRuns: 1, IsActive: true}); err != nil {
		t.Fatalf("seed budget: %v", err)
	}
	if _, err := svc.EvaluateBudget(ctx, "org-1", "", 1, 0); err == nil {
		t.Fatal("consumption-source failure must propagate (never fake availability)")
	}
	// No budget rows -> allowed without touching the (failing) source.
	svc2 := newBudgetTestService(&stubConsumption{err: errors.New("ledger unavailable")})
	if d, err := svc2.EvaluateBudget(ctx, "org-1", "", 1, 0); err != nil || !d.Allowed {
		t.Fatalf("no budgets means allowed without a ledger read: %+v err %v", d, err)
	}
}

func TestEvaluateBudgetNoBudgetsAllowed(t *testing.T) {
	svc := newBudgetTestService(nil)
	d, err := svc.EvaluateBudget(context.Background(), "org-1", "agent-a", 1, 999999)
	if err != nil || !d.Allowed || d.Reason != "" {
		t.Fatalf("no budgets -> allowed: %+v err %v", d, err)
	}
}

// ---------------------------------------------------------------------------
// Postgres store (sqlmock)
// ---------------------------------------------------------------------------

func newMockBudgetStore(t *testing.T) (sqlmock.Sqlmock, BudgetStore) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return mock, NewBudgetPostgresStore(db)
}

func budgetSelectRow(id, orgID string, active bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "scope", "ref_id", "period", "limit_runs",
		"limit_cost_cents", "is_active", "created_at", "updated_at",
	}).AddRow(id, orgID, "agent", "agent-1", "monthly", 100, 5000, active, tsA, tsA)
}

func TestBudgetPostgresStoreCRUD(t *testing.T) {
	mock, store := newMockBudgetStore(t)
	ctx := context.Background()

	budget := &Budget{
		ID: "bud-1", OrganizationID: "org-1", Scope: BudgetScopeAgent, RefID: "agent-1",
		Period: BudgetPeriodMonthly, LimitRuns: 100, LimitCostCents: 5000,
		IsActive: true, CreatedAt: tsA, UpdatedAt: tsA,
	}
	mock.ExpectExec(`INSERT INTO budgets`).
		WithArgs("bud-1", "org-1", "agent", "agent-1", "monthly", int64(100), int64(5000), true, tsA, tsA).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateBudget(ctx, budget); err != nil {
		t.Fatalf("CreateBudget returned error: %v", err)
	}

	mock.ExpectQuery(`SELECT id, organization_id, scope, ref_id, period, limit_runs, limit_cost_cents, is_active, created_at, updated_at FROM budgets WHERE organization_id = \$1 ORDER BY created_at ASC, id ASC`).
		WithArgs("org-1").
		WillReturnRows(budgetSelectRow("bud-1", "org-1", true))
	budgets, err := store.ListBudgets(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListBudgets returned error: %v", err)
	}
	if len(budgets) != 1 || budgets[0].ID != "bud-1" || budgets[0].Scope != BudgetScopeAgent || !budgets[0].IsActive {
		t.Fatalf("unexpected list: %+v", budgets)
	}

	// Tenant guard: the read carries organization_id and unknown/foreign rows
	// map to ErrBudgetNotFound.
	mock.ExpectQuery(`SELECT .* FROM budgets WHERE id = \$1 AND organization_id = \$2`).
		WithArgs("bud-missing", "org-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetBudget(ctx, "org-1", "bud-missing"); !errors.Is(err, ErrBudgetNotFound) {
		t.Fatalf("GetBudget missing: want ErrBudgetNotFound, got %v", err)
	}

	// Update: zero rows affected -> ErrBudgetNotFound.
	mock.ExpectExec(`UPDATE budgets SET`).
		WithArgs("bud-x", "agent", "agent-1", "monthly", int64(1), int64(0), true, tsA, "org-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateBudget(ctx, &Budget{ID: "bud-x", OrganizationID: "org-1", Scope: BudgetScopeAgent, RefID: "agent-1", Period: BudgetPeriodMonthly, LimitRuns: 1, IsActive: true, UpdatedAt: tsA}); !errors.Is(err, ErrBudgetNotFound) {
		t.Fatalf("UpdateBudget zero rows: want ErrBudgetNotFound, got %v", err)
	}

	// Delete: zero rows affected -> ErrBudgetNotFound; happy path deletes.
	mock.ExpectExec(`DELETE FROM budgets WHERE id = \$1 AND organization_id = \$2`).
		WithArgs("bud-x", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeleteBudget(ctx, "org-1", "bud-x"); !errors.Is(err, ErrBudgetNotFound) {
		t.Fatalf("DeleteBudget zero rows: want ErrBudgetNotFound, got %v", err)
	}
	mock.ExpectExec(`DELETE FROM budgets WHERE id = \$1 AND organization_id = \$2`).
		WithArgs("bud-1", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.DeleteBudget(ctx, "org-1", "bud-1"); err != nil {
		t.Fatalf("DeleteBudget returned error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestBudgetPostgresStoreNilDBGuard(t *testing.T) {
	store := NewBudgetPostgresStore(nil)
	ctx := context.Background()
	if err := store.CreateBudget(ctx, &Budget{}); err == nil {
		t.Fatal("CreateBudget on nil db should fail")
	}
	if _, err := store.GetBudget(ctx, "org-1", "bud-1"); err == nil {
		t.Fatal("GetBudget on nil db should fail")
	}
	if err := store.DeleteBudget(ctx, "org-1", "bud-1"); err == nil {
		t.Fatal("DeleteBudget on nil db should fail")
	}
}

// ---------------------------------------------------------------------------
// RunsConsumptionSource adapter
// ---------------------------------------------------------------------------

type stubCostAggregator struct {
	buckets []runs.CostBucket
	orgID   string
	groupBy runs.CostGroupBy
	err     error
}

func (s *stubCostAggregator) AggregateCostsCtx(_ context.Context, orgID string, _, _ time.Time, groupBy runs.CostGroupBy) ([]runs.CostBucket, float64, error) {
	if s.err != nil {
		return nil, 0, s.err
	}
	s.orgID, s.groupBy = orgID, groupBy
	return s.buckets, 0, nil
}

func TestRunsConsumptionSourceMapsAgentBuckets(t *testing.T) {
	agg := &stubCostAggregator{buckets: []runs.CostBucket{
		{AgentID: "agent-a", Runs: 3, CostCents: 12.5},
		{AgentID: "agent-b", Runs: 1, CostCents: 2},
	}}
	source := NewRunsConsumptionSource(agg)
	rows, err := source.ConsumptionForPeriod(context.Background(), "org-1", tsA, tsB)
	if err != nil {
		t.Fatalf("ConsumptionForPeriod returned error: %v", err)
	}
	if len(rows) != 2 || rows[0].AgentID != "agent-a" || rows[0].Runs != 3 || rows[0].CostCents != 12.5 {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if agg.groupBy != runs.CostGroupByAgent || agg.orgID != "org-1" {
		t.Fatalf("adapter must aggregate by agent for the caller's org: %+v", agg)
	}
	// Nil-dependency contract: a nil adapter/nil aggregator contributes zero.
	var nilSource *RunsConsumptionSource
	if rows, err := nilSource.ConsumptionForPeriod(context.Background(), "org-1", tsA, tsB); err != nil || rows != nil {
		t.Fatalf("nil source must be zero-contribution: %+v err %v", rows, err)
	}
	if rows, err := NewRunsConsumptionSource(nil).ConsumptionForPeriod(context.Background(), "org-1", tsA, tsB); err != nil || rows != nil {
		t.Fatalf("nil aggregator must be zero-contribution: %+v err %v", rows, err)
	}
}

func TestMonthWindow(t *testing.T) {
	from, to := monthWindow(time.Date(2026, 2, 17, 8, 30, 0, 0, time.UTC))
	if from != time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) || to != time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("monthWindow = %v..%v", from, to)
	}
	// Year boundary.
	from, to = monthWindow(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
	if from != time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) || to != time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("year-boundary monthWindow = %v..%v", from, to)
	}
}
