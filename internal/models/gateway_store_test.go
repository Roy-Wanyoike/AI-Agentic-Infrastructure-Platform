package models

// gateway_store_test.go — sqlmock coverage for the Postgres GatewayStore,
// following the internal/policies store-test conventions: row fixtures,
// tenant-guard assertions (zero-rows-affected -> ErrVirtualKeyNotFound),
// the JSONB nil-vs-empty allow-list round-trip, and the ledger SUM path.
// SQL correctness against a real server is covered by TestApplyMigrations
// (internal/database), which applies migrations/030_virtual_keys.sql.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newGatewayMockStore(t *testing.T) (sqlmock.Sqlmock, GatewayStore) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return mock, NewGatewayPostgresStore(db)
}

// gatewayKeyRow builds a SELECT result row; modelsRaw is the JSONB payload
// (nil = SQL NULL = allow-list disabled).
func gatewayKeyRow(id string, modelsRaw []byte) *sqlmock.Rows {
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{
		"id", "organization_id", "label", "prefix", "key_hash", "created_by", "models",
		"monthly_limit_cents", "status", "expires_at", "revoked_at", "created_at", "updated_at",
	})
	var expires, revoked any
	return rows.AddRow(id, "org-1", "ops key", "vk_abcdef123456", "hash-"+id, "user-1", modelsRaw,
		500, "active", expires, revoked, now, now)
}

func TestGatewayPGCreateAndGetByHash(t *testing.T) {
	mock, store := newGatewayMockStore(t)
	ctx := context.Background()

	key := &VirtualKey{
		ID: "vk-1", OrganizationID: "org-1", Label: "ops key", KeyPrefix: "vk_abcdef123456",
		KeyHash: "hash-vk-1", CreatedBy: "user-1", Models: []string{"gpt-4o", "o3-mini"},
		MonthlyLimitCents: 500, Status: VirtualKeyStatusActive,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	mock.ExpectExec(`INSERT INTO virtual_keys`).
		WithArgs("vk-1", "org-1", "ops key", "vk_abcdef123456", "hash-vk-1", "user-1",
			`["gpt-4o","o3-mini"]`, 500, "active", nil, nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateVirtualKey(ctx, key); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}

	mock.ExpectQuery(`SELECT id, organization_id, label, prefix, key_hash, created_by, models,`).
		WithArgs("hash-vk-1").
		WillReturnRows(gatewayKeyRow("vk-1", []byte(`["gpt-4o","o3-mini"]`)))
	got, err := store.GetVirtualKeyByHash(ctx, "hash-vk-1")
	if err != nil {
		t.Fatalf("GetVirtualKeyByHash: %v", err)
	}
	if got.ID != "vk-1" || got.OrganizationID != "org-1" {
		t.Fatalf("unexpected key: %+v", got)
	}
	if len(got.Models) != 2 || got.Models[1] != "o3-mini" {
		t.Fatalf("allow-list not decoded: %#v", got.Models)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestGatewayPGAllowListNilVsEmptyRoundTrip(t *testing.T) {
	mock, store := newGatewayMockStore(t)
	ctx := context.Background()

	// SQL NULL -> nil slice (allow-list DISABLED: ALL models).
	mock.ExpectQuery(`SELECT id, organization_id, label`).
		WithArgs("vk-nil", "org-1").
		WillReturnRows(gatewayKeyRow("vk-nil", nil))
	got, err := store.GetVirtualKey(ctx, "org-1", "vk-nil")
	if err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if got.Models != nil {
		t.Fatalf("NULL JSONB must decode as nil allow-list: %#v", got.Models)
	}

	// '[]' -> non-nil EMPTY slice (allow-list ENABLED and empty: NONE).
	mock.ExpectQuery(`SELECT id, organization_id, label`).
		WithArgs("vk-empty", "org-1").
		WillReturnRows(gatewayKeyRow("vk-empty", []byte(`[]`)))
	got, err = store.GetVirtualKey(ctx, "org-1", "vk-empty")
	if err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if got.Models == nil || len(got.Models) != 0 {
		t.Fatalf("'[]' must decode as enabled-and-empty allow-list: %#v", got.Models)
	}
}

func TestGatewayPGGetScopedNotFound(t *testing.T) {
	mock, store := newGatewayMockStore(t)
	mock.ExpectQuery(`SELECT id, organization_id, label`).
		WithArgs("missing", "org-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetVirtualKey(context.Background(), "org-1", "missing"); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("expected ErrVirtualKeyNotFound, got %v", err)
	}
}

func TestGatewayPGListTenantScoped(t *testing.T) {
	mock, store := newGatewayMockStore(t)
	mock.ExpectQuery(`SELECT id, organization_id, label`).
		WithArgs("org-1").
		WillReturnRows(
			gatewayKeyRow("vk-1", nil).AddRow(
				"vk-2", "org-1", "second", "vk_bbbbbbbbbbbb", "hash-vk-2", "user-1",
				[]byte(`[]`), 0, "revoked", nil, nil, time.Now().UTC(), time.Now().UTC(),
			))
	list, err := store.ListVirtualKeys(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListVirtualKeys: %v", err)
	}
	if len(list) != 2 || list[0].ID != "vk-1" || list[1].ID != "vk-2" {
		t.Fatalf("ordering/scoping wrong: %+v", list)
	}
	if list[1].Models == nil || len(list[1].Models) != 0 {
		t.Fatalf("second row allow-list must round-trip enabled-and-empty: %#v", list[1].Models)
	}
}

func TestGatewayPGUpdateTenantGuard(t *testing.T) {
	mock, store := newGatewayMockStore(t)
	ctx := context.Background()

	key := &VirtualKey{
		ID: "vk-1", OrganizationID: "org-1", Label: "renamed",
		Models: []string{"gpt-4o"}, MonthlyLimitCents: 1000,
		Status: VirtualKeyStatusActive, UpdatedAt: time.Now().UTC(),
	}
	mock.ExpectExec(`UPDATE virtual_keys SET`).
		WithArgs("vk-1", "org-1", "renamed", `["gpt-4o"]`, 1000, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateVirtualKey(ctx, key); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}

	// Zero rows affected (unknown id or tenant guard) -> ErrVirtualKeyNotFound.
	mock.ExpectExec(`UPDATE virtual_keys SET`).
		WithArgs("vk-1", "org-9", "renamed", `["gpt-4o"]`, 1000, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	key.OrganizationID = "org-9"
	if err := store.UpdateVirtualKey(ctx, key); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("expected ErrVirtualKeyNotFound from tenant guard, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestGatewayPGRevokeTenantGuard(t *testing.T) {
	mock, store := newGatewayMockStore(t)
	ctx := context.Background()
	when := time.Now().UTC()

	mock.ExpectExec(`UPDATE virtual_keys SET status = 'revoked'`).
		WithArgs("vk-1", "org-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.RevokeVirtualKey(ctx, "org-1", "vk-1", when); err != nil {
		t.Fatalf("RevokeVirtualKey: %v", err)
	}
	mock.ExpectExec(`UPDATE virtual_keys SET status = 'revoked'`).
		WithArgs("vk-1", "org-9", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.RevokeVirtualKey(ctx, "org-9", "vk-1", when); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("expected ErrVirtualKeyNotFound from tenant guard, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestGatewayPGRecordSpendAndSum(t *testing.T) {
	mock, store := newGatewayMockStore(t)
	ctx := context.Background()
	at := time.Date(2024, 7, 10, 12, 0, 0, 0, time.UTC)

	mock.ExpectExec(`INSERT INTO virtual_key_spend`).
		WithArgs(sqlmock.AnyArg(), "org-1", "vk-1", "2024-07", "gpt-4o", 0.135, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.RecordSpend(ctx, "org-1", "vk-1", "gpt-4o", 0.135, at); err != nil {
		t.Fatalf("RecordSpend: %v", err)
	}

	mock.ExpectQuery(`SELECT COALESCE\(SUM\(cost_cents\), 0\)`).
		WithArgs("org-1", "vk-1", "2024-07").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(12.5))
	total, err := store.SpendForMonth(ctx, "org-1", "vk-1", "2024-07")
	if err != nil {
		t.Fatalf("SpendForMonth: %v", err)
	}
	if total != 12.5 {
		t.Fatalf("unexpected ledger sum: %v", total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestGatewayPGNilDBGuard(t *testing.T) {
	store := NewGatewayPostgresStore(nil)
	ctx := context.Background()
	if err := store.CreateVirtualKey(ctx, &VirtualKey{}); err == nil {
		t.Fatal("expected nil-db guard error on create")
	}
	if _, err := store.SpendForMonth(ctx, "org", "key", "2024-01"); err == nil {
		t.Fatal("expected nil-db guard error on spend")
	}
}
