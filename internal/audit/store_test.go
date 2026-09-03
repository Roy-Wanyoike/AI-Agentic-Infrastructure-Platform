package audit

// Postgres-store tests for the keyset-paginated listing (issue #18), run
// against sqlmock so the SQL contract (tenant guard + keyset predicate +
// limit+1 overfetch) is pinned without a live database.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newAuditMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	cleanup := func() { _ = db.Close() }
	return db, mock, cleanup
}

func auditRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "organization_id", "actor", "action", "resource", "metadata", "created_at"})
}

func auditRow(id, orgID, action string, at time.Time) []driver.Value {
	return []driver.Value{id, orgID, "user-1", action, "resources/" + id, fmt.Sprintf(`{"seed":%q}`, id), at}
}

// The paged query must keep the tenant guard and the deterministic
// (created_at DESC, id DESC) keyset order.
const wantPagedSQL = `SELECT id, organization_id, COALESCE\(actor, ''\), action, COALESCE\(resource, ''\), COALESCE\(metadata::text, ''\), created_at FROM audit_logs WHERE organization_id = \$1 ORDER BY created_at DESC, id DESC LIMIT \$2`

const wantPagedCursorSQL = `SELECT id, organization_id, COALESCE\(actor, ''\), action, COALESCE\(resource, ''\), COALESCE\(metadata::text, ''\), created_at FROM audit_logs WHERE organization_id = \$1 AND \(created_at, id\) < \(\$2, \$3\) ORDER BY created_at DESC, id DESC LIMIT \$4`

func TestPgStoreListEntriesPagedFirstPageOverfetchesByOne(t *testing.T) {
	db, mock, cleanup := newAuditMockDB(t)
	defer cleanup()
	svc := NewServiceWithStore(NewPostgresStore(db))
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	// limit 3 -> the store fetches limit+1 = 4 rows; the 4th row proves a
	// follow-up page exists and is NOT returned.
	result := auditRows()
	for i := 3; i >= 0; i-- { // newest first, as ORDER BY would return them
		result.AddRow(auditRow(fmt.Sprintf("e%d", i), "org-1", "agent.created", base.Add(time.Duration(i)*time.Minute))...)
	}
	mock.ExpectQuery(wantPagedSQL).
		WithArgs("org-1", 4).
		WillReturnRows(result)

	entries, next, err := svc.ListEntriesPaged(context.Background(), "org-1", 3, "")
	if err != nil {
		t.Fatalf("ListEntriesPaged: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("page should carry exactly the limit, got %d", len(entries))
	}
	if entries[0].ID != "e3" || entries[2].ID != "e1" {
		t.Fatalf("page order wrong (newest first): %v", []string{entries[0].ID, entries[1].ID, entries[2].ID})
	}
	if next == "" {
		t.Fatal("a follow-up page exists, so a next cursor must be returned")
	}
	// The cursor encodes the LAST row of the returned page.
	_, cursorID, err := decodeCursor(next)
	if err != nil {
		t.Fatalf("next cursor must be decodable: %v", err)
	}
	if cursorID != "e1" {
		t.Fatalf("cursor should key the last row of the page (e1), got %q", cursorID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPgStoreListEntriesPagedContinuationUsesKeysetPredicate(t *testing.T) {
	db, mock, cleanup := newAuditMockDB(t)
	defer cleanup()
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	cursor := encodeCursor(&Entry{ID: "e1", CreatedAt: base.Add(1 * time.Minute)})

	// Only 2 remaining rows: fewer than the page size, so no next cursor.
	result := auditRows()
	result.AddRow(auditRow("e0", "org-1", "agent.created", base)...)
	result.AddRow(auditRow("e00", "org-1", "agent.created", base.Add(-time.Minute))...)
	mock.ExpectQuery(wantPagedCursorSQL).
		WithArgs("org-1", base.Add(1*time.Minute), "e1", 3).
		WillReturnRows(result)

	svc := NewServiceWithStore(NewPostgresStore(db))
	entries, next, err := svc.ListEntriesPaged(context.Background(), "org-1", 2, cursor)
	if err != nil {
		t.Fatalf("ListEntriesPaged: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("continuation page should carry the remaining rows, got %d", len(entries))
	}
	if next != "" {
		t.Fatalf("exhausted trail must not emit a cursor, got %q", next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPgStoreListEntriesPagedNormalizesLimit(t *testing.T) {
	db, mock, cleanup := newAuditMockDB(t)
	defer cleanup()
	svc := NewServiceWithStore(NewPostgresStore(db))
	// limit 0 -> NormalizeLimit -> DefaultListLimit (50), fetch 51.
	mock.ExpectQuery(wantPagedSQL).WithArgs("org-1", DefaultListLimit+1).WillReturnRows(auditRows())
	mock.ExpectQuery(wantPagedSQL).WithArgs("org-1", MaxListLimit+1).WillReturnRows(auditRows())

	if _, _, err := svc.ListEntriesPaged(context.Background(), "org-1", 0, ""); err != nil {
		t.Fatalf("zero limit should default, got %v", err)
	}
	if _, _, err := svc.ListEntriesPaged(context.Background(), "org-1", 100000, ""); err != nil {
		t.Fatalf("oversized limit should clamp, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPgStoreListEntriesPagedInvalidCursorFailsFast(t *testing.T) {
	db, mock, cleanup := newAuditMockDB(t)
	defer cleanup()
	svc := NewServiceWithStore(NewPostgresStore(db))
	if _, _, err := svc.ListEntriesPaged(context.Background(), "org-1", 10, "@@not-a-cursor@@"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("malformed cursor should be ErrInvalidCursor, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no query should run for a malformed cursor: %v", err)
	}
}

func TestPgStoreListEntriesPagedNilDBGuard(t *testing.T) {
	// Concrete type so the store-level guard is exercised directly.
	store, ok := NewPostgresStore(nil).(*pgStore)
	if !ok {
		t.Fatal("NewPostgresStore should return the pgStore implementation")
	}
	if _, _, err := store.ListEntriesPaged(context.Background(), "org-1", 10, ""); err == nil {
		t.Fatal("nil database must be reported as an error")
	}
}
