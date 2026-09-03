package sso

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

// OrgSSO couples the tenant identity with its (optional) OIDC configuration.
type OrgSSO struct {
	OrgID   string
	OrgName string
	Config  *SSOConfig // nil when SSO is not configured for the tenant
}

// ConfigStore resolves tenant OIDC configurations. Lookups by slug power the
// login entry point; the org-id lookup re-reads the config during the
// callback so a config change between Begin and Complete is honored (and so
// the state entry itself never carries configuration data).
type ConfigStore interface {
	// GetConfigByOrgSlug resolves a tenant by its URL slug (normalized
	// organization name). Returns ErrOrgNotFound for unknown slugs and
	// ErrNotConfigured when the org exists but sso_config is NULL.
	GetConfigByOrgSlug(ctx context.Context, slug string) (*OrgSSO, error)
	// GetConfig resolves the tenant by primary key.
	GetConfig(ctx context.Context, orgID string) (*OrgSSO, error)
	// SaveConfig upserts organizations.sso_config for one tenant.
	SaveConfig(ctx context.Context, orgID string, cfg *SSOConfig) error
}

// ErrNilDB is returned by pg constructors when handed a nil *sql.DB.
var ErrNilDB = errors.New("sso: database is nil")

// memoryConfigStore is the zero-infrastructure ConfigStore. Org rows are
// registered explicitly (dual-mode wiring seeds them from the auth service);
// configurations live in a map guarded by a mutex.
type memoryConfigStore struct {
	mu   sync.RWMutex
	orgs map[string]string // orgID -> org name
	cfgs map[string]*SSOConfig
}

// NewMemoryConfigStore returns an empty in-memory ConfigStore.
func NewMemoryConfigStore() ConfigStore {
	return &memoryConfigStore{
		orgs: make(map[string]string),
		cfgs: make(map[string]*SSOConfig),
	}
}

// SeedOrg registers (or renames) an organization so slug lookups can resolve
// it without a database. Used by dual-mode wiring and tests.
func SeedOrg(store ConfigStore, orgID, name string) {
	if m, ok := store.(*memoryConfigStore); ok {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.orgs[orgID] = name
	}
}

func (m *memoryConfigStore) lookup(_ context.Context, orgID, name string) *OrgSSO {
	cfg, ok := m.cfgs[orgID]
	if !ok || cfg == nil {
		return &OrgSSO{OrgID: orgID, OrgName: name, Config: nil}
	}
	return &OrgSSO{OrgID: orgID, OrgName: name, Config: cfg.clone()}
}

func (m *memoryConfigStore) GetConfigByOrgSlug(ctx context.Context, slug string) (*OrgSSO, error) {
	want := slugify(slug)
	if want == "" {
		return nil, ErrOrgNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for orgID, name := range m.orgs {
		if slugify(name) == want {
			org := m.lookup(ctx, orgID, name)
			if org.Config == nil {
				return nil, ErrNotConfigured
			}
			return org, nil
		}
	}
	return nil, ErrOrgNotFound
}

func (m *memoryConfigStore) GetConfig(ctx context.Context, orgID string) (*OrgSSO, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	name, ok := m.orgs[orgID]
	if !ok {
		return nil, ErrOrgNotFound
	}
	org := m.lookup(ctx, orgID, name)
	if org.Config == nil {
		return nil, ErrNotConfigured
	}
	return org, nil
}

func (m *memoryConfigStore) SaveConfig(_ context.Context, orgID string, cfg *SSOConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.orgs[orgID]; !ok {
		return ErrOrgNotFound
	}
	m.cfgs[orgID] = cfg.clone()
	return nil
}

// pgConfigStore reads/writes organizations.sso_config (JSONB, migration 019).
type pgConfigStore struct {
	db *sql.DB
}

// NewPostgresConfigStore returns the Postgres-backed ConfigStore.
func NewPostgresConfigStore(db *sql.DB) (ConfigStore, error) {
	if db == nil {
		return nil, ErrNilDB
	}
	return &pgConfigStore{db: db}, nil
}

const (
	// Slug resolution mirrors slugify(): case-insensitive name match first,
	// then the normalized (non-alphanumeric runs -> '-') form. sso_config is
	// returned as text and NULL mapped to ''.
	sqlSelectSSOConfigBySlug = `SELECT id, name, COALESCE(sso_config::text, '') FROM organizations ` +
		`WHERE lower(name) = lower($1) ` +
		`OR lower(regexp_replace(name, '[^a-zA-Z0-9]+', '-', 'g')) = lower($1) ` +
		`OR lower(regexp_replace(name, '^[^a-zA-Z0-9]+|[^a-zA-Z0-9]+$', '', 'g')) = lower($1) LIMIT 1`
	sqlSelectSSOConfigByOrgID = `SELECT id, name, COALESCE(sso_config::text, '') FROM organizations WHERE id = $1`
	sqlUpsertSSOConfig        = `UPDATE organizations SET sso_config = $1::jsonb, updated_at = NOW() WHERE id = $2`
)

func scanOrgSSO(row *sql.Row) (*OrgSSO, error) {
	var org OrgSSO
	var raw string
	if err := row.Scan(&org.OrgID, &org.OrgName, &raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrgNotFound
		}
		return nil, err
	}
	cfg, err := parseSSOConfig(raw)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &OrgSSO{OrgID: org.OrgID, OrgName: org.OrgName, Config: nil}, nil
	}
	return &OrgSSO{OrgID: org.OrgID, OrgName: org.OrgName, Config: cfg}, nil
}

func (p *pgConfigStore) GetConfigByOrgSlug(ctx context.Context, slug string) (*OrgSSO, error) {
	if p.db == nil {
		return nil, ErrNilDB
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, ErrOrgNotFound
	}
	row := p.db.QueryRowContext(ctx, sqlSelectSSOConfigBySlug, slug)
	org, err := scanOrgSSO(row)
	if err != nil {
		return nil, err
	}
	if org.Config == nil {
		return nil, ErrNotConfigured
	}
	return org, nil
}

func (p *pgConfigStore) GetConfig(ctx context.Context, orgID string) (*OrgSSO, error) {
	if p.db == nil {
		return nil, ErrNilDB
	}
	row := p.db.QueryRowContext(ctx, sqlSelectSSOConfigByOrgID, strings.TrimSpace(orgID))
	org, err := scanOrgSSO(row)
	if err != nil {
		return nil, err
	}
	if org.Config == nil {
		return nil, ErrNotConfigured
	}
	return org, nil
}

// SaveConfig upserts the JSONB config. SECURITY: the inline ClientSecret is
// rejected in Postgres mode — plaintext IdP credentials must never reach the
// database; store a secret and reference it via client_secret_ref instead.
func (p *pgConfigStore) SaveConfig(ctx context.Context, orgID string, cfg *SSOConfig) error {
	if p.db == nil {
		return ErrNilDB
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ClientSecret) != "" {
		return errors.New("refusing to persist a plaintext sso client secret; use client_secret_ref")
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	res, err := p.db.ExecContext(ctx, sqlUpsertSSOConfig, string(payload), strings.TrimSpace(orgID))
	if err != nil {
		return err
	}
	if n, rerr := res.RowsAffected(); rerr == nil && n == 0 {
		return ErrOrgNotFound
	}
	return nil
}
