package limiter

// store_test.go — the dual-mode configuration store (issue #124 requirement
// 2): MemoryStore lifecycle, and the PostgresStore over sqlmock (the
// internal/models gateway_store_test.go conventions: row fixtures, tenant
// guards, ErrConfigNotFound on zero rows). SQL correctness against a real
// server is covered by TestApplyMigrations (internal/database), which
// applies migrations/036_rate_limits.sql.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"agent subject", Config{Scope: ScopeAgent, Subject: "a1", RPM: 10}, false},
		{"org default", Config{Scope: ScopeAgent, RPM: 10}, false},
		{"org scope no subject", Config{Scope: ScopeOrg, RPM: 10}, false},
		{"org scope with subject", Config{Scope: ScopeOrg, Subject: "x", RPM: 10}, true},
		{"unknown scope", Config{Scope: "tenant", RPM: 10}, true},
		{"negative rpm", Config{Scope: ScopeTool, RPM: -1}, true},
		{"negative tpm", Config{Scope: ScopeModel, TPM: -5}, true},
		{"all zero valid", Config{Scope: ScopeTool}, false},
	}
	for _, tc := range cases {
		err := tc.cfg.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

func TestMemoryStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	if _, err := store.Get(ctx, "org-1", ScopeAgent, "a1"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("missing row: want ErrConfigNotFound, got %v", err)
	}

	cfg := Config{Scope: ScopeAgent, Subject: "a1", RPM: 5, RPD: 100, Enabled: true}
	if err := store.Upsert(ctx, "org-1", cfg); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Upsert validates: an invalid record must be rejected.
	if err := store.Upsert(ctx, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid upsert: want ErrInvalidConfig, got %v", err)
	}

	got, err := store.Get(ctx, "org-1", "agent", "a1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RPM != 5 || got.RPD != 100 || !got.Enabled {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Update over the same key keeps CreatedAt, bumps UpdatedAt.
	first := got.CreatedAt
	time.Sleep(time.Millisecond) // UpdatedAt resolution
	if err := store.Upsert(ctx, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 6, Enabled: true}); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}
	got, _ = store.Get(ctx, "org-1", "agent", "a1")
	if got.RPM != 6 {
		t.Fatalf("update lost: %+v", got)
	}
	if !got.CreatedAt.Equal(first) {
		t.Fatal("CreatedAt must be preserved on upsert")
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatal("UpdatedAt must move forward")
	}

	// Tenant guard: the org filter is part of the key.
	if _, err := store.Get(ctx, "org-2", ScopeAgent, "a1"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatal("cross-tenant read must miss")
	}
	if rows, err := store.List(ctx, "org-2"); err != nil || len(rows) != 0 {
		t.Fatalf("cross-tenant list must be empty: %v %v", rows, err)
	}

	// Org default (empty subject) coexists with subject rows.
	if err := store.Upsert(ctx, "org-1", Config{Scope: ScopeAgent, RPM: 1, Enabled: true}); err != nil {
		t.Fatalf("Upsert default: %v", err)
	}
	rows, err := store.List(ctx, "org-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	// Deterministic order: org default (subject "") first.
	if rows[0].Subject != "" || rows[1].Subject != "a1" {
		t.Fatalf("list order wrong: %+v", rows)
	}

	if err := store.Delete(ctx, "org-1", ScopeAgent, "a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(ctx, "org-1", ScopeAgent, "a1"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("double delete: want ErrConfigNotFound, got %v", err)
	}
}

func TestPostgresStoreUpsertGetDelete(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewPostgresStore(db)
	ctx := context.Background()

	// Upsert: subject row binds the subject; org default binds NULL
	// (NULLIF('')). The conflict target is the expression index.
	mock.ExpectExec(`INSERT INTO rate_limits`).
		WithArgs("org-1", "agent", "a1", 5, 100, 0, true, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Upsert(ctx, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 5, RPD: 100, Enabled: true}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	mock.ExpectExec(`INSERT INTO rate_limits`).
		WithArgs("org-1", "tool", "", 3, 0, 0, true, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Upsert(ctx, "org-1", Config{Scope: ScopeTool, Enabled: true, RPM: 3}); err != nil {
		t.Fatalf("Upsert org default: %v", err)
	}

	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT organization_id, scope, subject_id, rpm, rpd, tpm, enabled, created_at, updated_at FROM rate_limits`).
		WithArgs("org-1", "agent", "a1").
		WillReturnRows(rateLimitRows("org-1", "agent", "a1", 5, 100, 0, now))
	got, err := store.Get(ctx, "org-1", ScopeAgent, "a1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Subject != "a1" || got.RPM != 5 || got.RPD != 100 {
		t.Fatalf("row mismatch: %+v", got)
	}

	// Org default: subject "" binds NULL and scans back as "".
	mock.ExpectQuery(`SELECT organization_id, scope, subject_id`).
		WithArgs("org-1", "tool", "").
		WillReturnRows(rateLimitRows("org-1", "tool", "", 3, 0, 0, now))
	got, err = store.Get(ctx, "org-1", ScopeTool, "")
	if err != nil {
		t.Fatalf("Get org default: %v", err)
	}
	if got.Subject != "" {
		t.Fatalf("NULL subject must scan as empty string, got %q", got.Subject)
	}

	// Missing row -> ErrConfigNotFound.
	mock.ExpectQuery(`SELECT organization_id, scope, subject_id`).
		WithArgs("org-1", "model", "gpt-x").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.Get(ctx, "org-1", ScopeModel, "gpt-x"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("missing row: want ErrConfigNotFound, got %v", err)
	}

	mock.ExpectExec(`DELETE FROM rate_limits`).
		WithArgs("org-1", "agent", "a1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Delete(ctx, "org-1", ScopeAgent, "a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Zero rows affected -> ErrConfigNotFound.
	mock.ExpectExec(`DELETE FROM rate_limits`).
		WithArgs("org-1", "agent", "a1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.Delete(ctx, "org-1", ScopeAgent, "a1"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("delete missing: want ErrConfigNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreListTenantScoped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewPostgresStore(db)

	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"organization_id", "scope", "subject_id", "rpm", "rpd", "tpm", "enabled", "created_at", "updated_at"}).
		AddRow("org-1", "agent", nil, 2, 0, 0, true, now, now).
		AddRow("org-1", "agent", "a1", 5, 0, 0, true, now, now)
	mock.ExpectQuery(`SELECT organization_id, scope, subject_id, rpm, rpd, tpm, enabled, created_at, updated_at FROM rate_limits`).
		WithArgs("org-1").
		WillReturnRows(rows)
	got, err := store.List(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].Subject != "" || got[1].Subject != "a1" {
		t.Fatalf("order/subjects wrong: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

// rateLimitRows builds a one-row SELECT result; subject "" renders SQL NULL.
func rateLimitRows(org, scope, subject string, rpm, rpd, tpm int, now time.Time) *sqlmock.Rows {
	var sub any
	if subject != "" {
		sub = subject
	}
	return sqlmock.NewRows([]string{
		"organization_id", "scope", "subject_id", "rpm", "rpd", "tpm", "enabled", "created_at", "updated_at",
	}).AddRow(org, scope, sub, rpm, rpd, tpm, true, now, now)
}
