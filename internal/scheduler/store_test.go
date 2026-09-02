package scheduler

import (
	"context"
	"errors"
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

func scheduleRowColumns() []string {
	return []string{
		"id", "organization_id", "agent_id", "input", "kind",
		"run_at", "interval_seconds", "cron_expr", "timezone",
		"status", "next_run_at", "last_run_id", "last_fired_at",
		"created_at", "updated_at",
	}
}

func TestPostgresStoreCreate(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	now := testBase
	runAt := testBase.Add(time.Hour)

	mock.ExpectExec(`INSERT INTO schedules`).
		WithArgs("sched-1", "org-1", "agent-1", "hello", KindOnce, runAt, nil, "", "UTC",
			StatusActive, runAt, "", nil, now, now).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := store.Create(ctx, &Schedule{
		ID: "sched-1", OrganizationID: "org-1", AgentID: "agent-1", Input: "hello",
		Kind: KindOnce, RunAt: &runAt, Timezone: "UTC", Status: StatusActive,
		NextRunAt: &runAt, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreGetScoped(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	runAt := testBase.Add(time.Hour)
	rows := sqlmock.NewRows(scheduleRowColumns()).AddRow(
		"sched-1", "org-1", "agent-1", "hello", KindOnce,
		runAt, nil, "", "UTC", StatusActive, runAt, "", nil, testBase, testBase)

	// Tenant guard: WHERE id = $1 AND organization_id = $2.
	mock.ExpectQuery(`SELECT id, organization_id, agent_id, COALESCE\(input, ''\), kind, run_at, interval_seconds, COALESCE\(cron_expr, ''\), COALESCE\(timezone, 'UTC'\), status, next_run_at, COALESCE\(last_run_id, ''\), last_fired_at, created_at, updated_at FROM schedules WHERE id = \$1 AND organization_id = \$2`).
		WithArgs("sched-1", "org-1").
		WillReturnRows(rows)

	sched, err := store.Get(ctx, "org-1", "sched-1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if sched.Kind != KindOnce || sched.Status != StatusActive || sched.RunAt == nil || !sched.RunAt.Equal(runAt) {
		t.Fatalf("unexpected schedule: %+v", sched)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}

	// No rows -> ErrScheduleNotFound.
	mock.ExpectQuery(`SELECT .+ FROM schedules WHERE id = \$1 AND organization_id = \$2`).
		WithArgs("missing", "org-1").
		WillReturnError(ErrScheduleNotFound)
	if _, err := store.Get(ctx, "org-1", "missing"); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("expected ErrScheduleNotFound, got %v", err)
	}
}

func TestPostgresStoreDueQuery(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	next := testBase
	rows := sqlmock.NewRows(scheduleRowColumns()).AddRow(
		"sched-due", "org-9", "agent-1", "", KindCron,
		nil, nil, "*/5 * * * *", "UTC", StatusActive, next, "", nil, testBase, testBase)

	// Trusted worker path: filters on status + next_run_at <= now, all tenants.
	mock.ExpectQuery(`SELECT .+ FROM schedules\s+WHERE status = 'active' AND next_run_at IS NOT NULL AND next_run_at <= \$1\s+ORDER BY next_run_at ASC`).
		WithArgs(testBase).
		WillReturnRows(rows)

	due, err := store.Due(ctx, testBase)
	if err != nil {
		t.Fatalf("Due returned error: %v", err)
	}
	if len(due) != 1 || due[0].ID != "sched-due" || due[0].OrganizationID != "org-9" {
		t.Fatalf("unexpected due list: %+v", due)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

// TestPostgresStoreClaimForFire verifies the atomic conditional claim: the
// UPDATE only lands while the slot is still due, which is the double-fire
// protection across restarts and concurrent workers.
func TestPostgresStoreClaimForFire(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	firedAt := testBase
	next := testBase.Add(10 * time.Minute)

	mock.ExpectExec(`UPDATE schedules SET\s+status = \$2,\s+next_run_at = \$3,\s+last_fired_at = \$4,\s+updated_at = \$4\s+WHERE id = \$1 AND status = 'active' AND next_run_at IS NOT NULL AND next_run_at <= \$4`).
		WithArgs("sched-1", StatusActive, next, firedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err := store.ClaimForFire(ctx, "sched-1", firedAt, StatusActive, &next)
	if err != nil || !ok {
		t.Fatalf("claim should succeed, ok=%v err=%v", ok, err)
	}

	// Slot already consumed (restart race): 0 rows affected -> not claimed.
	mock.ExpectExec(`UPDATE schedules`).
		WithArgs("sched-1", StatusActive, next, firedAt).
		WillReturnResult(sqlmock.NewResult(0, 0))
	ok, err = store.ClaimForFire(ctx, "sched-1", firedAt, StatusActive, &next)
	if err != nil {
		t.Fatalf("claim race returned error: %v", err)
	}
	if ok {
		t.Fatal("second claim must be refused (catch-up protection)")
	}

	// Once schedule claim: status completed + NULL next_run_at.
	mock.ExpectExec(`UPDATE schedules`).
		WithArgs("sched-2", StatusCompleted, nil, firedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err = store.ClaimForFire(ctx, "sched-2", firedAt, StatusCompleted, nil)
	if err != nil || !ok {
		t.Fatalf("once claim should succeed, ok=%v err=%v", ok, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreSetLastRunAndStatusGuards(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()

	mock.ExpectExec(`UPDATE schedules SET last_run_id = \$2, updated_at = \$3 WHERE id = \$1`).
		WithArgs("sched-1", "run-42", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.SetLastRun(ctx, "sched-1", "run-42"); err != nil {
		t.Fatalf("SetLastRun returned error: %v", err)
	}

	mock.ExpectExec(`UPDATE schedules SET last_run_id = \$2, updated_at = \$3 WHERE id = \$1`).
		WithArgs("missing", "run-42", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.SetLastRun(ctx, "missing", "run-42"); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("expected ErrScheduleNotFound, got %v", err)
	}

	// Tenant guard: pause requires matching organization_id; completed rows are
	// excluded by status <> 'completed'.
	mock.ExpectExec(`UPDATE schedules SET status = \$1, updated_at = \$2\s+WHERE id = \$3 AND organization_id = \$4 AND status <> 'completed'`).
		WithArgs(StatusPaused, sqlmock.AnyArg(), "sched-1", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateStatus(ctx, "org-1", "sched-1", StatusPaused); err != nil {
		t.Fatalf("UpdateStatus returned error: %v", err)
	}

	mock.ExpectExec(`UPDATE schedules SET status`).
		WithArgs(StatusPaused, sqlmock.AnyArg(), "sched-1", "org-2").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateStatus(ctx, "org-2", "sched-1", StatusPaused); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("cross-tenant UpdateStatus should 404, got %v", err)
	}

	// Tenant guard: DELETE requires matching organization_id.
	mock.ExpectExec(`DELETE FROM schedules WHERE id = \$1 AND organization_id = \$2`).
		WithArgs("sched-1", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Delete(ctx, "org-1", "sched-1"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreNilDBGuard(t *testing.T) {
	store := &pgStore{}
	if err := store.Create(context.Background(), &Schedule{}); err == nil {
		t.Fatal("nil db should be guarded")
	}
	if _, err := store.Get(context.Background(), "org", "id"); err == nil {
		t.Fatal("nil db should be guarded")
	}
}

// TestServiceWithPostgresStoreDelegates checks the dual-mode service routes
// every read/write through the store when one is configured.
func TestServiceWithPostgresStoreDelegates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer func() { _ = db.Close() }()
	svc := NewServiceWithStore(NewPostgresStore(db)).WithClock(newFakeClock(testBase))
	ctx := context.Background()

	mock.ExpectQuery(`SELECT .+ FROM schedules WHERE id = \$1 AND organization_id = \$2`).
		WithArgs("sched-x", "org-1").
		WillReturnError(ErrScheduleNotFound)
	if _, err := svc.Get(ctx, "org-1", "sched-x"); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("expected store-backed 404, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
