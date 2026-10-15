package runs

// checkpoints_test.go — issue #115: durable run checkpoint stores.
// In-memory roundtrip semantics plus the Postgres store through go-sqlmock
// (upsert idempotency, tenant guard rejection, latest-checkpoint scan,
// has-checkpoint probe and the guarded status update used by every enforced
// transition and the recovery sweep).

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestInMemoryCheckpointStoreRoundtrip(t *testing.T) {
	store := NewInMemoryCheckpointStore()
	ctx := context.Background()

	if _, err := store.LatestCheckpoint(ctx, "org-1", "run-1"); !errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("empty store: want ErrCheckpointNotFound, got %v", err)
	}
	if store.HasCheckpoint(ctx, "run-1") {
		t.Fatal("empty store must report no checkpoint")
	}

	if err := store.SaveCheckpoint(ctx, "org-1", "run-1", 1, []byte(`{"step_index":1}`)); err != nil {
		t.Fatalf("save step 1: %v", err)
	}
	if err := store.SaveCheckpoint(ctx, "org-1", "run-1", 2, []byte(`{"step_index":2}`)); err != nil {
		t.Fatalf("save step 2: %v", err)
	}
	cp, err := store.LatestCheckpoint(ctx, "org-1", "run-1")
	if err != nil || cp.StepIndex != 2 {
		t.Fatalf("latest = %+v err %v, want step 2", cp, err)
	}
	if string(cp.State) != `{"step_index":2}` {
		t.Fatalf("state = %s", cp.State)
	}
	if !store.HasCheckpoint(ctx, "run-1") {
		t.Fatal("HasCheckpoint must be true after a save")
	}

	// Upsert: a replayed save for the same step overwrites in place.
	if err := store.SaveCheckpoint(ctx, "org-1", "run-1", 2, []byte(`{"step_index":2,"v":2}`)); err != nil {
		t.Fatalf("replay save: %v", err)
	}
	cp, _ = store.LatestCheckpoint(ctx, "org-1", "run-1")
	if string(cp.State) != `{"step_index":2,"v":2}` {
		t.Fatalf("upsert did not overwrite: %s", cp.State)
	}

	// Tenant guard: blank org or run id is rejected.
	if err := store.SaveCheckpoint(ctx, "", "run-1", 3, nil); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("blank org: want ErrRunNotFound, got %v", err)
	}
	if err := store.SaveCheckpoint(ctx, "org-1", " ", 3, nil); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("blank run: want ErrRunNotFound, got %v", err)
	}
	// Mutating the returned snapshot cannot corrupt the store (defensive copy).
	cp.State[0] = '['
	again, _ := store.LatestCheckpoint(ctx, "org-1", "run-1")
	if string(again.State) != `{"step_index":2,"v":2}` {
		t.Fatalf("store state mutated through the returned snapshot: %s", again.State)
	}
}

func TestPGCheckpointUpsertTenantGuardAndLatest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer func() { _ = db.Close() }()
	// The CheckpointStore is a distinct struct over the same handle (the
	// runs service wires it via SetCheckpointStore).
	checkpoints := NewPostgresCheckpointStore(db)
	ctx := context.Background()

	// Tenant-guarded upsert: 1 affected row = accepted.
	mock.ExpectExec(regexp.QuoteMeta(sqlUpsertRunCheckpoint)).
		WithArgs("run-1", 2, `{"step_index":2}`, sqlmock.AnyArg(), "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := checkpoints.SaveCheckpoint(ctx, "org-1", "run-1", 2, []byte(`{"step_index":2}`)); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	// 0 affected rows = the EXISTS guard rejected a foreign/unknown run.
	mock.ExpectExec(regexp.QuoteMeta(sqlUpsertRunCheckpoint)).
		WithArgs("run-foreign", 1, "{}", sqlmock.AnyArg(), "org-9").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := checkpoints.SaveCheckpoint(ctx, "org-9", "run-foreign", 1, []byte(`{}`)); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("tenant guard: want ErrRunNotFound, got %v", err)
	}

	// Latest: highest step_index wins (LIMIT 1 ... ORDER BY step_index DESC).
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectLatestRunCheckpoint)).
		WithArgs("org-1", "run-1").
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "step_index", "state", "created_at"}).
			AddRow("run-1", 2, `{"step_index":2}`, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)))
	cp, err := checkpoints.LatestCheckpoint(ctx, "org-1", "run-1")
	if err != nil || cp.StepIndex != 2 || cp.RunID != "run-1" {
		t.Fatalf("latest = %+v err %v", cp, err)
	}

	// No checkpoint yet → ErrCheckpointNotFound (never a raw sql.ErrNoRows);
	// other store failures propagate verbatim.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectLatestRunCheckpoint)).
		WithArgs("org-1", "run-2").
		WillReturnError(errors.New("db down"))
	if _, err := checkpoints.LatestCheckpoint(ctx, "org-1", "run-2"); err == nil || errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("store failure must propagate verbatim, got %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectLatestRunCheckpoint)).
		WithArgs("org-1", "run-4").
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "step_index", "state", "created_at"}))
	if _, err := checkpoints.LatestCheckpoint(ctx, "org-1", "run-4"); !errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("no rows: want ErrCheckpointNotFound, got %v", err)
	}

	// HasCheckpoint probe (recovery sweep's paused-vs-failed decision).
	mock.ExpectQuery(regexp.QuoteMeta(sqlExistsRunCheckpoint)).
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	if !checkpoints.HasCheckpoint(ctx, "run-1") {
		t.Fatal("HasCheckpoint must be true")
	}
	mock.ExpectQuery(regexp.QuoteMeta(sqlExistsRunCheckpoint)).
		WithArgs("run-x").
		WillReturnError(errors.New("db down"))
	if checkpoints.HasCheckpoint(ctx, "run-x") {
		t.Fatal("probe failures must report false (no checkpoint), never panic")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGUpdateRunStatusGuarded(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	guarded, ok := any(store).(StatusGuardStore)
	if !ok {
		t.Fatal("pgStore must implement StatusGuardStore")
	}
	ctx := context.Background()

	// Guard hit: WHERE status = expected matched → updated.
	mock.ExpectExec(regexp.QuoteMeta(sqlUpdateRunStatusGuarded)).
		WithArgs("RUNNING", "", sqlmock.AnyArg(), "run-1", "org-1", "QUEUED").
		WillReturnResult(sqlmock.NewResult(0, 1))
	updated, err := guarded.UpdateRunStatusGuarded(ctx, "org-1", "run-1", StatusQueued, StatusRunning, "")
	if err != nil || !updated {
		t.Fatalf("guard hit: updated=%v err=%v, want true/nil", updated, err)
	}

	// Guard miss (a racing writer moved the row first): 0 rows → updated=false,
	// which the service surfaces as ErrStatusConflict.
	mock.ExpectExec(regexp.QuoteMeta(sqlUpdateRunStatusGuarded)).
		WithArgs("COMPLETED", "done", sqlmock.AnyArg(), "run-1", "org-1", "RUNNING").
		WillReturnResult(sqlmock.NewResult(0, 0))
	updated, err = guarded.UpdateRunStatusGuarded(ctx, "org-1", "run-1", StatusRunning, StatusCompleted, "done")
	if err != nil || updated {
		t.Fatalf("guard miss: updated=%v err=%v, want false/nil", updated, err)
	}
}

func TestPGStaleControlRunsScan(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	scanner, ok := any(store).(StaleRunScanner)
	if !ok {
		t.Fatal("pgStore must implement StaleRunScanner")
	}
	ctx := context.Background()
	cutoff := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectStaleControlRuns)).
		WithArgs(cutoff, "", 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "lower", "has_checkpoint"}).
			AddRow("run-1", "org-1", "running", true).
			AddRow("run-2", "org-2", "cancelling", false))
	got, err := scanner.StaleControlRuns(ctx, cutoff, "", 50)
	if err != nil {
		t.Fatalf("StaleControlRuns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].ID != "run-1" || got[0].OrganizationID != "org-1" || got[0].Status != "running" || !got[0].HasCheckpoint {
		t.Fatalf("record 0 = %+v", got[0])
	}
	if got[1].Status != "cancelling" || got[1].HasCheckpoint {
		t.Fatalf("record 1 = %+v", got[1])
	}

	// Org-scoped sweep passes the tenant through ($2).
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectStaleControlRuns)).
		WithArgs(cutoff, "org-1", 10).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "lower", "has_checkpoint"}))
	if _, err := scanner.StaleControlRuns(ctx, cutoff, "org-1", 10); err != nil {
		t.Fatalf("org-scoped sweep: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
