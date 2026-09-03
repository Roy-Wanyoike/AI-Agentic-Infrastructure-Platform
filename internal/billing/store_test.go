package billing

// Postgres store tests (sqlmock). The SQL statements are pinned so the
// tenant guards (organization_id filters), the atomic invoice transaction and
// the four-statement rollover batch cannot silently regress. Mirrors the
// store-test conventions of internal/policies and internal/knowledge.

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

var (
	tsA = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	tsB = tsA.Add(30 * 24 * time.Hour)
)

func planRow(id string, metadata any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "name", "price_cents", "currency", "included_quota",
		"COALESCE(metadata::text, '')", "created_at", "updated_at",
	}).AddRow(id, "starter", 1900, "usd", 1000, metadata, tsA, tsA)
}

func subRow(id, orgID, status string, canceledAt any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "plan_id", "status", "period_start", "period_end",
		"cancel_at_period_end", "canceled_at", "created_at", "updated_at",
	}).AddRow(id, orgID, "plan-1", status, tsA, tsB, false, canceledAt, tsA, tsA)
}

func invoiceRow(id, orgID string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "subscription_id", "period_start", "period_end",
		"subtotal_cents", "currency", "status", "created_at", "updated_at",
	}).AddRow(id, orgID, "sub-1", tsA, tsB, 1200, "usd", "open", tsA, tsA)
}

func lineRow(id, invoiceID string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "invoice_id", "organization_id", "source", "description",
		"quantity", "amount_cents", "COALESCE(refs::text, '')", "created_at",
	}).AddRow(id, invoiceID, "org-1", "run", "run usage — model m", 3, 2, `{"model":"m"}`, tsA)
}

// ---------------------------------------------------------------------------
// nil-guard
// ---------------------------------------------------------------------------

func TestPostgresStoreNilDBGuard(t *testing.T) {
	store := NewPostgresStore(nil)
	ctx := context.Background()
	if err := store.CreatePlan(ctx, &Plan{}); err == nil {
		t.Fatal("CreatePlan on nil db should fail")
	}
	if _, err := store.GetPlan(ctx, "plan-1"); err == nil {
		t.Fatal("GetPlan on nil db should fail")
	}
	if _, err := store.ListInvoicesByOrg(ctx, "org-1"); err == nil {
		t.Fatal("ListInvoicesByOrg on nil db should fail")
	}
	if _, err := store.RolloverDueSubscriptions(ctx, tsA, 30); err == nil {
		t.Fatal("RolloverDueSubscriptions on nil db should fail")
	}
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

func TestPostgresStorePlanCRUD(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	plan := &Plan{
		ID: "plan-1", Name: "starter", PriceCents: 1900, Currency: "usd",
		IncludedQuota: 1000, Metadata: map[string]any{"overage_run_rate_cents": 2},
		CreatedAt: tsA, UpdatedAt: tsA,
	}
	mock.ExpectExec(`INSERT INTO plans`).
		WithArgs("plan-1", "starter", int64(1900), "usd", int64(1000), `{"overage_run_rate_cents":2}`, tsA, tsA).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("CreatePlan returned error: %v", err)
	}

	// List: metadata NULL arrives as '' via COALESCE.
	mock.ExpectQuery(`SELECT id, name, price_cents, currency, included_quota, COALESCE\(metadata::text, ''\), created_at, updated_at FROM plans ORDER BY created_at ASC, id ASC`).
		WillReturnRows(planRow("plan-1", ""))
	plans, err := store.ListPlans(ctx)
	if err != nil {
		t.Fatalf("ListPlans returned error: %v", err)
	}
	if len(plans) != 1 || plans[0].ID != "plan-1" || plans[0].Metadata != nil {
		t.Fatalf("unexpected list: %+v", plans)
	}

	// Get: metadata JSON decodes back into the map.
	mock.ExpectQuery(`SELECT id, name, price_cents, currency, included_quota, COALESCE\(metadata::text, ''\), created_at, updated_at FROM plans WHERE id = \$1`).
		WithArgs("plan-1").
		WillReturnRows(planRow("plan-1", `{"overage_run_rate_cents":2}`))
	got, err := store.GetPlan(ctx, "plan-1")
	if err != nil {
		t.Fatalf("GetPlan returned error: %v", err)
	}
	if got.Metadata["overage_run_rate_cents"] != float64(2) {
		t.Fatalf("metadata not decoded: %+v", got.Metadata)
	}

	// Get unknown -> ErrPlanNotFound (sql.ErrNoRows mapping).
	mock.ExpectQuery(`SELECT .* FROM plans WHERE id = \$1`).WillReturnError(sql.ErrNoRows)
	if _, err := store.GetPlan(ctx, "plan-x"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("expected ErrPlanNotFound, got %v", err)
	}

	// Update: 0 rows affected -> ErrPlanNotFound.
	mock.ExpectExec(`UPDATE plans SET`).
		WithArgs("plan-1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdatePlan(ctx, plan); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("expected ErrPlanNotFound, got %v", err)
	}

	// Delete guarded: 0 rows (referenced or unknown) -> ErrPlanInUse.
	mock.ExpectExec(`DELETE FROM plans WHERE id = \$1 AND NOT EXISTS`).
		WithArgs("plan-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeletePlan(ctx, "plan-1"); !errors.Is(err, ErrPlanInUse) {
		t.Fatalf("expected ErrPlanInUse, got %v", err)
	}
	// Delete freed: 1 row affected.
	mock.ExpectExec(`DELETE FROM plans WHERE id = \$1 AND NOT EXISTS`).
		WithArgs("plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.DeletePlan(ctx, "plan-1"); err != nil {
		t.Fatalf("DeletePlan returned error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

func TestPostgresStoreSubscriptionPersistence(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	sub := &Subscription{
		ID: "sub-1", OrganizationID: "org-1", PlanID: "plan-1",
		Status: StatusTrial, PeriodStart: tsA, PeriodEnd: tsB,
		CreatedAt: tsA, UpdatedAt: tsA,
	}
	mock.ExpectExec(`INSERT INTO subscriptions`).
		WithArgs("sub-1", "org-1", "plan-1", "trial", tsA, tsB, false, nil, tsA, tsA).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("CreateSubscription returned error: %v", err)
	}

	// List: newest first is the store's contract; canceled_at NULL scans.
	mock.ExpectQuery(`SELECT .* FROM subscriptions WHERE organization_id = \$1 ORDER BY created_at DESC, id DESC`).
		WithArgs("org-1").
		WillReturnRows(subRow("sub-2", "org-1", StatusActive, nil).
			AddRow("sub-1", "org-1", "plan-1", StatusCanceled, tsA, tsB, false, tsB, tsA, tsA))
	subs, err := store.ListSubscriptionsByOrg(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListSubscriptionsByOrg returned error: %v", err)
	}
	if len(subs) != 2 || subs[0].ID != "sub-2" || subs[1].ID != "sub-1" {
		t.Fatalf("unexpected order: %+v", subs)
	}
	if subs[0].CanceledAt != nil {
		t.Fatal("NULL canceled_at should scan as nil")
	}
	if subs[1].CanceledAt == nil || !subs[1].CanceledAt.Equal(tsB) {
		t.Fatalf("canceled_at not decoded: %+v", subs[1].CanceledAt)
	}
	// Tenant guard is in the SQL predicate (checked via the pinned regex above).

	// Update: 0 rows affected -> ErrNoSubscription.
	mock.ExpectExec(`UPDATE subscriptions SET`).
		WithArgs("sub-1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateSubscription(ctx, sub); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("expected ErrNoSubscription, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreRolloverBatch(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	// One transaction, the four transition statements in the fixed order
	// (trials, flagged cancels, expired past_due, renewals), affected rows
	// summed.
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE subscriptions SET status = 'past_due', period_start = period_end`).
		WithArgs(tsA, 30).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE subscriptions SET status = 'canceled', canceled_at = \$1, cancel_at_period_end = FALSE`).
		WithArgs(tsA).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE subscriptions SET status = 'canceled', canceled_at = \$1, updated_at = \$1 WHERE status = 'past_due'`).
		WithArgs(tsA).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE subscriptions SET period_start = period_end, period_end = period_end \+ make_interval`).
		WithArgs(tsA, 30).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()
	changed, err := store.RolloverDueSubscriptions(ctx, tsA, 30)
	if err != nil {
		t.Fatalf("RolloverDueSubscriptions returned error: %v", err)
	}
	if changed != 6 {
		t.Fatalf("expected 6 affected rows, got %d", changed)
	}

	// A failing statement rolls the whole batch back.
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE subscriptions SET status = 'past_due'`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()
	if _, err := store.RolloverDueSubscriptions(ctx, tsA, 30); err == nil {
		t.Fatal("expected the statement error to propagate")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Invoices
// ---------------------------------------------------------------------------

func TestPostgresStoreCreateInvoiceAtomic(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	inv := &Invoice{
		ID: "inv-1", OrganizationID: "org-1", SubscriptionID: "sub-1",
		PeriodStart: tsA, PeriodEnd: tsB, SubtotalCents: 8, Currency: "usd",
		Status: InvoiceOpen, CreatedAt: tsA, UpdatedAt: tsA,
		Lines: []*InvoiceLine{
			{ID: "line-1", InvoiceID: "inv-1", OrganizationID: "org-1", Source: LineSourceRun,
				Description: "run usage", Quantity: 3, AmountCents: 2,
				Refs: map[string]any{"model": "m"}, CreatedAt: tsA},
			{ID: "line-2", InvoiceID: "inv-1", OrganizationID: "org-1", Source: LineSourceOverage,
				Description: "overage", Quantity: 3, AmountCents: 6, CreatedAt: tsA},
		},
	}
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO invoices`).
		WithArgs("inv-1", "org-1", "sub-1", tsA, tsB, int64(8), "usd", "open", tsA, tsA).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO invoice_lines`).
		WithArgs("line-1", "inv-1", "org-1", "run", "run usage", int64(3), int64(2), `{"model":"m"}`, tsA).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO invoice_lines`).
		WithArgs("line-2", "inv-1", "org-1", "overage", "overage", int64(3), int64(6), nil, tsA).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := store.CreateInvoice(ctx, inv); err != nil {
		t.Fatalf("CreateInvoice returned error: %v", err)
	}

	// A failing line rolls back the invoice header (atomicity).
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO invoices`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO invoice_lines`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()
	if err := store.CreateInvoice(ctx, inv); err == nil {
		t.Fatal("expected the line error to abort CreateInvoice")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreInvoiceReads(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	// Tenant-scoped single read: WHERE id = $1 AND organization_id = $2.
	mock.ExpectQuery(`SELECT .* FROM invoices WHERE id = \$1 AND organization_id = \$2`).
		WithArgs("inv-1", "org-1").
		WillReturnRows(invoiceRow("inv-1", "org-1"))
	got, err := store.GetInvoice(ctx, "org-1", "inv-1")
	if err != nil {
		t.Fatalf("GetInvoice returned error: %v", err)
	}
	if got.ID != "inv-1" || got.SubtotalCents != 1200 || got.Status != InvoiceOpen {
		t.Fatalf("unexpected invoice: %+v", got)
	}

	// Unknown/foreign invoice -> ErrInvoiceNotFound.
	mock.ExpectQuery(`SELECT .* FROM invoices WHERE id = \$1 AND organization_id = \$2`).
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetInvoice(ctx, "org-1", "inv-x"); !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("expected ErrInvoiceNotFound, got %v", err)
	}

	// Org listing.
	mock.ExpectQuery(`SELECT .* FROM invoices WHERE organization_id = \$1 ORDER BY created_at DESC, id DESC`).
		WithArgs("org-1").
		WillReturnRows(invoiceRow("inv-2", "org-1").
			AddRow("inv-1", "org-1", "sub-1", tsA, tsB, 1200, "usd", "open", tsA, tsA))
	list, err := store.ListInvoicesByOrg(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListInvoicesByOrg returned error: %v", err)
	}
	if len(list) != 2 || list[0].ID != "inv-2" {
		t.Fatalf("unexpected list: %+v", list)
	}

	// Idempotency lookup by exact period (non-void only).
	mock.ExpectQuery(`SELECT .* FROM invoices WHERE organization_id = \$1 AND period_start = \$2 AND period_end = \$3 AND status <> 'void'`).
		WithArgs("org-1", tsA, tsB).
		WillReturnRows(invoiceRow("inv-1", "org-1"))
	found, err := store.FindInvoiceByPeriod(ctx, "org-1", tsA, tsB)
	if err != nil || found.ID != "inv-1" {
		t.Fatalf("FindInvoiceByPeriod: inv=%+v err=%v", found, err)
	}
	mock.ExpectQuery(`SELECT .* FROM invoices WHERE organization_id = \$1 AND period_start = \$2`).
		WillReturnError(sql.ErrNoRows)
	if _, err := store.FindInvoiceByPeriod(ctx, "org-1", tsA, tsB); !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("expected ErrInvoiceNotFound, got %v", err)
	}

	// Lines: refs JSON decodes back into the map.
	mock.ExpectQuery(`SELECT .* FROM invoice_lines WHERE invoice_id = \$1 ORDER BY created_at ASC, id ASC`).
		WithArgs("inv-1").
		WillReturnRows(lineRow("line-1", "inv-1"))
	lines, err := store.GetInvoiceLines(ctx, "inv-1")
	if err != nil {
		t.Fatalf("GetInvoiceLines returned error: %v", err)
	}
	if len(lines) != 1 || lines[0].Refs["model"] != "m" || lines[0].Quantity != 3 || lines[0].AmountCents != 2 {
		t.Fatalf("unexpected lines: %+v", lines)
	}

	// Status flip is tenant-scoped; 0 rows -> ErrInvoiceNotFound.
	mock.ExpectExec(`UPDATE invoices SET status = \$2, updated_at = \$3 WHERE id = \$1 AND organization_id = \$4`).
		WithArgs("inv-1", "paid", tsB, "org-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateInvoiceStatus(ctx, "org-1", "inv-1", InvoicePaid, tsB); !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("expected ErrInvoiceNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreUpdateInvoiceStatusScoped(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()
	// The org guard is part of the WHERE clause: args carry (id, status, updatedAt, orgID).
	mock.ExpectExec(`UPDATE invoices SET status = \$2, updated_at = \$3 WHERE id = \$1 AND organization_id = \$4`).
		WithArgs("inv-1", "void", tsB, "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateInvoiceStatus(ctx, "org-1", "inv-1", InvoiceVoid, tsB); err != nil {
		t.Fatalf("UpdateInvoiceStatus returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
