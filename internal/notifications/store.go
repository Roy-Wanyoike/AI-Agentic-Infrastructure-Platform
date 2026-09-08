package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	// Tenant guard: subscriptions are inserted with their organization_id scope.
	sqlInsertSubscription = `INSERT INTO notification_subscriptions (id, organization_id, webhook_id, event_types, created_at) VALUES ($1, $2, $3, $4::jsonb, $5)`
	// Tenant guard: single-subscription reads are scoped to one organization_id.
	sqlSelectSubscriptionScoped = `SELECT id, organization_id, webhook_id, COALESCE(event_types::text, '[]'), created_at FROM notification_subscriptions WHERE id = $1 AND organization_id = $2`
	// Tenant guard: listings filter on organization_id (+created_at index).
	sqlSelectSubscriptionsByOrg = `SELECT id, organization_id, webhook_id, COALESCE(event_types::text, '[]'), created_at FROM notification_subscriptions WHERE organization_id = $1 ORDER BY created_at DESC`
	// Tenant guard: deletes require a matching organization_id.
	sqlDeleteSubscription = `DELETE FROM notification_subscriptions WHERE id = $1 AND organization_id = $2`
)

// pgStore is the Postgres-backed Store implementation (migration 024).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("notifications: database is nil")
	}
	return nil
}

func marshalEventTypes(types []string) string {
	if len(types) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(types)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func (s *pgStore) CreateSubscription(ctx context.Context, sub *Subscription) error {
	if err := s.guard(); err != nil {
		return err
	}
	createdAt := sub.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
		sub.CreatedAt = createdAt
	}
	_, err := s.db.ExecContext(ctx, sqlInsertSubscription,
		sub.ID, sub.OrganizationID, sub.WebhookID, marshalEventTypes(sub.EventTypes), sub.CreatedAt)
	return err
}

func (s *pgStore) GetSubscription(ctx context.Context, orgID, id string) (*Subscription, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	return scanSubscription(s.db.QueryRowContext(ctx, sqlSelectSubscriptionScoped, id, orgID))
}

func (s *pgStore) ListSubscriptions(ctx context.Context, orgID string) ([]*Subscription, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE organization_id = $1
	rows, err := s.db.QueryContext(ctx, sqlSelectSubscriptionsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Subscription, 0)
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *pgStore) DeleteSubscription(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteSubscription, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrSubscriptionNotFound
	}
	return nil
}

func scanSubscription(scanner interface{ Scan(dest ...any) error }) (*Subscription, error) {
	var sub Subscription
	var typesJSON string
	if err := scanner.Scan(&sub.ID, &sub.OrganizationID, &sub.WebhookID, &typesJSON, &sub.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, err
	}
	sub.EventTypes = []string{}
	if err := json.Unmarshal([]byte(typesJSON), &sub.EventTypes); err != nil {
		sub.EventTypes = []string{}
	}
	return &sub, nil
}
