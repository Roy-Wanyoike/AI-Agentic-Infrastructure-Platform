package apikeys

// Postgres persistence for agent-scoped API keys (wave-8, agent identity v1).
// The pgStore already implements the user-key Store; these additive methods
// make the SAME handle implement AgentKeyStore, so NewServiceWithStore adopts
// both surfaces from one construction site (see service.go).
//
// The SQL mirrors the user-key contract exactly, plus one extra tenant guard:
// every id-based statement filters organization_id AND agent_id, so a key can
// never be read, updated or revoked outside its (org, agent) pair. The
// allow-list rides a JSONB column where SQL NULL ↔ nil (allow-list disabled,
// all tools) and '[]' ↔ empty non-nil slice (no tools) round-trip losslessly —
// the nil-vs-empty semantics pinned in agent_keys.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	// Tenant guard: the key row is written with its organization_id scope.
	sqlInsertAgentKey = `INSERT INTO agent_api_keys (id, organization_id, agent_id, name, prefix, key_hash, tool_allow_list, expires_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	// Credential-based tenant resolution: key_hash is the SHA-256 of a unique
	// secret, so the returned row identifies the single owning tenant.
	sqlSelectAgentKeyByHash = `SELECT id, organization_id, agent_id, name, prefix, key_hash, tool_allow_list, expires_at, revoked_at, last_used_at, created_at FROM agent_api_keys WHERE key_hash = $1 AND revoked_at IS NULL`
	// Double tenant guard: (organization_id, agent_id, id) must all match.
	sqlSelectAgentKey = `SELECT id, organization_id, agent_id, name, prefix, key_hash, tool_allow_list, expires_at, revoked_at, last_used_at, created_at FROM agent_api_keys WHERE id = $1 AND organization_id = $2 AND agent_id = $3`
	// Double tenant guard: updates only within the key's (org, agent) pair.
	sqlUpdateAgentKey = `UPDATE agent_api_keys SET name = $1, tool_allow_list = $2, expires_at = $3 WHERE id = $4 AND organization_id = $5 AND agent_id = $6`
	// Double tenant guard: revocation only within the key's (org, agent) pair;
	// the revoked_at IS NULL predicate keeps re-revocation a silent success.
	sqlRevokeAgentKey = `UPDATE agent_api_keys SET revoked_at = $1 WHERE id = $2 AND organization_id = $3 AND agent_id = $4 AND revoked_at IS NULL`
	// Double tenant guard: keys are listed for exactly one (org, agent) pair.
	sqlSelectAgentKeysByAgent = `SELECT id, organization_id, agent_id, name, prefix, key_hash, tool_allow_list, expires_at, revoked_at, last_used_at, created_at FROM agent_api_keys WHERE organization_id = $1 AND agent_id = $2 ORDER BY created_at DESC`
	// Best-effort usage stamp (authentication path; PK lookup).
	sqlTouchAgentKeyLastUsed = `UPDATE agent_api_keys SET last_used_at = $1 WHERE id = $2`
)

// CreateAgentKey implements AgentKeyStore.
func (s *pgStore) CreateAgentKey(ctx context.Context, key *AgentKey) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := key.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var expiresAt any
	if key.ExpiresAt != nil {
		expiresAt = *key.ExpiresAt
	}
	_, err := s.db.ExecContext(ctx, sqlInsertAgentKey,
		key.ID, key.OrganizationID, key.AgentID, key.Name, key.KeyPrefix, key.KeyHash,
		jsonParam(key.ToolAllowList), expiresAt, createdAt)
	return err
}

// GetAgentKeyByHash implements AgentKeyStore (authentication path).
func (s *pgStore) GetAgentKeyByHash(ctx context.Context, hash string) (*AgentKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanAgentKey(s.db.QueryRowContext(ctx, sqlSelectAgentKeyByHash, hash))
}

// GetAgentKey implements AgentKeyStore.
func (s *pgStore) GetAgentKey(ctx context.Context, orgID, agentID, keyID string) (*AgentKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanAgentKey(s.db.QueryRowContext(ctx, sqlSelectAgentKey, keyID, orgID, agentID))
}

// UpdateAgentKey implements AgentKeyStore.
func (s *pgStore) UpdateAgentKey(ctx context.Context, key *AgentKey) error {
	if err := s.guard(); err != nil {
		return err
	}
	var expiresAt any
	if key.ExpiresAt != nil {
		expiresAt = *key.ExpiresAt
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateAgentKey,
		key.Name, jsonParam(key.ToolAllowList), expiresAt,
		key.ID, key.OrganizationID, key.AgentID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (unknown key or wrong org/agent)
		return ErrAgentKeyNotFound
	}
	return nil
}

// RevokeAgentKey implements AgentKeyStore. Idempotent: re-revoking an
// already-revoked key updates zero rows on purpose (revoked_at IS NULL
// predicate) and stays a success, mirroring the user-key convention.
func (s *pgStore) RevokeAgentKey(ctx context.Context, orgID, agentID, keyID string, revokedAt time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlRevokeAgentKey, revokedAt, keyID, orgID, agentID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Either the key is already revoked (idempotent success) or the
		// tenant guard rejected the update (unknown/foreign key). Distinguish
		// the two so 404 semantics stay exact.
		var exists bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_api_keys WHERE id = $1 AND organization_id = $2 AND agent_id = $3)`, keyID, orgID, agentID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrAgentKeyNotFound
		}
	}
	return nil
}

// ListAgentKeys implements AgentKeyStore.
func (s *pgStore) ListAgentKeys(ctx context.Context, orgID, agentID string) ([]*AgentKey, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Double tenant guard: WHERE organization_id = $1 AND agent_id = $2
	rows, err := s.db.QueryContext(ctx, sqlSelectAgentKeysByAgent, orgID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*AgentKey, 0)
	for rows.Next() {
		key, err := scanAgentKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// TouchAgentKeyLastUsed implements AgentKeyStore (best-effort by contract:
// callers ignore the error).
func (s *pgStore) TouchAgentKeyLastUsed(ctx context.Context, id string, usedAt time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlTouchAgentKeyLastUsed, usedAt, id)
	return err
}

// scanAgentKey decodes one agent_api_keys row. NULL tool_allow_list maps to
// nil (allow-list disabled) and '[]' to an empty non-nil slice (no tools);
// json.Unmarshal of a JSON null leaves the slice nil, which is exactly the
// same mapping.
func scanAgentKey(scanner interface{ Scan(dest ...any) error }) (*AgentKey, error) {
	var key AgentKey
	var allowListRaw []byte
	var expiresAt, revokedAt, lastUsedAt sql.NullTime
	if err := scanner.Scan(&key.ID, &key.OrganizationID, &key.AgentID, &key.Name, &key.KeyPrefix, &key.KeyHash,
		&allowListRaw, &expiresAt, &revokedAt, &lastUsedAt, &key.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAgentKeyNotFound
		}
		return nil, err
	}
	if allowListRaw != nil {
		var allowList []string
		if err := json.Unmarshal(allowListRaw, &allowList); err != nil {
			return nil, err
		}
		key.ToolAllowList = allowList
	}
	if expiresAt.Valid {
		key.ExpiresAt = &expiresAt.Time
	}
	if revokedAt.Valid {
		key.RevokedAt = &revokedAt.Time
	}
	if lastUsedAt.Valid {
		key.LastUsedAt = &lastUsedAt.Time
	}
	return &key, nil
}
