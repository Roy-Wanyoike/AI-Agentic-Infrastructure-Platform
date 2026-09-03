package connectors

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// probe_test.go pins the live health-check semantics of Service.Test
// (issue #30): GET to base_url, 5s default timeout, outcome recorded as
// last_check_at + last_check_status ("ok" 2xx-3xx / "error" otherwise).

func newProbeService(t *testing.T, values map[string]string) (*Service, *fakeResolver) {
	t.Helper()
	svc := NewService()
	res := newFakeResolver(values)
	svc.SetSecretResolver(res)
	return svc, res
}

func TestProbeSuccessRecordsOK(t *testing.T) {
	svc, res := newProbeService(t, map[string]string{"org-1/TOK": "tok_123"})
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	created, err := svc.Create(ctx, "org-1", CreateInput{
		Name:      "crm",
		Type:      TypeHTTP,
		BaseURL:   srv.URL,
		Config:    Config{AuthStyle: AuthStyleBearer},
		SecretRef: "TOK",
	}, "user-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	// Prime the secret resolution path before checking lookups.
	before := len(res.lookups)

	result, err := svc.Test(ctx, "org-1", created.ID)
	if err != nil {
		t.Fatalf("Test failed: %v", err)
	}
	if result.Status != "ok" || result.StatusCode != http.StatusOK {
		t.Fatalf("expected ok probe, got %+v", result)
	}
	if result.ConnectorID != created.ID || result.CheckedAt.IsZero() {
		t.Fatalf("probe metadata missing: %+v", result)
	}
	if result.Error != "" {
		t.Fatalf("success must carry no error: %q", result.Error)
	}
	if gotAuth != "Bearer tok_123" || gotPath != "/" {
		t.Fatalf("probe must send auth to base_url, got auth=%q path=%q", gotAuth, gotPath)
	}
	if len(res.lookups) <= before || res.lookups[before] != "org-1/TOK" {
		t.Fatalf("probe must resolve secret_ref, lookups=%v", res.lookups)
	}

	// Outcome recorded on the connector.
	got, _ := svc.Get(ctx, "org-1", created.ID)
	if got.LastCheckStatus != "ok" || got.LastCheckAt == nil {
		t.Fatalf("last_check not recorded: %+v", got)
	}
	if got.LastCheckAt.Before(result.CheckedAt.Add(-time.Second)) || got.LastCheckAt.After(result.CheckedAt.Add(time.Second)) {
		t.Fatal("last_check_at should match the probe time")
	}
}

func TestProbeNon2xxRecordsError(t *testing.T) {
	svc, _ := newProbeService(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx := context.Background()
	created, _ := svc.Create(ctx, "org-1", CreateInput{
		Name: "crm", Type: TypeHTTP, BaseURL: srv.URL,
	}, "user-1")

	result, err := svc.Test(ctx, "org-1", created.ID)
	if err != nil {
		t.Fatalf("Test must not fail hard on probe outcomes: %v", err)
	}
	if result.Status != "error" || result.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected error probe 503, got %+v", result)
	}
	if !strings.Contains(result.Error, "503") {
		t.Fatalf("error should mention the status code: %q", result.Error)
	}
	got, _ := svc.Get(ctx, "org-1", created.ID)
	if got.LastCheckStatus != "error" || got.LastCheckAt == nil {
		t.Fatalf("error outcome not recorded: %+v", got)
	}
}

func TestProbeTimeoutRecordsError(t *testing.T) {
	svc, _ := newProbeService(t, nil)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release) // unblock the handler after the test

	// Inject a short-timeout client instead of waiting the 5s default.
	svc.SetHTTPClient(&http.Client{Timeout: 100 * time.Millisecond})

	ctx := context.Background()
	created, _ := svc.Create(ctx, "org-1", CreateInput{
		Name: "slow", Type: TypeHTTP, BaseURL: srv.URL,
	}, "user-1")

	start := time.Now()
	result, err := svc.Test(ctx, "org-1", created.ID)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("timeout is a probe outcome, not a hard error: %v", err)
	}
	if result.Status != "error" || result.StatusCode != 0 {
		t.Fatalf("expected error probe without status code, got %+v", result)
	}
	if result.Error == "" {
		t.Fatal("timeout probe must carry an error message")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("probe must respect the injected client timeout, took %s", elapsed)
	}
	got, _ := svc.Get(ctx, "org-1", created.ID)
	if got.LastCheckStatus != "error" {
		t.Fatalf("timeout outcome not recorded: %+v", got)
	}
}

func TestProbeSecretResolutionFailureRecordsError(t *testing.T) {
	svc := NewService()
	res := &fakeResolver{err: errors.New("secret not found")}
	svc.SetSecretResolver(res)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("probe must not be sent when the secret cannot be resolved")
	}))
	defer srv.Close()

	ctx := context.Background()
	created, _ := svc.Create(ctx, "org-1", CreateInput{
		Name: "crm", Type: TypeHTTP, BaseURL: srv.URL,
		Config: Config{AuthStyle: AuthStyleBearer}, SecretRef: "MISSING",
	}, "user-1")

	result, err := svc.Test(ctx, "org-1", created.ID)
	if err != nil {
		t.Fatalf("resolution failure is a probe outcome, not a hard error: %v", err)
	}
	if result.Status != "error" {
		t.Fatalf("expected error status, got %+v", result)
	}
	if !strings.Contains(result.Error, "MISSING") || !strings.Contains(result.Error, "secret not found") {
		t.Fatalf("error should name the secret and the cause: %q", result.Error)
	}
	got, _ := svc.Get(ctx, "org-1", created.ID)
	if got.LastCheckStatus != "error" {
		t.Fatalf("resolution failure not recorded: %+v", got)
	}
}

func TestProbeDisabledConnectorStillProbes(t *testing.T) {
	svc, _ := newProbeService(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	created, _ := svc.Create(ctx, "org-1", CreateInput{
		Name: "off", Type: TypeHTTP, BaseURL: srv.URL, Status: StatusDisabled,
	}, "user-1")

	result, err := svc.Test(ctx, "org-1", created.ID)
	if err != nil || result.Status != "ok" {
		t.Fatalf("disabled connectors must remain probeable, got %+v (%v)", result, err)
	}
}

func TestProbeErrors(t *testing.T) {
	svc, _ := newProbeService(t, nil)
	ctx := context.Background()

	if _, err := svc.Test(ctx, "org-1", "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown connector must be ErrNotFound, got %v", err)
	}
	if _, err := svc.Test(ctx, " ", "x"); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("missing org must be ErrOrgRequired, got %v", err)
	}
}

func TestProbeOverStoreRecordsCheckStatus(t *testing.T) {
	svc, mock, close := newMockService(t)
	defer close()
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mock.ExpectExec(`INSERT INTO connectors`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	rec, err := svc.Create(ctx, "org-1", CreateInput{
		Name: "crm", Type: TypeHTTP, BaseURL: srv.URL,
	}, "user-1")
	if err != nil {
		t.Fatalf("Create over store failed: %v", err)
	}

	// Test() loads the connector through the org-guarded SELECT first.
	mock.ExpectQuery(`SELECT id, organization_id`).
		WithArgs(rec.ID, "org-1").
		WillReturnRows(connectorRow(rec, `{}`))
	// The probe outcome is persisted through the org-guarded UPDATE.
	mock.ExpectExec(`(?s)UPDATE connectors SET last_check_at = \$3, last_check_status = \$4\s+WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rec.ID, "org-1", sqlmock.AnyArg(), "ok").
		WillReturnResult(sqlmock.NewResult(0, 1))
	result, err := svc.Test(ctx, "org-1", rec.ID)
	if err != nil || result.Status != "ok" {
		t.Fatalf("probe over store failed: %+v (%v)", result, err)
	}

	// Unknown connector: the scoped SELECT fails before any probe/UPDATE.
	mock.ExpectQuery(`SELECT id, organization_id`).
		WithArgs("nope", "org-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := svc.Test(ctx, "org-1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown probe must be ErrNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestDefaultTestTimeoutConstant(t *testing.T) {
	if DefaultTestTimeout != 5*time.Second {
		t.Fatalf("issue #30 pins a 5s probe timeout, got %s", DefaultTestTimeout)
	}
	svc := NewService()
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if svc.httpClient.Timeout != DefaultTestTimeout {
		t.Fatalf("default client timeout = %s, want %s", svc.httpClient.Timeout, DefaultTestTimeout)
	}
}
