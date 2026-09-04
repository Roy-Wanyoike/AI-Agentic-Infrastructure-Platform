package evaluations

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockDB(t *testing.T) (*pgStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return &pgStore{db: db}, mock, func() { _ = db.Close() }
}

func testDataset() *Dataset {
	return &Dataset{
		ID:             "ds-1",
		OrganizationID: "org-1",
		Name:           "Demo",
		Description:    "demo dataset",
		CaseCount:      2,
		Cases: []Case{
			{ID: "c1", Input: "q1", Expected: "a1", Scorer: ScorerExact},
			{ID: "c2", Input: "q2", Expected: "^ok$", Scorer: ScorerRegex, Params: Params{Pattern: "^ok$"}},
		},
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestPGCreateDataset(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	ds := testDataset()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertDataset)).
		WithArgs(ds.ID, ds.OrganizationID, ds.Name, ds.Description, ds.CreatedAt, ds.UpdatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	for i, c := range ds.Cases {
		mock.ExpectExec(regexp.QuoteMeta(sqlInsertCase)).
			WithArgs(ds.ID, c.ID, ds.OrganizationID, i, c.Input, c.Expected, string(c.Scorer), paramsParam(c.Params), ds.CreatedAt).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()

	if err := store.CreateDataset(context.Background(), ds); err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGCreateDatasetRollsBackOnCaseError(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	ds := testDataset()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertDataset)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertCase)).
		WillReturnError(errors.New("case insert failed"))
	mock.ExpectRollback()

	if err := store.CreateDataset(context.Background(), ds); err == nil {
		t.Fatal("case insert failure should abort dataset creation")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGGetDatasetScoped(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	ds := testDataset()
	rows := sqlmock.NewRows([]string{"id", "organization_id", "name", "description", "created_at", "updated_at"}).
		AddRow(ds.ID, ds.OrganizationID, ds.Name, ds.Description, ds.CreatedAt, ds.UpdatedAt)
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, organization_id, name, description, created_at, updated_at FROM eval_datasets WHERE id = $1 AND organization_id = $2`)).
		WithArgs(ds.ID, "org-1").
		WillReturnRows(rows)

	got, err := store.GetDataset(context.Background(), "org-1", ds.ID)
	if err != nil {
		t.Fatalf("GetDataset returned error: %v", err)
	}
	if got.Name != ds.Name || got.OrganizationID != "org-1" {
		t.Fatalf("unexpected dataset: %+v", got)
	}

	mock.ExpectQuery("FROM eval_datasets").
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "name", "description", "created_at", "updated_at"}))
	if _, err := store.GetDataset(context.Background(), "org-1", "missing"); !errors.Is(err, ErrDatasetNotFound) {
		t.Fatalf("missing dataset should map to ErrDatasetNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGListDatasetsWithCaseCount(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "organization_id", "name", "description", "created_at", "updated_at", "case_count"}).
		AddRow("ds-1", "org-1", "One", "", now, now, 3).
		AddRow("ds-2", "org-1", "Two", "", now, now, 0)
	// Tenant guard: WHERE d.organization_id = $1
	mock.ExpectQuery("FROM eval_datasets d WHERE d.organization_id = \\$1").
		WithArgs("org-1").
		WillReturnRows(rows)

	got, err := store.ListDatasets(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListDatasets returned error: %v", err)
	}
	if len(got) != 2 || got[0].CaseCount != 3 || got[1].CaseCount != 0 {
		t.Fatalf("unexpected listing: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGGetDatasetCases(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	rows := sqlmock.NewRows([]string{"case_id", "input", "expected", "scorer", "params", "position"}).
		AddRow("c1", "q1", "a1", "exact", "", 0).
		AddRow("c2", "q2", "", "latency_under_ms", `{"threshold_ms":1500}`, 1).
		AddRow("c3", "q3", "", "regex", `{"pattern":"^ok$"}`, 2)
	mock.ExpectQuery("FROM eval_cases").
		WillReturnRows(rows)

	got, err := store.GetDatasetCases(context.Background(), "org-1", "ds-1")
	if err != nil {
		t.Fatalf("GetDatasetCases returned error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 cases, got %d", len(got))
	}
	if got[1].Scorer != ScorerLatencyUnderMs || got[1].Params.ThresholdMS == nil || *got[1].Params.ThresholdMS != 1500 {
		t.Fatalf("threshold params should round-trip: %+v", got[1].Params)
	}
	if got[2].Params.Pattern != "^ok$" {
		t.Fatalf("pattern should round-trip: %+v", got[2].Params)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGRunLifecycle(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	run := &EvalRun{ID: "run-1", OrganizationID: "org-1", DatasetID: "ds-1", AgentID: "agent-1", Status: StatusRunning, CreatedAt: now}

	mock.ExpectExec(regexp.QuoteMeta(sqlInsertRun)).
		WithArgs(run.ID, run.OrganizationID, run.DatasetID, run.AgentID, run.Status, run.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("CreateRun returned error: %v", err)
	}

	results := []Result{
		{ID: "r1", CaseID: "c1", Scorer: ScorerExact, Output: "a1", Passed: true, Score: 1, LatencyMS: 12.5, CostCents: 0},
		{ID: "r2", CaseID: "c2", Scorer: ScorerRegex, Output: "nope", Passed: false, Score: 0, LatencyMS: 8.25, CostCents: 0, Error: "no match"},
	}
	mock.ExpectBegin()
	for i, r := range results {
		mock.ExpectExec(regexp.QuoteMeta(sqlInsertResult)).
			WithArgs(r.ID, "run-1", "org-1", r.CaseID, string(r.Scorer), i, r.Output, r.Passed, r.Score, r.LatencyMS, r.CostCents, r.Error, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()
	if err := store.CreateResults(context.Background(), "org-1", "run-1", results); err != nil {
		t.Fatalf("CreateResults returned error: %v", err)
	}

	completedAt := now.Add(time.Minute)
	mock.ExpectExec(regexp.QuoteMeta(sqlUpdateRunStatus)).
		WithArgs(StatusCompleted, completedAt, "run-1", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateRunStatus(context.Background(), "org-1", "run-1", StatusCompleted, &completedAt); err != nil {
		t.Fatalf("UpdateRunStatus returned error: %v", err)
	}

	// Tenant guard: zero rows affected (wrong org) -> ErrRunNotFound.
	mock.ExpectExec(regexp.QuoteMeta(sqlUpdateRunStatus)).
		WithArgs(StatusCompleted, sqlmock.AnyArg(), "run-1", "org-other").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateRunStatus(context.Background(), "org-other", "run-1", StatusCompleted, &completedAt); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign update should be ErrRunNotFound, got %v", err)
	}

	runRows := sqlmock.NewRows([]string{"id", "organization_id", "dataset_id", "agent_id", "status", "created_at", "completed_at"}).
		AddRow("run-1", "org-1", "ds-1", "agent-1", StatusCompleted, now, completedAt)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, organization_id, dataset_id, agent_id, status, created_at, completed_at FROM eval_runs WHERE id = $1 AND organization_id = $2`)).
		WithArgs("run-1", "org-1").
		WillReturnRows(runRows)
	got, err := store.GetRun(context.Background(), "org-1", "run-1")
	if err != nil {
		t.Fatalf("GetRun returned error: %v", err)
	}
	if got.Status != StatusCompleted || got.CompletedAt == nil {
		t.Fatalf("unexpected run: %+v", got)
	}

	mock.ExpectQuery("FROM eval_results").
		WillReturnRows(sqlmock.NewRows([]string{"id", "case_id", "scorer", "output", "passed", "score", "latency_ms", "cost_cents", "error"}).
			AddRow("r1", "c1", "exact", "a1", true, 1, 12.5, 0, "").
			AddRow("r2", "c2", "regex", "nope", false, 0, 8.25, 0, "no match"))
	loaded, err := store.ListResults(context.Background(), "org-1", "run-1")
	if err != nil {
		t.Fatalf("ListResults returned error: %v", err)
	}
	if !reflect.DeepEqual(loaded, results) {
		t.Fatalf("results should round-trip:\n got %+v\nwant %+v", loaded, results)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGGetRunNotFound(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	mock.ExpectQuery("FROM eval_runs").
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "dataset_id", "agent_id", "status", "created_at", "completed_at"}))
	if _, err := store.GetRun(context.Background(), "org-1", "missing"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing run should map to ErrRunNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGNilDBGuard(t *testing.T) {
	var store *pgStore
	if err := store.CreateRun(context.Background(), &EvalRun{}); err == nil {
		t.Fatal("nil db should be rejected")
	}
	if _, err := store.GetDataset(context.Background(), "org", "id"); err == nil {
		t.Fatal("nil db should be rejected")
	}
}

// TestPGListCompletedRuns pins the canary-promotion sample SQL (issue #51):
// tenant + agent + completed-status guards, newest-first ordering, LIMIT
// passthrough.
func TestPGListCompletedRuns(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	createdAt := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	completedAt := createdAt.Add(time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectCompletedRunsByAgent)).
		WithArgs("org-1", "agent-1", 2).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "dataset_id", "agent_id", "status", "created_at", "completed_at"}).
			AddRow("run-new", "org-1", "ds-1", "agent-1", StatusCompleted, createdAt, completedAt).
			AddRow("run-old", "org-1", "ds-1", "agent-1", StatusCompleted, createdAt.Add(-time.Hour), completedAt))

	runs, err := store.ListCompletedRuns(context.Background(), "org-1", "agent-1", 2)
	if err != nil {
		t.Fatalf("ListCompletedRuns returned error: %v", err)
	}
	if len(runs) != 2 || runs[0].ID != "run-new" || runs[1].ID != "run-old" {
		t.Fatalf("expected newest-first completed runs, got %+v", runs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
