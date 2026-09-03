package scim

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"
)

// store.go persists SCIM bearer credentials. The Postgres table is
// scim_tokens (migration 019): token_hash is SHA-256 hex with a UNIQUE
// constraint — the presented secret resolves to exactly one tenant row, the
// same trust model as api_keys.key_hash.

// memoryTokenStore is the zero-infrastructure TokenStore (tests and
// in-memory mode). Hashes are process-local secrets; there is deliberately
// no way to enumerate plaintexts because none exist here either.
type memoryTokenStore struct {
	mu     sync.Mutex
	byHash map[string]*Token
}

func newMemoryTokenStore() *memoryTokenStore {
	return &memoryTokenStore{byHash: make(map[string]*Token)}
}

func cloneToken(t *Token) *Token {
	clone := *t
	return &clone
}

func (m *memoryTokenStore) CreateToken(_ context.Context, token *Token) error {
	if token == nil || strings.TrimSpace(token.TokenHash) == "" {
		return errors.New("scim token hash is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byHash[token.TokenHash]; exists {
		return errors.New("scim token already exists")
	}
	m.byHash[token.TokenHash] = cloneToken(token)
	return nil
}

func (m *memoryTokenStore) GetTokenByHash(_ context.Context, hash string) (*Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token, ok := m.byHash[hash]
	if !ok {
		return nil, ErrTokenNotFound
	}
	return cloneToken(token), nil
}

func (m *memoryTokenStore) RevokeToken(_ context.Context, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, token := range m.byHash {
		if token.ID == id && token.OrgID == orgID {
			if token.RevokedAt.IsZero() {
				token.RevokedAt = time.Now().UTC()
			}
			return nil
		}
	}
	return ErrTokenNotFound
}

var _ TokenStore = (*memoryTokenStore)(nil)

const (
	// sqlInsertSCIMToken persists the hash only — the plaintext never
	// reaches storage (CREATE ... RETURNING nothing).
	sqlInsertSCIMToken = `INSERT INTO scim_tokens (id, organization_id, token_hash, created_by, created_at) VALUES ($1, $2, $3, $4, $5)`
	// Authentication path: the presented secret's hash resolves the single
	// tenant row (UNIQUE token_hash in migration 019).
	sqlSelectSCIMTokenByHash = `SELECT id, organization_id, token_hash, created_by, created_at, revoked_at FROM scim_tokens WHERE token_hash = $1`
	// Revocation is org-guarded and one-way; revoked_at IS NULL keeps
	// repeated revokes idempotent.
	sqlRevokeSCIMToken = `UPDATE scim_tokens SET revoked_at = NOW() WHERE id = $2 AND organization_id = $1 AND revoked_at IS NULL`
)

// pgTokenStore is the Postgres TokenStore over scim_tokens (migration 019).
type pgTokenStore struct {
	db *sql.DB
}

// NewPostgresTokenStore returns the durable TokenStore.
func NewPostgresTokenStore(db *sql.DB) (TokenStore, error) {
	if db == nil {
		return nil, ErrNilDB
	}
	return &pgTokenStore{db: db}, nil
}

func (p *pgTokenStore) CreateToken(ctx context.Context, token *Token) error {
	if p.db == nil {
		return ErrNilDB
	}
	createdAt := token.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := p.db.ExecContext(ctx, sqlInsertSCIMToken,
		token.ID, token.OrgID, token.TokenHash, token.CreatedBy, createdAt)
	return err
}

func (p *pgTokenStore) GetTokenByHash(ctx context.Context, hash string) (*Token, error) {
	if p.db == nil {
		return nil, ErrNilDB
	}
	var token Token
	// revoked_at is NULL for every live credential, so it must be scanned
	// through sql.NullTime — database/sql cannot convert NULL into a plain
	// time.Time (a live token would fail its own authentication query).
	var revokedAt sql.NullTime
	err := p.db.QueryRowContext(ctx, sqlSelectSCIMTokenByHash, strings.TrimSpace(hash)).Scan(
		&token.ID, &token.OrgID, &token.TokenHash, &token.CreatedBy, &token.CreatedAt, &revokedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	if revokedAt.Valid {
		token.RevokedAt = revokedAt.Time
	}
	return &token, nil
}

func (p *pgTokenStore) RevokeToken(ctx context.Context, orgID, id string) error {
	if p.db == nil {
		return ErrNilDB
	}
	res, err := p.db.ExecContext(ctx, sqlRevokeSCIMToken, strings.TrimSpace(orgID), strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if n, rerr := res.RowsAffected(); rerr == nil && n == 0 {
		return ErrTokenNotFound
	}
	return nil
}

var _ TokenStore = (*pgTokenStore)(nil)
