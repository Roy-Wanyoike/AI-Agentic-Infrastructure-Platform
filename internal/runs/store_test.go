package runs

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockStore(t *testing.T) (*pgStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return &pgStore{db: db}, mock, func() { _ = db.Close() }
}

func TestPGInsertStepPersistsCostAndBumpsRunTotal(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	step := &Step{
		ID:         "step-1",
		RunID:      "run-1",
		StepType:   "model",
		Status:     "SUCCEEDED",
		TokenUsage: map[string]any{"prompt_tokens": 1000.0},
		Cost:       0.75,
		CreatedAt:  now,
	}
	// Atomic CTE: the step row (cost + cost_cents) and the runs.cost_cents
	// bump are one statement; affected = 1 (step) + 1 (bump) = 2.
	// jsonParam marshals the typed-nil InputMeta/OutputMeta maps to the
	// JSON string "null" (pre-existing pgStore behavior: jsonb 'null').
	mock.ExpectQuery(regexp.QuoteMeta(sqlInsertRunStep)).
		WithArgs(step.ID, step.RunID, step.StepType, step.Status,
			"null", "null", "",
			`{"prompt_tokens":1000}`, 0.75,
			nil, nil, now,
			"org-1").
		WillReturnRows(sqlmock.NewRows([]string{"affected"}).AddRow(2))

	if err := store.InsertStep(context.Background(), "org-1", step); err != nil {
		t.Fatalf("InsertStep returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGInsertStepTenantGuardRejected(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	step := &Step{ID: "step-1", RunID: "run-foreign", StepType: "model", Status: "SUCCEEDED", Cost: 0.75, CreatedAt: time.Now().UTC()}
	// affected = 0: the WHERE EXISTS guard rejected the write.
	mock.ExpectQuery(regexp.QuoteMeta(sqlInsertRunStep)).
		WillReturnRows(sqlmock.NewRows([]string{"affected"}).AddRow(0))

	if err := store.InsertStep(context.Background(), "org-2", step); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign-run step insert should map to ErrRunNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGGetRunScansCostTotal(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	created := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	rows := sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "input", "output", "status", "cost_cents", "created_at", "updated_at"}).
		AddRow("run-1", "org-1", "agent-1", "hi", "ho", "RUNNING", 1.25, created, updated)
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectRunScoped)).
		WithArgs("run-1", "org-1").
		WillReturnRows(rows)

	run, err := store.GetRun(context.Background(), "org-1", "run-1")
	if err != nil {
		t.Fatalf("GetRun returned error: %v", err)
	}
	if run.TotalCostCents != 1.25 {
		t.Fatalf("run total cost should be scanned, got %v", run.TotalCostCents)
	}
	if run.Status != RunStatus("RUNNING") {
		t.Fatalf("unexpected status: %q", run.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGAggregateCostsByDay(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"bucket", "sum", "count"}).
		AddRow("2026-09-01", 0.05, 2).
		AddRow("2026-09-02", 1.30, 5)
	// Tenant guard: WHERE r.organization_id = $1 AND created_at window.
	mock.ExpectQuery(regexp.QuoteMeta(sqlAggregateCostsByDay)).
		WithArgs("org-1", from, to).
		WillReturnRows(rows)

	series, err := store.AggregateCosts(context.Background(), "org-1", from, to, CostGroupByDay)
	if err != nil {
		t.Fatalf("AggregateCosts returned error: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 day buckets, got %d", len(series))
	}
	if series[0].Bucket != "2026-09-01" || series[0].CostCents != 0.05 || series[0].Runs != 2 {
		t.Fatalf("unexpected first bucket: %+v", series[0])
	}
	if series[1].Bucket != "2026-09-02" || series[1].CostCents != 1.30 || series[1].Runs != 5 {
		t.Fatalf("unexpected second bucket: %+v", series[1])
	}
	if series[0].AgentID != "" || series[0].Model != "" {
		t.Fatalf("day buckets must not carry agent/model labels: %+v", series[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGAggregateCostsByAgent(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"agent_id", "sum", "count"}).
		AddRow("agent-1", 2.50, 7).
		AddRow("agent-2", 0.0, 3)
	mock.ExpectQuery(regexp.QuoteMeta(sqlAggregateCostsByAgent)).
		WithArgs("org-1", from, to).
		WillReturnRows(rows)

	series, err := store.AggregateCosts(context.Background(), "org-1", from, to, CostGroupByAgent)
	if err != nil {
		t.Fatalf("AggregateCosts returned error: %v", err)
	}
	if len(series) != 2 || series[0].AgentID != "agent-1" || series[0].CostCents != 2.50 || series[0].Runs != 7 {
		t.Fatalf("unexpected agent buckets: %+v", series)
	}
	if series[0].Bucket != "" || series[0].Model != "" {
		t.Fatalf("agent buckets must not carry day/model labels: %+v", series[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGAggregateCostsByModel(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"model", "sum", "count"}).
		AddRow("gpt-4o", 3.20, 4).
		AddRow("", 0.0, 1) // deleted agent: empty model label still aggregates
	mock.ExpectQuery(regexp.QuoteMeta(sqlAggregateCostsByModel)).
		WithArgs("org-1", from, to).
		WillReturnRows(rows)

	series, err := store.AggregateCosts(context.Background(), "org-1", from, to, CostGroupByModel)
	if err != nil {
		t.Fatalf("AggregateCosts returned error: %v", err)
	}
	if len(series) != 2 || series[0].Model != "gpt-4o" || series[0].CostCents != 3.20 || series[0].Runs != 4 {
		t.Fatalf("unexpected model buckets: %+v", series)
	}
	if series[1].Model != "" || series[1].Runs != 1 {
		t.Fatalf("deleted agents should aggregate under the empty model label: %+v", series[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGAggregateCostsInvalidGroupBy(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	if _, err := store.AggregateCosts(context.Background(), "org-1", time.Now(), time.Now().Add(time.Hour), CostGroupBy("week")); !errors.Is(err, ErrInvalidGroupBy) {
		t.Fatalf("unknown group_by should map to ErrInvalidGroupBy, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no query should run for an invalid grouping: %v", err)
	}
}
