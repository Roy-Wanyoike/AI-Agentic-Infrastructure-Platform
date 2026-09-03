package memory

import (
        "context"
        "database/sql"
        "encoding/json"
        "errors"
        "strings"
        "time"
)

// Store abstracts durable snippet storage. Implementations MUST scope every
// query by organization_id; short-term expiry is enforced inside the SQL
// (expires_at IS NULL OR expires_at > NOW()) so expired rows never surface.
type SnippetStore interface {
        // ReplaceAgentSnippets atomically replaces the snippet set of one
        // (organization, agent) pair; agentID may be empty for org-level memory.
        ReplaceAgentSnippets(ctx context.Context, orgID, agentID string, snippets []*Snippet) error
        // ListSnippets returns visible snippets of one tenant; agentID filters to
        // exactly that agent's rows, empty returns all rows of the organization.
        ListSnippets(ctx context.Context, orgID, agentID string) ([]*Snippet, error)
        // ListSnippetsForAgent returns one agent's rows plus the org-level shared
        // memory (agent_id IS NULL). agentID must be non-empty.
        ListSnippetsForAgent(ctx context.Context, orgID, agentID string) ([]*Snippet, error)
}

// Tenant guard: the delete/insert pair runs in one transaction keyed by the
// (organization_id, agent_id) scope; agent_id "" maps to SQL NULL (shared
// organization-level memory).
const (
        sqlDeleteAgentSnippets = `DELETE FROM memory_snippets WHERE organization_id = $1 AND (($2 = '' AND agent_id IS NULL) OR ($2 <> '' AND agent_id = $2))`
        sqlInsertSnippet       = `INSERT INTO memory_snippets (id, organization_id, agent_id, scope, content, importance, expires_at, embedding, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)`
        // Tenant guard + expiry guard: expired rows are filtered in SQL.
        sqlSelectSnippetsAll     = `SELECT id, organization_id, COALESCE(agent_id, ''), scope, content, importance, expires_at, COALESCE(embedding::text, ''), created_at, updated_at FROM memory_snippets WHERE organization_id = $1 AND (expires_at IS NULL OR expires_at > NOW()) ORDER BY created_at DESC, id ASC`
        sqlSelectSnippetsByAgent = `SELECT id, organization_id, COALESCE(agent_id, ''), scope, content, importance, expires_at, COALESCE(embedding::text, ''), created_at, updated_at FROM memory_snippets WHERE organization_id = $1 AND agent_id = $2 AND (expires_at IS NULL OR expires_at > NOW()) ORDER BY created_at DESC, id ASC`
        // Retrieval candidate set: the agent's own rows + shared org-level memory.
        sqlSelectSnippetsForAgent = `SELECT id, organization_id, COALESCE(agent_id, ''), scope, content, importance, expires_at, COALESCE(embedding::text, ''), created_at, updated_at FROM memory_snippets WHERE organization_id = $1 AND (agent_id = $2 OR agent_id IS NULL) AND (expires_at IS NULL OR expires_at > NOW()) ORDER BY created_at DESC, id ASC`
)

// pgStore is the Postgres-backed Store implementation (migration 014).
type pgStore struct {
        db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) SnippetStore {
        return &pgStore{db: db}
}

func (s *pgStore) guard() error {
        if s == nil || s.db == nil {
                return errors.New("memory: database is nil")
        }
        return nil
}

// ReplaceAgentSnippets deletes the scope's rows and inserts the new set in a
// single transaction (all-or-nothing: a failed PUT never leaves the agent
// with a partial memory).
func (s *pgStore) ReplaceAgentSnippets(ctx context.Context, orgID, agentID string, snippets []*Snippet) error {
        if err := s.guard(); err != nil {
                return err
        }
        tx, err := s.db.BeginTx(ctx, nil)
        if err != nil {
                return err
        }
        defer func() { _ = tx.Rollback() }()

        if _, err := tx.ExecContext(ctx, sqlDeleteAgentSnippets, orgID, agentID); err != nil {
                return err
        }
        for _, sn := range snippets {
                if _, err := tx.ExecContext(ctx, sqlInsertSnippet,
                        sn.ID, orgID, nullableString(sn.AgentID), sn.Scope, sn.Content,
                        sn.Importance, nullableTime(sn.ExpiresAt), embeddingParam(sn.Embedding),
                        sn.CreatedAt, sn.UpdatedAt); err != nil {
                        return err
                }
        }
        return tx.Commit()
}

func (s *pgStore) ListSnippets(ctx context.Context, orgID, agentID string) ([]*Snippet, error) {
        if err := s.guard(); err != nil {
                return nil, err
        }
        if strings.TrimSpace(agentID) == "" {
                // Tenant guard: WHERE organization_id = $1
                return scanSnippets(s.db.QueryContext(ctx, sqlSelectSnippetsAll, orgID))
        }
        // Tenant guard: WHERE organization_id = $1 AND agent_id = $2
        return scanSnippets(s.db.QueryContext(ctx, sqlSelectSnippetsByAgent, orgID, agentID))
}

func (s *pgStore) ListSnippetsForAgent(ctx context.Context, orgID, agentID string) ([]*Snippet, error) {
        if err := s.guard(); err != nil {
                return nil, err
        }
        if strings.TrimSpace(agentID) == "" {
                return nil, errors.New("memory: agent id is required")
        }
        // Tenant guard: WHERE organization_id = $1 AND (agent_id = $2 OR agent_id IS NULL)
        return scanSnippets(s.db.QueryContext(ctx, sqlSelectSnippetsForAgent, orgID, agentID))
}

func scanSnippets(rows *sql.Rows, err error) ([]*Snippet, error) {
        if err != nil {
                return nil, err
        }
        defer rows.Close()
        out := make([]*Snippet, 0)
        for rows.Next() {
                sn := &Snippet{}
                var agentID string
                var expiresAt sql.NullTime
                var embeddingRaw string
                if err := rows.Scan(&sn.ID, &sn.OrganizationID, &agentID, &sn.Scope, &sn.Content,
                        &sn.Importance, &expiresAt, &embeddingRaw, &sn.CreatedAt, &sn.UpdatedAt); err != nil {
                        return nil, err
                }
                sn.AgentID = agentID
                if expiresAt.Valid {
                        t := expiresAt.Time
                        sn.ExpiresAt = &t
                }
                sn.Embedding = embeddingFromParam(embeddingRaw)
                out = append(out, sn)
        }
        return out, rows.Err()
}

// nullableString maps the in-memory "" (org-level) to SQL NULL.
func nullableString(v string) any {
        if strings.TrimSpace(v) == "" {
                return nil
        }
        return v
}

func nullableTime(t *time.Time) any {
        if t == nil || t.IsZero() {
                return nil
        }
        return *t
}

// embeddingParam marshals the vector for the JSONB column; nil stays NULL.
func embeddingParam(vec []float64) any {
        if len(vec) == 0 {
                return nil
        }
        b, err := json.Marshal(vec)
        if err != nil {
                return nil
        }
        return string(b)
}

func embeddingFromParam(raw string) []float64 {
        if strings.TrimSpace(raw) == "" {
                return nil
        }
        var vec []float64
        if err := json.Unmarshal([]byte(raw), &vec); err != nil {
                return nil
        }
        return vec
}
