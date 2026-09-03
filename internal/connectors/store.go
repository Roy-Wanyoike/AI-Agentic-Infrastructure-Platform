package connectors

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/lib/pq"
)

// store.go is the Postgres-backed Store (lib/pq driver, same pattern as
// internal/secrets/store.go). Every statement is tenant-guarded:
//
//   - tenant-facing paths (Create/Update/Delete/Get/List/UpdateCheckStatus)
//     filter organization_id, so a rogue caller cannot reach another tenant's
//     connector by primary key;
//   - the config projection round-trips JSONB through encoding/json; config
//     carries header TEMPLATES and auth style parameters only — secret values
//     never enter this table (secret_ref is a NAME reference).
//
// Rows are hard-deleted, so the FULL UNIQUE(organization_id, name) constraint
// (migration 020) is correct here; a violating insert surfaces as
// SQLSTATE 23505 -> ErrDuplicate. RowsAffected == 0 on mutation -> ErrNotFound.

const (
	sqlInsertConnector = `INSERT INTO connectors
                (id, organization_id, name, type, base_url, config, secret_ref, status, last_check_at, last_check_status, created_by, created_at, updated_at)
                VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12, $13)`

	sqlUpdateConnector = `UPDATE connectors SET
                name = $3,
                type = $4,
                base_url = $5,
                config = $6::jsonb,
                secret_ref = $7,
                status = $8,
                updated_at = $9
                WHERE id = $1 AND organization_id = $2`

	// Tenant guard: deletes require a matching organization_id.
	sqlDeleteConnector = `DELETE FROM connectors WHERE id = $1 AND organization_id = $2`

	// Tenant guard: single-connector reads are scoped to one organization_id.
	sqlSelectConnectorScoped = `SELECT id, organization_id, name, type, base_url,
                COALESCE(config::text, '{}'), COALESCE(secret_ref, ''), status,
                last_check_at, COALESCE(last_check_status, ''), created_by, created_at, updated_at
                FROM connectors WHERE id = $1 AND organization_id = $2`

	// Tenant guard: listings filter on organization_id.
	sqlSelectConnectorsByOrg = `SELECT id, organization_id, name, type, base_url,
                COALESCE(config::text, '{}'), COALESCE(secret_ref, ''), status,
                last_check_at, COALESCE(last_check_status, ''), created_by, created_at, updated_at
                FROM connectors WHERE organization_id = $1
                ORDER BY name ASC`

	// Health-check bookkeeping only: updated_at is deliberately untouched
	// (last_check_at carries the freshness of the probe).
	sqlUpdateCheckStatus = `UPDATE connectors SET last_check_at = $3, last_check_status = $4
                WHERE id = $1 AND organization_id = $2`
)

// pgStore is the Postgres-backed Store implementation (migration 020).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("connectors: database is nil")
	}
	return nil
}

func marshalConfig(cfg Config) string {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func unmarshalConfig(raw string) Config {
	var cfg Config
	if raw == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		// A malformed config never crashes the governance surface; the
		// connector simply behaves as unconfigured.
		return Config{}
	}
	return cfg
}

func isDuplicate(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

func (s *pgStore) Create(ctx context.Context, c *Connector) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := c.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
		c.CreatedAt = createdAt
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = createdAt
	}
	var lastCheckAt any
	if c.LastCheckAt != nil {
		lastCheckAt = *c.LastCheckAt
	}
	if _, err := s.db.ExecContext(ctx, sqlInsertConnector,
		c.ID, c.OrganizationID, c.Name, c.Type, c.BaseURL,
		marshalConfig(c.Config), c.SecretRef, c.Status,
		lastCheckAt, c.LastCheckStatus, c.CreatedBy, c.CreatedAt, c.UpdatedAt); err != nil {
		if isDuplicate(err) {
			return ErrDuplicate
		}
		return err
	}
	return nil
}

func (s *pgStore) Update(ctx context.Context, c *Connector) error {
	if err := s.guard(); err != nil {
		return err
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateConnector,
		c.ID, c.OrganizationID, c.Name, c.Type, c.BaseURL,
		marshalConfig(c.Config), c.SecretRef, c.Status, c.UpdatedAt)
	if err != nil {
		if isDuplicate(err) {
			return ErrDuplicate
		}
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) Delete(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteConnector, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) Get(ctx context.Context, orgID, id string) (*Connector, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	return scanConnector(s.db.QueryRowContext(ctx, sqlSelectConnectorScoped, id, orgID))
}

func (s *pgStore) List(ctx context.Context, orgID string) ([]*Connector, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectConnectorsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Connector, 0)
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateCheckStatus(ctx context.Context, orgID, id string, checkedAt time.Time, status string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateCheckStatus, id, orgID, checkedAt, status)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanner abstracts *sql.Row and *sql.Rows for the shared column scan.
type scanner interface {
	Scan(dest ...any) error
}

func scanConnector(row scanner) (*Connector, error) {
	c := &Connector{}
	var (
		configRaw       string
		lastCheckAt     sql.NullTime
		lastCheckStatus string
	)
	if err := row.Scan(&c.ID, &c.OrganizationID, &c.Name, &c.Type, &c.BaseURL,
		&configRaw, &c.SecretRef, &c.Status,
		&lastCheckAt, &lastCheckStatus, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.Config = unmarshalConfig(configRaw)
	if lastCheckAt.Valid {
		checkAt := lastCheckAt.Time
		c.LastCheckAt = &checkAt
	}
	c.LastCheckStatus = lastCheckStatus
	return c, nil
}

// Compile-time interface check.
var _ Store = (*pgStore)(nil)
