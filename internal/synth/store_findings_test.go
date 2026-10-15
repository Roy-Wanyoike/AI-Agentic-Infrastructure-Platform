package synth

// store_findings_test.go — issue #150: sqlmock-backed tests for the
// Postgres FindingStore. These pin the exact SQL (upsert keyed on the
// ux_synth_findings_fp(org_id, fingerprint, run_id) unique index), the
// org-scoping of every query, nullable-column round-trips and the
// ErrNotFound mapping.

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

var pgNow = time.Date(2025, 6, 3, 8, 0, 0, 0, time.UTC)

func newPgStore(t *testing.T) (*PostgresFindingStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresFindingStore(db), mock
}

func pgFindingCols() []string {
	return strings.Split(pgFindingColumns, ", ")
}

func pgSampleFinding() Finding {
	refs := []string{"screenshot:shot-01.png", "console:console-01.log"}
	repro := true
	return Finding{
		ID: "fnd_abc", OrgID: "org-1", RunID: "run-100", JourneyID: "j-checkout",
		AssertionIx: 2, Status: FindingConfirmed, Severity: SevHigh,
		Title:        "element_visible assertion failed on #order-confirm",
		Description:  "Assertion #2 (element_visible) on #order-confirm expected \"visible\"; observed \"missing\".",
		Fingerprint:  strings.Repeat("ab", 32),
		ReproSteps:   "Re-run journey \"checkout smoke\" (v4)...\n1. navigate /checkout",
		EvidenceRefs: refs,
		ReproRunID:   "run-repro-77",
		Reproducible: &repro,
		Confidence:   0.99,
		CreatedAt:    pgNow.Add(-time.Hour),
		UpdatedAt:    pgNow,
	}
}

func pgSampleRow(f Finding) *sqlmock.Rows {
	refsJSON := "[]"
	if f.EvidenceRefs != nil {
		refsJSON = `["` + strings.Join(f.EvidenceRefs, `","`) + `"]`
	}
	return sqlmock.NewRows(pgFindingCols()).AddRow(
		f.ID, f.OrgID, f.RunID, f.JourneyID, f.AssertionIx,
		string(f.Status), string(f.Severity), f.Title, f.Description,
		f.Fingerprint, f.ReproSteps, refsJSON,
		f.ReproRunID, f.Reproducible != nil && *f.Reproducible, f.Confidence,
		f.CreatedAt, f.UpdatedAt,
	)
}

func TestPostgresRecordExecutesOrgScopedUpsert(t *testing.T) {
	t.Parallel()
	store, mock := newPgStore(t)
	f := pgSampleFinding()
	mock.ExpectExec(regexp.QuoteMeta(pgRecordFindingSQL)).
		WithArgs(
			f.ID, f.OrgID, f.RunID, f.JourneyID, f.AssertionIx,
			string(f.Status), string(f.Severity), f.Title, f.Description,
			f.Fingerprint, f.ReproSteps, refsToJSON(f.EvidenceRefs),
			"run-repro-77", true, f.Confidence,
			sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Record(context.Background(), f); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPostgresRecordNilReproAndEmptyRefs(t *testing.T) {
	t.Parallel()
	store, mock := newPgStore(t)
	f := Finding{
		ID: "fnd_min", OrgID: "org-1", RunID: "run-1", JourneyID: "j-1",
		AssertionIx: -1, Status: FindingInconclusive, Severity: SevMedium,
		Fingerprint: strings.Repeat("cd", 32),
		CreatedAt:   pgNow, UpdatedAt: pgNow,
	}
	mock.ExpectExec(regexp.QuoteMeta(pgRecordFindingSQL)).
		WithArgs(
			f.ID, f.OrgID, f.RunID, f.JourneyID, f.AssertionIx,
			string(f.Status), string(f.Severity), f.Title, f.Description,
			f.Fingerprint, f.ReproSteps, "[]",
			nil, nil, f.Confidence,
			sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Record(context.Background(), f); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPostgresRecordRequiresOrgAndID(t *testing.T) {
	t.Parallel()
	store, mock := newPgStore(t)
	if err := store.Record(context.Background(), Finding{OrgID: "org-1"}); err == nil {
		t.Fatal("record without id must fail")
	}
	if err := store.Record(context.Background(), Finding{ID: "fnd_x"}); err == nil {
		t.Fatal("record without org must fail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no SQL must be issued for invalid input: %v", err)
	}
}

func TestPostgresGetRoundTrip(t *testing.T) {
	t.Parallel()
	store, mock := newPgStore(t)
	f := pgSampleFinding()
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings WHERE id = $1 AND org_id = $2`
	mock.ExpectQuery(regexp.QuoteMeta(q)).
		WithArgs("fnd_abc", "org-1").
		WillReturnRows(pgSampleRow(f))
	got, err := store.Get(context.Background(), "org-1", "fnd_abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != f.ID || got.Status != FindingConfirmed || got.Severity != SevHigh {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Reproducible == nil || !*got.Reproducible || got.ReproRunID != "run-repro-77" {
		t.Fatalf("repro fields lost: %+v", got)
	}
	if len(got.EvidenceRefs) != 2 || got.EvidenceRefs[0] != "screenshot:shot-01.png" {
		t.Fatalf("evidence refs lost: %v", got.EvidenceRefs)
	}
	if !got.UpdatedAt.Equal(pgNow) {
		t.Fatalf("updated_at = %v, want %v", got.UpdatedAt, pgNow)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPostgresGetNotFoundMapping(t *testing.T) {
	t.Parallel()
	store, mock := newPgStore(t)
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings WHERE id = $1 AND org_id = $2`
	mock.ExpectQuery(regexp.QuoteMeta(q)).
		WithArgs("fnd_missing", "org-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.Get(context.Background(), "org-1", "fnd_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgresGetByFingerprintCanonicalRow(t *testing.T) {
	t.Parallel()
	store, mock := newPgStore(t)
	fp := strings.Repeat("ab", 32)
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings ` +
		`WHERE org_id = $1 AND fingerprint = $2 ` +
		`ORDER BY updated_at DESC, run_id DESC LIMIT 1`
	latest := pgSampleFinding()
	mock.ExpectQuery(regexp.QuoteMeta(q)).
		WithArgs("org-1", fp).
		WillReturnRows(pgSampleRow(latest))
	got, err := store.GetByFingerprint(context.Background(), "org-1", fp)
	if err != nil {
		t.Fatalf("GetByFingerprint: %v", err)
	}
	if got.ID != "fnd_abc" {
		t.Fatalf("canonical row = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPostgresListByFingerprintIsOccurrenceHistory(t *testing.T) {
	t.Parallel()
	store, mock := newPgStore(t)
	fp := strings.Repeat("ab", 32)
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings ` +
		`WHERE org_id = $1 AND fingerprint = $2 ` +
		`ORDER BY created_at ASC, run_id ASC`
	older := pgSampleFinding()
	older.ID = "fnd_first"
	older.RunID = "run-99"
	older.CreatedAt = pgNow.Add(-2 * time.Hour)
	older.UpdatedAt = pgNow.Add(-2 * time.Hour)
	newer := pgSampleFinding()
	mock.ExpectQuery(regexp.QuoteMeta(q)).
		WithArgs("org-1", fp).
		WillReturnRows(pgSampleRow(older).AddRow(
			newer.ID, newer.OrgID, newer.RunID, newer.JourneyID, newer.AssertionIx,
			string(newer.Status), string(newer.Severity), newer.Title, newer.Description,
			newer.Fingerprint, newer.ReproSteps, refsToJSON(newer.EvidenceRefs),
			newer.ReproRunID, true, newer.Confidence, newer.CreatedAt, newer.UpdatedAt,
		))
	hist, err := store.ListByFingerprint(context.Background(), "org-1", fp)
	if err != nil {
		t.Fatalf("ListByFingerprint: %v", err)
	}
	if len(hist) != 2 || hist[0].ID != "fnd_first" || hist[1].ID != "fnd_abc" {
		t.Fatalf("history order broken: %+v", hist)
	}
	// Occurrence log derivation: (RunID, CreatedAt) per row.
	if hist[0].RunID != "run-99" {
		t.Fatalf("occurrence run lost: %+v", hist[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPostgresListWithFilterAndLimit(t *testing.T) {
	t.Parallel()
	store, mock := newPgStore(t)
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings ` +
		`WHERE org_id = $1 AND status = $2 ` +
		`ORDER BY updated_at DESC, id DESC LIMIT $3`
	mock.ExpectQuery(regexp.QuoteMeta(q)).
		WithArgs("org-1", string(FindingConfirmed), 5).
		WillReturnRows(pgSampleRow(pgSampleFinding()))
	got, err := store.List(context.Background(), "org-1", FindingFilter{
		Status: FindingConfirmed, Limit: 5,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Status != FindingConfirmed {
		t.Fatalf("list = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPostgresScanNullables(t *testing.T) {
	t.Parallel()
	store, mock := newPgStore(t)
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings WHERE id = $1 AND org_id = $2`
	rows := sqlmock.NewRows(pgFindingCols()).AddRow(
		"fnd_null", "org-1", "run-1", "j-1", -1,
		string(FindingInconclusive), string(SevLow), "", "",
		strings.Repeat("ef", 32), "", "[]",
		nil, nil, 0.10,
		pgNow, pgNow,
	)
	mock.ExpectQuery(regexp.QuoteMeta(q)).
		WithArgs("fnd_null", "org-1").
		WillReturnRows(rows)
	got, err := store.Get(context.Background(), "org-1", "fnd_null")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ReproRunID != "" || got.Reproducible != nil {
		t.Fatalf("nullable repro fields must scan to zero values: %+v", got)
	}
	if got.EvidenceRefs == nil || len(got.EvidenceRefs) != 0 {
		t.Fatalf("empty refs must scan to an empty slice: %#v", got.EvidenceRefs)
	}
}

func TestPostgresStoreImplementsFindingStore(t *testing.T) {
	t.Parallel()
	var _ FindingStore = (*PostgresFindingStore)(nil)
	var _ FindingStore = (*InMemoryFindingStore)(nil)
}
