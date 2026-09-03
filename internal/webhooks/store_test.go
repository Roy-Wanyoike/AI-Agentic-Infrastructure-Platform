package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPgStoreCreateAndGetWebhook(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewPostgresStore(db)

	now := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	wh := &Webhook{
		ID:             "wh-1",
		OrganizationID: "org-1",
		URL:            "https://example.com/hook",
		Events:         []string{"run.failed"},
		SecretHash:     "abc123",
		Status:         StatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	mock.ExpectExec("INSERT INTO webhooks").
		WithArgs("wh-1", "org-1", "https://example.com/hook", `["run.failed"]`, "abc123", "active", now, now).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := store.CreateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	eventsJSON, _ := json.Marshal(wh.Events)
	rows := sqlmock.NewRows([]string{"id", "organization_id", "url", "events", "secret_hash", "status", "created_at", "updated_at"}).
		AddRow(wh.ID, wh.OrganizationID, wh.URL, string(eventsJSON), wh.SecretHash, wh.Status, now, now)
	mock.ExpectQuery("SELECT id, organization_id, url, COALESCE\\(events::text, '\\[\\]'\\), secret_hash, status, created_at, updated_at FROM webhooks WHERE id = \\$1 AND organization_id = \\$2").
		WithArgs("wh-1", "org-1").
		WillReturnRows(rows)

	got, err := store.GetWebhook(context.Background(), "org-1", "wh-1")
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if got.ID != "wh-1" || got.URL != wh.URL || len(got.Events) != 1 || got.Events[0] != "run.failed" || got.SecretHash != "abc123" {
		t.Errorf("scanned webhook wrong: %+v", got)
	}

	// tenant guard: unknown org -> no rows -> ErrWebhookNotFound
	mock.ExpectQuery("SELECT id, organization_id").
		WithArgs("wh-1", "org-other").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetWebhook(context.Background(), "org-other", "wh-1"); !errors.Is(err, ErrWebhookNotFound) {
		t.Errorf("expected ErrWebhookNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestPgStoreListDeleteUpdateWebhook(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewPostgresStore(db)

	now := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "organization_id", "url", "events", "secret_hash", "status", "created_at", "updated_at"}).
		AddRow("wh-2", "org-1", "https://two.example.com", "[]", "hash2", "active", now, now).
		AddRow("wh-3", "org-1", "https://three.example.com", `["run.completed","run.failed"]`, "hash3", "disabled", now, now)
	mock.ExpectQuery("SELECT id, organization_id, url, COALESCE\\(events::text, '\\[\\]'\\), secret_hash, status, created_at, updated_at FROM webhooks WHERE organization_id = \\$1 ORDER BY created_at DESC").
		WithArgs("org-1").
		WillReturnRows(rows)

	list, err := store.ListWebhooks(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 webhooks, got %d", len(list))
	}
	if list[1].Events[1] != "run.failed" {
		t.Errorf("events JSON should round-trip, got %v", list[1].Events)
	}

	mock.ExpectExec("DELETE FROM webhooks WHERE id = \\$1 AND organization_id = \\$2").
		WithArgs("wh-2", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.DeleteWebhook(context.Background(), "org-1", "wh-2"); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}

	mock.ExpectExec("DELETE FROM webhooks WHERE id = \\$1 AND organization_id = \\$2").
		WithArgs("missing", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeleteWebhook(context.Background(), "org-1", "missing"); !errors.Is(err, ErrWebhookNotFound) {
		t.Errorf("delete of unknown row must be ErrWebhookNotFound, got %v", err)
	}

	mock.ExpectExec("UPDATE webhooks SET status = \\$1, updated_at = \\$2 WHERE id = \\$3 AND organization_id = \\$4").
		WithArgs(StatusDisabled, sqlmock.AnyArg(), "wh-3", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	wh := &Webhook{ID: "wh-3", OrganizationID: "org-1", Status: StatusDisabled}
	if err := store.UpdateWebhook(context.Background(), wh); err != nil {
		t.Fatalf("UpdateWebhook: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestPgStoreDeliveriesUpsertAndList(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewPostgresStore(db)

	now := time.Date(2025, 6, 1, 11, 0, 0, 0, time.UTC)
	delivery := &Delivery{
		ID:             "d-1",
		WebhookID:      "wh-1",
		OrganizationID: "org-1",
		EventID:        "evt-1",
		EventType:      "run.failed",
		Status:         DeliveryRetrying,
		Attempts:       1,
		LastStatusCode: 500,
		LatencyMS:      34,
		Error:          "internal server error",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	mock.ExpectExec("INSERT INTO webhook_deliveries .* ON CONFLICT \\(id\\) DO UPDATE SET status = EXCLUDED.status, attempts = EXCLUDED.attempts, last_status_code = EXCLUDED.last_status_code, latency_ms = EXCLUDED.latency_ms, error = EXCLUDED.error, updated_at = EXCLUDED.updated_at").
		WithArgs("d-1", "org-1", "wh-1", "evt-1", "run.failed", "retrying", 1, 500, int64(34), "internal server error", now, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.UpsertDelivery(context.Background(), delivery); err != nil {
		t.Fatalf("UpsertDelivery: %v", err)
	}

	// attempt 2 mutates the same row
	delivery.Attempts = 2
	delivery.Status = DeliveryFailed
	delivery.LastStatusCode = 502
	mock.ExpectExec("INSERT INTO webhook_deliveries").
		WithArgs("d-1", "org-1", "wh-1", "evt-1", "run.failed", "failed", 2, 502, sqlmock.AnyArg(), sqlmock.AnyArg(), now, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.UpsertDelivery(context.Background(), delivery); err != nil {
		t.Fatalf("UpsertDelivery retry: %v", err)
	}

	listRows := sqlmock.NewRows([]string{"id", "webhook_id", "event_id", "event_type", "status", "attempts", "last_status_code", "latency_ms", "error", "created_at", "updated_at"}).
		AddRow("d-1", "wh-1", "evt-1", "run.failed", "failed", 2, 502, 40, "bad gateway", now, now)
	mock.ExpectQuery("SELECT d.id, d.webhook_id, d.event_id, d.event_type, d.status, d.attempts, d.last_status_code, d.latency_ms, COALESCE\\(d\\.error, ''\\), d.created_at, d.updated_at FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id WHERE d.organization_id = \\$1 AND d.webhook_id = \\$2 ORDER BY d.created_at DESC LIMIT \\$3").
		WithArgs("org-1", "wh-1", 50).
		WillReturnRows(listRows)

	list, err := store.ListDeliveries(context.Background(), "org-1", "wh-1", 50)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(list) != 1 || list[0].Attempts != 2 || list[0].Status != DeliveryFailed || list[0].Error != "bad gateway" {
		t.Errorf("delivery list wrong: %+v", list)
	}
	if !strings.Contains(list[0].EventType, "run.failed") {
		t.Errorf("event type should round-trip: %+v", list[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestPgStoreNilDBGuard(t *testing.T) {
	store := NewPostgresStore(nil)
	ctx := context.Background()
	if err := store.CreateWebhook(ctx, &Webhook{}); err == nil {
		t.Error("nil db must error")
	}
	if _, err := store.GetWebhook(ctx, "org-1", "wh"); err == nil {
		t.Error("nil db must error")
	}
	if _, err := store.ListWebhooks(ctx, "org-1"); err == nil {
		t.Error("nil db must error")
	}
	if err := store.DeleteWebhook(ctx, "org-1", "wh"); err == nil {
		t.Error("nil db must error")
	}
	if err := store.UpdateWebhook(ctx, &Webhook{}); err == nil {
		t.Error("nil db must error")
	}
	if err := store.UpsertDelivery(ctx, &Delivery{}); err == nil {
		t.Error("nil db must error")
	}
	if _, err := store.ListDeliveries(ctx, "org-1", "wh", 50); err == nil {
		t.Error("nil db must error")
	}
}
