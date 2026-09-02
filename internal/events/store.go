package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Store persists the append-only audit of published events (migration 010,
// `events` table). Implementations must scope rows by organization_id
// (= Event.TenantID) and never update or delete rows.
type Store interface {
	AppendEvent(ctx context.Context, event *Event) error
}

const sqlInsertEvent = `INSERT INTO events (id, organization_id, type, project_id, resource_type, resource_id, execution_id, trace_id, payload, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)`

// pgStore is the Postgres-backed append-only audit Store.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store writing the append-only event audit rows.
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) AppendEvent(ctx context.Context, event *Event) error {
	if s == nil || s.db == nil {
		return errors.New("events: database is nil")
	}
	if event == nil {
		return ErrInvalidEvent
	}
	ensureDefaults(event)
	if err := validate(event); err != nil {
		return err
	}
	payload := []byte("{}")
	if event.Payload != nil {
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			return err
		}
		payload = encoded
	}
	createdAt := event.Timestamp
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, sqlInsertEvent,
		event.ID, event.TenantID, event.Type, event.ProjectID,
		event.Resource.Type, event.Resource.ID, event.ExecutionID, event.TraceID,
		string(payload), createdAt)
	return err
}

// AuditPublisher is a Publisher decorator that persists every published event
// to the append-only audit Store before forwarding it to the next publisher.
// A failing audit write fails the Publish call (audit is the source of truth);
// the event is then not forwarded.
type AuditPublisher struct {
	store Store
	next  Publisher
}

// NewAuditPublisher wraps next with an append-only audit write. When store is
// nil it degrades to a pass-through decorator.
func NewAuditPublisher(store Store, next Publisher) *AuditPublisher {
	return &AuditPublisher{store: store, next: next}
}

// Publish persists the event (ensuring identity defaults first so the audit
// row carries the same ID consumers see) and forwards it.
func (a *AuditPublisher) Publish(ctx context.Context, event Event) error {
	ensureDefaults(&event)
	if err := validate(&event); err != nil {
		return err
	}
	if a.store != nil {
		if err := a.store.AppendEvent(ctx, &event); err != nil {
			return err
		}
	}
	if a.next == nil {
		return nil
	}
	return a.next.Publish(ctx, event)
}
