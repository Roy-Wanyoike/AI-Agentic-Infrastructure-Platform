package policies

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// SQL for the policies table created by migrations/008_policies.sql. Every
// read/write is guarded by organization_id (tenant isolation rule).
const (
	sqlInsertPolicy = `INSERT INTO policies
		(id, organization_id, name, effect, resource_type, actions, conditions, priority, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	sqlSelectPolicy = `SELECT id, organization_id, name, effect, resource_type, actions, conditions,
		priority, enabled, created_at, updated_at
		FROM policies WHERE id = $1 AND organization_id = $2`

	sqlUpdatePolicy = `UPDATE policies SET name = $3, effect = $4, resource_type = $5, actions = $6,
		conditions = $7, priority = $8, enabled = $9, updated_at = $10
		WHERE id = $1 AND organization_id = $2`

	sqlDeletePolicy = `DELETE FROM policies WHERE id = $1 AND organization_id = $2`

	sqlSelectPoliciesByOrg = `SELECT id, organization_id, name, effect, resource_type, actions, conditions,
		priority, enabled, created_at, updated_at
		FROM policies WHERE organization_id = $1
		ORDER BY priority DESC, created_at ASC, id ASC`
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
		return errors.New("policies: database is nil")
	}
	return nil
}

func (s *pgStore) CreatePolicy(ctx context.Context, policy *Policy) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := policy.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
		policy.CreatedAt = createdAt
	}
	if policy.UpdatedAt.IsZero() {
		policy.UpdatedAt = createdAt
	}
	actions, err := jsonParam(policy.Actions)
	if err != nil {
		return err
	}
	conditions, err := jsonParam(policy.Conditions)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, sqlInsertPolicy,
		policy.ID, policy.OrganizationID, policy.Name, policy.Effect, policy.ResourceType,
		actions, conditions, policy.Priority, policy.Enabled, policy.CreatedAt, policy.UpdatedAt)
	return err
}

func (s *pgStore) GetPolicy(ctx context.Context, orgID, id string) (*Policy, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	policy, err := scanPolicy(s.db.QueryRowContext(ctx, sqlSelectPolicy, id, orgID))
	if err != nil {
		return nil, err
	}
	return policy, nil
}

func (s *pgStore) UpdatePolicy(ctx context.Context, policy *Policy) error {
	if err := s.guard(); err != nil {
		return err
	}
	actions, err := jsonParam(policy.Actions)
	if err != nil {
		return err
	}
	conditions, err := jsonParam(policy.Conditions)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdatePolicy,
		policy.ID, policy.OrganizationID, policy.Name, policy.Effect, policy.ResourceType,
		actions, conditions, policy.Priority, policy.Enabled, policy.UpdatedAt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (wrong org or unknown policy)
		return ErrPolicyNotFound
	}
	return nil
}

func (s *pgStore) DeletePolicy(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeletePolicy, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrPolicyNotFound
	}
	return nil
}

func (s *pgStore) ListPolicies(ctx context.Context, orgID string) ([]*Policy, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectPoliciesByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Policy, 0)
	for rows.Next() {
		policy, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, policy)
	}
	return out, rows.Err()
}

func scanPolicy(scanner interface{ Scan(dest ...any) error }) (*Policy, error) {
	var policy Policy
	var actions, conditions []byte
	if err := scanner.Scan(
		&policy.ID, &policy.OrganizationID, &policy.Name, &policy.Effect, &policy.ResourceType,
		&actions, &conditions, &policy.Priority, &policy.Enabled, &policy.CreatedAt, &policy.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPolicyNotFound
		}
		return nil, err
	}
	policy.Actions = []string{}
	if len(actions) > 0 {
		if err := json.Unmarshal(actions, &policy.Actions); err != nil {
			return nil, err
		}
	}
	if len(conditions) > 0 {
		if err := json.Unmarshal(conditions, &policy.Conditions); err != nil {
			return nil, err
		}
	}
	return &policy, nil
}

// jsonParam marshals a value for a JSONB column; nil stays SQL NULL.
func jsonParam(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}
