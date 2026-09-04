package organizations

// Issue #52: sqlmock contracts for the pg store half of the members API —
// the org-guarded UPDATE/DELETE (RowsAffected == 0 must map to
// ErrMembershipNotFound so unknown AND foreign-organization ids answer 404
// with no existence leak) and the joined_at projection on the listing.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newOrgStoreMock(t *testing.T) (*pgStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	store, ok := NewPostgresStore(db).(*pgStore)
	if !ok {
		t.Fatalf("NewPostgresStore must return *pgStore")
	}
	return store, mock, func() { _ = db.Close() }
}

func TestPgStoreUpdateMembershipRoleBindsOrgGuard(t *testing.T) {
	store, mock, closeDB := newOrgStoreMock(t)
	defer closeDB()

	mock.ExpectExec("UPDATE organization_memberships SET role").
		WithArgs("ADMIN", "org-1", "user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.UpdateMembershipRole(context.Background(), "org-1", "user-1", "ADMIN"); err != nil {
		t.Fatalf("UpdateMembershipRole: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPgStoreMembershipMissMapsToSentinel(t *testing.T) {
	store, mock, closeDB := newOrgStoreMock(t)
	defer closeDB()

	// UPDATE and DELETE both rely on RowsAffected: 0 rows means the
	// (organization_id, user_id) pair does not exist for THIS tenant.
	mock.ExpectExec("UPDATE organization_memberships SET role").
		WithArgs("ADMIN", "org-1", "ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM organization_memberships").
		WithArgs("org-1", "ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))

	ctx := context.Background()
	if err := store.UpdateMembershipRole(ctx, "org-1", "ghost", "ADMIN"); !errors.Is(err, ErrMembershipNotFound) {
		t.Fatalf("update miss: expected ErrMembershipNotFound, got %v", err)
	}
	if err := store.DeleteMembership(ctx, "org-1", "ghost"); !errors.Is(err, ErrMembershipNotFound) {
		t.Fatalf("delete miss: expected ErrMembershipNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPgStoreDeleteMembershipBindsOrgGuard(t *testing.T) {
	store, mock, closeDB := newOrgStoreMock(t)
	defer closeDB()

	mock.ExpectExec("DELETE FROM organization_memberships").
		WithArgs("org-1", "user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.DeleteMembership(context.Background(), "org-1", "user-1"); err != nil {
		t.Fatalf("DeleteMembership: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPgStoreListMembershipsProjectsJoinedAt(t *testing.T) {
	store, mock, closeDB := newOrgStoreMock(t)
	defer closeDB()

	joined := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"user_id", "organization_id", "role", "created_at"}).
		AddRow("user-1", "org-1", "OWNER", joined).
		AddRow("user-2", "org-1", "MEMBER", joined.Add(time.Minute))
	mock.ExpectQuery("SELECT user_id, organization_id, role, created_at FROM organization_memberships WHERE organization_id").
		WithArgs("org-1").
		WillReturnRows(rows)

	members, err := store.ListMemberships(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 memberships, got %d", len(members))
	}
	if !reflect.DeepEqual(members[0], Membership{UserID: "user-1", OrganizationID: "org-1", Role: "OWNER", CreatedAt: joined}) {
		t.Fatalf("first membership projection wrong: %+v", members[0])
	}
	if !members[1].CreatedAt.Equal(joined.Add(time.Minute)) {
		t.Fatalf("joined_at must come from the row, got %v", members[1].CreatedAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPgStoreNilDBGuard(t *testing.T) {
	var store *pgStore
	ctx := context.Background()
	if err := store.UpdateMembershipRole(ctx, "org-1", "user-1", "ADMIN"); err == nil {
		t.Fatal("nil store must fail the guard")
	}
	if err := store.DeleteMembership(ctx, "org-1", "user-1"); err == nil {
		t.Fatal("nil store must fail the guard")
	}
}
