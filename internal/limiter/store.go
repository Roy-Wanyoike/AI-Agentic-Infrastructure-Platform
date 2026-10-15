package limiter

// store.go — rate-limit CONFIGURATION persistence (issue #124, migration
// 036 rate_limits). The policies package has no rate-limit condition type
// and is intentionally NOT modified: limit resolution reads this dedicated
// table instead (see resolver.go).
//
// Row shape (migration 036_rate_limits.sql):
//
//      rate_limits (organization_id, scope, subject_id NULLABLE,
//                   rpm, rpd, tpm, enabled)
//
// subject_id NULL (in-memory sentinel "") is the ORG DEFAULT of a scope;
// a non-null subject_id is the subject-specific row. Uniqueness is enforced
// by the expression index
//
//      ux_rate_limits_subject (organization_id, scope, COALESCE(subject_id, ''))
//
// because a PRIMARY KEY cannot span a nullable column in Postgres — the
// functional equivalent of the issue's "PK (org_id, scope, subject_id)"
// while keeping subject_id genuinely nullable. The in-memory store maps the
// wire value "" to the same org-default row; the Postgres store maps it to
// SQL NULL via NULLIF.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrInvalidConfig is returned when a Config record fails validation.
var ErrInvalidConfig = errors.New("limiter: invalid rate limit config")

// ErrConfigNotFound is returned by Store.Get/Delete when no row matches.
var ErrConfigNotFound = errors.New("limiter: rate limit config not found")

// Config is one stored rate-limit row. Subject "" is the org default of the
// scope. Zero RPM/RPD/TPM mean unlimited for that dimension; a Config with
// no positive dimension is valid but inactive (kept so operators can stage
// rows; Enabled=false disables the row entirely — and an explicitly disabled
// SUBJECT row suppresses the org default for that subject, the documented
// per-subject opt-out).
type Config struct {
	OrganizationID string    `json:"-"`
	Scope          Scope     `json:"scope"`
	Subject        string    `json:"subject"` // "" = org default
	RPM            int       `json:"rpm"`
	RPD            int       `json:"rpd"`
	TPM            int       `json:"tpm"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

// Validate checks the record invariants (scope, non-negative limits,
// org scope has no subject).
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: config is nil", ErrInvalidConfig)
	}
	if !c.Scope.Valid() {
		return fmt.Errorf("%w: scope must be one of org|agent|tool|model", ErrInvalidConfig)
	}
	if c.Scope == ScopeOrg && strings.TrimSpace(c.Subject) != "" {
		return fmt.Errorf("%w: the org scope has no subject id", ErrInvalidConfig)
	}
	if c.RPM < 0 || c.RPD < 0 || c.TPM < 0 {
		return fmt.Errorf("%w: limits must be >= 0 (0 = unlimited)", ErrInvalidConfig)
	}
	return nil
}

// Normalize trims the subject and stamps timestamps. Mutates and returns the
// receiver.
func (c *Config) Normalize() *Config {
	c.Subject = strings.TrimSpace(c.Subject)
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	return c
}

// Store persists rate-limit configuration rows for one organization at a
// time (tenant scoping is by convention: every method takes orgID and
// implementations MUST filter on it). Implementations must be safe for
// concurrent use.
type Store interface {
	// List returns every row of the org (both org defaults and
	// subject-specific rows), ordered deterministically.
	List(ctx context.Context, orgID string) ([]Config, error)
	// Get returns one row, or ErrConfigNotFound.
	Get(ctx context.Context, orgID string, scope Scope, subject string) (*Config, error)
	// Upsert inserts or updates the row identified by
	// (orgID, scope, subject).
	Upsert(ctx context.Context, orgID string, cfg Config) error
	// Delete removes the row; ErrConfigNotFound when absent.
	Delete(ctx context.Context, orgID string, scope Scope, subject string) error
}

// MemoryStore is the zero-infrastructure Store (the residency/marketplace
// precedent): process-local state, safe for concurrent use.
type MemoryStore struct {
	mu      sync.RWMutex
	rows    map[string]*Config
	counter int // deterministic insert-order tiebreaker for List
}

// NewMemoryStore builds an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]*Config)}
}

func memConfigKey(orgID string, scope Scope, subject string) string {
	return orgID + "\x00" + string(scope) + "\x00" + subject
}

// List implements Store.
func (m *MemoryStore) List(_ context.Context, orgID string) ([]Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Config, 0, len(m.rows))
	for _, cfg := range m.rows {
		if cfg.OrganizationID == orgID {
			out = append(out, *cfg)
		}
	}
	sortConfigs(out)
	return out, nil
}

// Get implements Store.
func (m *MemoryStore) Get(_ context.Context, orgID string, scope Scope, subject string) (*Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfg, ok := m.rows[memConfigKey(orgID, scope, subject)]
	if !ok {
		return nil, ErrConfigNotFound
	}
	out := *cfg
	return &out, nil
}

// Upsert implements Store.
func (m *MemoryStore) Upsert(_ context.Context, orgID string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.OrganizationID = orgID
	cfg.Normalize()
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memConfigKey(orgID, cfg.Scope, cfg.Subject)
	if prev, ok := m.rows[key]; ok {
		cfg.CreatedAt = prev.CreatedAt
	}
	m.rows[key] = &cfg
	return nil
}

// Delete implements Store.
func (m *MemoryStore) Delete(_ context.Context, orgID string, scope Scope, subject string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memConfigKey(orgID, scope, subject)
	if _, ok := m.rows[key]; !ok {
		return ErrConfigNotFound
	}
	delete(m.rows, key)
	return nil
}

// sortConfigs orders rows by scope then subject (org defaults first: the
// empty subject sorts before every subject id).
func sortConfigs(rows []Config) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Scope != rows[j].Scope {
			return rows[i].Scope < rows[j].Scope
		}
		return rows[i].Subject < rows[j].Subject
	})
}

// PostgresStore persists rate-limit rows in the rate_limits table
// (migration 036). subject "" maps to SQL NULL (NULLIF) so org-default rows
// are genuinely nullable rows, matched with IS NOT DISTINCT FROM.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore builds the SQL-backed store.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

const rateLimitColumns = `organization_id, scope, subject_id, rpm, rpd, tpm, enabled, created_at, updated_at`

// Upsert implements Store via the expression-index conflict target (see the
// file header): one statement, atomic, idempotent.
func (p *PostgresStore) Upsert(ctx context.Context, orgID string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.OrganizationID = orgID
	cfg.Normalize()
	const q = `INSERT INTO rate_limits (organization_id, scope, subject_id, rpm, rpd, tpm, enabled, created_at, updated_at)
VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9)
ON CONFLICT (organization_id, scope, COALESCE(subject_id, '')) DO UPDATE SET
rpm = EXCLUDED.rpm, rpd = EXCLUDED.rpd, tpm = EXCLUDED.tpm,
enabled = EXCLUDED.enabled, updated_at = EXCLUDED.updated_at`
	_, err := p.db.ExecContext(ctx, q,
		orgID, string(cfg.Scope), cfg.Subject, cfg.RPM, cfg.RPD, cfg.TPM, cfg.Enabled,
		cfg.CreatedAt, cfg.UpdatedAt)
	return err
}

// Get implements Store.
func (p *PostgresStore) Get(ctx context.Context, orgID string, scope Scope, subject string) (*Config, error) {
	const q = `SELECT ` + rateLimitColumns + ` FROM rate_limits
WHERE organization_id = $1 AND scope = $2 AND subject_id IS NOT DISTINCT FROM NULLIF($3, '')`
	row := p.db.QueryRowContext(ctx, q, orgID, string(scope), subject)
	cfg, err := scanRateLimit(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConfigNotFound
	}
	return cfg, err
}

// List implements Store.
func (p *PostgresStore) List(ctx context.Context, orgID string) ([]Config, error) {
	const q = `SELECT ` + rateLimitColumns + ` FROM rate_limits
WHERE organization_id = $1 ORDER BY scope ASC, subject_id ASC NULLS FIRST`
	rows, err := p.db.QueryContext(ctx, q, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Config, 0)
	for rows.Next() {
		cfg, err := scanRateLimit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *cfg)
	}
	return out, rows.Err()
}

// Delete implements Store.
func (p *PostgresStore) Delete(ctx context.Context, orgID string, scope Scope, subject string) error {
	const q = `DELETE FROM rate_limits
WHERE organization_id = $1 AND scope = $2 AND subject_id IS NOT DISTINCT FROM NULLIF($3, '')`
	res, err := p.db.ExecContext(ctx, q, orgID, string(scope), subject)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrConfigNotFound
	}
	return nil
}

// scanner abstracts *sql.Row / *sql.Rows for one scan helper.
type scanner interface {
	Scan(dest ...any) error
}

func scanRateLimit(row scanner) (*Config, error) {
	var cfg Config
	var scope string
	var subject sql.NullString
	if err := row.Scan(&cfg.OrganizationID, &scope, &subject, &cfg.RPM, &cfg.RPD, &cfg.TPM,
		&cfg.Enabled, &cfg.CreatedAt, &cfg.UpdatedAt); err != nil {
		return nil, err
	}
	cfg.Scope = Scope(scope)
	cfg.Subject = subject.String // NULL -> "" (org default)
	return &cfg, nil
}
