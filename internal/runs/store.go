package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	// Tenant guard: runs are inserted with their organization_id scope.
	sqlInsertRun = `INSERT INTO runs (id, organization_id, agent_id, status, input, output, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	// Tenant guard: single-run reads are scoped to one organization_id.
	sqlSelectRunScoped = `SELECT id, organization_id, agent_id, COALESCE(input, ''), COALESCE(output, ''), status, COALESCE(cost_cents, 0), created_at, updated_at FROM runs WHERE id = $1 AND organization_id = $2`
	// Trusted internal PK lookup (worker path); the returned row carries
	// organization_id so callers can enforce tenancy.
	sqlSelectRunByID = `SELECT id, organization_id, agent_id, COALESCE(input, ''), COALESCE(output, ''), status, COALESCE(cost_cents, 0), created_at, updated_at FROM runs WHERE id = $1`
	// Tenant guard: listings filter on organization_id (+created_at index).
	sqlSelectRunsByOrg = `SELECT id, organization_id, agent_id, COALESCE(input, ''), COALESCE(output, ''), status, COALESCE(cost_cents, 0), created_at, updated_at FROM runs WHERE organization_id = $1 ORDER BY created_at DESC`
	// Tenant guard: status transitions require a matching organization_id.
	sqlUpdateRunStatus = `UPDATE runs SET status = $1, output = CASE WHEN $2 = '' THEN output ELSE $2 END, updated_at = $3 WHERE id = $4 AND organization_id = $5`
	// Tenant guard: the INSERT ... SELECT ... WHERE EXISTS clause only accepts
	// rows when the run belongs to the caller's organization_id; step_index is
	// derived from the existing steps of the same run. The single statement is
	// atomic: the step row insert (cost + contract-canonical cost_cents carry
	// the same value) and the runs.cost_cents total bump succeed or fail
	// together, so a step cost can never be recorded without its total.
	sqlInsertRunStep = `WITH step_insert AS (
    INSERT INTO run_steps (id, run_id, step_index, step_type, status, input_meta, output_meta, error, token_usage, cost, cost_cents, started_at, completed_at, created_at)
    SELECT $1, $2, (SELECT COALESCE(MAX(step_index), 0) + 1 FROM run_steps WHERE run_id = $2), $3, $4, $5::jsonb, $6::jsonb, $7, $8::jsonb, $9, $9, $10, $11, $12
    WHERE EXISTS (SELECT 1 FROM runs WHERE id = $2 AND organization_id = $13)
    RETURNING run_id
), run_bump AS (
    UPDATE runs SET cost_cents = runs.cost_cents + $9
    WHERE id = (SELECT run_id FROM step_insert) AND organization_id = $13
)
SELECT (SELECT COUNT(*) FROM step_insert) + (SELECT COUNT(*) FROM run_bump) AS affected`
	// Tenant guard: step listings join runs and filter on organization_id.
	sqlSelectRunSteps = `SELECT rs.id, rs.run_id, rs.step_type, rs.status, COALESCE(rs.input_meta::text, ''), COALESCE(rs.output_meta::text, ''), COALESCE(rs.error, ''), COALESCE(rs.token_usage::text, ''), rs.cost, rs.started_at, rs.completed_at, rs.created_at FROM run_steps rs JOIN runs r ON r.id = rs.run_id WHERE r.organization_id = $1 AND rs.run_id = $2 ORDER BY rs.step_index ASC`
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
		return errors.New("runs: database is nil")
	}
	return nil
}

func (s *pgStore) CreateRun(ctx context.Context, run *Run) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertRun,
		run.ID, run.OrganizationID, run.AgentID, string(run.Status),
		run.Input, run.Output, run.CreatedAt, run.UpdatedAt)
	return err
}

func (s *pgStore) GetRun(ctx context.Context, orgID, id string) (*Run, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	return scanRun(s.db.QueryRowContext(ctx, sqlSelectRunScoped, id, orgID))
}

func (s *pgStore) GetRunByID(ctx context.Context, id string) (*Run, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanRun(s.db.QueryRowContext(ctx, sqlSelectRunByID, id))
}

func (s *pgStore) ListRuns(ctx context.Context, orgID string) ([]*Run, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectRunsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateRunStatus(ctx context.Context, orgID, id string, status RunStatus, output string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateRunStatus, string(status), output, time.Now().UTC(), id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (wrong org or unknown run)
		return ErrRunNotFound
	}
	return nil
}

func (s *pgStore) InsertStep(ctx context.Context, orgID string, step *Step) error {
	if err := s.guard(); err != nil {
		return err
	}
	if step.ID == "" {
		step.ID = uuid.NewString()
	}
	// Atomic single statement (see sqlInsertRunStep): step row + runs cost
	// total bump. affected == 0 means the tenant guard (WHERE EXISTS) or
	// the run lookup rejected the write: run not in this org.
	var affected int64
	if err := s.db.QueryRowContext(ctx, sqlInsertRunStep,
		step.ID, step.RunID, step.StepType, step.Status,
		jsonParam(step.InputMeta), jsonParam(step.OutputMeta), step.Error,
		jsonParam(step.TokenUsage), step.Cost,
		nullableTime(step.StartedAt), nullableTime(step.CompletedAt), step.CreatedAt,
		orgID).Scan(&affected); err != nil {
		return err
	}
	if affected == 0 {
		return ErrRunNotFound
	}
	return nil
}

func (s *pgStore) ListSteps(ctx context.Context, orgID, runID string) ([]*Step, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: JOIN runs + WHERE r.organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectRunSteps, orgID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Step, 0)
	for rows.Next() {
		var step Step
		var inputMeta, outputMeta, tokenUsage, stepErr string
		var startedAt, completedAt sql.NullTime
		if err := rows.Scan(&step.ID, &step.RunID, &step.StepType, &step.Status, &inputMeta, &outputMeta, &stepErr, &tokenUsage, &step.Cost, &startedAt, &completedAt, &step.CreatedAt); err != nil {
			return nil, err
		}
		step.Error = stepErr
		step.InputMeta = unmarshalJSONMap(inputMeta)
		step.OutputMeta = unmarshalJSONMap(outputMeta)
		step.TokenUsage = unmarshalJSONMap(tokenUsage)
		step.StartedAt = startedAt.Time
		step.CompletedAt = completedAt.Time
		out = append(out, &step)
	}
	return out, rows.Err()
}

func scanRun(scanner interface{ Scan(dest ...any) error }) (*Run, error) {
	var run Run
	var status string
	if err := scanner.Scan(&run.ID, &run.OrganizationID, &run.AgentID, &run.Input, &run.Output, &status, &run.TotalCostCents, &run.CreatedAt, &run.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	run.Status = RunStatus(status)
	return &run, nil
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

func unmarshalJSONMap(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
