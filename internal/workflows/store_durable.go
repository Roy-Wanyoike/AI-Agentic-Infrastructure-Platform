package workflows

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Durable-execution SQL (wave-3 track 3-c, migration 013). Every statement is
// guarded by organization_id; every terminal transition is a conditional
// UPDATE so concurrent recovery passes and workers converge idempotently.
const (
	// Idempotent checkpoint insert: the unique index
	// uq_workflow_node_runs_attempt (workflow_run_id, node_id, attempt) is the
	// conflict arbiter; DO NOTHING + RowsAffected tells replays from firsts.
	sqlInsertNodeRunCheckpoint = `INSERT INTO workflow_node_runs (id, workflow_run_id, organization_id, node_id, run_id, status, error, attempt, locked_at, heartbeat_at, error_code, started_at, finished_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) ON CONFLICT (workflow_run_id, node_id, attempt) DO NOTHING`
	// Highest-attempt checkpoint of one node (idempotency lookup).
	sqlSelectLatestNodeRun = `SELECT id, workflow_run_id, node_id, COALESCE(run_id, ''), status, COALESCE(error, ''), attempt, locked_at, heartbeat_at, COALESCE(error_code, ''), started_at, finished_at, created_at FROM workflow_node_runs WHERE organization_id = $1 AND workflow_run_id = $2 AND node_id = $3 ORDER BY attempt DESC LIMIT 1`
	// Atomic claim: one pending/running row becomes running with a fresh
	// lease; attempt bumps (one claim == one execution attempt). An empty
	// run_id inherits the previous attempt's linkage.
	sqlClaimNodeRun = `UPDATE workflow_node_runs SET status = 'running', run_id = COALESCE(NULLIF($3, ''), run_id), attempt = attempt + 1, locked_at = $4, heartbeat_at = $4, started_at = COALESCE(started_at, $4) WHERE id = $1 AND organization_id = $2 AND status IN ('pending', 'running')`
	// Heartbeat refresh, running rows only (a finished attempt stays put).
	sqlTouchNodeRun = `UPDATE workflow_node_runs SET heartbeat_at = $3 WHERE id = $1 AND organization_id = $2 AND status = 'running'`
	// Terminal node-run transition, guarded against already-terminal rows
	// (idempotent finish). waiting_approval may still transition (the decide
	// flow completes gates).
	sqlMarkNodeRunStatus = `UPDATE workflow_node_runs SET status = $3, error_code = $4, finished_at = $5 WHERE id = $1 AND organization_id = $2 AND status NOT IN ('completed', 'failed', 'cancelled', 'timeout')`
	// Orphan/timeout pass: fail every pending/running checkpoint of one
	// workflow run with a machine error code.
	sqlFailNonTerminalNodeRuns = `UPDATE workflow_node_runs SET status = 'failed', error_code = $3, finished_at = $4 WHERE organization_id = $1 AND workflow_run_id = $2 AND status IN ('pending', 'running')`
	// Workflow-run liveness (only while the run can still make progress).
	sqlTouchWorkflowRunHeartbeat = `UPDATE workflow_runs SET heartbeat_at = $3, updated_at = $3 WHERE id = $1 AND organization_id = $2 AND status IN ('pending', 'running', 'waiting_approval')`
	// Watchdog input: pin deadline_at on a non-terminal run.
	sqlSetWorkflowRunDeadline = `UPDATE workflow_runs SET deadline_at = $3, updated_at = NOW() WHERE id = $1 AND organization_id = $2 AND status IN ('pending', 'running', 'waiting_approval')`
	// Terminal timeout transition (watchdog), guarded against terminal rows.
	sqlTimeoutWorkflowRun = `UPDATE workflow_runs SET status = 'timeout', error_code = $3, finished_at = $4, updated_at = $4 WHERE id = $1 AND organization_id = $2 AND status IN ('pending', 'running', 'waiting_approval')`
	// Terminal completion/failure transition, guarded against terminal rows.
	sqlFinalizeWorkflowRun = `UPDATE workflow_runs SET status = $3, error_code = $4, finished_at = $5, updated_at = $5 WHERE id = $1 AND organization_id = $2 AND status IN ('pending', 'running', 'waiting_approval')`
	// Recovery candidate listings. FOR UPDATE SKIP LOCKED keeps concurrent
	// recovery passes from fighting over the same candidate rows inside the
	// selection transaction; the guarded claim UPDATEs above remain the
	// serialization point afterwards. An empty orgID sweeps every tenant
	// (internal worker path; never exposed via HTTP).
	sqlSelectStaleWorkflowRuns    = `SELECT id, workflow_id, organization_id, COALESCE(input, ''), status, COALESCE(created_by, ''), attempt, locked_at, heartbeat_at, finished_at, deadline_at, COALESCE(error_code, ''), created_at, updated_at FROM workflow_runs WHERE status IN ('pending', 'running', 'waiting_approval') AND COALESCE(heartbeat_at, updated_at) < $1 AND ($2 = '' OR organization_id = $2) ORDER BY updated_at ASC, id ASC LIMIT $3 FOR UPDATE SKIP LOCKED`
	sqlSelectTimedOutWorkflowRuns = `SELECT id, workflow_id, organization_id, COALESCE(input, ''), status, COALESCE(created_by, ''), attempt, locked_at, heartbeat_at, finished_at, deadline_at, COALESCE(error_code, ''), created_at, updated_at FROM workflow_runs WHERE status IN ('pending', 'running', 'waiting_approval') AND deadline_at IS NOT NULL AND deadline_at < $1 AND ($2 = '' OR organization_id = $2) ORDER BY deadline_at ASC, id ASC LIMIT $3 FOR UPDATE SKIP LOCKED`
)

// knownRunStatuses is the whitelist for dynamic status lists (claim source
// statuses come from in-process constants only; the guard rejects anything
// else before it can reach the SQL layer).
var knownRunStatuses = map[string]bool{
	RunStatusPending:         true,
	RunStatusRunning:         true,
	RunStatusWaitingApproval: true,
	RunStatusCompleted:       true,
	RunStatusFailed:          true,
	RunStatusCancelled:       true,
	RunStatusTimeout:         true,
}

// sqlClaimWorkflowRun builds the atomic recovery-claim UPDATE for the given
// source statuses: attempt bumps, the lease is stamped and a rescued
// waiting_approval run moves back to running. Statuses are whitelisted and
// interpolated as literals (they are compile-time constants, never user
// input) so the statement stays a single static shape for pg planning.
func sqlClaimWorkflowRun(fromStatuses []string) (string, error) {
	if len(fromStatuses) == 0 {
		return "", errors.New("workflows: claim requires at least one source status")
	}
	seen := make(map[string]bool, len(fromStatuses))
	list := ""
	for _, st := range fromStatuses {
		if !knownRunStatuses[st] {
			return "", fmt.Errorf("workflows: unknown workflow run status %q", st)
		}
		if seen[st] {
			continue
		}
		seen[st] = true
		if list != "" {
			list += ", "
		}
		list += "'" + st + "'"
	}
	return `UPDATE workflow_runs SET status = 'running', attempt = attempt + 1, locked_at = $3, heartbeat_at = $3, updated_at = $3 WHERE id = $1 AND organization_id = $2 AND status IN (` + list + `)`, nil
}

// rowsAffectedResult maps a guarded UPDATE to whether it matched a row.
func rowsAffectedResult(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// listWorkflowRunsInTx scans candidate rows from one transaction (the SELECT
// ... FOR UPDATE SKIP LOCKED listings share the row shape with
// sqlSelectWorkflowRunScoped).
func (s *pgStore) listWorkflowRunsInTx(ctx context.Context, query string, args ...any) ([]*WorkflowRun, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]*WorkflowRun, 0)
	for rows.Next() {
		wr, err := scanWorkflowRun(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, wr)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if err := tx.Commit(); err != nil {
		tx = nil
		return nil, err
	}
	tx = nil
	return out, nil
}

// ---------------------------------------------------------------------------
// pgStore: durable-execution Store surface (checkpointing + recovery).
// ---------------------------------------------------------------------------

// InsertNodeRun writes one per-attempt checkpoint row. created is false when
// the (workflow_run_id, node_id, attempt) row already existed: replayed tasks
// never duplicate a node execution.
func (s *pgStore) InsertNodeRun(ctx context.Context, orgID string, nr *NodeRun) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, sqlInsertNodeRunCheckpoint,
		nr.ID, nr.WorkflowRunID, orgID, nr.NodeID, nullableString(nr.RunID), nr.Status, nr.Error,
		nr.Attempt, nullableTime(nr.LockedAt), nullableTime(nr.HeartbeatAt), nr.ErrorCode,
		nullableTime(nr.StartedAt), nullableTime(nr.FinishedAt), nr.CreatedAt)
	if err != nil {
		return false, err
	}
	return rowsAffectedResult(res)
}

// LatestNodeRun returns the highest-attempt checkpoint of one node, or nil
// when the node has no checkpoint row yet.
func (s *pgStore) LatestNodeRun(ctx context.Context, orgID, workflowRunID, nodeID string) (*NodeRun, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	nr, err := scanNodeRun(s.db.QueryRowContext(ctx, sqlSelectLatestNodeRun, orgID, workflowRunID, nodeID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return nr, nil
}

// ClaimNodeRun atomically claims one non-terminal node run for execution
// (attempt + lease). claimed is false when the row went terminal or belongs
// to another worker's fresh lease window.
func (s *pgStore) ClaimNodeRun(ctx context.Context, orgID, nodeRunID, runID string, at time.Time) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, sqlClaimNodeRun, nodeRunID, orgID, runID, at)
	if err != nil {
		return false, err
	}
	return rowsAffectedResult(res)
}

// TouchNodeRun refreshes the heartbeat of one running node run.
func (s *pgStore) TouchNodeRun(ctx context.Context, orgID, nodeRunID string, at time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlTouchNodeRun, nodeRunID, orgID, at)
	return err
}

// MarkNodeRunStatus persists a terminal node-run transition; marked is false
// when the row was already terminal (idempotent finish).
func (s *pgStore) MarkNodeRunStatus(ctx context.Context, orgID, nodeRunID, status, errorCode string, at time.Time) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, sqlMarkNodeRunStatus, nodeRunID, orgID, status, errorCode, at)
	if err != nil {
		return false, err
	}
	return rowsAffectedResult(res)
}

// FailNonTerminalNodeRuns marks every pending/running checkpoint of one
// workflow run failed with the given machine error code (orphan/timeout pass)
// and returns how many rows changed.
func (s *pgStore) FailNonTerminalNodeRuns(ctx context.Context, orgID, workflowRunID, errorCode string, at time.Time) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, sqlFailNonTerminalNodeRuns, orgID, workflowRunID, errorCode, at)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchWorkflowRunHeartbeat refreshes the liveness stamp of one non-terminal
// workflow run.
func (s *pgStore) TouchWorkflowRunHeartbeat(ctx context.Context, orgID, workflowRunID string, at time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlTouchWorkflowRunHeartbeat, workflowRunID, orgID, at)
	return err
}

// SetWorkflowRunDeadline pins deadline_at on a non-terminal workflow run.
func (s *pgStore) SetWorkflowRunDeadline(ctx context.Context, orgID, workflowRunID string, deadline time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlSetWorkflowRunDeadline, workflowRunID, orgID, deadline)
	return err
}

// ClaimWorkflowRun atomically claims one workflow run for recovery: the run
// moves to running (recovered work is live again, including rescued
// waiting_approval runs), attempt bumps and the lease is stamped. claimed is
// false when the run left the source statuses (another pass won, or the run
// went terminal).
func (s *pgStore) ClaimWorkflowRun(ctx context.Context, orgID, workflowRunID string, fromStatuses []string, at time.Time) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	query, err := sqlClaimWorkflowRun(fromStatuses)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, query, workflowRunID, orgID, at)
	if err != nil {
		return false, err
	}
	return rowsAffectedResult(res)
}

// TimeoutWorkflowRun transitions one non-terminal workflow run to the
// terminal 'timeout' status; timedOut is false when it was already terminal.
func (s *pgStore) TimeoutWorkflowRun(ctx context.Context, orgID, workflowRunID, errorCode string, at time.Time) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, sqlTimeoutWorkflowRun, workflowRunID, orgID, errorCode, at)
	if err != nil {
		return false, err
	}
	return rowsAffectedResult(res)
}

// FinalizeWorkflowRun transitions one non-terminal workflow run to a terminal
// status (completed/failed); finalized is false when it was already terminal.
func (s *pgStore) FinalizeWorkflowRun(ctx context.Context, orgID, workflowRunID, status, errorCode string, at time.Time) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, sqlFinalizeWorkflowRun, workflowRunID, orgID, status, errorCode, at)
	if err != nil {
		return false, err
	}
	return rowsAffectedResult(res)
}

// StaleWorkflowRuns lists non-terminal workflow runs whose heartbeat (or
// updated_at for legacy rows) is older than cutoff, selected FOR UPDATE SKIP
// LOCKED inside one transaction so concurrent recovery passes never fight
// over the same candidates. An empty orgID sweeps every tenant (internal
// worker path only).
func (s *pgStore) StaleWorkflowRuns(ctx context.Context, orgID string, cutoff time.Time, limit int) ([]*WorkflowRun, error) {
	if limit <= 0 {
		limit = RecoveryBatchLimit
	}
	return s.listWorkflowRunsInTx(ctx, sqlSelectStaleWorkflowRuns, cutoff, orgID, limit)
}

// TimedOutWorkflowRuns lists non-terminal workflow runs past their deadline_at
// (watchdog candidates) with the same SKIP LOCKED semantics.
func (s *pgStore) TimedOutWorkflowRuns(ctx context.Context, orgID string, now time.Time, limit int) ([]*WorkflowRun, error) {
	if limit <= 0 {
		limit = RecoveryBatchLimit
	}
	return s.listWorkflowRunsInTx(ctx, sqlSelectTimedOutWorkflowRuns, now, orgID, limit)
}
