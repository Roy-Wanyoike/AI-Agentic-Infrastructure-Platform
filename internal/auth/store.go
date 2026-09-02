package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrUserNotFound = errors.New("user not found")

const (
	// Tenant root row for a new registration.
	sqlInsertAuthOrganization = `INSERT INTO organizations (id, name, status, created_at, updated_at) VALUES ($1, $2, 'ACTIVE', NOW(), NOW())`
	// Tenant guard: the user row carries its organization_id scope.
	sqlInsertAuthUser = `INSERT INTO users (id, organization_id, email, password_hash, role, created_at) VALUES ($1, $2, $3, $4, $5, $6)`
	// Credential-based tenant resolution: users.email is globally UNIQUE
	// (migration 001), so the returned row identifies the single tenant.
	sqlSelectAuthUserByEmail = `SELECT id, organization_id, email, password_hash, role, created_at FROM users WHERE email = $1`
)

// pgStore is the Postgres-backed Store implementation for auth identities.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("auth: database is nil")
	}
	return nil
}

func (s *pgStore) CreateOrganization(ctx context.Context, org *Organization) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertAuthOrganization, org.ID, org.Name)
	return err
}

func (s *pgStore) CreateUser(ctx context.Context, user *User) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := user.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	role := strings.TrimSpace(user.Role)
	if role == "" {
		role = defaultRole
	}
	_, err := s.db.ExecContext(ctx, sqlInsertAuthUser, user.ID, user.Organization, user.Email, user.PasswordHash, role, createdAt)
	return err
}

func (s *pgStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var user User
	err := s.db.QueryRowContext(ctx, sqlSelectAuthUserByEmail, strings.TrimSpace(email)).Scan(
		&user.ID, &user.Organization, &user.Email, &user.PasswordHash, &user.Role, &user.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}
