package secrets

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
)

// store.go is the Postgres-backed Store (lib/pq driver, same pattern as
// internal/scheduler/store.go). Every statement is tenant-guarded:
//
//   - tenant-facing paths (Create/Update/SoftDelete/ListMeta/GetMeta/
//     GetEncrypted) filter organization_id, so a rogue caller cannot even
//     reach another tenant's ciphertext by primary key;
//   - metadata projections (ListMeta/GetMeta) never select the
//     ciphertext/nonce columns — encrypted material leaves the DB only via
//     GetEncrypted on the Resolve path.
//
// Rows are soft-deleted (deleted_at tombstone). Uniqueness among LIVE rows is
// enforced by the partial unique index uq_secrets_org_name_live
// (migrations/017_secrets.sql): UNIQUE(org_id, name) would make a name
// permanently un-recreatable after a soft delete, so the index skips
// tombstoned rows. A violating insert surfaces as 23505 -> ErrDuplicate.

const (
	// Metadata projection (no ciphertext columns by design).
	secretMetaColumns = `name, key_version, created_by, created_at, updated_at`

	sqlInsertSecret = `INSERT INTO secrets
		(organization_id, name, ciphertext, nonce, key_version, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	sqlUpdateSecret = `UPDATE secrets SET
		ciphertext = $3,
		nonce = $4,
		key_version = $5,
		updated_at = $6
		WHERE organization_id = $1 AND name = $2 AND deleted_at IS NULL`

	// Soft delete: tombstone keeps the row for audit/forensics but stops all
	// reads (every SELECT below filters deleted_at IS NULL).
	sqlSoftDeleteSecret = `UPDATE secrets SET deleted_at = $3, updated_at = $3
		WHERE organization_id = $1 AND name = $2 AND deleted_at IS NULL`

	sqlSelectSecretsByOrg = `SELECT ` + secretMetaColumns + ` FROM secrets
		WHERE organization_id = $1 AND deleted_at IS NULL
		ORDER BY name ASC`

	sqlSelectSecretMeta = `SELECT ` + secretMetaColumns + ` FROM secrets
		WHERE organization_id = $1 AND name = $2 AND deleted_at IS NULL`

	// Full row incl. sealed material; consumed exclusively by Resolve.
	sqlSelectSecretEncrypted = `SELECT name, key_version, created_by, created_at, updated_at, ciphertext, nonce
		FROM secrets
		WHERE organization_id = $1 AND name = $2 AND deleted_at IS NULL`
)

// pgStore is the Postgres Store implementation.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB.
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("secrets: database is nil")
	}
	return nil
}

// mapConstraintErr translates Postgres unique violations (partial index
// uq_secrets_org_name_live, SQLSTATE 23505) into ErrDuplicate.
func mapConstraintErr(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrDuplicate
	}
	return err
}

func (s *pgStore) Create(ctx context.Context, rec *Record) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertSecret,
		rec.OrgID, rec.Name, rec.Ciphertext, rec.Nonce, rec.KeyVersion,
		rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt)
	return mapConstraintErr(err)
}

func (s *pgStore) Update(ctx context.Context, rec *Record) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateSecret,
		rec.OrgID, rec.Name, rec.Ciphertext, rec.Nonce, rec.KeyVersion, rec.UpdatedAt)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) SoftDelete(ctx context.Context, orgID, name string, deletedAt time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlSoftDeleteSecret, orgID, name, deletedAt)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) ListMeta(ctx context.Context, orgID string) ([]*Record, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectSecretsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Record, 0)
	for rows.Next() {
		rec, err := scanMeta(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *pgStore) GetMeta(ctx context.Context, orgID, name string) (*Record, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanMeta(s.db.QueryRowContext(ctx, sqlSelectSecretMeta, orgID, name))
}

func (s *pgStore) GetEncrypted(ctx context.Context, orgID, name string) (*Record, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var (
		rec       Record
		createdAt time.Time
		updatedAt time.Time
	)
	err := s.db.QueryRowContext(ctx, sqlSelectSecretEncrypted, orgID, name).
		Scan(&rec.Name, &rec.KeyVersion, &rec.CreatedBy, &createdAt, &updatedAt,
			&rec.Ciphertext, &rec.Nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rec.OrgID = orgID
	rec.CreatedAt = createdAt
	rec.UpdatedAt = updatedAt
	return &rec, nil
}

// metaScanner abstracts *sql.Row / *sql.Rows for the metadata projection.
type metaScanner interface {
	Scan(dest ...any) error
}

func scanMeta(sc metaScanner) (*Record, error) {
	var (
		rec       Record
		createdAt time.Time
		updatedAt time.Time
	)
	err := sc.Scan(&rec.Name, &rec.KeyVersion, &rec.CreatedBy, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rec.CreatedAt = createdAt
	rec.UpdatedAt = updatedAt
	return &rec, nil
}
