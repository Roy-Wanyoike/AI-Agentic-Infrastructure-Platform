package organizations

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	// Tenant root table.
	sqlInsertOrganization = `INSERT INTO organizations (id, name, status, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)`
	// Tenant guard: lookup by tenant primary key.
	sqlSelectOrganizationByID = `SELECT id, name, status, created_at, updated_at FROM organizations WHERE id = $1`
	// Registration uniqueness check (name lookup happens before a tenant exists).
	sqlSelectOrganizationByName = `SELECT id, name, status, created_at, updated_at FROM organizations WHERE LOWER(name) = LOWER($1) ORDER BY id LIMIT 1`
	// Tenant guard: membership rows are always written with their organization_id.
	sqlInsertMembership = `INSERT INTO organization_memberships (organization_id, user_id, role, created_at) VALUES ($1, $2, $3, $4)`
	// Tenant guard: memberships are listed for exactly one organization_id.
	sqlSelectMemberships = `SELECT user_id, organization_id, role FROM organization_memberships WHERE organization_id = $1 ORDER BY created_at ASC`
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
		return errors.New("organizations: database is nil")
	}
	return nil
}

func (s *pgStore) CreateOrganization(ctx context.Context, org *Organization) error {
	if err := s.guard(); err != nil {
		return err
	}
	if org.CreatedAt.IsZero() || org.UpdatedAt.IsZero() {
		now := time.Now().UTC()
		if org.CreatedAt.IsZero() {
			org.CreatedAt = now
		}
		if org.UpdatedAt.IsZero() {
			org.UpdatedAt = now
		}
	}
	status := org.Status
	if status == "" {
		status = "ACTIVE"
	}
	_, err := s.db.ExecContext(ctx, sqlInsertOrganization, org.ID, org.Name, status, org.CreatedAt, org.UpdatedAt)
	return err
}

func (s *pgStore) GetOrganization(ctx context.Context, id string) (*Organization, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	org, err := scanOrganization(s.db.QueryRowContext(ctx, sqlSelectOrganizationByID, id))
	if err != nil {
		return nil, err
	}
	return org, nil
}

func (s *pgStore) GetOrganizationByName(ctx context.Context, name string) (*Organization, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	org, err := scanOrganization(s.db.QueryRowContext(ctx, sqlSelectOrganizationByName, name))
	if err != nil {
		return nil, err
	}
	return org, nil
}

func (s *pgStore) CreateMembership(ctx context.Context, membership *Membership) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertMembership, membership.OrganizationID, membership.UserID, membership.Role, time.Now().UTC())
	return err
}

func (s *pgStore) ListMemberships(ctx context.Context, orgID string) ([]Membership, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectMemberships, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Membership, 0)
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.UserID, &m.OrganizationID, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanOrganization(scanner interface{ Scan(dest ...any) error }) (*Organization, error) {
	var org Organization
	if err := scanner.Scan(&org.ID, &org.Name, &org.Status, &org.CreatedAt, &org.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrgNotFound
		}
		return nil, err
	}
	return &org, nil
}
