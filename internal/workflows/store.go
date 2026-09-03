package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	// Tenant guard: workflows are inserted with their organization_id scope.
	// The `definition` column (NOT NULL since migration 004) stores the DSL.
	sqlInsertWorkflow = `INSERT INTO workflows (id, organization_id, name, description, status, current_version, definition, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	// Tenant guard: single-workflow reads are scoped to one organization_id.
	sqlSelectWorkflowScoped = `SELECT id, organization_id, name, COALESCE(description, ''), status, current_version, COALESCE(definition::text, '{}'), created_at, updated_at FROM workflows WHERE id = $1 AND organization_id = $2`
	// Tenant guard: listings filter on organization_id (+created_at index).
	sqlSelectWorkflowsByOrg = `SELECT id, organization_id, name, COALESCE(description, ''), status, current_version, COALESCE(definition::text, '{}'), created_at, updated_at FROM workflows WHERE organization_id = $1 ORDER BY created_at DESC`
	// Tenant guard: publish transitions require a matching organization_id.
	sqlUpdateWorkflowStatus = `UPDATE workflows SET status = $1, current_version = $2, updated_at = $3 WHERE id = $4 AND organization_id = $5`
	// Tenant guard: versions are inserted with their organization_id scope.
	sqlInsertVersion = `INSERT INTO workflow_versions (id, workflow_id, organization_id, version, status, dsl_snapshot, published_by, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	// Tenant guard: version listings are scoped to one organization_id.
	sqlSelectVersionsByWorkflow = `SELECT id, workflow_id, version, status, COALESCE(dsl_snapshot::text, '{}'), COALESCE(published_by, ''), created_at FROM workflow_versions WHERE organization_id = $1 AND workflow_id = $2 ORDER BY version ASC`
	// Tenant guard: the INSERT ... SELECT ... WHERE EXISTS clause only accepts
	// rows when the workflow belongs to the caller's organization_id.
	sqlInsertWorkflowRun = `INSERT INTO workflow_runs (id, workflow_id, organization_id, input, status, created_by, attempt, locked_at, heartbeat_at, finished_at, deadline_at, error_code, created_at, updated_at) SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14 WHERE EXISTS (SELECT 1 FROM workflows WHERE id = $2 AND organization_id = $3)`
	// Tenant guard: single workflow-run reads are scoped to one organization_id.
	sqlSelectWorkflowRunScoped = `SELECT id, workflow_id, organization_id, COALESCE(input, ''), status, COALESCE(created_by, ''), attempt, locked_at, heartbeat_at, finished_at, deadline_at, COALESCE(error_code, ''), created_at, updated_at FROM workflow_runs WHERE id = $1 AND organization_id = $2`
	// Tenant guard: workflow-run status transitions require the organization_id guard.
	sqlUpdateWorkflowRunStatus = `UPDATE workflow_runs SET status = $1, updated_at = $2 WHERE id = $3 AND organization_id = $4`
	// Tenant guard: node runs are inserted with their organization_id scope.
	sqlInsertNodeRun = `INSERT INTO workflow_node_runs (id, workflow_run_id, organization_id, node_id, run_id, status, error, attempt, locked_at, heartbeat_at, error_code, started_at, finished_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`
	// Tenant guard: node-run listings join workflow_runs scoped by organization_id.
	sqlSelectNodeRunsByWorkflowRun = `SELECT id, workflow_run_id, node_id, COALESCE(run_id, ''), status, COALESCE(error, ''), attempt, locked_at, heartbeat_at, COALESCE(error_code, ''), started_at, finished_at, created_at FROM workflow_node_runs WHERE organization_id = $1 AND workflow_run_id = $2 ORDER BY created_at ASC, attempt ASC, id ASC`
)

// Store abstracts durable workflow storage. Every tenant-facing query is
// guarded by organization_id.
type Store interface {
	// CreateWorkflow inserts the workflow row with its organization_id scope.
	CreateWorkflow(ctx context.Context, orgID string, wf *Workflow) error
	// ListWorkflows returns the workflows of exactly one tenant.
	ListWorkflows(ctx context.Context, orgID string) ([]*Workflow, error)
	// GetWorkflow fetches one workflow strictly within one tenant.
	GetWorkflow(ctx context.Context, orgID, id string) (*Workflow, error)
	// UpdateWorkflowStatus persists a publish transition within one tenant.
	UpdateWorkflowStatus(ctx context.Context, orgID, id, status string, currentVersion int, updatedAt time.Time) error
	// CreateVersion inserts one immutable published version.
	CreateVersion(ctx context.Context, orgID string, v *Version) error
	// ListVersions returns the published versions of one workflow.
	ListVersions(ctx context.Context, orgID, workflowID string) ([]*Version, error)
	// CreateWorkflowRun inserts one workflow run (guarded against foreign
	// workflows via WHERE EXISTS on organization_id).
	CreateWorkflowRun(ctx context.Context, orgID string, wr *WorkflowRun) error
	// GetWorkflowRun fetches one workflow run strictly within one tenant.
	GetWorkflowRun(ctx context.Context, orgID, id string) (*WorkflowRun, error)
	// UpdateWorkflowRunStatus transitions a workflow run within one tenant.
	UpdateWorkflowRunStatus(ctx context.Context, orgID, id, status string, updatedAt time.Time) error
	// CreateNodeRun inserts one node -> run mapping row.
	CreateNodeRun(ctx context.Context, orgID string, nr *NodeRun) error
	// ListNodeRuns returns the node runs of one workflow run.
	ListNodeRuns(ctx context.Context, orgID, workflowRunID string) ([]*NodeRun, error)

	// -----------------------------------------------------------------------
	// Durable-execution surface (wave-3 track 3-c). The recovery/checkpoint
	// SQL lives in store_durable.go; see DurableStore below for the contract.
	// -----------------------------------------------------------------------
	// InsertNodeRun writes one per-attempt checkpoint row keyed by
	// (workflow_run_id, node_id, attempt); created reports whether the row was
	// newly inserted (false = the attempt already existed).
	InsertNodeRun(ctx context.Context, orgID string, nr *NodeRun) (created bool, err error)
	// LatestNodeRun returns the highest-attempt node run of one node, or nil
	// when the node has no checkpoint row yet.
	LatestNodeRun(ctx context.Context, orgID, workflowRunID, nodeID string) (*NodeRun, error)
	// ClaimNodeRun atomically moves one non-terminal node run to running and
	// stamps locked_at/heartbeat_at; claimed is false when the row already
	// reached a terminal state (lost race).
	ClaimNodeRun(ctx context.Context, orgID, nodeRunID, runID string, at time.Time) (claimed bool, err error)
	// TouchNodeRun refreshes the heartbeat of one running node run.
	TouchNodeRun(ctx context.Context, orgID, nodeRunID string, at time.Time) error
	// MarkNodeRunStatus persists a terminal node-run transition guarded
	// against already-terminal rows (idempotent finish); marked is false when
	// the row was already terminal.
	MarkNodeRunStatus(ctx context.Context, orgID, nodeRunID, status, errorCode string, at time.Time) (marked bool, err error)
	// FailNonTerminalNodeRuns marks every pending/running node run of one
	// workflow run failed with the given machine error code (orphan pass).
	FailNonTerminalNodeRuns(ctx context.Context, orgID, workflowRunID, errorCode string, at time.Time) (int64, error)
	// TouchWorkflowRunHeartbeat refreshes the liveness stamp of one
	// non-terminal workflow run.
	TouchWorkflowRunHeartbeat(ctx context.Context, orgID, workflowRunID string, at time.Time) error
	// SetWorkflowRunDeadline sets the wall-clock budget of one non-terminal
	// workflow run (watchdog input).
	SetWorkflowRunDeadline(ctx context.Context, orgID, workflowRunID string, deadline time.Time) error
	// ClaimWorkflowRun atomically claims one workflow run for recovery from
	// the given (non-terminal) statuses: the run moves to running, attempt is
	// bumped and locked_at/heartbeat_at stamped. claimed is false when the
	// status changed (another pass won or the run went terminal).
	ClaimWorkflowRun(ctx context.Context, orgID, workflowRunID string, fromStatuses []string, at time.Time) (claimed bool, err error)
	// TimeoutWorkflowRun transitions one workflow run to the terminal
	// 'timeout' status (watchdog); timedOut is false when the run already
	// reached a terminal state.
	TimeoutWorkflowRun(ctx context.Context, orgID, workflowRunID, errorCode string, at time.Time) (timedOut bool, err error)
	// FinalizeWorkflowRun transitions one workflow run to a terminal status
	// (completed/failed) once its nodes have converged; finalized is false
	// when the run already reached a terminal state.
	FinalizeWorkflowRun(ctx context.Context, orgID, workflowRunID, status, errorCode string, at time.Time) (finalized bool, err error)
	// StaleWorkflowRuns lists non-terminal workflow runs whose heartbeat (or
	// updated_at for legacy rows) is older than cutoff. Rows are selected
	// FOR UPDATE SKIP LOCKED so concurrent recovery passes never fight over
	// the same candidate. An empty orgID sweeps every tenant (internal
	// worker path; never exposed via HTTP).
	StaleWorkflowRuns(ctx context.Context, orgID string, cutoff time.Time, limit int) ([]*WorkflowRun, error)
	// TimedOutWorkflowRuns lists non-terminal workflow runs past their
	// deadline_at (watchdog candidates) with the same SKIP LOCKED semantics.
	TimedOutWorkflowRuns(ctx context.Context, orgID string, now time.Time, limit int) ([]*WorkflowRun, error)
}

// pgStore is the Postgres-backed Store implementation (lib/pq driver).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB.
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("workflows: database is nil")
	}
	return nil
}

func (s *pgStore) CreateWorkflow(ctx context.Context, orgID string, wf *Workflow) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertWorkflow,
		wf.ID, orgID, wf.Name, wf.Description, wf.Status, wf.CurrentVersion,
		jsonParam(wf.DSL), wf.CreatedAt, wf.UpdatedAt)
	return err
}

func (s *pgStore) ListWorkflows(ctx context.Context, orgID string) ([]*Workflow, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectWorkflowsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Workflow, 0)
	for rows.Next() {
		wf, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wf)
	}
	return out, rows.Err()
}

func (s *pgStore) GetWorkflow(ctx context.Context, orgID, id string) (*Workflow, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanWorkflow(s.db.QueryRowContext(ctx, sqlSelectWorkflowScoped, id, orgID))
}

func (s *pgStore) UpdateWorkflowStatus(ctx context.Context, orgID, id, status string, currentVersion int, updatedAt time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateWorkflowStatus, status, currentVersion, updatedAt, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrWorkflowNotFound
	}
	return nil
}

func (s *pgStore) CreateVersion(ctx context.Context, orgID string, v *Version) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertVersion,
		v.ID, v.WorkflowID, orgID, v.Version, v.Status, jsonParam(v.DSL), v.PublishedBy, v.CreatedAt)
	return err
}

func (s *pgStore) ListVersions(ctx context.Context, orgID, workflowID string) ([]*Version, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectVersionsByWorkflow, orgID, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Version, 0)
	for rows.Next() {
		var v Version
		var dslRaw string
		if err := rows.Scan(&v.ID, &v.WorkflowID, &v.Version, &v.Status, &dslRaw, &v.PublishedBy, &v.CreatedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrWorkflowNotFound
			}
			return nil, err
		}
		v.OrganizationID = orgID
		v.DSL = unmarshalDSL(dslRaw)
		out = append(out, &v)
	}
	return out, rows.Err()
}

func (s *pgStore) CreateWorkflowRun(ctx context.Context, orgID string, wr *WorkflowRun) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlInsertWorkflowRun,
		wr.ID, wr.WorkflowID, orgID, wr.Input, wr.Status, wr.CreatedBy,
		wr.Attempt, nullableTime(wr.LockedAt), nullableTime(wr.HeartbeatAt), nullableTime(wr.FinishedAt),
		nullableTime(wr.DeadlineAt), wr.ErrorCode, wr.CreatedAt, wr.UpdatedAt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard (WHERE EXISTS) rejected the row: workflow not in this org
		return ErrWorkflowNotFound
	}
	return nil
}

func (s *pgStore) GetWorkflowRun(ctx context.Context, orgID, id string) (*WorkflowRun, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanWorkflowRun(s.db.QueryRowContext(ctx, sqlSelectWorkflowRunScoped, id, orgID))
}

func (s *pgStore) UpdateWorkflowRunStatus(ctx context.Context, orgID, id, status string, updatedAt time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateWorkflowRunStatus, status, updatedAt, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrWorkflowRunNotFound
	}
	return nil
}

func (s *pgStore) CreateNodeRun(ctx context.Context, orgID string, nr *NodeRun) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertNodeRun,
		nr.ID, nr.WorkflowRunID, orgID, nr.NodeID, nullableString(nr.RunID), nr.Status, nr.Error,
		nr.Attempt, nullableTime(nr.LockedAt), nullableTime(nr.HeartbeatAt), nr.ErrorCode,
		nullableTime(nr.StartedAt), nullableTime(nr.FinishedAt), nr.CreatedAt)
	return err
}

func (s *pgStore) ListNodeRuns(ctx context.Context, orgID, workflowRunID string) ([]*NodeRun, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectNodeRunsByWorkflowRun, orgID, workflowRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*NodeRun, 0)
	for rows.Next() {
		nr, err := scanNodeRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, nr)
	}
	return out, rows.Err()
}

// scanWorkflowRun reads one workflow_runs row including the durable-execution
// columns added by migration 013 (attempt/locked_at/heartbeat_at/finished_at/
// deadline_at/error_code).
func scanWorkflowRun(scanner interface{ Scan(dest ...any) error }) (*WorkflowRun, error) {
	var wr WorkflowRun
	var lockedAt, heartbeatAt, finishedAt, deadlineAt sql.NullTime
	if err := scanner.Scan(
		&wr.ID, &wr.WorkflowID, &wr.OrganizationID, &wr.Input, &wr.Status, &wr.CreatedBy,
		&wr.Attempt, &lockedAt, &heartbeatAt, &finishedAt, &deadlineAt, &wr.ErrorCode,
		&wr.CreatedAt, &wr.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrWorkflowRunNotFound
		}
		return nil, err
	}
	nullTimePtr(lockedAt, &wr.LockedAt)
	nullTimePtr(heartbeatAt, &wr.HeartbeatAt)
	nullTimePtr(finishedAt, &wr.FinishedAt)
	nullTimePtr(deadlineAt, &wr.DeadlineAt)
	return &wr, nil
}

// scanNodeRun reads one workflow_node_runs row including the per-attempt
// checkpoint columns added by migration 013.
func scanNodeRun(scanner interface{ Scan(dest ...any) error }) (*NodeRun, error) {
	var nr NodeRun
	var runID string
	var lockedAt, heartbeatAt, startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(
		&nr.ID, &nr.WorkflowRunID, &nr.NodeID, &runID, &nr.Status, &nr.Error,
		&nr.Attempt, &lockedAt, &heartbeatAt, &nr.ErrorCode, &startedAt, &finishedAt, &nr.CreatedAt,
	); err != nil {
		return nil, err
	}
	nr.RunID = runID
	nullTimePtr(lockedAt, &nr.LockedAt)
	nullTimePtr(heartbeatAt, &nr.HeartbeatAt)
	nullTimePtr(startedAt, &nr.StartedAt)
	nullTimePtr(finishedAt, &nr.FinishedAt)
	return &nr, nil
}

// nullTimePtr copies a nullable SQL timestamp into an optional Go pointer.
func nullTimePtr(src sql.NullTime, dst **time.Time) {
	if src.Valid {
		t := src.Time
		*dst = &t
		return
	}
	*dst = nil
}

func scanWorkflow(scanner interface{ Scan(dest ...any) error }) (*Workflow, error) {
	var wf Workflow
	var dslRaw string
	if err := scanner.Scan(&wf.ID, &wf.OrganizationID, &wf.Name, &wf.Description, &wf.Status, &wf.CurrentVersion, &dslRaw, &wf.CreatedAt, &wf.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrWorkflowNotFound
		}
		return nil, err
	}
	wf.DSL = unmarshalDSL(dslRaw)
	return &wf, nil
}

// jsonParam marshals a value for a JSONB column; nil stays SQL NULL.
func jsonParam(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

func unmarshalDSL(raw string) DSL {
	var dsl DSL
	if err := json.Unmarshal([]byte(raw), &dsl); err != nil {
		return DSL{}
	}
	return dsl
}

// nullableString maps empty strings to SQL NULL for optional columns.
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// nullableTime maps a nil/zero timestamp to SQL NULL.
func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}
