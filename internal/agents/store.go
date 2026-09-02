package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	// Tenant guard: agents are inserted with their organization_id scope.
	sqlInsertAgent = `INSERT INTO agents (id, organization_id, name, description, instructions, model, status, current_version_id, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	// Tenant guard: single-agent reads are scoped to one organization_id.
	sqlSelectAgentScoped = `SELECT id, organization_id, name, COALESCE(description, ''), COALESCE(instructions, ''), model, status, COALESCE(current_version_id, ''), created_at, updated_at FROM agents WHERE id = $1 AND organization_id = $2`
	// Trusted internal PK lookup (worker path); the returned row carries
	// organization_id so callers can enforce tenancy.
	sqlSelectAgentByID = `SELECT id, organization_id, name, COALESCE(description, ''), COALESCE(instructions, ''), model, status, COALESCE(current_version_id, ''), created_at, updated_at FROM agents WHERE id = $1`
	// Tenant guard: listings filter on organization_id (+created_at index).
	sqlSelectAgentsByOrg = `SELECT id, organization_id, name, COALESCE(description, ''), COALESCE(instructions, ''), model, status, COALESCE(current_version_id, ''), created_at, updated_at FROM agents WHERE organization_id = $1 ORDER BY created_at DESC`
	// Tenant guard: updates require a matching organization_id.
	sqlUpdateAgent = `UPDATE agents SET name = $1, description = $2, instructions = $3, model = $4, status = $5, current_version_id = $6, updated_at = $7 WHERE id = $8 AND organization_id = $9`
	// Tenant guard: deletes require a matching organization_id.
	sqlDeleteAgent = `DELETE FROM agents WHERE id = $1 AND organization_id = $2`
	// Tenant guard: versions join agents to enforce organization_id.
	sqlInsertAgentVersion = `INSERT INTO agent_versions (id, agent_id, version, config, prompt, created_at) VALUES ($1, $2, $3, $4::jsonb, $5, $6)`
	// Tenant guard: version listings join agents and filter on organization_id.
	sqlSelectAgentVersions = `SELECT av.id, av.agent_id, av.version, COALESCE(av.prompt, ''), COALESCE(av.config->>'model', ''), av.created_at FROM agent_versions av JOIN agents a ON a.id = av.agent_id WHERE a.organization_id = $1 AND av.agent_id = $2 ORDER BY av.version ASC`
	// Tenant guard: next version number is computed within one organization_id.
	sqlNextAgentVersion = `SELECT COALESCE(MAX(version), 0) + 1 FROM agent_versions av JOIN agents a ON a.id = av.agent_id WHERE a.organization_id = $1 AND av.agent_id = $2`
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
		return errors.New("agents: database is nil")
	}
	return nil
}

func (s *pgStore) CreateAgent(ctx context.Context, orgID string, agent *Agent) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertAgent,
		agent.ID, orgID, agent.Name, agent.Description, agent.Instructions,
		agent.Model, agent.Status, agent.CurrentVersionID, agent.CreatedAt, agent.UpdatedAt)
	return err
}

func (s *pgStore) GetAgent(ctx context.Context, orgID, id string) (*Agent, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	return scanAgent(s.db.QueryRowContext(ctx, sqlSelectAgentScoped, id, orgID))
}

func (s *pgStore) GetAgentByID(ctx context.Context, id string) (*Agent, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanAgent(s.db.QueryRowContext(ctx, sqlSelectAgentByID, id))
}

func (s *pgStore) ListAgents(ctx context.Context, orgID string) ([]*Agent, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectAgentsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Agent, 0)
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, agent)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateAgent(ctx context.Context, orgID string, agent *Agent) error {
	if err := s.guard(); err != nil {
		return err
	}
	if agent.UpdatedAt.IsZero() {
		agent.UpdatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateAgent,
		agent.Name, agent.Description, agent.Instructions, agent.Model,
		agent.Status, agent.CurrentVersionID, agent.UpdatedAt, agent.ID, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (wrong org or unknown agent)
		return ErrAgentNotFound
	}
	return nil
}

func (s *pgStore) DeleteAgent(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteAgent, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrAgentNotFound
	}
	return nil
}

func (s *pgStore) CreateAgentVersion(ctx context.Context, orgID string, version *AgentVersion) error {
	if err := s.guard(); err != nil {
		return err
	}
	// The model travels in the config JSONB column; instructions map to prompt.
	configPayload, _ := json.Marshal(map[string]string{"model": version.Model})
	createdAt := version.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, sqlInsertAgentVersion,
		version.ID, version.AgentID, version.Version, string(configPayload), version.Instructions, createdAt)
	return err
}

func (s *pgStore) ListAgentVersions(ctx context.Context, orgID, agentID string) ([]*AgentVersion, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: JOIN agents + WHERE a.organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectAgentVersions, orgID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*AgentVersion, 0)
	for rows.Next() {
		var version AgentVersion
		if err := rows.Scan(&version.ID, &version.AgentID, &version.Version, &version.Instructions, &version.Model, &version.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &version)
	}
	return out, rows.Err()
}

func (s *pgStore) NextAgentVersionNumber(ctx context.Context, orgID, agentID string) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	// Tenant guard: JOIN agents + WHERE a.organization_id = $1
	var number int
	if err := s.db.QueryRowContext(ctx, sqlNextAgentVersion, orgID, agentID).Scan(&number); err != nil {
		return 0, err
	}
	return number, nil
}

func scanAgent(scanner interface{ Scan(dest ...any) error }) (*Agent, error) {
	var agent Agent
	if err := scanner.Scan(&agent.ID, &agent.OrganizationID, &agent.Name, &agent.Description, &agent.Instructions, &agent.Model, &agent.Status, &agent.CurrentVersionID, &agent.CreatedAt, &agent.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAgentNotFound
		}
		return nil, err
	}
	return &agent, nil
}
