package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrOrgRequired = errors.New("organization id is required")

const (
	// Tenant guard: entries are written with their organization_id scope.
	sqlInsertAuditEntry = `INSERT INTO audit_logs (id, organization_id, actor, action, resource, metadata, created_at) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`
	// Tenant guard: the trail is read for exactly one organization_id.
	sqlSelectAuditEntries = `SELECT id, organization_id, COALESCE(actor, ''), action, COALESCE(resource, ''), COALESCE(metadata::text, ''), created_at FROM audit_logs WHERE organization_id = $1 ORDER BY created_at DESC`
	// Keyset-paginated listing (issue #18): first page. The id tiebreaker
	// keeps the (created_at DESC, id DESC) order deterministic so cursor
	// pagination cannot skip or repeat rows with identical timestamps. One
	// extra row beyond the page size is fetched to detect exhaustion.
	sqlSelectAuditEntriesPaged = `SELECT id, organization_id, COALESCE(actor, ''), action, COALESCE(resource, ''), COALESCE(metadata::text, ''), created_at FROM audit_logs WHERE organization_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`
	// Keyset-paginated listing: continuation page. The cursor carries the
	// (created_at, id) key of the previous page's last row; the row-value
	// comparison (a Postgres feature) is the keyset predicate.
	sqlSelectAuditEntriesPagedCursor = `SELECT id, organization_id, COALESCE(actor, ''), action, COALESCE(resource, ''), COALESCE(metadata::text, ''), created_at FROM audit_logs WHERE organization_id = $1 AND (created_at, id) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT $4`
)

// pgStore is the Postgres-backed Store implementation.
type pgStore struct {
	db *sql.DB
}

// Compile-time check: the Postgres store serves keyset-paginated listings
// natively (issue #18).
var _ PagedStore = (*pgStore)(nil)

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
	return scanAuditRows(rows)
}

// ListEntriesPaged implements PagedStore (issue #18): one keyset page of the
// tenant's trail, newest first, plus the next-page cursor ("" = exhausted).
// The limit is clamped again here so direct callers get the same bounds as
// Service callers, and the fetch takes limit+1 rows so the next cursor is
// only emitted when a follow-up page actually exists.
func (s *pgStore) ListEntriesPaged(ctx context.Context, orgID string, limit int, cursor string) ([]*Entry, string, error) {
	if err := s.guard(); err != nil {
		return nil, "", err
	}
	limit = NormalizeLimit(limit)

	var (
		rows *sql.Rows
		err  error
	)
	if strings.TrimSpace(cursor) == "" {
		// Tenant guard: WHERE organization_id = $1
		rows, err = s.db.QueryContext(ctx, sqlSelectAuditEntriesPaged, orgID, limit+1)
	} else {
		before, beforeID, decodeErr := decodeCursor(cursor)
		if decodeErr != nil {
			return nil, "", decodeErr
		}
		// Tenant guard: WHERE organization_id = $1 AND keyset < ($2, $3)
		rows, err = s.db.QueryContext(ctx, sqlSelectAuditEntriesPagedCursor, orgID, before, beforeID, limit+1)
	}
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out, err := scanAuditRows(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = encodeCursor(out[len(out)-1])
	}
	return out, next, nil
}

// scanAuditRows drains an audit listing result set (shared by the plain and
// keyset-paginated queries).
func scanAuditRows(rows *sql.Rows) ([]*Entry, error) {
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
