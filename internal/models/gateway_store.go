package models

// gateway_store.go — Postgres GatewayStore (issue #139, migration
// 030_virtual_keys.sql). Conventions mirror internal/approvals/store.go and
// the api_keys / agent_api_keys stores:
//
//   - every tenant-facing query is guarded by organization_id;
//   - the by-hash lookup is the authentication path (the hash resolves the
//     tenant from the credential itself, so it is intentionally org-free);
//   - the models allow-list round-trips through JSONB with the nil-vs-empty
//     contract lossless: SQL NULL = allow-list DISABLED (ALL models), '[]' =
//     allow-list ENABLED and empty (NONE);
//   - updates return ErrVirtualKeyNotFound on zero rows affected (unknown id
//     or tenant guard), never leaking existence across tenants;
//   - the spend ledger is APPEND-ONLY (one row per recorded completion,
//     bucketed by month "YYYY-MM" UTC + model) and aggregated on read —
//     per-call attribution survives, mirroring the audit-trail posture.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	sqlInsertVirtualKey = `INSERT INTO virtual_keys
                (id, organization_id, label, prefix, key_hash, created_by, models, monthly_limit_cents, status, expires_at, revoked_at, created_at, updated_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

	sqlSelectVirtualKeyColumns = `SELECT id, organization_id, label, prefix, key_hash, created_by, models,
                monthly_limit_cents, status, expires_at, revoked_at, created_at, updated_at FROM virtual_keys`

	// Authentication path: credential-based tenant resolution by key hash.
	sqlSelectVirtualKeyByHash = sqlSelectVirtualKeyColumns + ` WHERE key_hash = $1`
	// Tenant guard: single-key reads are scoped to one organization_id.
	sqlSelectVirtualKeyScoped = sqlSelectVirtualKeyColumns + ` WHERE id = $1 AND organization_id = $2`
	// Tenant guard: listings filter on organization_id (deterministic order).
	sqlSelectVirtualKeysByOrg = sqlSelectVirtualKeyColumns + ` WHERE organization_id = $1 ORDER BY created_at ASC, id ASC`

	// Mutable fields only; status moves exclusively through RevokeVirtualKey.
	sqlUpdateVirtualKey = `UPDATE virtual_keys SET label = $3, models = $4, monthly_limit_cents = $5,
                expires_at = $6, updated_at = $7 WHERE id = $1 AND organization_id = $2`

	sqlRevokeVirtualKey = `UPDATE virtual_keys SET status = 'revoked', revoked_at = $3, updated_at = $3
                WHERE id = $1 AND organization_id = $2`

	sqlInsertVirtualKeySpend = `INSERT INTO virtual_key_spend
                (id, organization_id, key_id, month, model, cost_cents, recorded_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7)`

	sqlSpendForMonth = `SELECT COALESCE(SUM(cost_cents), 0) FROM virtual_key_spend
                WHERE organization_id = $1 AND key_id = $2 AND month = $3`
)

// gatewayPostgresStore is the Postgres-backed GatewayStore (lib/pq driver).
type gatewayPostgresStore struct {
	db *sql.DB
}

// NewGatewayPostgresStore returns a GatewayStore backed by *sql.DB.
func NewGatewayPostgresStore(db *sql.DB) GatewayStore {
	return &gatewayPostgresStore{db: db}
}

func (s *gatewayPostgresStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("models: gateway database is nil")
	}
	return nil
}

// CreateVirtualKey inserts the key row with its organization_id scope.
func (s *gatewayPostgresStore) CreateVirtualKey(ctx context.Context, key *VirtualKey) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertVirtualKey,
		key.ID, key.OrganizationID, key.Label, key.KeyPrefix, key.KeyHash, key.CreatedBy,
		marshalModelsParam(key.Models), key.MonthlyLimitCents, key.Status,
		nullableTime(key.ExpiresAt), nullableTime(key.RevokedAt), key.CreatedAt, key.UpdatedAt)
	return err
}

// GetVirtualKeyByHash resolves a key by the SHA-256 hash of the presented
// secret (authentication path; see the file header).
func (s *gatewayPostgresStore) GetVirtualKeyByHash(ctx context.Context, hash string) (*VirtualKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanVirtualKey(s.db.QueryRowContext(ctx, sqlSelectVirtualKeyByHash, hash))
}

// GetVirtualKey fetches one key strictly within one tenant.
func (s *gatewayPostgresStore) GetVirtualKey(ctx context.Context, orgID, id string) (*VirtualKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanVirtualKey(s.db.QueryRowContext(ctx, sqlSelectVirtualKeyScoped, id, orgID))
}

// ListVirtualKeys returns the tenant's keys in deterministic order.
func (s *gatewayPostgresStore) ListVirtualKeys(ctx context.Context, orgID string) ([]*VirtualKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectVirtualKeysByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*VirtualKey, 0)
	for rows.Next() {
		key, err := scanVirtualKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// UpdateVirtualKey applies a mutable-field update within one tenant; zero
// rows affected (unknown id or tenant guard) is ErrVirtualKeyNotFound.
func (s *gatewayPostgresStore) UpdateVirtualKey(ctx context.Context, key *VirtualKey) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateVirtualKey,
		key.ID, key.OrganizationID, key.Label, marshalModelsParam(key.Models),
		key.MonthlyLimitCents, nullableTime(key.ExpiresAt), key.UpdatedAt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrVirtualKeyNotFound
	}
	return nil
}

// RevokeVirtualKey flips status to revoked within one tenant.
func (s *gatewayPostgresStore) RevokeVirtualKey(ctx context.Context, orgID, id string, revokedAt time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlRevokeVirtualKey, id, orgID, revokedAt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrVirtualKeyNotFound
	}
	return nil
}

// RecordSpend appends one ledger row (append-only; see the file header).
func (s *gatewayPostgresStore) RecordSpend(ctx context.Context, orgID, keyID, model string, costCents float64, at time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertVirtualKeySpend,
		uuid.NewString(), orgID, keyID, monthKey(at), model, costCents, at.UTC())
	return err
}

// SpendForMonth sums one (org, key, month) bucket.
func (s *gatewayPostgresStore) SpendForMonth(ctx context.Context, orgID, keyID, month string) (float64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	var total float64
	if err := s.db.QueryRowContext(ctx, sqlSpendForMonth, orgID, keyID, month).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// scanVirtualKey decodes one row; JSONB NULL keeps the allow-list DISABLED
// (nil) and the '[]' payload keeps it ENABLED-and-empty — the nil-vs-empty
// contract survives the database round-trip.
func scanVirtualKey(scanner interface{ Scan(dest ...any) error }) (*VirtualKey, error) {
	var key VirtualKey
	var rawModels []byte
	var expiresAt, revokedAt sql.NullTime
	if err := scanner.Scan(&key.ID, &key.OrganizationID, &key.Label, &key.KeyPrefix, &key.KeyHash,
		&key.CreatedBy, &rawModels, &key.MonthlyLimitCents, &key.Status,
		&expiresAt, &revokedAt, &key.CreatedAt, &key.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVirtualKeyNotFound
		}
		return nil, err
	}
	if rawModels == nil {
		key.Models = nil
	} else if err := json.Unmarshal(rawModels, &key.Models); err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		key.ExpiresAt = &t
	}
	if revokedAt.Valid {
		t := revokedAt.Time
		key.RevokedAt = &t
	}
	return &key, nil
}

// nullableTime maps a nil/zero timestamp to SQL NULL (approvals-store
// convention).
func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

var _ GatewayStore = (*gatewayPostgresStore)(nil)
