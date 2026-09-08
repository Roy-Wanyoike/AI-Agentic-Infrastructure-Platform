package marketplace

// signing_store.go — durable storage for the provenance subsystem (issue #78,
// migration 022): publisher public keys (publisher_keys) and the org-level
// install policy (marketplace_org_settings).
//
// SigningKeyStore is a SEPARATE capability interface, deliberately NOT part
// of Store: the listing store contract stays unchanged (issue #28) and the
// service degrades gracefully (ErrSigningStoreUnavailable) for stores that
// cannot hold keys. pgStore implements both.
//
// Tenant guards mirror store.go:
//   - every key statement filters organization_id — a foreign org cannot read,
//     revoke or even confirm the existence of another org's keys (no
//     existence leak; unknown and foreign key ids are both ErrKeyNotFound);
//   - revocation is idempotent (COALESCE keeps the FIRST revoked_at) so a
//     retried DELETE still returns success;
//   - org settings are an upsert keyed by organization_id; a missing row IS
//     the default policy (allow_unsigned=false), never an error.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
)

const (
	signingKeyColumns = `id, organization_id, public_key, alg, status, created_at, revoked_at`

	sqlInsertSigningKey = `INSERT INTO publisher_keys
		(id, organization_id, public_key, alg, status, created_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	// Org-scoped point lookup (verification paths pass the PUBLISHER org from
	// the listing row itself, so no caller-controlled tenant mix-up is
	// possible).
	sqlSelectSigningKey = `SELECT ` + signingKeyColumns + ` FROM publisher_keys
		WHERE id = $1 AND organization_id = $2`

	sqlSelectSigningKeysByOrg = `SELECT ` + signingKeyColumns + ` FROM publisher_keys
		WHERE organization_id = $1
		ORDER BY created_at, id`

	// Idempotent revoke: status flips on every call, revoked_at is kept from
	// the FIRST revoke (COALESCE); 0 rows affected (unknown OR foreign id)
	// maps to ErrKeyNotFound.
	sqlRevokeSigningKey = `UPDATE publisher_keys
		SET status = 'revoked', revoked_at = COALESCE(revoked_at, $3)
		WHERE id = $1 AND organization_id = $2`

	sqlSelectOrgSettings = `SELECT allow_unsigned, updated_at FROM marketplace_org_settings
		WHERE organization_id = $1`

	sqlUpsertOrgSettings = `INSERT INTO marketplace_org_settings (organization_id, allow_unsigned, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (organization_id)
		DO UPDATE SET allow_unsigned = EXCLUDED.allow_unsigned, updated_at = EXCLUDED.updated_at
		RETURNING allow_unsigned, updated_at`
)

// SigningKeyStore is the durable storage capability for publisher keys and
// org install policies. Implemented by *pgStore.
type SigningKeyStore interface {
	// CreateSigningKey inserts one key row ((organization_id, public_key)
	// conflicts surface as ErrDuplicateKey).
	CreateSigningKey(ctx context.Context, key *SigningKey) error
	// GetSigningKey fetches one key of the given org (unknown or foreign ids
	// surface as ErrKeyNotFound). Status is returned verbatim — the service
	// decides how non-active keys surface.
	GetSigningKey(ctx context.Context, orgID, keyID string) (*SigningKey, error)
	// ListSigningKeys returns the org's keys oldest-first.
	ListSigningKeys(ctx context.Context, orgID string) ([]*SigningKey, error)
	// RevokeSigningKey flips status to revoked (idempotent); unknown or
	// foreign ids surface as ErrKeyNotFound.
	RevokeSigningKey(ctx context.Context, orgID, keyID string) error
	// GetOrgSettings reads the org install policy; a missing row reads as the
	// zero value (allow_unsigned=false), never an error.
	GetOrgSettings(ctx context.Context, orgID string) (*OrgSettings, error)
	// UpsertOrgSettings writes the org install policy and returns the stored
	// row (including the server-stamped updated_at).
	UpsertOrgSettings(ctx context.Context, orgID string, allowUnsigned bool) (*OrgSettings, error)
}

// compile-time proof the pg store carries the signing capability.
var _ SigningKeyStore = (*pgStore)(nil)

func (s *pgStore) CreateSigningKey(ctx context.Context, key *SigningKey) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertSigningKey,
		key.ID, key.OrganizationID, key.PublicKey, key.Alg, key.Status,
		key.CreatedAt, key.RevokedAt)
	return mapSigningConstraintErr(err)
}

// mapSigningConstraintErr translates the (organization_id, public_key) unique
// violation into ErrDuplicateKey.
func mapSigningConstraintErr(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrDuplicateKey
	}
	return err
}

func (s *pgStore) GetSigningKey(ctx context.Context, orgID, keyID string) (*SigningKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanSigningKey(s.db.QueryRowContext(ctx, sqlSelectSigningKey, keyID, orgID))
}

func (s *pgStore) ListSigningKeys(ctx context.Context, orgID string) ([]*SigningKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectSigningKeysByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*SigningKey, 0)
	for rows.Next() {
		key, err := scanSigningKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

func (s *pgStore) RevokeSigningKey(ctx context.Context, orgID, keyID string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlRevokeSigningKey, keyID, orgID, time.Now().UTC())
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrKeyNotFound
	}
	return nil
}

func (s *pgStore) GetOrgSettings(ctx context.Context, orgID string) (*OrgSettings, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var (
		settings  OrgSettings
		updatedAt time.Time
	)
	err := s.db.QueryRowContext(ctx, sqlSelectOrgSettings, orgID).Scan(&settings.AllowUnsigned, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// No row IS the default policy: verify (allow_unsigned=false).
		return &OrgSettings{}, nil
	}
	if err != nil {
		return nil, err
	}
	settings.UpdatedAt = updatedAt
	return &settings, nil
}

func (s *pgStore) UpsertOrgSettings(ctx context.Context, orgID string, allowUnsigned bool) (*OrgSettings, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var (
		settings  OrgSettings
		updatedAt time.Time
	)
	err := s.db.QueryRowContext(ctx, sqlUpsertOrgSettings, orgID, allowUnsigned, time.Now().UTC()).
		Scan(&settings.AllowUnsigned, &updatedAt)
	if err != nil {
		return nil, err
	}
	settings.UpdatedAt = updatedAt
	return &settings, nil
}

func scanSigningKey(sc scanner) (*SigningKey, error) {
	var key SigningKey
	err := sc.Scan(&key.ID, &key.OrganizationID, &key.PublicKey, &key.Alg,
		&key.Status, &key.CreatedAt, &key.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}
