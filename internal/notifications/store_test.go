package notifications

// Postgres store tests (migration 024) — sqlmock, no live database.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/webhooks"
)

func newPgStore(t *testing.T) (Store, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresStore(db), mock, db
}

func TestPgStoreCreateAndGetSubscription(t *testing.T) {
	store, mock, _ := newPgStore(t)
	ctx := context.Background()

	now := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	sub := &Subscription{
		ID:             "nsub-1",
		OrganizationID: "org-1",
		WebhookID:      "wh-1",
		EventTypes:     []string{"run.failed", "approval.requested"},
		CreatedAt:      now,
	}
	mock.ExpectExec("INSERT INTO notification_subscriptions").
		WithArgs("nsub-1", "org-1", "wh-1", `["run.failed","approval.requested"]`, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	eventsJSON, _ := json.Marshal(sub.EventTypes)
	rows := sqlmock.NewRows([]string{"id", "organization_id", "webhook_id", "event_types", "created_at"}).
		AddRow(sub.ID, sub.OrganizationID, sub.WebhookID, string(eventsJSON), now)
	mock.ExpectQuery("SELECT id, organization_id, webhook_id, COALESCE\\(event_types::text, '\\[\\]'\\), created_at FROM notification_subscriptions").
		WithArgs("nsub-1", "org-1").
		WillReturnRows(rows)

	got, err := store.GetSubscription(ctx, "org-1", "nsub-1")
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if got.ID != "nsub-1" || got.WebhookID != "wh-1" || len(got.EventTypes) != 2 || got.EventTypes[0] != "run.failed" {
		t.Errorf("scanned subscription wrong: %+v", got)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("created_at round-trip failed: %v", got.CreatedAt)
	}

	// Tenant guard: unknown org -> no rows -> ErrSubscriptionNotFound.
	mock.ExpectQuery("SELECT id, organization_id").
		WithArgs("nsub-1", "org-other").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetSubscription(ctx, "org-other", "nsub-1"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Errorf("cross-tenant get should be ErrSubscriptionNotFound, got %v", err)
	}
}

func TestPgStoreListSubscriptions(t *testing.T) {
	store, mock, _ := newPgStore(t)
	ctx := context.Background()

	now := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "organization_id", "webhook_id", "event_types", "created_at"}).
		AddRow("nsub-2", "org-1", "wh-2", `["deployment.failed"]`, now).
		AddRow("nsub-1", "org-1", "wh-1", `["run.failed"]`, now.Add(-time.Minute))
	mock.ExpectQuery("SELECT id, organization_id, webhook_id, COALESCE\\(event_types::text, '\\[\\]'\\), created_at FROM notification_subscriptions").
		WithArgs("org-1").
		WillReturnRows(rows)

	list, err := store.ListSubscriptions(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(list) != 2 || list[0].ID != "nsub-2" || list[1].ID != "nsub-1" {
		t.Fatalf("listing order wrong: %+v", list)
	}
}

func TestPgStoreDeleteSubscriptionGuard(t *testing.T) {
	store, mock, _ := newPgStore(t)
	ctx := context.Background()

	// RowsAffected == 0 (unknown or foreign id) -> ErrSubscriptionNotFound.
	mock.ExpectExec("DELETE FROM notification_subscriptions").
		WithArgs("nsub-x", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeleteSubscription(ctx, "org-1", "nsub-x"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("expected ErrSubscriptionNotFound, got %v", err)
	}

	mock.ExpectExec("DELETE FROM notification_subscriptions").
		WithArgs("nsub-1", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.DeleteSubscription(ctx, "org-1", "nsub-1"); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
}

func TestPgStoreNilDBGuard(t *testing.T) {
	store := NewPostgresStore(nil)
	ctx := context.Background()
	if err := store.CreateSubscription(ctx, &Subscription{ID: "nsub"}); err == nil {
		t.Fatal("nil db create should fail")
	}
	if _, err := store.GetSubscription(ctx, "org-1", "nsub"); err == nil {
		t.Fatal("nil db get should fail")
	}
	if _, err := store.ListSubscriptions(ctx, "org-1"); err == nil {
		t.Fatal("nil db list should fail")
	}
	if err := store.DeleteSubscription(ctx, "org-1", "nsub"); err == nil {
		t.Fatal("nil db delete should fail")
	}
}

// End-to-end over the durable store: a store-backed Service routes CRUD into
// the SQL store while the webhook existence check still resolves in-memory.
func TestServiceWithStoreCRUD(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewPostgresStore(db)
	hooks := webhooks.NewService()
	wh, _, err := hooks.CreateWebhook(context.Background(), "org-1", "https://hooks.example.com/x", nil)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	svc := NewServiceWithStore(store, nil, hooks, nil)

	// The service generates id/timestamps: match any arg for those.
	mock.ExpectExec("INSERT INTO notification_subscriptions").
		WithArgs(sqlmock.AnyArg(), "org-1", wh.ID, `["run.failed"]`, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	created, err := svc.CreateSubscription(context.Background(), "org-1", wh.ID, []string{"run.failed"})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if created.ID == "" || created.OrganizationID != "org-1" || created.WebhookID != wh.ID {
		t.Fatalf("created subscription wrong: %+v", created)
	}

	rows := sqlmock.NewRows([]string{"id", "organization_id", "webhook_id", "event_types", "created_at"}).
		AddRow(created.ID, "org-1", wh.ID, `["run.failed"]`, created.CreatedAt)
	mock.ExpectQuery("SELECT id, organization_id, webhook_id").
		WithArgs("org-1").
		WillReturnRows(rows)
	list, err := svc.ListSubscriptions(context.Background(), "org-1")
	if err != nil || len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("ListSubscriptions via store: %v %+v", err, list)
	}

	mock.ExpectExec("DELETE FROM notification_subscriptions").
		WithArgs(created.ID, "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := svc.DeleteSubscription(context.Background(), "org-1", created.ID); err != nil {
		t.Fatalf("DeleteSubscription via store: %v", err)
	}
}
