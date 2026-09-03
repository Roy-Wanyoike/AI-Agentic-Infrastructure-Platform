package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const (
	// Tenant guard: rows are written with their organization_id scope.
	sqlInsertUsageRecord = `INSERT INTO usage_records (id, organization_id, resource, quantity, metadata, created_at) VALUES ($1, $2, $3, $4, $5::jsonb, $6)`
	// Tenant guard: rows are read for exactly one organization_id.
	sqlSelectUsageRecords = `SELECT id, organization_id, resource, quantity, COALESCE(metadata::text, ''), created_at FROM usage_records WHERE organization_id = $1 ORDER BY created_at DESC`
	// Tenant guard: aggregation is scoped to exactly one organization_id.
	sqlSelectUsageTotals = `SELECT resource, SUM(quantity) FROM usage_records WHERE organization_id = $1 GROUP BY resource`
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
		return errors.New("usage: database is nil")
	}
	return nil
}

func (s *pgStore) InsertRecord(ctx context.Context, record *Record) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertUsageRecord,
		record.ID, record.OrganizationID, record.Resource, record.Quantity,
		jsonParam(record.Metadata), record.CreatedAt)
	return err
}

func (s *pgStore) ListRecords(ctx context.Context, orgID string) ([]*Record, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectUsageRecords, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Record, 0)
	for rows.Next() {
		var record Record
		var metadata string
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.Resource, &record.Quantity, &metadata, &record.CreatedAt); err != nil {
			return nil, err
		}
		record.Metadata = unmarshalJSONMap(metadata)
		out = append(out, &record)
	}
	return out, rows.Err()
}

func (s *pgStore) TotalsByResource(ctx context.Context, orgID string) (map[string]int, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectUsageTotals, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var resource string
		var total int64
		if err := rows.Scan(&resource, &total); err != nil {
			return nil, err
		}
		out[resource] = int(total)
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
