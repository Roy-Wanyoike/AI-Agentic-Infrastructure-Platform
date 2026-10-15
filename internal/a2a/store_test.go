package a2a

// store_test.go — sqlmock tests for the Postgres CardStore, pinning the SQL
// contract (tenant guard on every statement, JSONB round-trips, 0-rows ->
// ErrNotFound) without a live database. Follows internal/audit/store_test.go
// patterns.

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newA2AMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	cleanup := func() { _ = db.Close() }
	return db, mock, cleanup
}

func fixtureCard() *AgentCard {
	now := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	return &AgentCard{
		ID:                 "card-1",
		OrganizationID:     "org-1",
		AgentID:            "agent-1",
		Name:               "Weather Agent",
		Description:        "Talks about weather",
		Version:            "1.0.0",
		Provider:           CardProvider{Organization: "Acme", URL: "https://acme.example"},
		EndpointURL:        "https://agents.example/weather",
		Capabilities:       Capabilities{Streaming: true, PushNotifications: false, StateTransitionHistory: true},
		Skills:             []Skill{{ID: "forecast", Name: "Forecast", Description: "5-day", Tags: []string{"weather"}}},
		AuthScheme:         "bearer",
		PreferredTransport: TransportJSONRPC,
		CreatedBy:          "user-1",
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

func cardRow(id, orgID string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "agent_id", "name", "description", "version",
		"provider_organization", "provider_url", "endpoint_url",
		"capabilities", "skills", "auth_scheme", "preferred_transport",
		"created_by", "created_at", "updated_at",
	}).AddRow(
		id, orgID, "agent-1", "Weather Agent", "Talks about weather", "1.0.0",
		"Acme", "https://acme.example", "https://agents.example/weather",
		`{"streaming":true,"pushNotifications":false,"stateTransitionHistory":true}`,
		`[{"id":"forecast","name":"Forecast","description":"5-day","tags":["weather"]}]`,
		"bearer", TransportJSONRPC,
		"user-1", time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC),
	)
}

func TestPgStoreCreateCard(t *testing.T) {
	db, mock, cleanup := newA2AMockDB(t)
	defer cleanup()
	store := NewPostgresStore(db)
	card := fixtureCard()

	mock.ExpectExec(regexp.QuoteMeta(sqlInsertCard)).
		WithArgs(
			card.ID, card.OrganizationID, card.AgentID, card.Name, card.Description, card.Version,
			card.Provider.Organization, card.Provider.URL, card.EndpointURL,
			`{"streaming":true,"pushNotifications":false,"stateTransitionHistory":true}`,
			`[{"id":"forecast","name":"Forecast","description":"5-day","tags":["weather"]}]`,
			card.AuthScheme, card.PreferredTransport, card.CreatedBy, card.CreatedAt, card.UpdatedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := store.CreateCard(context.Background(), "org-1", card); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPgStoreGetCardRoundTrip(t *testing.T) {
	db, mock, cleanup := newA2AMockDB(t)
	defer cleanup()
	store := NewPostgresStore(db)

	// The org guard is part of the SQL contract: id AND organization_id.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectCardScoped)).
		WithArgs("card-1", "org-1").
		WillReturnRows(cardRow("card-1", "org-1"))

	card, err := store.GetCard(context.Background(), "org-1", "card-1")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	want := fixtureCard()
	if card.ID != want.ID || card.OrganizationID != "org-1" || card.AgentID != want.AgentID ||
		card.Name != want.Name || card.Version != want.Version || card.EndpointURL != want.EndpointURL ||
		card.AuthScheme != "bearer" || card.PreferredTransport != TransportJSONRPC || card.CreatedBy != "user-1" {
		t.Fatalf("round trip mismatch: %+v", card)
	}
	if card.Provider != want.Provider {
		t.Fatalf("provider round trip mismatch: %+v", card.Provider)
	}
	if !card.Capabilities.Streaming || card.Capabilities.PushNotifications || !card.Capabilities.StateTransitionHistory {
		t.Fatalf("capabilities round trip mismatch: %+v", card.Capabilities)
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "forecast" || card.Skills[0].Tags[0] != "weather" {
		t.Fatalf("skills round trip mismatch: %+v", card.Skills)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPgStoreGetCardNoRows(t *testing.T) {
	db, mock, cleanup := newA2AMockDB(t)
	defer cleanup()
	store := NewPostgresStore(db)
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectCardScoped)).
		WithArgs("unknown", "org-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetCard(context.Background(), "org-1", "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestPgStoreListCardsByOrg(t *testing.T) {
	db, mock, cleanup := newA2AMockDB(t)
	defer cleanup()
	store := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectCardsByOrg)).
		WithArgs("org-1").
		WillReturnRows(cardRow("card-1", "org-1").AddRow(
			"card-2", "org-1", "agent-1", "Second", "", "0.1.0",
			"", "", "https://x.example", "{}", "[]", "", "", "", time.Time{}, time.Time{},
		))

	cards, err := store.ListCards(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("list length = %d", len(cards))
	}
	if cards[1].Name != "Second" || cards[1].Capabilities.Streaming {
		t.Fatalf("second row wrong: %+v", cards[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPgStoreUpdateCard(t *testing.T) {
	db, mock, cleanup := newA2AMockDB(t)
	defer cleanup()
	store := NewPostgresStore(db)
	card := fixtureCard()
	card.Name = "Weather v2"

	// Happy path: the update lands (rows affected 1).
	mock.ExpectExec(regexp.QuoteMeta(sqlUpdateCard)).
		WithArgs(
			card.ID, card.OrganizationID, card.AgentID, card.Name, card.Description, card.Version,
			card.Provider.Organization, card.Provider.URL, card.EndpointURL,
			`{"streaming":true,"pushNotifications":false,"stateTransitionHistory":true}`,
			`[{"id":"forecast","name":"Forecast","description":"5-day","tags":["weather"]}]`,
			card.AuthScheme, card.PreferredTransport, card.UpdatedAt,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateCard(context.Background(), "org-1", card); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}

	// Tenant guard: 0 rows affected (wrong org or unknown card) -> ErrNotFound.
	mock.ExpectExec(regexp.QuoteMeta(sqlUpdateCard)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateCard(context.Background(), "org-other", card); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on 0 rows, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPgStoreDeleteCard(t *testing.T) {
	db, mock, cleanup := newA2AMockDB(t)
	defer cleanup()
	store := NewPostgresStore(db)

	mock.ExpectExec(regexp.QuoteMeta(sqlDeleteCard)).
		WithArgs("card-1", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.DeleteCard(context.Background(), "org-1", "card-1"); err != nil {
		t.Fatalf("DeleteCard: %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta(sqlDeleteCard)).
		WithArgs("card-1", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeleteCard(context.Background(), "org-1", "card-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on 0 rows, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPgStoreNilDBGuard(t *testing.T) {
	store, ok := NewPostgresStore(nil).(*pgStore)
	if !ok {
		t.Fatal("NewPostgresStore should return the pgStore implementation")
	}
	ctx := context.Background()
	if err := store.CreateCard(ctx, "org-1", fixtureCard()); err == nil {
		t.Error("nil database must be reported on CreateCard")
	}
	if _, err := store.GetCard(ctx, "org-1", "card-1"); err == nil {
		t.Error("nil database must be reported on GetCard")
	}
	if _, err := store.ListCards(ctx, "org-1"); err == nil {
		t.Error("nil database must be reported on ListCards")
	}
	if err := store.UpdateCard(ctx, "org-1", fixtureCard()); err == nil {
		t.Error("nil database must be reported on UpdateCard")
	}
	if err := store.DeleteCard(ctx, "org-1", "card-1"); err == nil {
		t.Error("nil database must be reported on DeleteCard")
	}
}

// The store is the durable source of truth; the service must route reads
// through it in store mode (and keep the org guard intact end to end).
func TestServiceWithStoreUsesStore(t *testing.T) {
	db, mock, cleanup := newA2AMockDB(t)
	defer cleanup()
	svc := NewServiceWithStore(NewPostgresStore(db))

	// Create writes through: the INSERT runs AND the cache is populated.
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertCard)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	card, err := svc.CreateCard(context.Background(), "org-1", validInput("agent-1"))
	if err != nil {
		t.Fatalf("create through store: %v", err)
	}

	// Get hits the STORE (not the cache): one scoped SELECT.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectCardScoped)).
		WithArgs(card.ID, "org-1").
		WillReturnRows(cardRow(card.ID, "org-1"))
	if _, err := svc.GetCard(context.Background(), "org-1", card.ID); err != nil {
		t.Fatalf("get through store: %v", err)
	}

	// Delete also flows through the store: the service existence-checks
	// (scoped SELECT) and then hard-deletes.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectCardScoped)).
		WithArgs(card.ID, "org-1").
		WillReturnRows(cardRow(card.ID, "org-1"))
	mock.ExpectExec(regexp.QuoteMeta(sqlDeleteCard)).
		WithArgs(card.ID, "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := svc.DeleteCard(context.Background(), "org-1", card.ID); err != nil {
		t.Fatalf("delete through store: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
