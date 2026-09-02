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
	sqlInsertWorkflowRun = `INSERT INTO workflow_runs (id, workflow_id, organization_id, input, status, created_by, created_at, updated_at) SELECT $1, $2, $3, $4, $5, $6, $7, $8 WHERE EXISTS (SELECT 1 FROM workflows WHERE id = $2 AND organization_id = $3)`
	// Tenant guard: single workflow-run reads are scoped to one organization_id.
	sqlSelectWorkflowRunScoped = `SELECT id, workflow_id, organization_id, COALESCE(input, ''), status, COALESCE(created_by, ''), created_at, updated_at FROM workflow_runs WHERE id = $1 AND organization_id = $2`
	// Tenant guard: workflow-run status transitions require the organization_id guard.
	sqlUpdateWorkflowRunStatus = `UPDATE workflow_runs SET status = $1, updated_at = $2 WHERE id = $3 AND organization_id = $4`
	// Tenant guard: node runs are inserted with their organization_id scope.
	sqlInsertNodeRun = `INSERT INTO workflow_node_runs (id, workflow_run_id, organization_id, node_id, run_id, status, error, started_at, finished_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	// Tenant guard: node-run listings join workflow_runs scoped by organization_id.
	sqlSelectNodeRunsByWorkflowRun = `SELECT id, workflow_run_id, node_id, COALESCE(run_id, ''), status, COALESCE(error, ''), started_at, finished_at, created_at FROM workflow_node_runs WHERE organization_id = $1 AND workflow_run_id = $2 ORDER BY created_at ASC, id ASC`
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
		wr.ID, wr.WorkflowID, orgID, wr.Input, wr.Status, wr.CreatedBy, wr.CreatedAt, wr.UpdatedAt)
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
	var wr WorkflowRun
	if err := s.db.QueryRowContext(ctx, sqlSelectWorkflowRunScoped, id, orgID).Scan(
		&wr.ID, &wr.WorkflowID, &wr.OrganizationID, &wr.Input, &wr.Status, &wr.CreatedBy, &wr.CreatedAt, &wr.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrWorkflowRunNotFound
		}
		return nil, err
	}
	return &wr, nil
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
		var nr NodeRun
		var runID string
		var startedAt, finishedAt sql.NullTime
		if err := rows.Scan(&nr.ID, &nr.WorkflowRunID, &nr.NodeID, &runID, &nr.Status, &nr.Error, &startedAt, &finishedAt, &nr.CreatedAt); err != nil {
			return nil, err
		}
		nr.RunID = runID
		if startedAt.Valid {
			t := startedAt.Time
			nr.StartedAt = &t
		}
		if finishedAt.Valid {
			t := finishedAt.Time
			nr.FinishedAt = &t
		}
		out = append(out, &nr)
	}
	return out, rows.Err()
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
