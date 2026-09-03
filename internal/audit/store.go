package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrOrgRequired = errors.New("organization id is required")

const (
	// Tenant guard: entries are written with their organization_id scope.
	sqlInsertAuditEntry = `INSERT INTO audit_logs (id, organization_id, actor, action, resource, metadata, created_at) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`
	// Tenant guard: the trail is read for exactly one organization_id.
	sqlSelectAuditEntries = `SELECT id, organization_id, COALESCE(actor, ''), action, COALESCE(resource, ''), COALESCE(metadata::text, ''), created_at FROM audit_logs WHERE organization_id = $1 ORDER BY created_at DESC`
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
		return errors.New("audit: database is nil")
	}
	return nil
}

func (s *pgStore) InsertEntry(ctx context.Context, entry *Entry) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, sqlInsertAuditEntry,
		entry.ID, entry.OrganizationID, entry.Actor, entry.Action, entry.Resource,
		jsonParam(entry.Metadata), createdAt)
	return err
}

func (s *pgStore) ListEntries(ctx context.Context, orgID string) ([]*Entry, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectAuditEntries, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Entry, 0)
	for rows.Next() {
		var entry Entry
		var metadata string
		if err := rows.Scan(&entry.ID, &entry.OrganizationID, &entry.Actor, &entry.Action, &entry.Resource, &metadata, &entry.CreatedAt); err != nil {
			return nil, err
		}
		entry.Metadata = unmarshalJSONMap(metadata)
		out = append(out, &entry)
	}
	return out, rows.Err()
}

// jsonParam marshals a value for a JSONB column; nil stays SQL NULL.
func jsonParam(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

func unmarshalJSONMap(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}
