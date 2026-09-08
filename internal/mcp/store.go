package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/lib/pq"
)

// store.go is the Postgres-backed ServerStore (lib/pq driver, same pattern
// as internal/connectors/store.go). Every statement is tenant-guarded:
//
//   - tenant-facing paths (Create/Update/Delete/Get/List) filter
//     organization_id, so a rogue caller cannot reach another tenant's MCP
//     server by primary key;
//   - the tools cache round-trips JSONB through encoding/json; it carries
//     remote tool NAME/description/inputSchema only — secret VALUES never
//     enter this table (secret_ref is a NAME reference).
//
// Rows are hard-deleted, so the FULL UNIQUE(organization_id, name)
// constraint (migration 025) is correct; a violating insert surfaces as
// SQLSTATE 23505 -> ErrDuplicate. RowsAffected == 0 on mutation ->
// ErrNotFound.

const (
	sqlInsertServer = `INSERT INTO mcp_servers
                (id, organization_id, name, endpoint_url, secret_ref, auth_header,
                 status, last_check_at, protocol_version, tools_cache, created_by, created_at, updated_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13)`

	sqlUpdateServer = `UPDATE mcp_servers SET
                name = $3,
                endpoint_url = $4,
                secret_ref = $5,
                auth_header = $6,
                status = $7,
                last_check_at = $8,
                protocol_version = $9,
                tools_cache = $10::jsonb,
                updated_at = $11
                WHERE id = $1 AND organization_id = $2`

	// Tenant guard: deletes require a matching organization_id.
	sqlDeleteServer = `DELETE FROM mcp_servers WHERE id = $1 AND organization_id = $2`

	// Tenant guard: single-server reads are scoped to one organization_id.
	sqlSelectServerScoped = `SELECT id, organization_id, name, endpoint_url,
                COALESCE(secret_ref, ''), COALESCE(auth_header, 'Authorization'), status,
                last_check_at, COALESCE(protocol_version, ''), COALESCE(tools_cache::text, '[]'),
                created_by, created_at, updated_at
                FROM mcp_servers WHERE id = $1 AND organization_id = $2`

	// Tenant guard: listings filter on organization_id.
	sqlSelectServersByOrg = `SELECT id, organization_id, name, endpoint_url,
                COALESCE(secret_ref, ''), COALESCE(auth_header, 'Authorization'), status,
                last_check_at, COALESCE(protocol_version, ''), COALESCE(tools_cache::text, '[]'),
                created_by, created_at, updated_at
                FROM mcp_servers WHERE organization_id = $1
                ORDER BY name ASC`
)

// pgStore is the Postgres-backed ServerStore implementation (migration 025).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a ServerStore backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) ServerStore {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("mcp: database is nil")
	}
	return nil
}

// toolsCacheJSON serializes the static tools cache for the JSONB column.
// A nil/absent cache is stored as the honest empty list.
func toolsCacheJSON(tools []RemoteTool) string {
	if tools == nil {
		tools = []RemoteTool{}
	}
	b, err := json.Marshal(tools)
	if err != nil {
		// Unreachable for plain data shapes; degrade to an empty cache
		// rather than failing the write (the cache is refreshable by
		// re-test).
		return "[]"
	}
	return string(b)
}

func (s *pgStore) Create(ctx context.Context, srv *Server) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlInsertServer,
		srv.ID, srv.OrganizationID, srv.Name, srv.EndpointURL, srv.SecretRef,
		srv.AuthHeader, srv.Status, srv.LastCheckAt, srv.ProtocolVersion,
		toolsCacheJSON(srv.Tools), srv.CreatedBy, srv.CreatedAt, srv.UpdatedAt)
	if err != nil {
		return mapStoreError(err)
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) Update(ctx context.Context, srv *Server) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateServer,
		srv.ID, srv.OrganizationID, srv.Name, srv.EndpointURL, srv.SecretRef,
		srv.AuthHeader, srv.Status, srv.LastCheckAt, srv.ProtocolVersion,
		toolsCacheJSON(srv.Tools), srv.UpdatedAt)
	if err != nil {
		return mapStoreError(err)
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) Delete(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteServer, id, orgID)
	if err != nil {
		return mapStoreError(err)
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) Get(ctx context.Context, orgID, id string) (*Server, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, sqlSelectServerScoped, id, orgID)
	return scanServer(row.Scan)
}

func (s *pgStore) List(ctx context.Context, orgID string) ([]*Server, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectServersByOrg, orgID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	defer rows.Close()
	out := make([]*Server, 0)
	for rows.Next() {
		srv, err := scanServer(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// scanServer adapts a row scan into a Server (shared by Get and List).
func scanServer(scan func(dest ...any) error) (*Server, error) {
	var srv Server
	var toolsRaw string
	if err := scan(
		&srv.ID, &srv.OrganizationID, &srv.Name, &srv.EndpointURL,
		&srv.SecretRef, &srv.AuthHeader, &srv.Status,
		&srv.LastCheckAt, &srv.ProtocolVersion, &toolsRaw,
		&srv.CreatedBy, &srv.CreatedAt, &srv.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	srv.Tools = []RemoteTool{}
	if toolsRaw != "" && toolsRaw != "[]" {
		if err := json.Unmarshal([]byte(toolsRaw), &srv.Tools); err != nil {
			return nil, errors.New("mcp: corrupt tools_cache on row")
		}
	}
	return &srv, nil
}

// mapStoreError maps driver-level failures onto service errors.
func mapStoreError(err error) error {
	if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
		return ErrDuplicate
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
