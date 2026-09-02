package evaluations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Store abstracts durable evaluation storage. Implementations MUST scope
// every tenant-facing query by organization_id; foreign rows surface as
// ErrDatasetNotFound / ErrRunNotFound (never cross-tenant leaks).
type Store interface {
	// CreateDataset inserts the dataset row plus all of its case rows atomically.
	CreateDataset(ctx context.Context, dataset *Dataset) error
	// GetDataset fetches one dataset row within one tenant.
	GetDataset(ctx context.Context, orgID, id string) (*Dataset, error)
	// ListDatasets returns the datasets of one tenant with case counts.
	ListDatasets(ctx context.Context, orgID string) ([]*Dataset, error)
	// GetDatasetCases returns the ordered cases of one dataset within one tenant.
	GetDatasetCases(ctx context.Context, orgID, datasetID string) ([]Case, error)
	// CreateRun inserts an eval run row within one tenant.
	CreateRun(ctx context.Context, run *EvalRun) error
	// UpdateRunStatus transitions a run status within one tenant (guard rejects
	// unknown runs or foreign tenants with ErrRunNotFound).
	UpdateRunStatus(ctx context.Context, orgID, runID, status string, completedAt *time.Time) error
	// CreateResults persists every result row of one run atomically.
	CreateResults(ctx context.Context, orgID, runID string, results []Result) error
	// GetRun fetches one eval run row within one tenant.
	GetRun(ctx context.Context, orgID, id string) (*EvalRun, error)
	// ListResults returns the ordered results of one run within one tenant.
	ListResults(ctx context.Context, orgID, runID string) ([]Result, error)
}

const (
	// Tenant guard: datasets are inserted with their organization_id scope.
	sqlInsertDataset = `INSERT INTO eval_datasets (id, organization_id, name, description, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6)`
	// Tenant guard: case rows carry organization_id + a dataset FK; position
	// preserves the caller's case order.
	sqlInsertCase = `INSERT INTO eval_cases (dataset_id, case_id, organization_id, position, input, expected, scorer, params, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)`
	// Tenant guard: single-dataset reads are scoped to one organization_id.
	sqlSelectDataset = `SELECT id, organization_id, name, description, created_at, updated_at FROM eval_datasets WHERE id = $1 AND organization_id = $2`
	// Tenant guard: listings filter on organization_id (+created_at index);
	// case_count comes from a correlated subquery over eval_cases.
	sqlSelectDatasetsByOrg = `SELECT d.id, d.organization_id, d.name, d.description, d.created_at, d.updated_at, (SELECT COUNT(*) FROM eval_cases c WHERE c.dataset_id = d.id) AS case_count FROM eval_datasets d WHERE d.organization_id = $1 ORDER BY d.created_at DESC`
	// Tenant guard: case reads join the dataset scope through organization_id.
	sqlSelectCases = `SELECT case_id, input, expected, scorer, COALESCE(params::text, ''), position FROM eval_cases WHERE dataset_id = $1 AND organization_id = $2 ORDER BY position ASC, case_id ASC`
	// Tenant guard: eval runs are inserted with their organization_id scope.
	sqlInsertRun = `INSERT INTO eval_runs (id, organization_id, dataset_id, agent_id, status, created_at) VALUES ($1, $2, $3, $4, $5, $6)`
	// Tenant guard: result rows carry organization_id + case_index order.
	sqlInsertResult = `INSERT INTO eval_results (id, run_id, organization_id, case_id, scorer, case_index, output, passed, score, latency_ms, cost_cents, error, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	// Tenant guard: status transitions require a matching organization_id.
	sqlUpdateRunStatus = `UPDATE eval_runs SET status = $1, completed_at = $2 WHERE id = $3 AND organization_id = $4`
	// Tenant guard: single-run reads are scoped to one organization_id.
	sqlSelectRun = `SELECT id, organization_id, dataset_id, agent_id, status, created_at, completed_at FROM eval_runs WHERE id = $1 AND organization_id = $2`
	// Tenant guard: result listings filter on organization_id and preserve
	// the execution order via case_index.
	sqlSelectResults = `SELECT id, case_id, scorer, output, passed, score, latency_ms, cost_cents, error FROM eval_results WHERE run_id = $1 AND organization_id = $2 ORDER BY case_index ASC, case_id ASC`
)

// pgStore is the Postgres-backed Store implementation.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("evaluations: database is nil")
	}
	return nil
}

// CreateDataset inserts the dataset row and all case rows in one transaction
// (all-or-nothing, so a dataset never exists without its cases).
func (s *pgStore) CreateDataset(ctx context.Context, dataset *Dataset) error {
	if err := s.guard(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, sqlInsertDataset,
		dataset.ID, dataset.OrganizationID, dataset.Name, dataset.Description,
		dataset.CreatedAt, dataset.UpdatedAt); err != nil {
		return err
	}
	for i, c := range dataset.Cases {
		if _, err := tx.ExecContext(ctx, sqlInsertCase,
			dataset.ID, c.ID, dataset.OrganizationID, i, c.Input, c.Expected,
			string(c.Scorer), paramsParam(c.Params), dataset.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *pgStore) GetDataset(ctx context.Context, orgID, id string) (*Dataset, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	return scanDataset(s.db.QueryRowContext(ctx, sqlSelectDataset, id, orgID))
}

func (s *pgStore) ListDatasets(ctx context.Context, orgID string) ([]*Dataset, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectDatasetsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Dataset, 0)
	for rows.Next() {
		var d Dataset
		if err := rows.Scan(&d.ID, &d.OrganizationID, &d.Name, &d.Description,
			&d.CreatedAt, &d.UpdatedAt, &d.CaseCount); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

func (s *pgStore) GetDatasetCases(ctx context.Context, orgID, datasetID string) ([]Case, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE dataset_id = $1 AND organization_id = $2
	rows, err := s.db.QueryContext(ctx, sqlSelectCases, datasetID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Case, 0)
	for rows.Next() {
		var c Case
		var scorer, paramsRaw string
		if err := rows.Scan(&c.ID, &c.Input, &c.Expected, &scorer, &paramsRaw, new(int)); err != nil {
			return nil, err
		}
		c.Scorer = Scorer(scorer)
		c.Params = unmarshalParams(paramsRaw)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *pgStore) CreateRun(ctx context.Context, run *EvalRun) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertRun,
		run.ID, run.OrganizationID, run.DatasetID, run.AgentID,
		run.Status, run.CreatedAt)
	return err
}

func (s *pgStore) UpdateRunStatus(ctx context.Context, orgID, runID, status string, completedAt *time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateRunStatus, status, nullableTime(completedAt), runID, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (wrong org or unknown run)
		return ErrRunNotFound
	}
	return nil
}

// CreateResults inserts every result row of one run in a single transaction.
func (s *pgStore) CreateResults(ctx context.Context, orgID, runID string, results []Result) error {
	if err := s.guard(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	for i, r := range results {
		id := r.ID
		if id == "" {
			id = uuid.NewString()
		}
		if _, err := tx.ExecContext(ctx, sqlInsertResult,
			id, runID, orgID, r.CaseID, string(r.Scorer), i, r.Output,
			r.Passed, r.Score, r.LatencyMS, r.CostCents, r.Error, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *pgStore) GetRun(ctx context.Context, orgID, id string) (*EvalRun, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	return scanRun(s.db.QueryRowContext(ctx, sqlSelectRun, id, orgID))
}

func (s *pgStore) ListResults(ctx context.Context, orgID, runID string) ([]Result, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE run_id = $1 AND organization_id = $2
	rows, err := s.db.QueryContext(ctx, sqlSelectResults, runID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Result, 0)
	for rows.Next() {
		var r Result
		var scorer string
		if err := rows.Scan(&r.ID, &r.CaseID, &scorer, &r.Output, &r.Passed,
			&r.Score, &r.LatencyMS, &r.CostCents, &r.Error); err != nil {
			return nil, err
		}
		r.Scorer = Scorer(scorer)
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanDataset(scanner interface{ Scan(dest ...any) error }) (*Dataset, error) {
	var d Dataset
	if err := scanner.Scan(&d.ID, &d.OrganizationID, &d.Name, &d.Description,
		&d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDatasetNotFound
		}
		return nil, err
	}
	return &d, nil
}

func scanRun(scanner interface{ Scan(dest ...any) error }) (*EvalRun, error) {
	var r EvalRun
	var status string
	var completedAt sql.NullTime
	if err := scanner.Scan(&r.ID, &r.OrganizationID, &r.DatasetID, &r.AgentID,
		&status, &r.CreatedAt, &completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	r.Status = status
	if completedAt.Valid {
		t := completedAt.Time
		r.CompletedAt = &t
	}
	return &r, nil
}

// paramsParam marshals Params for the JSONB column; the zero value stays NULL.
func paramsParam(p Params) any {
	if p.Pattern == "" && p.ThresholdMS == nil && p.ThresholdCents == nil {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return string(b)
}

func unmarshalParams(raw string) Params {
	var p Params
	if strings.TrimSpace(raw) == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Params{}
	}
	return p
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}
