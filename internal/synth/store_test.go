package synth

// store_test.go — issue #147 Postgres store over the migration 034 tables,
// exercised with sqlmock (internal/policies store_test.go precedent, exact-
// SQL matching via QueryMatcherEqual so the org-guard predicates are pinned
// verbatim). Covers: JSONB round-trips (allowed_domains, quiet/test
// windows, role_hints, constraints), verified_at NULL handling, the
// tenant guards (org-scoped SELECT/UPDATE/DELETE; zero-affected-rows ->
// synth.ErrNotFound), sql.ErrNoRows mapping and error propagation.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// newMockStore opens a sqlmock-backed store. Exact-SQL matching is used so
// the tests pin the org-guard predicates verbatim.
func newMockStore(t *testing.T) (sqlmock.Sqlmock, Store) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return mock, NewPostgresStore(db)
}

func mustSucceed(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

var testNow = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

// --- projects ---------------------------------------------------------------

func TestPostgresStoreProjectCRUD(t *testing.T) {
	ctx := context.Background()
	mock, store := newMockStore(t)
	p := &Project{ID: "p1", OrgID: "org-a", Name: "checkout", CreatedAt: testNow, UpdatedAt: testNow}

	mock.ExpectExec(sqlInsertProject).
		WithArgs("p1", "org-a", "checkout", testNow, testNow).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	projectRows := func(orgID string) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "org_id", "name", "created_at", "updated_at"}).
			AddRow("p1", orgID, "checkout", testNow, testNow)
	}

	// The tenant guard: the SELECT carries BOTH id and org_id.
	mock.ExpectQuery(sqlSelectProject).WithArgs("p1", "org-a").WillReturnRows(projectRows("org-a"))
	got, err := store.GetProject(ctx, "org-a", "p1")
	if err != nil || got.Name != "checkout" {
		t.Fatalf("GetProject: %v %+v", err, got)
	}

	// Foreign-org lookup -> no row -> contract ErrNotFound.
	mock.ExpectQuery(sqlSelectProject).WithArgs("p1", "org-b").WillReturnError(sql.ErrNoRows)
	if _, err := store.GetProject(ctx, "org-b", "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org GetProject err = %v, want ErrNotFound", err)
	}

	dupRows := projectRows("org-a")
	dupRows.AddRow("p2", "org-a", "billing", testNow, testNow)
	mock.ExpectQuery(sqlSelectProjectsByOrg).WithArgs("org-a").WillReturnRows(dupRows)
	list, err := store.ListProjects(ctx, "org-a")
	if err != nil || len(list) != 2 {
		t.Fatalf("ListProjects: %v len=%d", err, len(list))
	}

	mock.ExpectExec(sqlUpdateProject).WithArgs("p1", "org-a", "checkout-v2", testNow).
		WillReturnResult(sqlmock.NewResult(0, 1))
	p.Name = "checkout-v2"
	if err := store.UpdateProject(ctx, p); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	// Zero affected rows (foreign org) -> ErrNotFound.
	mock.ExpectExec(sqlUpdateProject).WithArgs("p1", "org-b", "hijack", testNow).
		WillReturnResult(sqlmock.NewResult(0, 0))
	p.Name = "hijack"
	if err := store.UpdateProject(ctx, &Project{ID: "p1", OrgID: "org-b", Name: "hijack", UpdatedAt: testNow}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org UpdateProject err = %v, want ErrNotFound", err)
	}

	mock.ExpectExec(sqlDeleteProject).WithArgs("p1", "org-b").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeleteProject(ctx, "org-b", "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org DeleteProject err = %v, want ErrNotFound", err)
	}
	mustSucceed(t, mock)
}

// --- targets ----------------------------------------------------------------

func TestPostgresStoreTargetCRUDAndVerifiedAt(t *testing.T) {
	ctx := context.Background()
	mock, store := newMockStore(t)
	verifiedAt := testNow.Add(time.Hour)

	// Insert carries the verification columns verbatim.
	mock.ExpectExec(sqlInsertTarget).
		WithArgs("t1", "org-a", "p1", "app", "https://app.example.com", false, nil, "tok-1", testNow, testNow).
		WillReturnResult(sqlmock.NewResult(1, 1))
	tg := &Target{ID: "t1", OrgID: "org-a", ProjectID: "p1", Name: "app", BaseURL: "https://app.example.com",
		Verified: false, VerifiedAt: nil, VerifyToken: "tok-1", CreatedAt: testNow, UpdatedAt: testNow}
	if err := store.CreateTarget(ctx, tg); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	// NULL verified_at decodes as nil pointer (no synthetic zero-times).
	nullRows := sqlmock.NewRows([]string{"id", "org_id", "project_id", "name", "base_url", "verified", "verified_at", "verify_token", "created_at", "updated_at"}).
		AddRow("t1", "org-a", "p1", "app", "https://app.example.com", false, nil, "tok-1", testNow, testNow)
	mock.ExpectQuery(sqlSelectTarget).WithArgs("t1", "org-a").WillReturnRows(nullRows)
	got, err := store.GetTarget(ctx, "org-a", "t1")
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	if got.Verified || got.VerifiedAt != nil || got.VerifyToken != "tok-1" {
		t.Fatalf("unverified decode wrong: %+v", got)
	}

	// Set verified_at round-trips. Row builders return FRESH rows per
	// expectation (sqlmock rows are single-consumption).
	verifiedRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "org_id", "project_id", "name", "base_url", "verified", "verified_at", "verify_token", "created_at", "updated_at"}).
			AddRow("t1", "org-a", "p1", "app", "https://app.example.com", true, verifiedAt, "", testNow, testNow)
	}
	mock.ExpectQuery(sqlSelectTarget).WithArgs("t1", "org-a").WillReturnRows(verifiedRows())
	got, err = store.GetTarget(ctx, "org-a", "t1")
	if err != nil {
		t.Fatalf("GetTarget(verified): %v", err)
	}
	if !got.Verified || got.VerifiedAt == nil || !got.VerifiedAt.Equal(verifiedAt) {
		t.Fatalf("verified decode wrong: %+v (want %v)", got.VerifiedAt, verifiedAt)
	}

	// The verify-flow UPDATE writes verified/verified_at/token together and
	// stays org-guarded.
	mock.ExpectExec(sqlUpdateTarget).
		WithArgs("t1", "org-a", "app", true, verifiedAt, "", testNow.Add(time.Minute)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	updated := &Target{ID: "t1", OrgID: "org-a", Name: "app", Verified: true, VerifiedAt: &verifiedAt, VerifyToken: "", UpdatedAt: testNow.Add(time.Minute)}
	if err := store.UpdateTarget(ctx, updated); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}

	mock.ExpectQuery(sqlSelectTargetsByOrg).WithArgs("org-a", "p1").
		WillReturnRows(verifiedRows())
	list, err := store.ListTargets(ctx, "org-a", "p1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTargets: %v len=%d", err, len(list))
	}

	mock.ExpectExec(sqlDeleteTarget).WithArgs("t1", "org-b").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeleteTarget(ctx, "org-b", "t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org DeleteTarget err = %v, want ErrNotFound", err)
	}
	mustSucceed(t, mock)
}

// --- environments (JSONB) ---------------------------------------------------

func TestPostgresStoreEnvironmentJSONBRoundtrip(t *testing.T) {
	ctx := context.Background()
	mock, store := newMockStore(t)
	e := &Environment{
		ID: "e1", OrgID: "org-a", TargetID: "t1",
		Kind: EnvProduction, Mode: ModeProductionReadOnly,
		AllowedDomains: []string{"app.example.com", "cdn.example.com"},
		RateCapPerMin:  0, BudgetPerRun: 200,
		QuietWindows: []QuietWindow{{Weekdays: []int{0, 6}, Start: "00:00", End: "23:59", Location: "Europe/Berlin", Reason: "weekend"}},
		TestWindows:  []QuietWindow{{Weekdays: []int{1, 2, 3, 4, 5}, Start: "22:00", End: "04:00", Location: "UTC"}},
		CreatedAt:    testNow, UpdatedAt: testNow,
	}

	// The JSONB parameters are pinned as exact JSON documents.
	mock.ExpectExec(sqlInsertEnvironment).
		WithArgs("e1", "org-a", "t1", "production", "production_read_only",
			`["app.example.com","cdn.example.com"]`, 0, 200,
			`[{"Weekdays":[0,6],"Start":"00:00","End":"23:59","Location":"Europe/Berlin","Reason":"weekend"}]`,
			`[{"Weekdays":[1,2,3,4,5],"Start":"22:00","End":"04:00","Location":"UTC","Reason":""}]`,
			testNow, testNow).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateEnvironment(ctx, e); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	envRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "org_id", "target_id", "kind", "mode", "allowed_domains", "rate_cap_per_min", "budget_per_run", "quiet_windows", "test_windows", "created_at", "updated_at"}).
			AddRow("e1", "org-a", "t1", "production", "production_read_only",
				[]byte(`["app.example.com","cdn.example.com"]`), 0, 200,
				[]byte(`[{"Weekdays":[0,6],"Start":"00:00","End":"23:59","Location":"Europe/Berlin","Reason":"weekend"}]`),
				[]byte(`[{"Weekdays":[1,2,3,4,5],"Start":"22:00","End":"04:00","Location":"UTC","Reason":""}]`),
				testNow, testNow)
	}
	mock.ExpectQuery(sqlSelectEnvironment).WithArgs("e1", "org-a").WillReturnRows(envRows())
	got, err := store.GetEnvironment(ctx, "org-a", "e1")
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if got.Kind != EnvProduction || got.Mode != ModeProductionReadOnly || got.BudgetPerRun != 200 {
		t.Fatalf("scalar columns wrong: %+v", got)
	}
	if len(got.AllowedDomains) != 2 || got.AllowedDomains[0] != "app.example.com" {
		t.Fatalf("allowed_domains not decoded: %v", got.AllowedDomains)
	}
	if len(got.QuietWindows) != 1 || got.QuietWindows[0].Location != "Europe/Berlin" || len(got.QuietWindows[0].Weekdays) != 2 {
		t.Fatalf("quiet_windows not decoded: %+v", got.QuietWindows)
	}
	if len(got.TestWindows) != 1 || got.TestWindows[0].End != "04:00" {
		t.Fatalf("test_windows not decoded: %+v", got.TestWindows)
	}

	// SQL NULL JSONB columns decode as empty (never nil-broken).
	nullEnvRows := sqlmock.NewRows([]string{"id", "org_id", "target_id", "kind", "mode", "allowed_domains", "rate_cap_per_min", "budget_per_run", "quiet_windows", "test_windows", "created_at", "updated_at"}).
		AddRow("e2", "org-a", "t1", "dev", "pre_production", nil, 0, 0, nil, nil, testNow, testNow)
	mock.ExpectQuery(sqlSelectEnvironment).WithArgs("e2", "org-a").WillReturnRows(nullEnvRows)
	got, err = store.GetEnvironment(ctx, "org-a", "e2")
	if err != nil {
		t.Fatalf("GetEnvironment(null jsonb): %v", err)
	}
	if len(got.AllowedDomains) != 0 || len(got.QuietWindows) != 0 || len(got.TestWindows) != 0 {
		t.Fatalf("null jsonb decode wrong: %+v", got)
	}

	// The ($2 = '' OR target_id = $2) list predicate is pinned verbatim.
	mock.ExpectQuery(sqlSelectEnvironmentsByOrg).WithArgs("org-a", "").
		WillReturnRows(envRows())
	if list, err := store.ListEnvironments(ctx, "org-a", ""); err != nil || len(list) != 1 {
		t.Fatalf("ListEnvironments: %v len=%d", err, len(list))
	}

	mock.ExpectExec(sqlUpdateEnvironment).
		WithArgs("e1", "org-a", "production", "production_read_only",
			`["app.example.com"]`, 0, 100, `[]`, `[]`, testNow).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.BudgetPerRun, e.AllowedDomains, e.QuietWindows, e.TestWindows = 100, []string{"app.example.com"}, nil, nil
	if err := store.UpdateEnvironment(ctx, e); err != nil {
		t.Fatalf("UpdateEnvironment: %v", err)
	}

	mock.ExpectExec(sqlDeleteEnvironment).WithArgs("e1", "org-b").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeleteEnvironment(ctx, "org-b", "e1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org DeleteEnvironment err = %v, want ErrNotFound", err)
	}
	mustSucceed(t, mock)
}

// --- personas (JSONB) -------------------------------------------------------

func TestPostgresStorePersonaJSONBRoundtrip(t *testing.T) {
	ctx := context.Background()
	mock, store := newMockStore(t)
	p := &Persona{
		ID: "per1", OrgID: "org-a", Name: "Mobile Shopper", BuiltIn: "mobile_user",
		RoleHints:   []string{"shopper", "returning"},
		Constraints: map[string]string{"network": "slow3g", "viewport": "mobile"},
		Version:     2, CreatedAt: testNow, UpdatedAt: testNow,
	}

	mock.ExpectExec(sqlInsertPersona).
		WithArgs("per1", "org-a", "Mobile Shopper", "mobile_user",
			`["shopper","returning"]`, `{"network":"slow3g","viewport":"mobile"}`, 2, testNow, testNow).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreatePersona(ctx, p); err != nil {
		t.Fatalf("CreatePersona: %v", err)
	}

	personaRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "org_id", "name", "built_in", "role_hints", "constraints", "version", "created_at", "updated_at"}).
			AddRow("per1", "org-a", "Mobile Shopper", "mobile_user",
				[]byte(`["shopper","returning"]`), []byte(`{"network":"slow3g","viewport":"mobile"}`), 2, testNow, testNow)
	}
	mock.ExpectQuery(sqlSelectPersona).WithArgs("per1", "org-a").WillReturnRows(personaRows())
	got, err := store.GetPersona(ctx, "org-a", "per1")
	if err != nil {
		t.Fatalf("GetPersona: %v", err)
	}
	if got.BuiltIn != "mobile_user" || got.Version != 2 {
		t.Fatalf("scalar columns wrong: %+v", got)
	}
	if len(got.RoleHints) != 2 || got.RoleHints[1] != "returning" {
		t.Fatalf("role_hints not decoded: %v", got.RoleHints)
	}
	if got.Constraints["network"] != "slow3g" || len(got.Constraints) != 2 {
		t.Fatalf("constraints not decoded: %v", got.Constraints)
	}

	// Nil lists/maps marshal as [] and {} (JSONB NOT NULL DEFAULT shapes).
	mock.ExpectExec(sqlInsertPersona).
		WithArgs("per2", "org-a", "QA Bot", "", `[]`, `{}`, 1, testNow, testNow).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreatePersona(ctx, &Persona{ID: "per2", OrgID: "org-a", Name: "QA Bot", Version: 1, CreatedAt: testNow, UpdatedAt: testNow}); err != nil {
		t.Fatalf("CreatePersona(nil jsonb): %v", err)
	}

	mock.ExpectQuery(sqlSelectPersonasByOrg).WithArgs("org-a").WillReturnRows(personaRows())
	if list, err := store.ListPersonas(ctx, "org-a"); err != nil || len(list) != 1 {
		t.Fatalf("ListPersonas: %v len=%d", err, len(list))
	}

	mock.ExpectExec(sqlUpdatePersona).
		WithArgs("per1", "org-a", "Mobile Shopper v2", "mobile_user", `["shopper"]`, `{"a11y":"strict"}`, 3, testNow).
		WillReturnResult(sqlmock.NewResult(0, 1))
	p.Name, p.RoleHints, p.Constraints, p.Version = "Mobile Shopper v2", []string{"shopper"}, map[string]string{"a11y": "strict"}, 3
	if err := store.UpdatePersona(ctx, p); err != nil {
		t.Fatalf("UpdatePersona: %v", err)
	}

	mock.ExpectExec(sqlDeletePersona).WithArgs("per1", "org-b").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.DeletePersona(ctx, "org-b", "per1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org DeletePersona err = %v, want ErrNotFound", err)
	}
	mustSucceed(t, mock)
}

// --- error propagation + guards ----------------------------------------------

func TestPostgresStoreErrorPropagation(t *testing.T) {
	ctx := context.Background()
	mock, store := newMockStore(t)
	boom := errors.New("connection refused")

	// Write failures surface (never swallowed into a silent success).
	mock.ExpectExec(sqlInsertProject).WithArgs("p1", "org-a", "x", testNow, testNow).WillReturnError(boom)
	if err := store.CreateProject(ctx, &Project{ID: "p1", OrgID: "org-a", Name: "x", CreatedAt: testNow, UpdatedAt: testNow}); !errors.Is(err, boom) {
		t.Fatalf("CreateProject err = %v, want boom", err)
	}

	// Read failures surface; only sql.ErrNoRows maps to ErrNotFound.
	mock.ExpectQuery(sqlSelectTarget).WithArgs("t1", "org-a").WillReturnError(boom)
	if _, err := store.GetTarget(ctx, "org-a", "t1"); !errors.Is(err, boom) {
		t.Fatalf("GetTarget err = %v, want boom", err)
	}
	mock.ExpectQuery(sqlSelectEnvironment).WithArgs("e1", "org-a").WillReturnError(sql.ErrNoRows)
	if _, err := store.GetEnvironment(ctx, "org-a", "e1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetEnvironment err = %v, want ErrNotFound", err)
	}
	mock.ExpectQuery(sqlSelectPersona).WithArgs("per1", "org-a").WillReturnError(sql.ErrNoRows)
	if _, err := store.GetPersona(ctx, "org-a", "per1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPersona err = %v, want ErrNotFound", err)
	}
	mustSucceed(t, mock)
}

func TestPostgresStoreNilDBGuard(t *testing.T) {
	var store Store = NewPostgresStore(nil)
	ctx := context.Background()
	if err := store.CreateProject(ctx, &Project{ID: "p"}); err == nil {
		t.Fatal("nil db must error")
	}
	if _, err := store.GetProject(ctx, "org", "p"); err == nil {
		t.Fatal("nil db get must error")
	}
	if _, err := store.ListProjects(ctx, "org"); err == nil {
		t.Fatal("nil db list must error")
	}
	if err := store.DeleteProject(ctx, "org", "p"); err == nil {
		t.Fatal("nil db delete must error")
	}
	if _, err := store.GetTarget(ctx, "org", "t"); err == nil {
		t.Fatal("nil db target get must error")
	}
	if _, err := store.GetEnvironment(ctx, "org", "e"); err == nil {
		t.Fatal("nil db env get must error")
	}
	if _, err := store.GetPersona(ctx, "org", "per"); err == nil {
		t.Fatal("nil db persona get must error")
	}
}

// The dual-mode constructor wires the Postgres store in as the source of
// truth (smoke: one org-guarded create flows through the mock).
func TestNewServiceWithPostgresStore(t *testing.T) {
	mock, store := newMockStore(t)
	s := NewServiceWithStore(store)
	mock.ExpectExec(sqlInsertProject).WithArgs(sqlmock.AnyArg(), "org-a", "durable", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if _, err := s.CreateProject(context.Background(), "org-a", "durable"); err != nil {
		t.Fatalf("store-backed create: %v", err)
	}
	mustSucceed(t, mock)
}
