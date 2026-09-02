package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Config-version SQL. Tenant guard: every statement joins agents and filters
// on organization_id so a tenant can only ever touch its own versions.
const (
	// INSERT ... SELECT enforces the organization_id guard on insert (the
	// agent must belong to the caller's tenant); 0 rows affected means the
	// agent was not found in this tenant. The snapshot travels in BOTH the
	// new snapshot column and the legacy config column (config->>'model' is
	// still read by the wave-1 queries), and instructions map to prompt for
	// backward compatibility.
	sqlInsertConfigVersion = `INSERT INTO agent_versions
                (id, agent_id, version, config, snapshot, prompt, status, published_at, published_by, created_at)
                SELECT $1, a.id, $3, $4::jsonb, $5::jsonb, $6, $7, $8, $9, $10
                FROM agents a WHERE a.id = $2 AND a.organization_id = $1`

	sqlSelectConfigVersionColumns = `av.id, av.agent_id, a.organization_id, av.version,
                COALESCE(av.snapshot::text, av.config::text, '{}'), COALESCE(av.status, 'draft'),
                av.published_at, COALESCE(av.published_by, ''), av.created_at`

	sqlSelectConfigVersion = `SELECT ` + sqlSelectConfigVersionColumns + `
                FROM agent_versions av JOIN agents a ON a.id = av.agent_id
                WHERE a.organization_id = $1 AND av.agent_id = $2 AND av.version = $3`

	// At most one published version per agent; newest wins defensively.
	sqlSelectPublishedConfigVersion = `SELECT ` + sqlSelectConfigVersionColumns + `
                FROM agent_versions av JOIN agents a ON a.id = av.agent_id
                WHERE a.organization_id = $1 AND av.agent_id = $2 AND COALESCE(av.status, 'draft') = 'published'
                ORDER BY av.version DESC LIMIT 1`

	sqlSelectConfigVersionsByAgent = `SELECT ` + sqlSelectConfigVersionColumns + `
                FROM agent_versions av JOIN agents a ON a.id = av.agent_id
                WHERE a.organization_id = $1 AND av.agent_id = $2
                ORDER BY av.version ASC`

	// Tenant guard on update: JOIN-style FROM clause scoped by organization_id.
	sqlUpdateConfigVersionStatus = `UPDATE agent_versions av
                SET status = $1, published_at = $2, published_by = $3
                FROM agents a
                WHERE a.id = av.agent_id AND a.organization_id = $4 AND av.id = $5`

	sqlNextConfigVersionNumber = `SELECT COALESCE(MAX(av.version), 0) + 1
                FROM agent_versions av JOIN agents a ON a.id = av.agent_id
                WHERE a.organization_id = $1 AND av.agent_id = $2`
)

// pgVersionsStore is the Postgres-backed VersionStore implementation.
type pgVersionsStore struct {
	db *sql.DB
}

// NewVersionsPostgresStore returns a VersionStore backed by *sql.DB (lib/pq).
func NewVersionsPostgresStore(db *sql.DB) VersionStore {
	return &pgVersionsStore{db: db}
}

func (s *pgVersionsStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("agents: versions database is nil")
	}
	return nil
}

func (s *pgVersionsStore) CreateVersion(ctx context.Context, orgID string, version *ConfigVersion) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := version.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
		version.CreatedAt = createdAt
	}
	res, err := s.db.ExecContext(ctx, sqlInsertConfigVersion,
		version.ID, version.AgentID, version.Version,
		version.Snapshot, version.Snapshot, snapshotInstructions(version.Snapshot),
		version.Status, nullTime(version.PublishedAt), version.PublishedBy, createdAt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Tenant guard rejected the insert (unknown agent for this org).
		return ErrAgentNotFound
	}
	return nil
}

func (s *pgVersionsStore) GetVersion(ctx context.Context, orgID, agentID string, version int) (*ConfigVersion, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanConfigVersion(s.db.QueryRowContext(ctx, sqlSelectConfigVersion, orgID, agentID, version))
}

func (s *pgVersionsStore) GetPublishedVersion(ctx context.Context, orgID, agentID string) (*ConfigVersion, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	version, err := scanConfigVersion(s.db.QueryRowContext(ctx, sqlSelectPublishedConfigVersion, orgID, agentID))
	if err != nil {
		if errors.Is(err, ErrVersionNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return version, nil
}

func (s *pgVersionsStore) ListVersions(ctx context.Context, orgID, agentID string) ([]*ConfigVersion, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectConfigVersionsByAgent, orgID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*ConfigVersion, 0)
	for rows.Next() {
		version, err := scanConfigVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, version)
	}
	return out, rows.Err()
}

func (s *pgVersionsStore) UpdateVersionStatus(ctx context.Context, orgID string, version *ConfigVersion) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateConfigVersionStatus,
		version.Status, nullTime(version.PublishedAt), version.PublishedBy, orgID, version.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrVersionNotFound
	}
	return nil
}

func (s *pgVersionsStore) NextVersionNumber(ctx context.Context, orgID, agentID string) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	var number int
	if err := s.db.QueryRowContext(ctx, sqlNextConfigVersionNumber, orgID, agentID).Scan(&number); err != nil {
		return 0, err
	}
	return number, nil
}

// scanConfigVersion reads one row; sql.ErrNoRows maps to ErrVersionNotFound.
func scanConfigVersion(scanner interface{ Scan(dest ...any) error }) (*ConfigVersion, error) {
	var (
		version     ConfigVersion
		snapshot    string
		publishedAt sql.NullTime
	)
	if err := scanner.Scan(&version.ID, &version.AgentID, &version.OrganizationID, &version.Version,
		&snapshot, &version.Status, &publishedAt, &version.PublishedBy, &version.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVersionNotFound
		}
		return nil, err
	}
	version.Snapshot = snapshot
	if publishedAt.Valid {
		t := publishedAt.Time.UTC()
		version.PublishedAt = &t
	}
	return &version, nil
}

// nullTime converts a *time.Time into a database/sql NULL-able value.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// snapshotInstructions extracts the instructions field from a snapshot JSON
// document for the legacy prompt column (best-effort, empty on parse issues).
func snapshotInstructions(snapshot string) string {
	var fields map[string]any
	if snapshot == "" || json.Unmarshal([]byte(snapshot), &fields) != nil {
		return ""
	}
	if instructions, ok := fields["instructions"].(string); ok {
		return instructions
	}
	return ""
}
