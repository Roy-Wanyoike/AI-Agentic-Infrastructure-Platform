package connectors

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// fakeResolver is the in-memory SecretResolver used across the tests: it maps
// (orgID, name) -> value, records every lookup and can be made to fail.
type fakeResolver struct {
	values  map[string]string // key: orgID + "/" + name
	lookups []string          // key: orgID + "/" + name, in call order
	err     error
}

func (f *fakeResolver) Resolve(_ context.Context, orgID, name string) (string, error) {
	key := orgID + "/" + name
	f.lookups = append(f.lookups, key)
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.values[key]
	if !ok {
		return "", errors.New("secret not found")
	}
	return v, nil
}

func (f *fakeResolver) key(orgID, name string) string { return orgID + "/" + name }

func newFakeResolver(pairs map[string]string) *fakeResolver {
	return &fakeResolver{values: pairs}
}

func validInput() CreateInput {
	return CreateInput{
		Name:    "salesforce-prod",
		Type:    TypeHTTP,
		BaseURL: "https://api.example.com/v1",
		Config:  Config{AuthStyle: AuthStyleNone},
		Status:  StatusActive,
	}
}

// ---------------------------------------------------------------------------
// CRUD, in-memory mode
// ---------------------------------------------------------------------------

func TestServiceCrudInMemory(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	created, err := svc.Create(ctx, "org-1", validInput(), "user-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if created.ID == "" || created.OrganizationID != "org-1" || created.CreatedBy != "user-1" {
		t.Fatalf("unexpected connector: %+v", created)
	}
	if created.Status != StatusActive || created.Type != TypeHTTP {
		t.Fatalf("defaults not applied: %+v", created)
	}

	// Duplicate name within the same org -> ErrDuplicate.
	if _, err := svc.Create(ctx, "org-1", validInput(), "user-1"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate create must be ErrDuplicate, got %v", err)
	}
	// Same name in another org is fine.
	if _, err := svc.Create(ctx, "org-2", validInput(), "user-2"); err != nil {
		t.Fatalf("same name in other org must succeed: %v", err)
	}

	got, err := svc.Get(ctx, "org-1", created.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Name != "salesforce-prod" || got.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("unexpected round-trip: %+v", got)
	}

	// Tenant isolation: foreign id is ErrNotFound (no existence leak).
	if _, err := svc.Get(ctx, "org-2", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Get must be ErrNotFound, got %v", err)
	}
	if err := svc.Delete(ctx, "org-2", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Delete must be ErrNotFound, got %v", err)
	}

	// List is name-sorted and org-scoped.
	for _, name := range []string{"zzz-later", "aaa-early"} {
		in := validInput()
		in.Name = name
		if _, err := svc.Create(ctx, "org-1", in, "user-1"); err != nil {
			t.Fatalf("seed %s failed: %v", name, err)
		}
	}
	list, err := svc.List(ctx, "org-1")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 connectors for org-1, got %d", len(list))
	}
	if list[0].Name != "aaa-early" || list[2].Name != "zzz-later" {
		t.Fatalf("list must be name ASC, got %v", []string{list[0].Name, list[1].Name, list[2].Name})
	}
	if list2, err := svc.List(ctx, "org-2"); err != nil || len(list2) != 1 {
		t.Fatalf("org-2 list must be scoped, got %d items (err=%v)", len(list2), err)
	}

	// Update preserves creator + created_at, applies new fields.
	in := validInput()
	in.Name = "salesforce-renamed"
	in.BaseURL = "https://api2.example.com"
	in.Status = StatusDisabled
	in.Config = Config{AuthStyle: AuthStyleBearer}
	updated, err := svc.Update(ctx, "org-1", created.ID, in, "user-9")
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if updated.CreatedBy != "user-1" || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("Update must preserve creator/created_at: %+v vs %+v", updated, created)
	}
	if updated.Status != StatusDisabled || updated.Config.AuthStyle != AuthStyleBearer {
		t.Fatalf("update not applied: %+v", updated)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) {
		t.Fatal("UpdatedAt must advance")
	}

	// Delete then Get -> ErrNotFound.
	if err := svc.Delete(ctx, "org-1", created.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := svc.Get(ctx, "org-1", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-delete Get must be ErrNotFound, got %v", err)
	}

	// Unknown id everywhere -> ErrNotFound.
	if _, err := svc.Get(ctx, "org-1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown Get must be ErrNotFound, got %v", err)
	}
	if _, err := svc.Update(ctx, "org-1", "nope", validInput(), "u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown Update must be ErrNotFound, got %v", err)
	}
	if err := svc.Delete(ctx, "org-1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown Delete must be ErrNotFound, got %v", err)
	}
}

func TestServiceValidation(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	cases := []struct {
		name string
		mut  func(*CreateInput)
		want error
	}{
		{"missing name", func(i *CreateInput) { i.Name = "   " }, ErrNameRequired},
		{"name too long", func(i *CreateInput) { i.Name = strings.Repeat("x", 256) }, ErrNameTooLong},
		{"bad type", func(i *CreateInput) { i.Type = "ftp" }, ErrTypeInvalid},
		{"missing base_url", func(i *CreateInput) { i.BaseURL = "" }, ErrBaseURLRequired},
		{"non-http scheme", func(i *CreateInput) { i.BaseURL = "ftp://api.example.com" }, ErrBaseURLInvalid},
		{"no host", func(i *CreateInput) { i.BaseURL = "https://" }, ErrBaseURLInvalid},
		{"bad status", func(i *CreateInput) { i.Status = "paused" }, ErrStatusInvalid},
		{"bad auth style", func(i *CreateInput) { i.Config.AuthStyle = "hmac" }, ErrAuthStyleInvalid},
		{"crlf header value", func(i *CreateInput) { i.Config.Headers = map[string]string{"X-A": "v\r\nX-Evil: 1"} }, ErrAuthStyleInvalid},
		{"crlf header key", func(i *CreateInput) { i.Config.Headers = map[string]string{"X-A\r\nB": "v"} }, ErrAuthStyleInvalid},
	}
	for _, tc := range cases {
		in := validInput()
		tc.mut(&in)
		if _, err := svc.Create(ctx, "org-1", in, "user-1"); !errors.Is(err, tc.want) {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.want, err)
		}
	}
	// Defaults: empty status -> active, empty auth style -> none.
	in := validInput()
	in.Status = ""
	in.Config = Config{}
	got, err := svc.Create(ctx, "org-1", in, "user-1")
	if err != nil {
		t.Fatalf("default create failed: %v", err)
	}
	if got.Status != StatusActive || got.Config.AuthStyle != AuthStyleNone {
		t.Fatalf("defaults must be active/none, got %+v", got.Config)
	}
	// Actor + org required.
	if _, err := svc.Create(ctx, "org-1", validInput(), " "); !errors.Is(err, ErrUpdatedByRequired) {
		t.Fatalf("missing actor must be ErrUpdatedByRequired, got %v", err)
	}
	if _, err := svc.Create(ctx, " ", validInput(), "user-1"); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("missing org must be ErrOrgRequired, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CRUD, Postgres mode (sqlmock, routed through the Service)
// ---------------------------------------------------------------------------

func newMockService(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	svc, err := NewServiceWithStore(NewPostgresStore(db), nil)
	if err != nil {
		t.Fatalf("NewServiceWithStore failed: %v", err)
	}
	return svc, mock, func() { _ = db.Close() }
}

func connectorRow(c *Connector, cfgRaw string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "name", "type", "base_url", "config",
		"secret_ref", "status", "last_check_at", "last_check_status",
		"created_by", "created_at", "updated_at",
	}).AddRow(c.ID, c.OrganizationID, c.Name, c.Type, c.BaseURL, cfgRaw,
		c.SecretRef, c.Status, c.LastCheckAt, c.LastCheckStatus,
		c.CreatedBy, c.CreatedAt, c.UpdatedAt)
}

func TestServiceCrudOverStore(t *testing.T) {
	svc, mock, close := newMockService(t)
	defer close()
	ctx := context.Background()

	mock.ExpectExec(`INSERT INTO connectors`).
		WithArgs(sqlmock.AnyArg(), "org-1", "salesforce-prod", "http",
			"https://api.example.com/v1", `{"auth_style":"bearer"}`, "SF_API_KEY", "active",
			nil, "", "user-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	created, err := svc.Create(ctx, "org-1", CreateInput{
		Name:      "salesforce-prod",
		Type:      TypeHTTP,
		BaseURL:   "https://api.example.com/v1",
		Config:    Config{AuthStyle: AuthStyleBearer},
		SecretRef: "SF_API_KEY",
	}, "user-1")
	if err != nil {
		t.Fatalf("Create over store failed: %v", err)
	}

	// Get: scoped SELECT round-trips config JSONB.
	mock.ExpectQuery(`SELECT id, organization_id, name, type, base_url,`).
		WithArgs(created.ID, "org-1").
		WillReturnRows(connectorRow(created, `{"auth_style":"bearer"}`))
	got, err := svc.Get(ctx, "org-1", created.ID)
	if err != nil {
		t.Fatalf("Get over store failed: %v", err)
	}
	if got.Config.AuthStyle != AuthStyleBearer || got.SecretRef != "SF_API_KEY" {
		t.Fatalf("config/secret_ref round-trip failed: %+v", got)
	}

	// Get with sql.ErrNoRows -> ErrNotFound.
	mock.ExpectQuery(`SELECT id, organization_id`).
		WithArgs("other", "org-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := svc.Get(ctx, "org-1", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no-rows Get must be ErrNotFound, got %v", err)
	}

	// Update: Get-then-UPDATE preserving creator fields.
	mock.ExpectQuery(`SELECT id, organization_id`).
		WithArgs(created.ID, "org-1").
		WillReturnRows(connectorRow(created, `{"auth_style":"bearer"}`))
	mock.ExpectExec(`UPDATE connectors SET`).
		WithArgs(created.ID, "org-1", "renamed", "webhook", "https://api2.example.com",
			`{"auth_style":"none"}`, "", "disabled", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	in := validInput()
	in.Name = "renamed"
	in.Type = TypeWebhook
	in.BaseURL = "https://api2.example.com"
	in.Config = Config{}
	in.Status = StatusDisabled
	if _, err := svc.Update(ctx, "org-1", created.ID, in, "user-9"); err != nil {
		t.Fatalf("Update over store failed: %v", err)
	}

	// List returns org-scoped rows.
	mock.ExpectQuery(`SELECT id, organization_id, name, type, base_url,`).
		WithArgs("org-1").
		WillReturnRows(connectorRow(created, `{"auth_style":"bearer"}`))
	list, err := svc.List(ctx, "org-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("List over store failed: %v (n=%d)", err, len(list))
	}

	// Delete: 1 row -> nil; 0 rows -> ErrNotFound.
	mock.ExpectExec(`DELETE FROM connectors WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(created.ID, "org-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := svc.Delete(ctx, "org-1", created.ID); err != nil {
		t.Fatalf("Delete over store failed: %v", err)
	}
	mock.ExpectExec(`DELETE FROM connectors`).
		WithArgs("nope", "org-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := svc.Delete(ctx, "org-1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("zero-rows Delete must be ErrNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestServiceCreateDuplicateOverStore(t *testing.T) {
	svc, mock, close := newMockService(t)
	defer close()
	ctx := context.Background()

	mock.ExpectExec(`INSERT INTO connectors`).
		WillReturnError(&pq.Error{Code: "23505"})
	if _, err := svc.Create(ctx, "org-1", validInput(), "user-1"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("23505 must map to ErrDuplicate, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestNewServiceWithStoreFailFastWithoutStore(t *testing.T) {
	if _, err := NewServiceWithStore(nil, nil); err == nil {
		t.Fatal("nil store must fail construction")
	}
}

// ---------------------------------------------------------------------------
// BuildRequest: auth styles, header merge, guards
// ---------------------------------------------------------------------------

func TestBuildRequestAuthStyleNone(t *testing.T) {
	svc := NewService()
	c := &Connector{
		OrganizationID: "org-1",
		BaseURL:        "https://api.example.com/v1",
		Config:         Config{AuthStyle: AuthStyleNone},
		SecretRef:      "SHOULD_NOT_BE_RESOLVED",
	}
	res := newFakeResolver(map[string]string{"org-1/SHOULD_NOT_BE_RESOLVED": "leak-me"})
	svc.SetSecretResolver(res)

	req, err := svc.BuildRequest(context.Background(), "org-1", c, http.MethodGet, "/contacts", nil, nil)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	if req.URL.String() != "https://api.example.com/v1/contacts" {
		t.Fatalf("unexpected URL: %s", req.URL)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("auth style none must not inject an Authorization header")
	}
	if len(res.lookups) != 0 {
		t.Fatalf("auth style none must not resolve secrets, got %v", res.lookups)
	}
}

func TestBuildRequestAuthStyleBearer(t *testing.T) {
	svc := NewService()
	svc.SetSecretResolver(newFakeResolver(map[string]string{"org-1/SF_KEY": "tok_123"}))
	c := &Connector{
		OrganizationID: "org-1",
		BaseURL:        "https://api.example.com",
		Config:         Config{AuthStyle: AuthStyleBearer},
		SecretRef:      "SF_KEY",
	}
	req, err := svc.BuildRequest(context.Background(), "org-1", c, http.MethodGet, "", nil, nil)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok_123" {
		t.Fatalf("bearer header = %q, want %q", got, "Bearer tok_123")
	}
}

func TestBuildRequestAuthStyleBasic(t *testing.T) {
	svc := NewService()
	svc.SetSecretResolver(newFakeResolver(map[string]string{"org-1/BASIC_PW": "s3cret-pw"}))
	c := &Connector{
		OrganizationID: "org-1",
		BaseURL:        "https://api.example.com",
		Config:         Config{AuthStyle: AuthStyleBasic, Username: "svc-account"},
		SecretRef:      "BASIC_PW",
	}
	req, err := svc.BuildRequest(context.Background(), "org-1", c, http.MethodPost, "/orders", []byte(`{"a":1}`), nil)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("svc-account:s3cret-pw"))
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("basic header = %q, want %q", got, want)
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatal("body request must default Content-Type to application/json")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil || string(body) != `{"a":1}` {
		t.Fatalf("body round-trip failed: %q (%v)", body, err)
	}
}

func TestBuildRequestAuthStyleAPIKeyHeader(t *testing.T) {
	svc := NewService()
	svc.SetSecretResolver(newFakeResolver(map[string]string{"org-1/ZEN_KEY": "zen-abc"}))

	// Default header name.
	c := &Connector{
		OrganizationID: "org-1",
		BaseURL:        "https://api.example.com",
		Config:         Config{AuthStyle: AuthStyleAPIKeyHeader},
		SecretRef:      "ZEN_KEY",
	}
	req, err := svc.BuildRequest(context.Background(), "org-1", c, http.MethodGet, "", nil, nil)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	if got := req.Header.Get(DefaultAPIKeyHeader); got != "zen-abc" {
		t.Fatalf("api key header = %q, want %q", got, "zen-abc")
	}

	// Custom header name + prefix.
	c.Config.APIKeyHeader = "X-Zendesk-Api-Token"
	c.Config.APIKeyPrefix = "Token token="
	req, err = svc.BuildRequest(context.Background(), "org-1", c, http.MethodGet, "", nil, nil)
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	if got := req.Header.Get("X-Zendesk-Api-Token"); got != "Token token=zen-abc" {
		t.Fatalf("custom api key header = %q", got)
	}
}

func TestBuildRequestHeaderMergePrecedence(t *testing.T) {
	svc := NewService()
	svc.SetSecretResolver(newFakeResolver(map[string]string{"org-1/K": "tok"}))
	c := &Connector{
		OrganizationID: "org-1",
		BaseURL:        "https://api.example.com",
		Config: Config{
			AuthStyle: AuthStyleBearer,
			Headers: map[string]string{
				"X-Tenant":      "template-org",
				"X-Static":      "always",
				"Authorization": "Bearer FORGED",
			},
		},
		SecretRef: "K",
	}
	req, err := svc.BuildRequest(context.Background(), "org-1", c, http.MethodGet, "/x",
		nil, map[string]string{"X-Tenant": "per-request", "X-Req": "yes"})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	if got := req.Header.Get("X-Tenant"); got != "per-request" {
		t.Fatalf("per-request headers must win over templates, got %q", got)
	}
	if got := req.Header.Get("X-Static"); got != "always" {
		t.Fatalf("template header missing: %q", got)
	}
	if got := req.Header.Get("X-Req"); got != "yes" {
		t.Fatalf("per-request header missing: %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("injected auth must win over forged template, got %q", got)
	}
}

func TestBuildRequestGuards(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	if _, err := svc.BuildRequest(ctx, "org-1", nil, "GET", "", nil, nil); !errors.Is(err, ErrInvalidConnectorRef) {
		t.Fatalf("nil connector must be ErrInvalidConnectorRef, got %v", err)
	}
	foreign := &Connector{OrganizationID: "org-OTHER", BaseURL: "https://api.example.com"}
	if _, err := svc.BuildRequest(ctx, "org-1", foreign, "GET", "", nil, nil); !errors.Is(err, ErrInvalidConnectorRef) {
		t.Fatalf("cross-tenant connector must be ErrInvalidConnectorRef, got %v", err)
	}
	disabled := &Connector{OrganizationID: "org-1", BaseURL: "https://api.example.com", Status: StatusDisabled}
	if _, err := svc.BuildRequest(ctx, "org-1", disabled, "GET", "", nil, nil); !errors.Is(err, ErrConnectorDisabled) {
		t.Fatalf("disabled connector must refuse, got %v", err)
	}
	active := &Connector{OrganizationID: "org-1", BaseURL: "https://api.example.com", Status: StatusActive}
	if _, err := svc.BuildRequest(ctx, "org-1", active, "TRACE", "", nil, nil); !errors.Is(err, ErrInvalidMethod) {
		t.Fatalf("TRACE must be refused, got %v", err)
	}
	if _, err := svc.BuildRequest(ctx, " ", active, "GET", "", nil, nil); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("missing org must be ErrOrgRequired, got %v", err)
	}
	// Method normalization: lowercase get is accepted.
	if req, err := svc.BuildRequest(ctx, "org-1", active, "get", "", nil, nil); err != nil || req.Method != http.MethodGet {
		t.Fatalf("lowercase method must normalize, got %v (%v)", req, err)
	}

	// Auth style without secret ref.
	noRef := &Connector{OrganizationID: "org-1", BaseURL: "https://api.example.com", Config: Config{AuthStyle: AuthStyleBearer}}
	if _, err := svc.BuildRequest(ctx, "org-1", noRef, "GET", "", nil, nil); !errors.Is(err, ErrSecretRefRequired) {
		t.Fatalf("style without secret_ref must be ErrSecretRefRequired, got %v", err)
	}

	// No resolver wired.
	noRef.SecretRef = "K"
	if _, err := svc.BuildRequest(ctx, "org-1", noRef, "GET", "", nil, nil); !errors.Is(err, ErrSecretResolverRequired) {
		t.Fatalf("missing resolver must be ErrSecretResolverRequired, got %v", err)
	}

	// Resolver failure is wrapped (contains secret name, not values).
	svc.SetSecretResolver(newFakeResolver(map[string]string{}))
	_, err := svc.BuildRequest(ctx, "org-1", noRef, "GET", "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), `"K"`) {
		t.Fatalf("resolver failure must mention the secret name, got %v", err)
	}
}

func TestBuildRequestURLJoin(t *testing.T) {
	svc := NewService()
	cases := []struct{ base, path, want string }{
		{"https://api.example.com", "/v1/x", "https://api.example.com/v1/x"},
		{"https://api.example.com/", "v1/x", "https://api.example.com/v1/x"},
		{"https://api.example.com/v1", "/x", "https://api.example.com/v1/x"},
		{"https://api.example.com/v1", "", "https://api.example.com/v1"},
	}
	for _, tc := range cases {
		c := &Connector{OrganizationID: "org-1", BaseURL: tc.base, Status: StatusActive}
		req, err := svc.BuildRequest(context.Background(), "org-1", c, http.MethodGet, tc.path, nil, nil)
		if err != nil {
			t.Fatalf("join(%q,%q) failed: %v", tc.base, tc.path, err)
		}
		if req.URL.String() != tc.want {
			t.Errorf("join(%q,%q) = %q, want %q", tc.base, tc.path, req.URL.String(), tc.want)
		}
	}
	// Invalid target after join.
	bad := &Connector{OrganizationID: "org-1", BaseURL: "https://", Status: StatusActive}
	if _, err := svc.BuildRequest(context.Background(), "org-1", bad, "GET", "", nil, nil); !errors.Is(err, ErrBaseURLInvalid) {
		t.Fatalf("bad target must be ErrBaseURLInvalid, got %v", err)
	}
}

func TestSecretResolverFuncAdapter(t *testing.T) {
	svc := NewService()
	called := ""
	svc.SetSecretResolver(SecretResolverFunc(func(_ context.Context, org, name string) (string, error) {
		called = org + "/" + name
		return "v", nil
	}))
	c := &Connector{
		OrganizationID: "org-1",
		BaseURL:        "https://api.example.com",
		Config:         Config{AuthStyle: AuthStyleBearer},
		SecretRef:      "NAME",
	}
	if _, err := svc.BuildRequest(context.Background(), "org-1", c, http.MethodGet, "", nil, nil); err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	if called != "org-1/NAME" {
		t.Fatalf("SecretResolverFunc not invoked with (org,name): %q", called)
	}
}

// configJSON is exercised indirectly by the store, but pin the "{}" fallback.
func TestConfigJSONFallback(t *testing.T) {
	if got := configJSON(Config{}); got != "{}" && got != `{"auth_style":"none"}` {
		t.Fatalf("unexpected empty config encoding: %s", got)
	}
	raw, err := json.Marshal(Config{AuthStyle: AuthStyleBearer})
	if err != nil || string(raw) != `{"auth_style":"bearer"}` {
		t.Fatalf("unexpected config encoding: %s (%v)", raw, err)
	}
}

// compile-time: TestResult must serialize its documented fields.
func TestTestResultShape(t *testing.T) {
	now := time.Now().UTC()
	raw, err := json.Marshal(TestResult{ConnectorID: "c1", Status: "ok", StatusCode: 200, LatencyMS: 3, CheckedAt: now})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	for _, key := range []string{"connector_id", "status", "status_code", "latency_ms", "checked_at"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("TestResult JSON missing %q: %s", key, raw)
		}
	}
	if _, has := m["error"]; has {
		t.Fatal("empty error must be omitted")
	}
}
