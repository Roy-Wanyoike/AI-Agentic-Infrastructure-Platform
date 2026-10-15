package synth

// store.go — issue #147: Postgres-backed Store over the migration 034
// registry tables (synth_projects, synth_targets, synth_environments,
// synth_personas). journeys/runs/findings rows belong to the sibling
// tracks (#148-#156) and are deliberately untouched here.
//
// Tenant isolation rule: EVERY statement is guarded by organization_id.
// A lookup/update/delete addressed by a foreign org is indistinguishable
// from an unknown id — both surface as synth.ErrNotFound (no existence
// leak across tenants). JSONB columns (allowed_domains, quiet_windows,
// test_windows, role_hints, constraints) round-trip through encoding/json.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SQL over the migration 034 tables. Every read/write is guarded by
// organization_id (the tenant isolation rule).
const (
	sqlInsertProject = `INSERT INTO synth_projects (id, org_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)`

	sqlSelectProject = `SELECT id, org_id, name, created_at, updated_at
		FROM synth_projects WHERE id = $1 AND org_id = $2`

	sqlSelectProjectsByOrg = `SELECT id, org_id, name, created_at, updated_at
		FROM synth_projects WHERE org_id = $1 ORDER BY created_at ASC, id ASC`

	sqlUpdateProject = `UPDATE synth_projects SET name = $3, updated_at = $4
		WHERE id = $1 AND org_id = $2`

	sqlDeleteProject = `DELETE FROM synth_projects WHERE id = $1 AND org_id = $2`

	sqlInsertTarget = `INSERT INTO synth_targets
		(id, org_id, project_id, name, base_url, verified, verified_at, verify_token, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	sqlSelectTarget = `SELECT id, org_id, project_id, name, base_url, verified, verified_at, verify_token, created_at, updated_at
		FROM synth_targets WHERE id = $1 AND org_id = $2`

	sqlSelectTargetsByOrg = `SELECT id, org_id, project_id, name, base_url, verified, verified_at, verify_token, created_at, updated_at
		FROM synth_targets WHERE org_id = $1 AND ($2 = '' OR project_id = $2)
		ORDER BY created_at ASC, id ASC`

	sqlUpdateTarget = `UPDATE synth_targets SET name = $3, verified = $4, verified_at = $5, verify_token = $6, updated_at = $7
		WHERE id = $1 AND org_id = $2`

	sqlDeleteTarget = `DELETE FROM synth_targets WHERE id = $1 AND org_id = $2`

	sqlInsertEnvironment = `INSERT INTO synth_environments
		(id, org_id, target_id, kind, mode, allowed_domains, rate_cap_per_min, budget_per_run, quiet_windows, test_windows, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

	sqlSelectEnvironment = `SELECT id, org_id, target_id, kind, mode, allowed_domains, rate_cap_per_min, budget_per_run, quiet_windows, test_windows, created_at, updated_at
		FROM synth_environments WHERE id = $1 AND org_id = $2`

	sqlSelectEnvironmentsByOrg = `SELECT id, org_id, target_id, kind, mode, allowed_domains, rate_cap_per_min, budget_per_run, quiet_windows, test_windows, created_at, updated_at
		FROM synth_environments WHERE org_id = $1 AND ($2 = '' OR target_id = $2)
		ORDER BY created_at ASC, id ASC`

	sqlUpdateEnvironment = `UPDATE synth_environments SET kind = $3, mode = $4, allowed_domains = $5, rate_cap_per_min = $6,
		budget_per_run = $7, quiet_windows = $8, test_windows = $9, updated_at = $10
		WHERE id = $1 AND org_id = $2`

	sqlDeleteEnvironment = `DELETE FROM synth_environments WHERE id = $1 AND org_id = $2`

	sqlInsertPersona = `INSERT INTO synth_personas
		(id, org_id, name, built_in, role_hints, constraints, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	sqlSelectPersona = `SELECT id, org_id, name, built_in, role_hints, constraints, version, created_at, updated_at
		FROM synth_personas WHERE id = $1 AND org_id = $2`

	sqlSelectPersonasByOrg = `SELECT id, org_id, name, built_in, role_hints, constraints, version, created_at, updated_at
		FROM synth_personas WHERE org_id = $1 ORDER BY created_at ASC, id ASC`

	sqlUpdatePersona = `UPDATE synth_personas SET name = $3, built_in = $4, role_hints = $5, constraints = $6, version = $7, updated_at = $8
		WHERE id = $1 AND org_id = $2`

	sqlDeletePersona = `DELETE FROM synth_personas WHERE id = $1 AND org_id = $2`
)

// pgStore is the Postgres-backed Store implementation (dual-mode house
// pattern: attached via NewServiceWithStore when a db handle exists).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver) over
// the migration 034 tables.
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

// guard fails fast when the store was built without a database handle.
func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("synth: database is nil")
	}
	return nil
}

// mapNoRows converts driver-level "no rows" into the contract error so the
// service (and the HTTP layer) sees exactly one 404 vocabulary.
func mapNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// requireAffected enforces the tenant guard on UPDATE/DELETE: zero affected
// rows means "not yours / not there" — the same ErrNotFound either way.
func requireAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSONB helpers
// ---------------------------------------------------------------------------

// jsonStringList marshals a []string for a JSONB parameter; nil becomes [].
func jsonStringList(list []string) (string, error) {
	if list == nil {
		list = []string{}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", fmt.Errorf("synth: marshal jsonb list: %w", err)
	}
	return string(b), nil
}

// jsonWindows marshals a []QuietWindow for a JSONB parameter; nil becomes [].
func jsonWindows(windows []QuietWindow) (string, error) {
	if windows == nil {
		windows = []QuietWindow{}
	}
	b, err := json.Marshal(windows)
	if err != nil {
		return "", fmt.Errorf("synth: marshal jsonb windows: %w", err)
	}
	return string(b), nil
}

// jsonConstraints marshals a map for a JSONB parameter; nil becomes {}.
func jsonConstraints(m map[string]string) (string, error) {
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("synth: marshal jsonb constraints: %w", err)
	}
	return string(b), nil
}

// scanJSONList decodes a JSONB []string column; SQL NULL ([]byte nil)
// decodes as an empty list.
func scanJSONList(raw []byte) ([]string, error) {
	out := []string{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("synth: decode jsonb list: %w", err)
	}
	return out, nil
}

// scanJSONWindows decodes a JSONB []QuietWindow column.
func scanJSONWindows(raw []byte) ([]QuietWindow, error) {
	out := []QuietWindow{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("synth: decode jsonb windows: %w", err)
	}
	return out, nil
}

// scanJSONConstraints decodes a JSONB map column; SQL NULL decodes as {}.
func scanJSONConstraints(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("synth: decode jsonb constraints: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// projects
// ---------------------------------------------------------------------------

func (s *pgStore) CreateProject(ctx context.Context, p *Project) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertProject, p.ID, p.OrgID, p.Name, p.CreatedAt, p.UpdatedAt)
	return err
}

func scanProject(row *sql.Row) (*Project, error) {
	var p Project
	if err := row.Scan(&p.ID, &p.OrgID, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, mapNoRows(err)
	}
	return &p, nil
}

func (s *pgStore) GetProject(ctx context.Context, orgID, id string) (*Project, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanProject(s.db.QueryRowContext(ctx, sqlSelectProject, id, orgID))
}

func (s *pgStore) ListProjects(ctx context.Context, orgID string) ([]*Project, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectProjectsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read path; nothing actionable
	out := []*Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateProject(ctx context.Context, p *Project) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateProject, p.ID, p.OrgID, p.Name, p.UpdatedAt)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

func (s *pgStore) DeleteProject(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteProject, id, orgID)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

// ---------------------------------------------------------------------------
// targets
// ---------------------------------------------------------------------------

func (s *pgStore) CreateTarget(ctx context.Context, t *Target) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertTarget,
		t.ID, t.OrgID, t.ProjectID, t.Name, t.BaseURL, t.Verified, nullTime(t.VerifiedAt), t.VerifyToken, t.CreatedAt, t.UpdatedAt)
	return err
}

// nullTime adapts a possibly-nil timestamp to the driver.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// scanTarget decodes one target row; verified_at NULL maps to a nil
// VerifiedAt pointer (no synthetic zero-times on the wire).
func scanTarget(row *sql.Row) (*Target, error) {
	var t Target
	var verifiedAt sql.NullTime
	if err := row.Scan(&t.ID, &t.OrgID, &t.ProjectID, &t.Name, &t.BaseURL, &t.Verified, &verifiedAt, &t.VerifyToken, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, mapNoRows(err)
	}
	if verifiedAt.Valid {
		v := verifiedAt.Time
		t.VerifiedAt = &v
	}
	return &t, nil
}

func (s *pgStore) GetTarget(ctx context.Context, orgID, id string) (*Target, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanTarget(s.db.QueryRowContext(ctx, sqlSelectTarget, id, orgID))
}

// scanTargetRows decodes a target list (shared by the list query variants).
func scanTargetRows(rows *sql.Rows) ([]*Target, error) {
	defer rows.Close() //nolint:errcheck // read path; nothing actionable
	out := []*Target{}
	for rows.Next() {
		var t Target
		var verifiedAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.OrgID, &t.ProjectID, &t.Name, &t.BaseURL, &t.Verified, &verifiedAt, &t.VerifyToken, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if verifiedAt.Valid {
			v := verifiedAt.Time
			t.VerifiedAt = &v
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *pgStore) ListTargets(ctx context.Context, orgID, projectID string) ([]*Target, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectTargetsByOrg, orgID, projectID)
	if err != nil {
		return nil, err
	}
	return scanTargetRows(rows)
}

func (s *pgStore) UpdateTarget(ctx context.Context, t *Target) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateTarget,
		t.ID, t.OrgID, t.Name, t.Verified, nullTime(t.VerifiedAt), t.VerifyToken, t.UpdatedAt)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

func (s *pgStore) DeleteTarget(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteTarget, id, orgID)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

// ---------------------------------------------------------------------------
// environments
// ---------------------------------------------------------------------------

func (s *pgStore) CreateEnvironment(ctx context.Context, e *Environment) error {
	if err := s.guard(); err != nil {
		return err
	}
	domains, err := jsonStringList(e.AllowedDomains)
	if err != nil {
		return err
	}
	quiet, err := jsonWindows(e.QuietWindows)
	if err != nil {
		return err
	}
	test, err := jsonWindows(e.TestWindows)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, sqlInsertEnvironment,
		e.ID, e.OrgID, e.TargetID, string(e.Kind), string(e.Mode),
		domains, e.RateCapPerMin, e.BudgetPerRun, quiet, test, e.CreatedAt, e.UpdatedAt)
	return err
}

type envScan struct {
	e              Environment
	allowedDomains []byte
	quietWindows   []byte
	testWindows    []byte
}

// scanEnvCols scans the raw environment columns (JSONB columns stay bytes
// until the callers decode them).
func scanEnvCols(scanner interface{ Scan(dest ...any) error }) (*Environment, error) {
	var sc envScan
	if err := scanner.Scan(&sc.e.ID, &sc.e.OrgID, &sc.e.TargetID, &sc.e.Kind, &sc.e.Mode,
		&sc.allowedDomains, &sc.e.RateCapPerMin, &sc.e.BudgetPerRun,
		&sc.quietWindows, &sc.testWindows, &sc.e.CreatedAt, &sc.e.UpdatedAt); err != nil {
		return nil, mapNoRows(err)
	}
	e := &sc.e
	domains, err := scanJSONList(sc.allowedDomains)
	if err != nil {
		return nil, err
	}
	e.AllowedDomains = domains
	quiet, err := scanJSONWindows(sc.quietWindows)
	if err != nil {
		return nil, err
	}
	e.QuietWindows = quiet
	test, err := scanJSONWindows(sc.testWindows)
	if err != nil {
		return nil, err
	}
	e.TestWindows = test
	return e, nil
}

func (s *pgStore) GetEnvironment(ctx context.Context, orgID, id string) (*Environment, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanEnvCols(s.db.QueryRowContext(ctx, sqlSelectEnvironment, id, orgID))
}

func (s *pgStore) ListEnvironments(ctx context.Context, orgID, targetID string) ([]*Environment, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectEnvironmentsByOrg, orgID, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read path; nothing actionable
	out := []*Environment{}
	for rows.Next() {
		e, err := scanEnvCols(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateEnvironment(ctx context.Context, e *Environment) error {
	if err := s.guard(); err != nil {
		return err
	}
	domains, err := jsonStringList(e.AllowedDomains)
	if err != nil {
		return err
	}
	quiet, err := jsonWindows(e.QuietWindows)
	if err != nil {
		return err
	}
	test, err := jsonWindows(e.TestWindows)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateEnvironment,
		e.ID, e.OrgID, string(e.Kind), string(e.Mode), domains, e.RateCapPerMin, e.BudgetPerRun, quiet, test, e.UpdatedAt)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

func (s *pgStore) DeleteEnvironment(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteEnvironment, id, orgID)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

// ---------------------------------------------------------------------------
// personas
// ---------------------------------------------------------------------------

func (s *pgStore) CreatePersona(ctx context.Context, p *Persona) error {
	if err := s.guard(); err != nil {
		return err
	}
	hints, err := jsonStringList(p.RoleHints)
	if err != nil {
		return err
	}
	constraints, err := jsonConstraints(p.Constraints)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, sqlInsertPersona,
		p.ID, p.OrgID, p.Name, p.BuiltIn, hints, constraints, p.Version, p.CreatedAt, p.UpdatedAt)
	return err
}

// scanPersonaCols decodes one persona row (JSONB columns decoded inline).
func scanPersonaCols(scanner interface{ Scan(dest ...any) error }) (*Persona, error) {
	var p Persona
	var hints, constraints []byte
	if err := scanner.Scan(&p.ID, &p.OrgID, &p.Name, &p.BuiltIn, &hints, &constraints, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, mapNoRows(err)
	}
	decoded, err := scanJSONList(hints)
	if err != nil {
		return nil, err
	}
	p.RoleHints = decoded
	m, err := scanJSONConstraints(constraints)
	if err != nil {
		return nil, err
	}
	p.Constraints = m
	return &p, nil
}

func (s *pgStore) GetPersona(ctx context.Context, orgID, id string) (*Persona, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanPersonaCols(s.db.QueryRowContext(ctx, sqlSelectPersona, id, orgID))
}

func (s *pgStore) ListPersonas(ctx context.Context, orgID string) ([]*Persona, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectPersonasByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read path; nothing actionable
	out := []*Persona{}
	for rows.Next() {
		p, err := scanPersonaCols(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdatePersona(ctx context.Context, p *Persona) error {
	if err := s.guard(); err != nil {
		return err
	}
	hints, err := jsonStringList(p.RoleHints)
	if err != nil {
		return err
	}
	constraints, err := jsonConstraints(p.Constraints)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdatePersona,
		p.ID, p.OrgID, p.Name, p.BuiltIn, hints, constraints, p.Version, p.UpdatedAt)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

func (s *pgStore) DeletePersona(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeletePersona, id, orgID)
	if err != nil {
		return err
	}
	return requireAffected(res)
}
