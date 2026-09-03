package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"
)

// ProvisioningStore is an optional Store extension for the SSO (OIDC) and
// SCIM 2.0 user lifecycle (issue #29). It is implemented by pgStore and
// MemoryStore; the existing Store interface and its callers are untouched.
//
// Every mutating operation is guarded by organization_id so a tenant (or a
// SCIM token bound to one) can only ever touch its own identities.
type ProvisioningStore interface {
	Store
	// GetUserByID resolves one user by primary key. Used by SCIM point
	// lookups and by the SSO callback to re-read a freshly linked user.
	GetUserByID(ctx context.Context, userID string) (*User, error)
	// LinkSSOSubject sets users.sso_subject for one user within one
	// tenant. Idempotent: linking the same subject twice is a no-op.
	// RowsAffected == 0 (unknown user or foreign org) returns
	// ErrUserNotFound.
	LinkSSOSubject(ctx context.Context, orgID, userID, subject string) error
	// SetUserActive toggles the SCIM lifecycle flag (active=false blocks
	// password login). Org-guarded; unknown/foreign user returns
	// ErrUserNotFound.
	SetUserActive(ctx context.Context, orgID, userID string, active bool) error
}

// Compile-time guarantee: the Postgres store satisfies the full provisioning
// surface required by internal/sso and internal/scim.
var _ ProvisioningStore = (*pgStore)(nil)

const (
	// Org-guarded SSO subject link (issue #29). Partial unique index
	// uq_users_sso_subject (migration 019) makes a duplicate subject a
	// 23505 at the DB level; the service layer checks-before-write for a
	// friendlier error.
	sqlLinkSSOSubject = `UPDATE users SET sso_subject = $1 WHERE id = $2 AND organization_id = $3`
	// SCIM lifecycle toggle: WHERE active IS DISTINCT FROM $3 keeps the
	// affected-row count honest for idempotent repeated PATCHes.
	sqlSetUserActive = `UPDATE users SET active = $1 WHERE id = $2 AND organization_id = $3 AND active IS DISTINCT FROM $1`
	// Primary-key lookup used by SCIM point reads and the SSO callback.
	sqlSelectAuthUserByID = `SELECT id, organization_id, email, password_hash, role, created_at, COALESCE(sso_subject, ''), active FROM users WHERE id = $1`
)

// GetUserByID implements ProvisioningStore against Postgres.
func (s *pgStore) GetUserByID(ctx context.Context, userID string) (*User, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var user User
	err := s.db.QueryRowContext(ctx, sqlSelectAuthUserByID, strings.TrimSpace(userID)).Scan(
		&user.ID, &user.Organization, &user.Email, &user.PasswordHash, &user.Role, &user.CreatedAt,
		&user.SSOSubject, &user.Active,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// LinkSSOSubject implements ProvisioningStore against Postgres.
func (s *pgStore) LinkSSOSubject(ctx context.Context, orgID, userID, subject string) error {
	if err := s.guard(); err != nil {
		return err
	}
	if strings.TrimSpace(subject) == "" {
		return errors.New("sso subject is required")
	}
	res, err := s.db.ExecContext(ctx, sqlLinkSSOSubject, subject, userID, orgID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetUserActive implements ProvisioningStore against Postgres.
func (s *pgStore) SetUserActive(ctx context.Context, orgID, userID string, active bool) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlSetUserActive, active, userID, orgID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Distinguish "already in the requested state" (idempotent
		// success) from "unknown user / foreign tenant".
		var exists bool
		check := `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND organization_id = $2)`
		if err := s.db.QueryRowContext(ctx, check, userID, orgID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrUserNotFound
		}
	}
	return nil
}

// MemoryStore is the zero-infrastructure Store + ProvisioningStore used by
// in-memory mode (and by tests of internal/sso, internal/scim and cmd/api).
// It is safe for concurrent use. The dual-mode wiring shares ONE MemoryStore
// instance between auth.Service and the SCIM/SSO services so the identity
// table stays coherent without a database.
type MemoryStore struct {
	mu    sync.RWMutex
	orgs  map[string]*Organization
	users map[string]*User
	// usersByEmail indexes the case-insensitive login credential.
	usersByEmail map[string]string
}

// NewMemoryStore returns an empty in-memory identity store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		orgs:         make(map[string]*Organization),
		users:        make(map[string]*User),
		usersByEmail: make(map[string]string),
	}
}

var (
	_ Store             = (*MemoryStore)(nil)
	_ ProvisioningStore = (*MemoryStore)(nil)
)

// CreateOrganization inserts the tenant root row (duplicated id rejected).
func (m *MemoryStore) CreateOrganization(_ context.Context, org *Organization) error {
	if org == nil || strings.TrimSpace(org.ID) == "" {
		return errors.New("organization id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orgs[org.ID] = &Organization{ID: org.ID, Name: org.Name}
	return nil
}

// CreateUser persists a user; email must be globally unique like the
// users.email UNIQUE constraint in migration 001.
func (m *MemoryStore) CreateUser(_ context.Context, user *User) error {
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return errors.New("user id is required")
	}
	email := strings.ToLower(strings.TrimSpace(user.Email))
	if email == "" {
		return errors.New("email is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, taken := m.usersByEmail[email]; taken {
		return errors.New("email already registered")
	}
	if _, ok := m.orgs[user.Organization]; !ok && user.Organization != "" {
		return errors.New("organization not found")
	}
	clone := *user
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = time.Now().UTC()
	}
	if strings.TrimSpace(clone.Role) == "" {
		clone.Role = defaultRole
	}
	clone.Email = email
	m.users[clone.ID] = &clone
	m.usersByEmail[email] = clone.ID
	return nil
}

// GetUserByEmail resolves the login identity (case-insensitive credential).
func (m *MemoryStore) GetUserByEmail(_ context.Context, email string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.usersByEmail[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		return nil, ErrUserNotFound
	}
	clone := *m.users[id]
	return &clone, nil
}

// GetUserByID implements ProvisioningStore against the in-memory table.
func (m *MemoryStore) GetUserByID(_ context.Context, userID string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	user, ok := m.users[strings.TrimSpace(userID)]
	if !ok {
		return nil, ErrUserNotFound
	}
	clone := *user
	return &clone, nil
}

// LinkSSOSubject implements ProvisioningStore: sets the IdP subject for one
// user inside one tenant. Re-linking the same subject is a no-op; linking a
// different subject over an existing one is rejected (identity collision).
func (m *MemoryStore) LinkSSOSubject(_ context.Context, orgID, userID, subject string) error {
	if strings.TrimSpace(subject) == "" {
		return errors.New("sso subject is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[strings.TrimSpace(userID)]
	if !ok || user.Organization != orgID {
		return ErrUserNotFound
	}
	if user.SSOSubject != "" && user.SSOSubject != subject {
		return errors.New("user already linked to another sso subject")
	}
	user.SSOSubject = subject
	return nil
}

// SetUserActive implements ProvisioningStore: toggles the SCIM lifecycle flag
// for one user inside one tenant (idempotent).
func (m *MemoryStore) SetUserActive(_ context.Context, orgID, userID string, active bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[strings.TrimSpace(userID)]
	if !ok || user.Organization != orgID {
		return ErrUserNotFound
	}
	user.Active = active
	return nil
}
