package policies

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockStore(t *testing.T) (sqlmock.Sqlmock, Store) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return mock, NewPostgresStore(db)
}

func policyRow(id string, priority int) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "name", "effect", "resource_type", "actions", "conditions",
		"priority", "enabled", "created_at", "updated_at",
	}).AddRow(
		id, "org-1", "Deny prod tools", "deny", "tool",
		[]byte(`["tools.call"]`),
		[]byte(`{"tool_allowlist":["httpx"],"max_cost_cents":500,"environments":["production"],"require_approval":true}`),
		priority, true, time.Now().UTC(), time.Now().UTC(),
	)
}

func TestPostgresStoreCreateAndGet(t *testing.T) {
	mock, store := newMockStore(t)

	policy := &Policy{
		ID:             "pol-1",
		OrganizationID: "org-1",
		Name:           "Deny prod tools",
		Effect:         EffectDeny,
		ResourceType:   ResourceTool,
		Actions:        []string{"tools.call"},
		Conditions:     Conditions{Environments: []string{"production"}, MaxCostCents: costPtr(500)},
		Priority:       100,
		Enabled:        true,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}

	mock.ExpectExec(`INSERT INTO policies`).
		WithArgs("pol-1", "org-1", "Deny prod tools", "deny", "tool",
			sqlmock.AnyArg(), sqlmock.AnyArg(), 100, true, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := store.CreatePolicy(context.Background(), policy); err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	mock.ExpectQuery(`SELECT id, organization_id, name, effect, resource_type, actions, conditions,`).
		WithArgs("pol-1", "org-1").
		WillReturnRows(policyRow("pol-1", 100))

	got, err := store.GetPolicy(context.Background(), "org-1", "pol-1")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.Effect != EffectDeny || got.ResourceType != ResourceTool {
		t.Fatalf("unexpected policy: %+v", got)
	}
	if len(got.Conditions.Environments) != 1 || got.Conditions.Environments[0] != "production" {
		t.Fatalf("conditions not decoded: %+v", got.Conditions)
	}
	if got.Conditions.MaxCostCents == nil || *got.Conditions.MaxCostCents != 500 {
		t.Fatalf("max_cost_cents not decoded: %+v", got.Conditions)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreGetNotFound(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT id, organization_id, name, effect, resource_type, actions, conditions,`).
		WithArgs("missing", "org-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetPolicy(context.Background(), "org-1", "missing"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("expected ErrPolicyNotFound, got %v", err)
	}
}

func TestPostgresStoreListTenantScoped(t *testing.T) {
	mock, store := newMockStore(t)
	mock.ExpectQuery(`SELECT id, organization_id, name, effect, resource_type, actions, conditions,`).
		WithArgs("org-1").
		WillReturnRows(policyRow("pol-1", 100).AddRow(
			"pol-2", "org-1", "Allow search", "allow", "tool",
			[]byte(`["tools.call"]`), []byte(`{"tool_allowlist":["search"]}`),
			10, true, time.Now().UTC(), time.Now().UTC(),
		))

	list, err := store.ListPolicies(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(list))
	}
	if list[0].ID != "pol-1" || list[0].Priority != 100 {
		t.Fatalf("ordering not preserved from query: %+v", list[0])
	}
}

func TestPostgresStoreUpdateAndDeleteGuards(t *testing.T) {
	mock, store := newMockStore(t)

	policy := &Policy{
		ID:             "pol-1",
		OrganizationID: "org-1",
		Name:           "Renamed",
		Effect:         EffectAllow,
		ResourceType:   ResourceWildcard,
		Actions:        []string{},
		Priority:       5,
		Enabled:        false,
		UpdatedAt:      time.Now().UTC(),
	}
	mock.ExpectExec(`UPDATE policies SET`).
		WithArgs("pol-1", "org-1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), 5, false, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdatePolicy(context.Background(), policy); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}

	// Zero rows affected -> tenant guard or unknown id -> ErrPolicyNotFound.
	mock.ExpectExec(`UPDATE policies SET`).
		WithArgs("pol-1", "org-9", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), 5, false, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	policy.OrganizationID = "org-9"
	if err := store.UpdatePolicy(context.Background(), policy); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("expected ErrPolicyNotFound from tenant guard, got %v", err)
	}

	mock.ExpectExec(`DELETE FROM policies`).
		WithArgs("pol-1", "org-9").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.DeletePolicy(context.Background(), "org-9", "pol-1"); err != nil {
		t.Fatalf("DeletePolicy: %v", err)
	}

	mock.ExpectExec(`DELETE FROM policies`).
		WithArgs("pol-1", "org-9").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeletePolicy(context.Background(), "org-9", "pol-1"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("expected ErrPolicyNotFound on delete, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreNilDBGuard(t *testing.T) {
	store := NewPostgresStore(nil)
	if err := store.CreatePolicy(context.Background(), &Policy{}); err == nil {
		t.Fatal("expected nil-db guard error")
	}
}
