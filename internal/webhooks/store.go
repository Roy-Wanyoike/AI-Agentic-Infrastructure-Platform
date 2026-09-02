package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	// Tenant guard: webhooks are inserted with their organization_id scope.
	sqlInsertWebhook = `INSERT INTO webhooks (id, organization_id, url, events, secret_hash, status, created_at, updated_at) VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8)`
	// Tenant guard: single-webhook reads are scoped to one organization_id.
	sqlSelectWebhookScoped = `SELECT id, organization_id, url, COALESCE(events::text, '[]'), secret_hash, status, created_at, updated_at FROM webhooks WHERE id = $1 AND organization_id = $2`
	// Tenant guard: listings filter on organization_id (+created_at index).
	sqlSelectWebhooksByOrg = `SELECT id, organization_id, url, COALESCE(events::text, '[]'), secret_hash, status, created_at, updated_at FROM webhooks WHERE organization_id = $1 ORDER BY created_at DESC`
	// Tenant guard: deletes require a matching organization_id.
	sqlDeleteWebhook = `DELETE FROM webhooks WHERE id = $1 AND organization_id = $2`
	// Tenant guard: status updates require a matching organization_id.
	sqlUpdateWebhook = `UPDATE webhooks SET status = $1, updated_at = $2 WHERE id = $3 AND organization_id = $4`
	// Delivery rows upsert by primary key (attempt bookkeeping reuses the row);
	// both writes are organization_id-scoped values set by the service layer.
	sqlUpsertDelivery = `INSERT INTO webhook_deliveries (id, organization_id, webhook_id, event_id, event_type, status, attempts, last_status_code, latency_ms, error, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, attempts = EXCLUDED.attempts, last_status_code = EXCLUDED.last_status_code, latency_ms = EXCLUDED.latency_ms, error = EXCLUDED.error, updated_at = EXCLUDED.updated_at`
	// Tenant guard: delivery listings join webhooks and filter on organization_id.
	sqlSelectDeliveries = `SELECT d.id, d.webhook_id, d.event_id, d.event_type, d.status, d.attempts, d.last_status_code, d.latency_ms, COALESCE(d.error, ''), d.created_at, d.updated_at FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id WHERE d.organization_id = $1 AND d.webhook_id = $2 ORDER BY d.created_at DESC LIMIT $3`
)

// pgStore is the Postgres-backed Store implementation (migration 010).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("webhooks: database is nil")
	}
	return nil
}

func marshalEvents(events []string) string {
	if len(events) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func (s *pgStore) CreateWebhook(ctx context.Context, webhook *Webhook) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := webhook.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
		webhook.CreatedAt = createdAt
	}
	if webhook.UpdatedAt.IsZero() {
		webhook.UpdatedAt = createdAt
	}
	_, err := s.db.ExecContext(ctx, sqlInsertWebhook,
		webhook.ID, webhook.OrganizationID, webhook.URL, marshalEvents(webhook.Events),
		webhook.SecretHash, webhook.Status, webhook.CreatedAt, webhook.UpdatedAt)
	return err
}

func (s *pgStore) GetWebhook(ctx context.Context, orgID, id string) (*Webhook, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	return scanWebhook(s.db.QueryRowContext(ctx, sqlSelectWebhookScoped, id, orgID))
}

func (s *pgStore) ListWebhooks(ctx context.Context, orgID string) ([]*Webhook, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectWebhooksByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Webhook, 0)
	for rows.Next() {
		wh, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wh)
	}
	return out, rows.Err()
}

func (s *pgStore) DeleteWebhook(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteWebhook, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrWebhookNotFound
	}
	return nil
}

func (s *pgStore) UpdateWebhook(ctx context.Context, webhook *Webhook) error {
	if err := s.guard(); err != nil {
		return err
	}
	if webhook.UpdatedAt.IsZero() {
		webhook.UpdatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateWebhook,
		webhook.Status, webhook.UpdatedAt, webhook.ID, webhook.OrganizationID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrWebhookNotFound
	}
	return nil
}

func (s *pgStore) UpsertDelivery(ctx context.Context, delivery *Delivery) error {
	if err := s.guard(); err != nil {
		return err
	}
	if delivery.CreatedAt.IsZero() {
		delivery.CreatedAt = time.Now().UTC()
	}
	if delivery.UpdatedAt.IsZero() {
		delivery.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, sqlUpsertDelivery,
		delivery.ID, delivery.OrganizationID, delivery.WebhookID, delivery.EventID,
		delivery.EventType, delivery.Status, delivery.Attempts, delivery.LastStatusCode,
		delivery.LatencyMS, delivery.Error, delivery.CreatedAt, delivery.UpdatedAt)
	return err
}

func (s *pgStore) ListDeliveries(ctx context.Context, orgID, webhookID string, limit int) ([]*Delivery, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: JOIN webhooks + WHERE d.organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectDeliveries, orgID, webhookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Delivery, 0)
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.EventID, &d.EventType, &d.Status,
			&d.Attempts, &d.LastStatusCode, &d.LatencyMS, &d.Error, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

func scanWebhook(scanner interface{ Scan(dest ...any) error }) (*Webhook, error) {
	var wh Webhook
	var eventsJSON string
	if err := scanner.Scan(&wh.ID, &wh.OrganizationID, &wh.URL, &eventsJSON, &wh.SecretHash, &wh.Status, &wh.CreatedAt, &wh.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrWebhookNotFound
		}
		return nil, err
	}
	wh.Events = []string{}
	if err := json.Unmarshal([]byte(eventsJSON), &wh.Events); err != nil {
		wh.Events = []string{}
	}
	return &wh, nil
}
