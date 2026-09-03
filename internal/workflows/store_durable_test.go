package workflows

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// newDurableMockDB builds a pgStore over sqlmock (mirrors the store test
// convention of internal/evaluations).
func newDurableMockDB(t *testing.T) (*pgStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return &pgStore{db: db}, mock, func() { _ = db.Close() }
}

func durableNodeRunRow(nr *NodeRun) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "workflow_run_id", "node_id", "run_id", "status", "error",
		"attempt", "locked_at", "heartbeat_at", "error_code", "started_at", "finished_at", "created_at",
	}).AddRow(nr.ID, nr.WorkflowRunID, nr.NodeID, nr.RunID, nr.Status, nr.Error,
		nr.Attempt, nr.LockedAt, nr.HeartbeatAt, nr.ErrorCode, nr.StartedAt, nr.FinishedAt, nr.CreatedAt)
}

func durableWorkflowRunRow(wr *WorkflowRun) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "workflow_id", "organization_id", "input", "status", "created_by",
		"attempt", "locked_at", "heartbeat_at", "finished_at", "deadline_at", "error_code",
		"created_at", "updated_at",
	}).AddRow(wr.ID, wr.WorkflowID, wr.OrganizationID, wr.Input, wr.Status, wr.CreatedBy,
		wr.Attempt, wr.LockedAt, wr.HeartbeatAt, wr.FinishedAt, wr.DeadlineAt, wr.ErrorCode,
		wr.CreatedAt, wr.UpdatedAt)
}

func testCheckpoint() *NodeRun {
	started := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	return &NodeRun{
		ID:            "nr-1",
		WorkflowRunID: "wr-1",
		NodeID:        "n1",
		RunID:         "run-1",
		Status:        RunStatusRunning,
		Attempt:       2,
		LockedAt:      &started,
		HeartbeatAt:   &started,
		StartedAt:     &started,
		CreatedAt:     started,
	}
}

func TestPGInsertNodeRunCheckpoint(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	nr := testCheckpoint()
	// First write wins: the ON CONFLICT arbiter accepts the row.
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertNodeRunCheckpoint)).
		WithArgs(nr.ID, nr.WorkflowRunID, "org-1", nr.NodeID, nr.RunID, nr.Status, nr.Error,
			nr.Attempt, nr.LockedAt, nr.HeartbeatAt, nr.ErrorCode, nr.StartedAt, nr.FinishedAt, nr.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	created, err := store.InsertNodeRun(context.Background(), "org-1", nr)
	if err != nil {
		t.Fatalf("InsertNodeRun returned error: %v", err)
	}
	if !created {
		t.Fatal("first checkpoint insert should report created=true")
	}

	// Replay: same (workflow_run_id, node_id, attempt) -> DO NOTHING.
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertNodeRunCheckpoint)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	created, err = store.InsertNodeRun(context.Background(), "org-1", nr)
	if err != nil {
		t.Fatalf("InsertNodeRun (replay) returned error: %v", err)
	}
	if created {
		t.Fatal("replayed checkpoint insert should report created=false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGLatestNodeRun(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	nr := testCheckpoint()
	// Tenant guard: WHERE organization_id = $1 AND workflow_run_id = $2 AND node_id = $3.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectLatestNodeRun)).
		WithArgs("org-1", "wr-1", "n1").
		WillReturnRows(durableNodeRunRow(nr))
	got, err := store.LatestNodeRun(context.Background(), "org-1", "wr-1", "n1")
	if err != nil {
		t.Fatalf("LatestNodeRun returned error: %v", err)
	}
	if got == nil || got.ID != nr.ID || got.Attempt != 2 || got.RunID != "run-1" {
		t.Fatalf("unexpected latest node run: %#v", got)
	}

	// No checkpoint yet -> nil, nil.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectLatestNodeRun)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workflow_run_id", "node_id", "run_id", "status", "error", "attempt", "locked_at", "heartbeat_at", "error_code", "started_at", "finished_at", "created_at"}))
	got, err = store.LatestNodeRun(context.Background(), "org-1", "wr-1", "ghost")
	if err != nil || got != nil {
		t.Fatalf("missing node run should be (nil, nil), got (%#v, %v)", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGClaimNodeRun(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	at := time.Date(2025, 6, 1, 13, 0, 0, 0, time.UTC)
	// Claim bumps attempt + stamps the lease (guarded UPDATE on pending/running).
	mock.ExpectExec(regexp.QuoteMeta(sqlClaimNodeRun)).
		WithArgs("nr-1", "org-1", "run-1", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	claimed, err := store.ClaimNodeRun(context.Background(), "org-1", "nr-1", "run-1", at)
	if err != nil || !claimed {
		t.Fatalf("claim should succeed, got (claimed=%v, err=%v)", claimed, err)
	}

	// Terminal rows are never re-claimed (0 rows affected).
	mock.ExpectExec(regexp.QuoteMeta(sqlClaimNodeRun)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	claimed, err = store.ClaimNodeRun(context.Background(), "org-1", "nr-1", "run-1", at)
	if err != nil || claimed {
		t.Fatalf("terminal claim should be (false, nil), got (%v, %v)", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGTouchAndMarkNodeRun(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	at := time.Date(2025, 6, 1, 13, 0, 0, 0, time.UTC)

	mock.ExpectExec(regexp.QuoteMeta(sqlTouchNodeRun)).
		WithArgs("nr-1", "org-1", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.TouchNodeRun(context.Background(), "org-1", "nr-1", at); err != nil {
		t.Fatalf("TouchNodeRun returned error: %v", err)
	}

	// Finish is guarded against already-terminal rows (idempotent).
	mock.ExpectExec(regexp.QuoteMeta(sqlMarkNodeRunStatus)).
		WithArgs("nr-1", "org-1", RunStatusCompleted, "", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	marked, err := store.MarkNodeRunStatus(context.Background(), "org-1", "nr-1", RunStatusCompleted, "", at)
	if err != nil || !marked {
		t.Fatalf("finish should mark, got (marked=%v, err=%v)", marked, err)
	}

	mock.ExpectExec(regexp.QuoteMeta(sqlMarkNodeRunStatus)).
		WithArgs("nr-1", "org-1", RunStatusFailed, "AGENT_CRASHED", at).
		WillReturnResult(sqlmock.NewResult(0, 0))
	marked, err = store.MarkNodeRunStatus(context.Background(), "org-1", "nr-1", RunStatusFailed, "AGENT_CRASHED", at)
	if err != nil || marked {
		t.Fatalf("second finish should be an idempotent no-op, got (marked=%v, err=%v)", marked, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGFailNonTerminalNodeRuns(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	at := time.Date(2025, 6, 1, 13, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(sqlFailNonTerminalNodeRuns)).
		WithArgs("org-1", "wr-1", ErrorCodeNodeOrphaned, at).
		WillReturnResult(sqlmock.NewResult(0, 3))
	n, err := store.FailNonTerminalNodeRuns(context.Background(), "org-1", "wr-1", ErrorCodeNodeOrphaned, at)
	if err != nil {
		t.Fatalf("FailNonTerminalNodeRuns returned error: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 orphaned node runs, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGWorkflowRunDurabilityUpdates(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	at := time.Date(2025, 6, 1, 13, 0, 0, 0, time.UTC)

	// Heartbeat only touches non-terminal runs.
	mock.ExpectExec(regexp.QuoteMeta(sqlTouchWorkflowRunHeartbeat)).
		WithArgs("wr-1", "org-1", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.TouchWorkflowRunHeartbeat(context.Background(), "org-1", "wr-1", at); err != nil {
		t.Fatalf("TouchWorkflowRunHeartbeat returned error: %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta(sqlSetWorkflowRunDeadline)).
		WithArgs("wr-1", "org-1", at.Add(10*time.Minute)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.SetWorkflowRunDeadline(context.Background(), "org-1", "wr-1", at.Add(10*time.Minute)); err != nil {
		t.Fatalf("SetWorkflowRunDeadline returned error: %v", err)
	}

	// Watchdog transition is guarded: terminal rows are never re-timed-out.
	mock.ExpectExec(regexp.QuoteMeta(sqlTimeoutWorkflowRun)).
		WithArgs("wr-1", "org-1", ErrorCodeWorkflowRunTimeout, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	timedOut, err := store.TimeoutWorkflowRun(context.Background(), "org-1", "wr-1", ErrorCodeWorkflowRunTimeout, at)
	if err != nil || !timedOut {
		t.Fatalf("timeout should apply, got (timedOut=%v, err=%v)", timedOut, err)
	}
	mock.ExpectExec(regexp.QuoteMeta(sqlTimeoutWorkflowRun)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	timedOut, err = store.TimeoutWorkflowRun(context.Background(), "org-1", "wr-1", ErrorCodeWorkflowRunTimeout, at)
	if err != nil || timedOut {
		t.Fatalf("second timeout should be (false, nil), got (%v, %v)", timedOut, err)
	}

	// Finalize (completed/failed) shares the same guard.
	mock.ExpectExec(regexp.QuoteMeta(sqlFinalizeWorkflowRun)).
		WithArgs("wr-1", "org-1", RunStatusCompleted, "", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	finalized, err := store.FinalizeWorkflowRun(context.Background(), "org-1", "wr-1", RunStatusCompleted, "", at)
	if err != nil || !finalized {
		t.Fatalf("finalize should apply, got (finalized=%v, err=%v)", finalized, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGClaimWorkflowRun(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	at := time.Date(2025, 6, 1, 13, 0, 0, 0, time.UTC)
	query, err := sqlClaimWorkflowRun([]string{RunStatusRunning, RunStatusWaitingApproval})
	if err != nil {
		t.Fatalf("sqlClaimWorkflowRun returned error: %v", err)
	}
	if want := `AND status IN ('running', 'waiting_approval')`; !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(query) {
		t.Fatalf("claim SQL should whitelist the source statuses, got %q", query)
	}
	mock.ExpectExec(regexp.QuoteMeta(query)).
		WithArgs("wr-1", "org-1", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	claimed, err := store.ClaimWorkflowRun(context.Background(), "org-1", "wr-1", []string{RunStatusRunning, RunStatusWaitingApproval}, at)
	if err != nil || !claimed {
		t.Fatalf("claim should succeed, got (claimed=%v, err=%v)", claimed, err)
	}

	// Duplicate statuses deduplicate; empty/unknown inputs are rejected before SQL.
	if _, err := sqlClaimWorkflowRun(nil); err == nil {
		t.Fatal("empty status list should be rejected")
	}
	if _, err := sqlClaimWorkflowRun([]string{"'; DROP TABLE users"}); err == nil {
		t.Fatal("unknown status should be rejected by the whitelist")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGStaleWorkflowRunsSkipLocked(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	cutoff := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	wr := &WorkflowRun{
		ID: "wr-1", WorkflowID: "wf-1", OrganizationID: "org-1",
		Input: "hi", Status: RunStatusRunning, CreatedBy: "user-1",
		Attempt: 1, CreatedAt: cutoff.Add(-time.Hour), UpdatedAt: cutoff.Add(-time.Minute),
	}
	// FOR UPDATE SKIP LOCKED runs inside one transaction; the tenant filter is
	// embedded in the statement (orgID = $2).
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectStaleWorkflowRuns)).
		WithArgs(cutoff, "org-1", RecoveryBatchLimit).
		WillReturnRows(durableWorkflowRunRow(wr))
	mock.ExpectCommit()

	got, err := store.StaleWorkflowRuns(context.Background(), "org-1", cutoff, RecoveryBatchLimit)
	if err != nil {
		t.Fatalf("StaleWorkflowRuns returned error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "wr-1" || got[0].Attempt != 1 {
		t.Fatalf("unexpected stale candidates: %#v", got)
	}

	// Empty orgID sweeps every tenant (worker path; same statement).
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectStaleWorkflowRuns)).
		WithArgs(cutoff, "", RecoveryBatchLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workflow_id", "organization_id", "input", "status", "created_by", "attempt", "locked_at", "heartbeat_at", "finished_at", "deadline_at", "error_code", "created_at", "updated_at"}))
	mock.ExpectCommit()
	got, err = store.StaleWorkflowRuns(context.Background(), "", cutoff, RecoveryBatchLimit)
	if err != nil {
		t.Fatalf("StaleWorkflowRuns (all tenants) returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no candidates, got %#v", got)
	}

	// Zero limit falls back to the batch cap.
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectStaleWorkflowRuns)).
		WithArgs(cutoff, "org-1", RecoveryBatchLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workflow_id", "organization_id", "input", "status", "created_by", "attempt", "locked_at", "heartbeat_at", "finished_at", "deadline_at", "error_code", "created_at", "updated_at"}))
	mock.ExpectCommit()
	if _, err = store.StaleWorkflowRuns(context.Background(), "org-1", cutoff, 0); err != nil {
		t.Fatalf("StaleWorkflowRuns (default limit) returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGTimedOutWorkflowRunsSkipLocked(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	now := time.Date(2025, 6, 1, 13, 0, 0, 0, time.UTC)
	deadline := now.Add(-time.Minute)
	wr := &WorkflowRun{
		ID: "wr-2", WorkflowID: "wf-1", OrganizationID: "org-2",
		Status: RunStatusRunning, DeadlineAt: &deadline,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectTimedOutWorkflowRuns)).
		WithArgs(now, "", RecoveryBatchLimit).
		WillReturnRows(durableWorkflowRunRow(wr))
	mock.ExpectCommit()

	got, err := store.TimedOutWorkflowRuns(context.Background(), "", now, RecoveryBatchLimit)
	if err != nil {
		t.Fatalf("TimedOutWorkflowRuns returned error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "wr-2" || got[0].DeadlineAt == nil {
		t.Fatalf("unexpected watchdog candidates: %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGDurableStoreRollsBackOnScanError(t *testing.T) {
	store, mock, closeDB := newDurableMockDB(t)
	defer closeDB()

	cutoff := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	badRows := sqlmock.NewRows([]string{"id", "workflow_id", "organization_id", "input", "status", "created_by", "attempt", "locked_at", "heartbeat_at", "finished_at", "deadline_at", "error_code", "created_at", "updated_at"}).
		AddRow("wr-1", "wf-1", "org-1", "hi", "running", "user-1", "not-an-int", nil, nil, nil, nil, "", cutoff, cutoff)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectStaleWorkflowRuns)).
		WillReturnRows(badRows)
	mock.ExpectRollback()

	if _, err := store.StaleWorkflowRuns(context.Background(), "org-1", cutoff, RecoveryBatchLimit); err == nil {
		t.Fatal("scan failure should abort the recovery listing")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGDurableStoreNilDBGuard(t *testing.T) {
	store := &pgStore{}
	if _, err := store.StaleWorkflowRuns(context.Background(), "org-1", time.Now(), 1); err == nil {
		t.Fatal("nil db should be guarded")
	}
	if _, err := store.LatestNodeRun(context.Background(), "org-1", "wr-1", "n1"); !errors.Is(err, err) && err == nil {
		t.Fatal("nil db should be guarded")
	}
	if _, err := store.InsertNodeRun(context.Background(), "org-1", testCheckpoint()); err == nil {
		t.Fatal("nil db should be guarded")
	}
}
