package deployments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Deployment SQL. Tenant guard: every statement filters on organization_id.
// canary_promotion (issue #51, migration 021) is the eval-gated promotion
// state JSONB — NULL for legacy rows / no policy configured.
const (
	sqlInsertDeployment = `INSERT INTO deployments
                (id, organization_id, agent_id, version, environment, status, health, created_by, created_at, updated_at, superseded_at, canary_version, canary_weight, canary_promotion)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

	sqlSelectDeploymentColumns = `id, organization_id, agent_id, version, environment, status,
                health, created_by, created_at, updated_at, superseded_at, canary_version, canary_weight, canary_promotion`

	// Tenant guard: single-deployment reads are scoped to one organization_id.
	sqlSelectDeployment = `SELECT ` + sqlSelectDeploymentColumns + `
                FROM deployments WHERE id = $1 AND organization_id = $2`

	// Tenant guard: listings filter on organization_id (agent_id optional).
	sqlSelectDeploymentsByOrg = `SELECT ` + sqlSelectDeploymentColumns + `
                FROM deployments
                WHERE organization_id = $1 AND ($2 = '' OR agent_id = $2)
                ORDER BY created_at DESC, id`

	// Tenant guard: lifecycle updates require a matching organization_id.
	// canary_version/canary_weight are mutable too (configure/promote/abort
	// transitions) and always written together with the lifecycle fields.
	// canary_promotion rides along (policy attach + decision record).
	sqlUpdateDeployment = `UPDATE deployments
                SET status = $1, health = $2, updated_at = $3, superseded_at = $4, canary_version = $5, canary_weight = $6, canary_promotion = $7
                WHERE id = $8 AND organization_id = $9`

	// The environment's current deployment: the single healthy row.
	sqlSelectHealthyDeployment = `SELECT ` + sqlSelectDeploymentColumns + `
                FROM deployments
                WHERE organization_id = $1 AND agent_id = $2 AND environment = $3 AND status = 'healthy'
                ORDER BY updated_at DESC LIMIT 1`

	// The previous healthy deployment: the most recently superseded row
	// (healthy once, later demoted), excluding a row (the current deployment).
	sqlSelectPreviousHealthyDeployment = `SELECT ` + sqlSelectDeploymentColumns + `
                FROM deployments
                WHERE organization_id = $1 AND agent_id = $2 AND environment = $3
                        AND superseded_at IS NOT NULL AND id <> $4
                ORDER BY superseded_at DESC LIMIT 1`
)

// pgStore is the Postgres-backed Store implementation. The one-healthy-per-
// agent+environment invariant is enforced by the partial unique index
// uq_deployments_one_healthy plus the service-level demote-before-promote
// ordering.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("deployments: database is nil")
	}
	return nil
}

func (s *pgStore) CreateDeployment(ctx context.Context, orgID string, deployment *Deployment) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := deployment.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
		deployment.CreatedAt = createdAt
	}
	updatedAt := deployment.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
		deployment.UpdatedAt = updatedAt
	}
	_, err := s.db.ExecContext(ctx, sqlInsertDeployment,
		deployment.ID, orgID, deployment.AgentID, deployment.Version,
		deployment.Environment, deployment.Status, healthJSON(deployment.Health),
		deployment.CreatedBy, createdAt, updatedAt, nullTime(deployment.SupersededAt),
		deployment.CanaryVersion, deployment.CanaryWeight, promotionJSON(deployment.Promotion))
	return err
}

func (s *pgStore) GetDeployment(ctx context.Context, orgID, id string) (*Deployment, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	return scanDeployment(s.db.QueryRowContext(ctx, sqlSelectDeployment, id, orgID))
}

func (s *pgStore) ListDeployments(ctx context.Context, orgID, agentID string) ([]*Deployment, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1 (+ optional agent filter)
	rows, err := s.db.QueryContext(ctx, sqlSelectDeploymentsByOrg, orgID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Deployment, 0)
	for rows.Next() {
		deployment, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, deployment)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateDeployment(ctx context.Context, orgID string, deployment *Deployment) error {
	if err := s.guard(); err != nil {
		return err
	}
	updatedAt := deployment.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
		deployment.UpdatedAt = updatedAt
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateDeployment,
		deployment.Status, healthJSON(deployment.Health), updatedAt,
		nullTime(deployment.SupersededAt), deployment.CanaryVersion, deployment.CanaryWeight,
		promotionJSON(deployment.Promotion), deployment.ID, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (wrong org or unknown deployment)
		return ErrDeploymentNotFound
	}
	return nil
}

func (s *pgStore) GetHealthyDeployment(ctx context.Context, orgID, agentID, environment string) (*Deployment, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	deployment, err := scanDeployment(s.db.QueryRowContext(ctx, sqlSelectHealthyDeployment, orgID, agentID, environment))
	if err != nil {
		if errors.Is(err, ErrDeploymentNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return deployment, nil
}

func (s *pgStore) GetPreviousHealthyDeployment(ctx context.Context, orgID, agentID, environment, excludeID string) (*Deployment, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	deployment, err := scanDeployment(s.db.QueryRowContext(ctx, sqlSelectPreviousHealthyDeployment, orgID, agentID, environment, excludeID))
	if err != nil {
		if errors.Is(err, ErrDeploymentNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return deployment, nil
}

// scanDeployment reads one row; sql.ErrNoRows maps to ErrDeploymentNotFound.
func scanDeployment(scanner interface{ Scan(dest ...any) error }) (*Deployment, error) {
	var (
		deployment     Deployment
		healthBytes    []byte
		healthValid    bool
		superseded     sql.NullTime
		promotionBytes []byte
	)
	if err := scanner.Scan(&deployment.ID, &deployment.OrganizationID, &deployment.AgentID,
		&deployment.Version, &deployment.Environment, &deployment.Status,
		&healthBytes, &deployment.CreatedBy, &deployment.CreatedAt, &deployment.UpdatedAt, &superseded,
		&deployment.CanaryVersion, &deployment.CanaryWeight, &promotionBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDeploymentNotFound
		}
		return nil, err
	}
	if promotionBytes != nil {
		promotion := &CanaryPromotion{}
		if err := json.Unmarshal(promotionBytes, promotion); err == nil {
			deployment.Promotion = promotion
		}
	}
	healthValid = healthBytes != nil
	if healthValid {
		health := &Health{}
		if err := json.Unmarshal(healthBytes, health); err == nil {
			deployment.Health = health
		}
	}
	if superseded.Valid {
		t := superseded.Time.UTC()
		deployment.SupersededAt = &t
	}
	return &deployment, nil
}

// healthJSON marshals the health payload (nil -> SQL NULL).
func healthJSON(health *Health) any {
	if health == nil {
		return nil
	}
	b, err := json.Marshal(health)
	if err != nil {
		return nil
	}
	return string(b)
}

// promotionJSON marshals the eval-gated promotion state (nil -> SQL NULL;
// migration 021 column canary_promotion).
func promotionJSON(promotion *CanaryPromotion) any {
	if promotion == nil {
		return nil
	}
	b, err := json.Marshal(promotion)
	if err != nil {
		return nil
	}
	return string(b)
}

// nullTime converts a *time.Time into a database/sql NULL-able value.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
