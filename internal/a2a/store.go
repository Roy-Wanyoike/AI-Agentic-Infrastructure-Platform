package a2a

// store.go — the Postgres-backed CardStore (lib/pq driver), following the
// internal/mcp/store.go + internal/approvals/store.go conventions exactly.
// Every statement is tenant-guarded:
//
//   - tenant-facing paths (Create/Update/Delete/Get/List) filter
//     organization_id, so a rogue caller cannot reach another tenant's
//     Agent Card by primary key;
//   - capabilities/skills round-trip JSONB through encoding/json (same
//     shapes the canonical /card discovery endpoint renders);
//   - no secret material exists on the row (auth_scheme is a SCHEME NAME).
//
// Rows are hard-deleted. RowsAffected == 0 on mutation -> ErrNotFound;
// sql.ErrNoRows on read -> ErrNotFound.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	// Tenant guard: cards are inserted with their organization_id scope.
	sqlInsertCard = `INSERT INTO a2a_agent_cards
		(id, organization_id, agent_id, name, description, version,
		 provider_organization, provider_url, endpoint_url,
		 capabilities, skills, auth_scheme, preferred_transport,
		 created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12, $13, $14, $15, $16)`

	// Tenant guard: updates require a matching organization_id.
	sqlUpdateCard = `UPDATE a2a_agent_cards SET
		agent_id = $3,
		name = $4,
		description = $5,
		version = $6,
		provider_organization = $7,
		provider_url = $8,
		endpoint_url = $9,
		capabilities = $10::jsonb,
		skills = $11::jsonb,
		auth_scheme = $12,
		preferred_transport = $13,
		updated_at = $14
		WHERE id = $1 AND organization_id = $2`

	// Tenant guard: deletes require a matching organization_id.
	sqlDeleteCard = `DELETE FROM a2a_agent_cards WHERE id = $1 AND organization_id = $2`

	// Tenant guard: single-card reads are scoped to one organization_id.
	sqlSelectCardScoped = `SELECT id, organization_id, agent_id, name,
		COALESCE(description, ''), version,
		COALESCE(provider_organization, ''), COALESCE(provider_url, ''),
		COALESCE(endpoint_url, ''),
		COALESCE(capabilities::text, '{}'), COALESCE(skills::text, '[]'),
		COALESCE(auth_scheme, ''), COALESCE(preferred_transport, ''),
		COALESCE(created_by, ''), created_at, updated_at
		FROM a2a_agent_cards WHERE id = $1 AND organization_id = $2`

	// Tenant guard: listings filter on organization_id.
	sqlSelectCardsByOrg = `SELECT id, organization_id, agent_id, name,
		COALESCE(description, ''), version,
		COALESCE(provider_organization, ''), COALESCE(provider_url, ''),
		COALESCE(endpoint_url, ''),
		COALESCE(capabilities::text, '{}'), COALESCE(skills::text, '[]'),
		COALESCE(auth_scheme, ''), COALESCE(preferred_transport, ''),
		COALESCE(created_by, ''), created_at, updated_at
		FROM a2a_agent_cards WHERE organization_id = $1
		ORDER BY created_at ASC`
)

// pgStore is the Postgres-backed CardStore implementation (migration 028).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a CardStore backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) CardStore {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("a2a: database is nil")
	}
	return nil
}

func (s *pgStore) CreateCard(ctx context.Context, orgID string, c *AgentCard) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertCard,
		c.ID, orgID, c.AgentID, c.Name, c.Description, c.Version,
		c.Provider.Organization, c.Provider.URL, c.EndpointURL,
		capabilitiesJSON(c.Capabilities), skillsCacheJSON(c.Skills),
		c.AuthScheme, c.PreferredTransport, c.CreatedBy, c.CreatedAt, c.UpdatedAt)
	return err
}

func (s *pgStore) GetCard(ctx context.Context, orgID, id string) (*AgentCard, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanCard(s.db.QueryRowContext(ctx, sqlSelectCardScoped, id, orgID))
}

func (s *pgStore) ListCards(ctx context.Context, orgID string) ([]*AgentCard, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectCardsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*AgentCard, 0)
	for rows.Next() {
		card, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, card)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateCard(ctx context.Context, orgID string, c *AgentCard) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateCard,
		c.ID, orgID, c.AgentID, c.Name, c.Description, c.Version,
		c.Provider.Organization, c.Provider.URL, c.EndpointURL,
		capabilitiesJSON(c.Capabilities), skillsCacheJSON(c.Skills),
		c.AuthScheme, c.PreferredTransport, c.UpdatedAt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (wrong org or unknown card)
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) DeleteCard(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteCard, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// capabilitiesJSON serializes the capabilities block for the JSONB column.
func capabilitiesJSON(c Capabilities) string {
	b, err := json.Marshal(c)
	if err != nil {
		// Unreachable for plain data shapes; degrade to an empty block
		// rather than failing the write.
		return "{}"
	}
	return string(b)
}

// skillsCacheJSON serializes the skills list for the JSONB column. A nil
// list is stored as the honest empty list.
func skillsCacheJSON(skills []Skill) string {
	if skills == nil {
		skills = []Skill{}
	}
	b, err := json.Marshal(skills)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// scanCard hydrates one card row (sql.Row or sql.Rows).
func scanCard(scanner interface{ Scan(dest ...any) error }) (*AgentCard, error) {
	var (
		card      AgentCard
		capsRaw   string
		skillsRaw string
		createdAt time.Time
		updatedAt time.Time
	)
	if err := scanner.Scan(&card.ID, &card.OrganizationID, &card.AgentID, &card.Name,
		&card.Description, &card.Version,
		&card.Provider.Organization, &card.Provider.URL, &card.EndpointURL,
		&capsRaw, &skillsRaw,
		&card.AuthScheme, &card.PreferredTransport, &card.CreatedBy,
		&createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	card.CreatedAt = createdAt
	card.UpdatedAt = updatedAt
	if capsRaw != "" {
		// Best-effort: a malformed block degrades to the zero value instead
		// of failing the read (the row is authoritative on the next write).
		_ = json.Unmarshal([]byte(capsRaw), &card.Capabilities)
	}
	if skillsRaw != "" {
		var skills []Skill
		if err := json.Unmarshal([]byte(skillsRaw), &skills); err == nil {
			card.Skills = normalizeSkills(skills)
		}
	}
	return &card, nil
}
