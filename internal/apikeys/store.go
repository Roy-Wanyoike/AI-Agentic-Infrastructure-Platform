package apikeys

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	// Tenant guard: the key row is written with its organization_id scope.
	sqlInsertAPIKey = `INSERT INTO api_keys (id, organization_id, user_id, name, prefix, key_hash, created_at, revoked_at) VALUES ($1, $2, $3, $4, $5, $6, $7, NULL)`
	// Credential-based tenant resolution: key_hash is the SHA-256 of a unique
	// secret, so the returned row identifies the single owning tenant.
	sqlSelectAPIKeyByHash = `SELECT id, organization_id, user_id, name, prefix, key_hash, created_at, revoked_at, last_used_at FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`
	// Trusted internal PK lookup used to resolve the tenant for legacy
	// context-free revocations.
	sqlSelectAPIKeyByID = `SELECT id, organization_id, user_id, name, prefix, key_hash, created_at, revoked_at, last_used_at FROM api_keys WHERE id = $1`
	// Tenant guard: revocation only happens within the key's organization_id.
	sqlRevokeAPIKey = `UPDATE api_keys SET revoked_at = $1 WHERE id = $2 AND organization_id = $3`
	// Tenant guard: keys are listed for exactly one organization_id.
	sqlSelectAPIKeysByOrg = `SELECT id, organization_id, user_id, name, prefix, key_hash, created_at, revoked_at, last_used_at FROM api_keys WHERE organization_id = $1 ORDER BY created_at DESC`
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
		return errors.New("apikeys: database is nil")
	}
	return nil
}

func (s *pgStore) CreateKey(ctx context.Context, key *APIKey) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := key.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, sqlInsertAPIKey, key.ID, key.OrgID, key.UserID, key.Name, key.Prefix, key.Hash, createdAt)
	return err
}

func (s *pgStore) GetKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	key, err := scanAPIKey(s.db.QueryRowContext(ctx, sqlSelectAPIKeyByHash, hash))
	if err != nil {
		return nil, err
	}
	return key, nil
}

func (s *pgStore) GetKeyByID(ctx context.Context, id string) (*APIKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	key, err := scanAPIKey(s.db.QueryRowContext(ctx, sqlSelectAPIKeyByID, id))
	if err != nil {
		return nil, err
	}
	return key, nil
}

func (s *pgStore) RevokeKey(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlRevokeAPIKey, time.Now().UTC(), id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (wrong org or unknown key)
		return ErrKeyNotFound
	}
	return nil
}

func (s *pgStore) ListKeys(ctx context.Context, orgID string) ([]*APIKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectAPIKeysByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*APIKey, 0)
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

func scanAPIKey(scanner interface{ Scan(dest ...any) error }) (*APIKey, error) {
	var key APIKey
	var revokedAt, lastUsedAt sql.NullTime
	if err := scanner.Scan(&key.ID, &key.OrgID, &key.UserID, &key.Name, &key.Prefix, &key.Hash, &key.CreatedAt, &revokedAt, &lastUsedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	key.Revoked = revokedAt.Valid
	return &key, nil
}
