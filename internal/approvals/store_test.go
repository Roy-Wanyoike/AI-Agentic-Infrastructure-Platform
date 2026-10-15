package approvals

// store_test.go — sqlmock coverage for the Postgres store's issue #137
// oversight methods (delegation chain, escalation policy, oversight columns,
// approval_events evidence log) plus the extended base-row round-trip. The
// SQL contract (organization_id tenant guard, COALESCE scans, canonical
// ordering, upsert semantics) is pinned without a live database, following
// the internal/audit store_test.go convention.

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newApprovalMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func oversights(t *testing.T, store Store) OversightStore {
	t.Helper()
	o, ok := store.(OversightStore)
	if !ok {
		t.Fatal("the Postgres store must implement OversightStore (issue #137)")
	}
	return o
}

func TestPgStoreCreateDelegationPersistsChainRow(t *testing.T) {
	db, mock := newApprovalMockDB(t)
	store := oversights(t, NewPostgresStore(db))
	ctx := context.Background()

	expires := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertDelegation)).
		WithArgs("del-1", "org-1", "ap-1", "user-1", "user-2", "please handle", sqlmock.AnyArg(), expires).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateDelegation(ctx, "org-1", &Delegation{
		ID: "del-1", ApprovalID: "ap-1", FromUser: "user-1", ToUser: "user-2",
		Note: "please handle", CreatedAt: time.Now().UTC(), ExpiresAt: &expires,
	}); err != nil {
		t.Fatalf("CreateDelegation: %v", err)
	}

	// Open-ended delegation: nil expiry persists as SQL NULL.
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertDelegation)).
		WithArgs("del-2", "org-1", "ap-1", "user-1", "user-3", "", sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateDelegation(ctx, "org-1", &Delegation{
		ID: "del-2", ApprovalID: "ap-1", FromUser: "user-1", ToUser: "user-3",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateDelegation (open-ended): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL contract violated: %v", err)
	}
}

func TestPgStoreListDelegationsScansChainOrdered(t *testing.T) {
	db, mock := newApprovalMockDB(t)
	store := oversights(t, NewPostgresStore(db))
	ctx := context.Background()

	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	expires := base.Add(time.Hour)
	rows := sqlmock.NewRows([]string{"id", "organization_id", "approval_id", "from_user", "to_user", "note", "created_at", "expires_at"}).
		AddRow("del-1", "org-1", "ap-1", "user-1", "user-2", "first hop", base, expires).
		AddRow("del-2", "org-1", "ap-1", "user-2", "user-3", "", base.Add(time.Minute), nil)
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectDelegations)).
		WithArgs("org-1", "ap-1").
		WillReturnRows(rows)

	chain, err := store.ListDelegations(ctx, "org-1", "ap-1")
	if err != nil {
		t.Fatalf("ListDelegations: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("chain length = %d, want 2", len(chain))
	}
	if chain[0].ToUser != "user-2" || chain[0].ExpiresAt == nil || !chain[0].ExpiresAt.Equal(expires) {
		t.Fatalf("first hop malformed: %+v", chain[0])
	}
	if chain[1].ToUser != "user-3" || chain[1].ExpiresAt != nil || chain[1].Note != "" {
		t.Fatalf("second hop malformed: %+v", chain[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL contract violated: %v", err)
	}
}

func TestPgStoreUpsertAndReadEscalationPolicy(t *testing.T) {
	db, mock := newApprovalMockDB(t)
	store := oversights(t, NewPostgresStore(db))
	ctx := context.Background()

	updated := time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(sqlUpsertEscalationPolicy)).
		WithArgs("org-1", StatusPending, 45, "admin-1", updated).
		WillReturnResult(sqlmock.NewResult(0, 1))
	policy := &EscalationPolicy{OrganizationID: "org-1", TargetStatus: StatusPending, SLAMinutes: 45, UpdatedBy: "admin-1", UpdatedAt: updated}
	if err := store.UpsertEscalationPolicy(ctx, "org-1", policy); err != nil {
		t.Fatalf("UpsertEscalationPolicy: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectEscalationPolicy)).
		WithArgs("org-1").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "target_status", "sla_minutes", "updated_by", "updated_at"}).
			AddRow("org-1", StatusPending, 45, "admin-1", updated))
	got, err := store.GetEscalationPolicy(ctx, "org-1")
	if err != nil {
		t.Fatalf("GetEscalationPolicy: %v", err)
	}
	if got == nil || got.SLAMinutes != 45 || got.TargetStatus != StatusPending || got.UpdatedBy != "admin-1" {
		t.Fatalf("policy round-trip malformed: %+v", got)
	}

	// Unset policy is (nil, nil), not an error — the disabled default.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectEscalationPolicy)).
		WithArgs("org-9").
		WillReturnError(sql.ErrNoRows)
	got, err = store.GetEscalationPolicy(ctx, "org-9")
	if err != nil || got != nil {
		t.Fatalf("unset policy should be (nil, nil), got (%v, %v)", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL contract violated: %v", err)
	}
}

func TestPgStoreUpdateApprovalOversightTenantGuard(t *testing.T) {
	db, mock := newApprovalMockDB(t)
	store := oversights(t, NewPostgresStore(db))
	ctx := context.Background()

	firstAt := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	escAt := firstAt.Add(time.Hour)
	deadline := firstAt.Add(30 * time.Minute)
	a := &Approval{
		ID: "ap-1", OrganizationID: "org-1",
		FirstApprover: "user-1", FirstDecision: StatusApproved, FirstDecidedAt: &firstAt,
		DelegatedTo: "user-2", DelegatedBy: "user-1", DelegationNote: "hop",
		EscalatedAt: &escAt, EscalationReason: EscalationReasonSLABreach, EscalationDeadline: &deadline,
	}
	mock.ExpectExec(regexp.QuoteMeta(sqlUpdateApprovalOversight)).
		WithArgs("user-1", StatusApproved, firstAt, "user-2", "user-1", "hop", nil, escAt, EscalationReasonSLABreach, deadline, "ap-1", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateApprovalOversight(ctx, "org-1", "ap-1", a); err != nil {
		t.Fatalf("UpdateApprovalOversight: %v", err)
	}

	// Tenant guard: zero rows affected surfaces ErrApprovalNotFound.
	mock.ExpectExec(regexp.QuoteMeta(sqlUpdateApprovalOversight)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "ap-1", "org-2").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateApprovalOversight(ctx, "org-2", "ap-1", a); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("cross-tenant oversight update should be ErrApprovalNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL contract violated: %v", err)
	}
}

func TestPgStoreInsertApprovalEventEmptyDetailBecomesEmptyObject(t *testing.T) {
	db, mock := newApprovalMockDB(t)
	store := oversights(t, NewPostgresStore(db))
	ctx := context.Background()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertApprovalEvent)).
		WithArgs("ev-1", "org-1", "ap-1", EventEscalated, "admin-1", `{"reason":"sla_breach"}`, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.InsertApprovalEvent(ctx, "org-1", &Event{
		ID: "ev-1", ApprovalID: "ap-1", Type: EventEscalated, Actor: "admin-1",
		Detail: map[string]any{"reason": "sla_breach"}, CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertApprovalEvent: %v", err)
	}

	// nil detail persists as the empty JSON object (COALESCE-safe round trip).
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertApprovalEvent)).
		WithArgs("ev-2", "org-1", "ap-1", EventRequested, "user-1", "{}", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.InsertApprovalEvent(ctx, "org-1", &Event{
		ID: "ev-2", ApprovalID: "ap-1", Type: EventRequested, Actor: "user-1", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertApprovalEvent (nil detail): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL contract violated: %v", err)
	}
}

func TestPgStoreListApprovalEventsWindowAndScan(t *testing.T) {
	db, mock := newApprovalMockDB(t)
	store := oversights(t, NewPostgresStore(db))
	ctx := context.Background()

	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	rows := sqlmock.NewRows([]string{"id", "organization_id", "approval_id", "type", "actor", "detail", "created_at"}).
		AddRow("ev-1", "org-1", "ap-1", EventRequested, "user-1", `{"risk":"medium"}`, from.Add(time.Minute)).
		AddRow("ev-2", "org-1", "ap-1", EventDelegated, "user-1", `{}`, from.Add(2*time.Minute))
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectApprovalEvents)).
		WithArgs("org-1", from, to).
		WillReturnRows(rows)

	events, err := store.ListApprovalEvents(ctx, "org-1", from, to)
	if err != nil {
		t.Fatalf("ListApprovalEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Type != EventRequested || events[0].Detail["risk"] != "medium" {
		t.Fatalf("first event malformed: %+v", events[0])
	}
	if events[1].Type != EventDelegated || events[1].Detail == nil || len(events[1].Detail) != 0 {
		t.Fatalf("second event malformed: %+v", events[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL contract violated: %v", err)
	}
}

func TestPgStoreGetApprovalOversightColumnsRoundTrip(t *testing.T) {
	db, mock := newApprovalMockDB(t)
	store := NewPostgresStore(db)
	ctx := context.Background()

	created := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	firstAt := created.Add(time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectApprovalScoped)).
		WithArgs("ap-1", "org-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "organization_id", "run_id", "workflow_run_id", "resource", "action", "reason", "risk",
			"status", "requester", "approver", "decision_reason", "created_at", "decided_at",
			"require_dual", "first_approver", "first_decision", "first_decided_at",
			"delegated_to", "delegated_by", "delegation_note", "delegation_expires_at",
			"escalated_at", "escalation_reason", "escalation_deadline",
		}).AddRow("ap-1", "org-1", "", "", "resource-x", "deploy", "needs sign-off", "high",
			StatusPartiallyApproved, "user-1", "", "", created, nil,
			true, "user-2", StatusApproved, firstAt,
			"user-3", "user-1", "hop", created.Add(2*time.Minute),
			nil, "", nil))

	a, err := store.GetApproval(ctx, "org-1", "ap-1")
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if !a.RequireDual || a.Status != StatusPartiallyApproved {
		t.Fatalf("base columns malformed: %#v", a)
	}
	if a.FirstApprover != "user-2" || a.FirstDecision != StatusApproved || a.FirstDecidedAt == nil {
		t.Fatalf("dual-control columns malformed: %#v", a)
	}
	if a.DelegatedTo != "user-3" || a.DelegatedBy != "user-1" || a.DelegationNote != "hop" || a.DelegationExpiresAt == nil {
		t.Fatalf("delegation columns malformed: %#v", a)
	}
	if a.EscalatedAt != nil || a.EscalationReason != "" || a.EscalationDeadline != nil {
		t.Fatalf("escalation columns malformed: %#v", a)
	}

	// Unknown / cross-tenant row -> ErrApprovalNotFound.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectApprovalScoped)).
		WithArgs("ap-2", "org-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetApproval(ctx, "org-1", "ap-2"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("missing approval should be ErrApprovalNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL contract violated: %v", err)
	}
}

func TestPgStoreCreateApprovalCarriesRequireDual(t *testing.T) {
	db, mock := newApprovalMockDB(t)
	store := NewPostgresStore(db)
	ctx := context.Background()

	now := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertApproval)).
		WithArgs("ap-dual", "org-1", nil, nil, "", "deploy", "", "medium", StatusPending, "user-1", "", "", now, nil, true).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateApproval(ctx, "org-1", &Approval{
		ID: "ap-dual", OrganizationID: "org-1", Action: "deploy", Risk: RiskMedium,
		Status: StatusPending, Requester: "user-1", CreatedAt: now, RequireDual: true,
	}); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL contract violated: %v", err)
	}
}

// TestServiceWithStoreDelegatePersistsOversight proves the dual-mode wiring:
// NewServiceWithStore + a store that implements OversightStore persists the
// delegation chain, the live assignment columns and (best-effort) the event
// rows — the in-memory map stays a write-through cache.
func TestServiceWithStoreDelegatePersistsOversight(t *testing.T) {
	db, mock := newApprovalMockDB(t)
	svc := NewServiceWithStore(NewPostgresStore(db))
	ctx := context.Background()

	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	// Request -> CreateApproval (+ best-effort requested event insert).
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertApproval)).
		WithArgs(sqlmock.AnyArg(), "org-1", nil, nil, "", "deploy", "", "medium", StatusPending, "user-1", "", "", sqlmock.AnyArg(), nil, false).
		WillReturnResult(sqlmock.NewResult(1, 1))
	a, err := svc.Request(ctx, "org-1", RequestInput{Action: "deploy", Requester: "user-1"})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Delegate -> GetApproval (lazy pass) + CreateDelegation +
	// UpdateApprovalOversight (+ best-effort delegated event insert).
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectApprovalScoped)).
		WithArgs(a.ID, "org-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "organization_id", "run_id", "workflow_run_id", "resource", "action", "reason", "risk",
			"status", "requester", "approver", "decision_reason", "created_at", "decided_at",
			"require_dual", "first_approver", "first_decision", "first_decided_at",
			"delegated_to", "delegated_by", "delegation_note", "delegation_expires_at",
			"escalated_at", "escalation_reason", "escalation_deadline",
		}).AddRow(a.ID, "org-1", "", "", "", "deploy", "", "medium",
			StatusPending, "user-1", "", "", now, nil,
			false, "", "", nil,
			"", "", "", nil,
			nil, "", nil))
	expires := now.Add(time.Hour)
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertDelegation)).
		WithArgs(sqlmock.AnyArg(), "org-1", a.ID, "user-9", "user-2", "take this", sqlmock.AnyArg(), expires).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(sqlUpdateApprovalOversight)).
		WithArgs("", "", nil, "user-2", "user-9", "take this", expires, nil, "", nil, a.ID, "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	approval, delegation, err := svc.Delegate(ctx, "org-1", a.ID, "user-2", "take this", &expires, "user-9")
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if approval.DelegatedTo != "user-2" || delegation.ApprovalID != a.ID || delegation.ToUser != "user-2" {
		t.Fatalf("delegation result malformed: approval=%+v delegation=%+v", approval, delegation)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL contract violated: %v", err)
	}
}
