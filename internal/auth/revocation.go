package auth

// Server-side token revocation (issue #76): the deny list consulted by
// ValidateToken so logout and refresh-rotation can kill a token before its
// natural expiry.
//
// Revocation identity strategy (ONE documented strategy, pinned by tests):
//
//	Tokens WITH a jti claim (every token minted after issue #76) are
//	revoked by the deny-list key "jti:<jti>" — stable, tiny and unique per
//	token. Legacy tokens minted before issue #76 carry NO jti; they remain
//	fully revocable under the key "tok:<sha256hex(full token string)>",
//	which is derived deterministically from the presented credential, so a
//	legacy token can be logged out and every later presentation of the very
//	same string resolves the same key. The derivation lives in
//	TokenReference and is used identically by LogoutCtx, RefreshCtx and
//	ValidateToken.
//
// The store behind the deny list is optional and dual-mode (the package's
// established SetStore style): the in-memory implementation is ACTIVE BY
// DEFAULT (zero-infrastructure mode keeps working logout/refresh), and the
// Postgres implementation shares the deny list across API replicas. Passing
// nil to SetRevocationStore degrades the feature gracefully (validation
// succeeds, logout stays a 204 no-op) — tests cover both modes.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	revocationKeyJTIPrefix = "jti:"
	revocationKeyTokPrefix = "tok:"
)

// ErrTokenRevoked is returned by ValidateToken when the presented token's
// revocation key is on the deny list (logged out, or rotated away by a
// single-use refresh). The middleware surfaces it verbatim as the 401 body.
var ErrTokenRevoked = errors.New("token has been revoked")

// RevocationStore is the deny-list backend. Implementations must treat a
// ttl <= 0 as a no-op (the token is already past its expiry, so a deny row
// would outlive its usefulness) and Revoke idempotently (re-revoking the
// same key refreshes the TTL).
type RevocationStore interface {
	// Revoke adds key to the deny list for ttl (the remaining token
	// lifetime; entries self-expire afterwards).
	Revoke(ctx context.Context, key string, ttl time.Duration) error
	// Revoked reports whether key is currently on the deny list. Expired
	// entries MUST read as false (lazy expiry).
	Revoked(ctx context.Context, key string) (bool, error)
}

// TokenReference derives the deny-list key (and audit-safe reference) for a
// token: "jti:<jti>" when the parsed claims carry a jti, otherwise
// "tok:<sha256 hex of the full token string>" for legacy tokens. The
// derivation is pure: the same presented token always yields the same key.
func TokenReference(token string, claims *Claims) string {
	if claims != nil && strings.TrimSpace(claims.JTI) != "" {
		return revocationKeyJTIPrefix + strings.TrimSpace(claims.JTI)
	}
	sum := sha256.Sum256([]byte(token))
	return revocationKeyTokPrefix + hex.EncodeToString(sum[:])
}

// memoryRevocationSweepThreshold bounds the in-memory deny list: once this
// many entries exist, Revoke first drops every already-expired entry.
// Memory-bounding approach (documented contract): expiry is enforced lazily
// on lookup (an expired entry never reads as revoked), and the sweep-on-
// write keeps the map proportional to the tokens revoked but not yet
// naturally expired (<= 24h token lifetime). Live revocations are never
// evicted — dropping one would un-revoke a live token — so the hard ceiling
// is "distinct tokens logged out within one token lifetime", a few dozen
// bytes each.
const memoryRevocationSweepThreshold = 4096

// MemoryRevocationStore is the zero-infrastructure RevocationStore and the
// default wired by NewService/NewServiceWithStore. Safe for concurrent use.
type MemoryRevocationStore struct {
	mu      sync.RWMutex
	entries map[string]time.Time // key -> absolute expiry
}

// NewMemoryRevocationStore returns an empty in-memory deny list.
func NewMemoryRevocationStore() *MemoryRevocationStore {
	return &MemoryRevocationStore{entries: make(map[string]time.Time)}
}

var _ RevocationStore = (*MemoryRevocationStore)(nil)

// Revoke records key until now+ttl. A non-positive ttl is a no-op.
func (m *MemoryRevocationStore) Revoke(_ context.Context, key string, ttl time.Duration) error {
	if m == nil || strings.TrimSpace(key) == "" || ttl <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) >= memoryRevocationSweepThreshold {
		now := time.Now()
		for k, exp := range m.entries {
			if !now.Before(exp) {
				delete(m.entries, k)
			}
		}
	}
	m.entries[key] = time.Now().Add(ttl)
	return nil
}

// Revoked reports whether key is deny-listed and not yet expired (lazy
// expiry: an expired entry is indistinguishable from an absent one).
func (m *MemoryRevocationStore) Revoked(_ context.Context, key string) (bool, error) {
	if m == nil || strings.TrimSpace(key) == "" {
		return false, nil
	}
	m.mu.RLock()
	exp, ok := m.entries[key]
	m.mu.RUnlock()
	if !ok {
		return false, nil
	}
	return time.Now().Before(exp), nil
}

// size reports the raw entry count (expired entries included until they are
// swept or lazily ignored); used by tests to pin the sweep bound.
func (m *MemoryRevocationStore) size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// pgRevocationStore is the Postgres deny-list (migration 023,
// auth_revocations) shared by every API replica: a logout or refresh
// performed on one node kills the token on all of them.
type pgRevocationStore struct {
	db *sql.DB
}

// NewPostgresRevocationStore returns a RevocationStore backed by *sql.DB.
func NewPostgresRevocationStore(db *sql.DB) RevocationStore {
	return &pgRevocationStore{db: db}
}

var _ RevocationStore = (*pgRevocationStore)(nil)

const (
	// Upsert semantics make Revoke idempotent; re-revoking the same key
	// refreshes expires_at to the newly computed remaining lifetime.
	sqlRevokeToken = `INSERT INTO auth_revocations (key, expires_at) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET expires_at = EXCLUDED.expires_at`
	// Opportunistic cleanup bounding the table: dead entries (past their
	// TTL) can never serve a purpose, so every revoke sweeps them. The
	// expires_at index (migration 023) keeps this cheap.
	sqlRevokeTokenCleanup = `DELETE FROM auth_revocations WHERE expires_at <= NOW()`
	// Lazy expiry on lookup: only entries whose TTL is still live count.
	sqlTokenRevoked = `SELECT EXISTS (SELECT 1 FROM auth_revocations WHERE key = $1 AND expires_at > NOW())`
)

func (s *pgRevocationStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("auth: revocation database is nil")
	}
	return nil
}

// Revoke upserts the deny row and sweeps expired rows in the same call.
func (s *pgRevocationStore) Revoke(ctx context.Context, key string, ttl time.Duration) error {
	if err := s.guard(); err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" || ttl <= 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, sqlRevokeToken, key, time.Now().Add(ttl)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlRevokeTokenCleanup)
	return err
}

// Revoked consults the deny list; only live (unexpired) entries match.
func (s *pgRevocationStore) Revoked(ctx context.Context, key string) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	if strings.TrimSpace(key) == "" {
		return false, nil
	}
	var revoked bool
	if err := s.db.QueryRowContext(ctx, sqlTokenRevoked, key).Scan(&revoked); err != nil {
		return false, err
	}
	return revoked, nil
}
