package scim

// Service- and middleware-level tests for the SCIM 2.0 surface (issue #29),
// run against the shared in-process auth.MemoryStore identity table. The
// Postgres token path is pinned with sqlmock so the hashing-at-rest contract
// (INSERT carries the SHA-256 hex, never the plaintext) is verified without
// a live database.

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/auth"
)

type scimFixture struct {
	t          *testing.T
	svc        *Service
	identities *auth.MemoryStore
	authSvc    *auth.Service
}

func newScimFixture(t *testing.T) *scimFixture {
	t.Helper()
	identities := auth.NewMemoryStore()
	authSvc := auth.NewServiceWithStore("test-secret", identities)
	return &scimFixture{t: t, svc: NewService(identities), identities: identities, authSvc: authSvc}
}

func (f *scimFixture) seedOrg(id, name string) {
	f.t.Helper()
	if err := f.identities.CreateOrganization(context.Background(), &auth.Organization{ID: id, Name: name}); err != nil {
		f.t.Fatalf("CreateOrganization: %v", err)
	}
}

func TestSCIMCreateTokenMintsSecretAndHashesIt(t *testing.T) {
	f := newScimFixture(t)
	ctx := context.Background()

	token, secret, err := f.svc.CreateToken(ctx, "org-1", "owner-1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) {
		t.Fatalf("secret %q must carry the %q prefix", secret, TokenPrefix)
	}
	if len(secret) != len(TokenPrefix)+64 {
		t.Fatalf("secret %q must be 32 random bytes hex", secret)
	}
	if token.TokenHash != HashToken(secret) {
		t.Fatal("stored hash must be the SHA-256 hex of the secret")
	}
	if len(token.TokenHash) != 64 || strings.ContainsAny(token.TokenHash, "ghijklmnopqrstuvwxyz") {
		t.Fatalf("token hash %q is not sha256 hex", token.TokenHash)
	}
	if strings.Contains(token.TokenHash, secret) || token.TokenHash == secret {
		t.Fatal("plaintext secret must never be stored as the hash")
	}
	if token.OrgID != "org-1" || token.CreatedBy != "owner-1" || token.RevokedAt != (time.Time{}) {
		t.Fatalf("unexpected token metadata %+v", token)
	}

	// Round-trip: the minted secret authenticates to its tenant.
	got, err := f.svc.Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != token.ID || got.OrgID != "org-1" {
		t.Fatalf("unexpected token from authenticate: %+v", got)
	}

	// Unknown / malformed secrets are indistinguishable errors.
	for _, bad := range []string{"", "not-a-token", TokenPrefix + "deadbeef", secret + "x"} {
		if _, err := f.svc.Authenticate(ctx, bad); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("Authenticate(%q): expected ErrTokenInvalid, got %v", bad, err)
		}
	}
}

func TestSCIMCreateTokenValidation(t *testing.T) {
	f := newScimFixture(t)
	if _, _, err := f.svc.CreateToken(context.Background(), "", "u"); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("expected ErrOrgRequired, got %v", err)
	}
	if _, _, err := f.svc.CreateToken(context.Background(), "org-1", "  "); !errors.Is(err, ErrCreatorRequired) {
		t.Fatalf("expected ErrCreatorRequired, got %v", err)
	}
}

func TestSCIMRevokeTokenKillsAuthentication(t *testing.T) {
	f := newScimFixture(t)
	ctx := context.Background()
	token, secret, err := f.svc.CreateToken(ctx, "org-1", "owner-1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	// Revoking from a foreign org must not find the credential.
	if err := f.svc.RevokeToken(ctx, "other-org", token.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("expected ErrTokenNotFound for foreign org, got %v", err)
	}
	if err := f.svc.RevokeToken(ctx, "org-1", token.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if err := f.svc.RevokeToken(ctx, "org-1", token.ID); err != nil {
		t.Fatalf("repeated revoke must be idempotent in memory mode: %v", err)
	}
	if _, err := f.svc.Authenticate(ctx, secret); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("revoked token must not authenticate, got %v", err)
	}
}

// TestSCIMPostgresTokenHashingAtRest pins the storage contract with sqlmock:
// the INSERT argument is the SHA-256 hex of the secret (never the plaintext),
// the hash lookup drives authentication and revoked rows stop authenticating.
func TestSCIMPostgresTokenHashingAtRest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	pgStore, err := NewPostgresTokenStore(db)
	if err != nil {
		t.Fatalf("NewPostgresTokenStore: %v", err)
	}
	identities := auth.NewMemoryStore()
	svc := NewServiceWithStore(pgStore, identities)
	ctx := context.Background()

	// The INSERT bind values are captured with a custom sqlmock.Argument so
	// the test can assert the AT-REST contract on what the store actually
	// sent to the database: the third (token_hash) argument must be the
	// SHA-256 hex of the returned secret — never the plaintext.
	var capturedHash captureArg
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO scim_tokens (id, organization_id, token_hash, created_by, created_at) VALUES ($1, $2, $3, $4, $5)`)).
		WithArgs(sqlmock.AnyArg(), "org-1", &capturedHash, "owner-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	token, secret, err := svc.CreateToken(ctx, "org-1", "owner-1")
	if err != nil {
		t.Fatalf("CreateToken (mocked): %v", err)
	}
	if token.TokenHash != HashToken(secret) {
		t.Fatal("minting must record the SHA-256 hex of the returned secret")
	}
	if got, _ := capturedHash.value.(string); got != HashToken(secret) {
		t.Fatalf("INSERT must persist the SHA-256 hex of the secret, got %v", capturedHash.value)
	}
	if got, _ := capturedHash.value.(string); got == secret || strings.Contains(got, secret) {
		t.Fatal("plaintext secret must never be persisted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations after insert: %v", err)
	}

	// Authentication path: SELECT by the presented secret's hash.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, organization_id, token_hash, created_by, created_at, revoked_at FROM scim_tokens WHERE token_hash = $1`)).
		WithArgs(HashToken(secret)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "token_hash", "created_by", "created_at", "revoked_at"}).
			AddRow(token.ID, "org-1", HashToken(secret), "owner-1", time.Now().UTC(), nil))
	if got, err := svc.Authenticate(ctx, secret); err != nil || got.OrgID != "org-1" {
		t.Fatalf("Authenticate with pg store: %v (token=%+v)", err, got)
	}

	// Revocation then re-authentication: revoked_at stops the credential.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE scim_tokens SET revoked_at = NOW() WHERE id = $2 AND organization_id = $1 AND revoked_at IS NULL`)).
		WithArgs("org-1", token.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := svc.RevokeToken(ctx, "org-1", token.ID); err != nil {
		t.Fatalf("RevokeToken (mocked): %v", err)
	}
	revokedAt := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, organization_id, token_hash, created_by, created_at, revoked_at FROM scim_tokens WHERE token_hash = $1`)).
		WithArgs(HashToken(secret)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "token_hash", "created_by", "created_at", "revoked_at"}).
			AddRow(token.ID, "org-1", HashToken(secret), "owner-1", time.Now().UTC(), revokedAt))
	if _, err := svc.Authenticate(ctx, secret); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("revoked pg token must not authenticate, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSCIMCreateUserLifecycleAndFilter(t *testing.T) {
	f := newScimFixture(t)
	f.seedOrg("org-a", "Org A")
	ctx := context.Background()

	created, err := f.svc.CreateUser(ctx, "org-a", UserRequest{UserName: "New.Hire@OrgA.test"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.UserName != "new.hire@orga.test" {
		t.Fatalf("userName must be stored lowercased, got %q", created.UserName)
	}
	if !created.Active {
		t.Fatal("default active must be true")
	}
	if created.Schemas[0] != SchemaUser || created.Meta.ResourceType != "User" || created.ID == "" {
		t.Fatalf("unexpected resource shape: %+v", created)
	}
	if created.Meta.Location != "/scim/v2/Users/"+created.ID {
		t.Fatalf("unexpected meta.location %q", created.Meta.Location)
	}

	// The provisioned identity is MEMBER and passwordless (invite-pending).
	user, err := f.identities.GetUserByEmail(ctx, "new.hire@orga.test")
	if err != nil {
		t.Fatalf("identity not provisioned: %v", err)
	}
	if user.Role != "MEMBER" || user.PasswordHash != "" {
		t.Fatalf("SCIM users must be passwordless MEMBER, got %+v", user)
	}
	if _, err := f.authSvc.LoginCtx(ctx, "new.hire@orga.test", "whatever"); err == nil {
		t.Fatal("passwordless SCIM user must not password-login")
	}

	// Explicit active=false create (invited-but-disabled).
	disabled, err := f.svc.CreateUser(ctx, "org-a", UserRequest{UserName: "later@orga.test", Active: boolPtr(false)})
	if err != nil {
		t.Fatalf("CreateUser disabled: %v", err)
	}
	if disabled.Active {
		t.Fatal("explicit active=false must be honored")
	}

	// Duplicate email (any tenant) is a conflict.
	if _, err := f.svc.CreateUser(ctx, "org-a", UserRequest{UserName: "new.hire@orga.test"}); !errors.Is(err, ErrDuplicateUser) {
		t.Fatalf("expected ErrDuplicateUser, got %v", err)
	}

	// userName validation.
	for _, bad := range []string{"", "noemail", "a@@b.test", "a b@c.test", "  "} {
		if _, err := f.svc.CreateUser(ctx, "org-a", UserRequest{UserName: bad}); !errors.Is(err, ErrInvalidUserName) {
			t.Fatalf("CreateUser(%q): expected ErrInvalidUserName, got %v", bad, err)
		}
	}

	// Listing without filter returns both; empty orgs list as [] not null.
	list, err := f.svc.ListUsers(ctx, "org-a", "")
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if list.TotalResults != 2 || len(list.Resources) != 2 || list.Schemas[0] != SchemaListResponse {
		t.Fatalf("unexpected list response: total=%d resources=%d", list.TotalResults, len(list.Resources))
	}
	if list.StartIndex != 1 || list.ItemsPerPage != 2 {
		t.Fatalf("unexpected pagination envelope: %+v", list)
	}

	// userName eq filter is case-insensitive and exact.
	filtered, err := f.svc.ListUsers(ctx, "org-a", `userName eq "NEW.HIRE@ORGA.TEST"`)
	if err != nil {
		t.Fatalf("ListUsers filtered: %v", err)
	}
	if filtered.TotalResults != 1 || filtered.Resources[0].ID != created.ID {
		t.Fatalf("filter did not match exactly one user: %+v", filtered)
	}
	none, err := f.svc.ListUsers(ctx, "org-a", `userName eq "ghost@orga.test"`)
	if err != nil {
		t.Fatalf("ListUsers no match: %v", err)
	}
	if none.TotalResults != 0 || none.Resources == nil || len(none.Resources) != 0 {
		t.Fatal("empty listing must carry an empty (non-null) Resources array")
	}
	for _, bad := range []string{`userName pr`, `emails eq "x@y"`, `userName eq noquotes`, `and`, `userName ne "x"`} {
		if _, err := f.svc.ListUsers(ctx, "org-a", bad); !errors.Is(err, ErrInvalidFilter) {
			t.Fatalf("ListUsers(%q): expected ErrInvalidFilter, got %v", bad, err)
		}
	}

	// Point reads: tenant isolation without existence leak.
	got, err := f.svc.GetUser(ctx, "org-a", created.ID)
	if err != nil || got.UserName != "new.hire@orga.test" {
		t.Fatalf("GetUser: %v (%+v)", err, got)
	}
	if _, err := f.svc.GetUser(ctx, "org-b", created.ID); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("foreign-org read must be 404-equivalent, got %v", err)
	}
	if _, err := f.svc.GetUser(ctx, "org-a", "missing-id"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown id must be ErrUserNotFound, got %v", err)
	}
}

// TestSCIMPatchActiveBlocksPasswordLogin is the core deprovisioning flow:
// PATCH replace active=false blocks password login; true restores it.
func TestSCIMPatchActiveBlocksPasswordLogin(t *testing.T) {
	f := newScimFixture(t)
	ctx := context.Background()
	f.seedOrg("org-a", "Org A")
	local := f.seedLocalUser("org-a", "emp@orga.test", "s3cretpass")

	disable := PatchRequest{Schemas: []string{SchemaPatchOp}, Operations: []PatchOperation{
		{Op: "replace", Path: "active", Value: json.RawMessage("false")},
	}}
	patched, err := f.svc.PatchUser(ctx, "org-a", local.ID, disable)
	if err != nil {
		t.Fatalf("PatchUser disable: %v", err)
	}
	if patched.Active {
		t.Fatal("user must be disabled after patch")
	}
	if _, err := f.authSvc.LoginCtx(ctx, "emp@orga.test", "s3cretpass"); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("disabled user must fail login with ErrAccountDisabled, got %v", err)
	}

	// String-encoded boolean value (lenient per some IdP connectors) and
	// the pathless {"active": ...} form both reach the same result.
	for _, ops := range [][]PatchOperation{
		{{Op: "REPLACE", Path: "active", Value: json.RawMessage(`"true"`)}},
		{{Op: "replace", Value: json.RawMessage(`{"active": true}`)}},
	} {
		if _, err := f.svc.PatchUser(ctx, "org-a", local.ID, PatchRequest{Operations: ops}); err != nil {
			t.Fatalf("PatchUser ops %+v: %v", ops, err)
		}
		if _, err := f.authSvc.LoginCtx(ctx, "emp@orga.test", "s3cretpass"); err != nil {
			t.Fatalf("login after re-enable failed: %v", err)
		}
	}

	// Invalid operations are rejected BEFORE mutation.
	before, _ := f.svc.GetUser(ctx, "org-a", local.ID)
	for _, ops := range [][]PatchOperation{
		nil,
		{{Op: "add", Path: "active", Value: json.RawMessage("true")}},
		{{Op: "replace", Path: "userName", Value: json.RawMessage(`"x@y.test"`)}},
		{{Op: "replace", Path: "active", Value: json.RawMessage(`"maybe"`)}},
		{{Op: "replace", Path: "active", Value: json.RawMessage(`{"nope": 1}`)}},
	} {
		if _, err := f.svc.PatchUser(ctx, "org-a", local.ID, PatchRequest{Operations: ops}); !errors.Is(err, ErrInvalidPatch) {
			t.Fatalf("PatchUser ops %+v: expected ErrInvalidPatch, got %v", ops, err)
		}
	}
	after, _ := f.svc.GetUser(ctx, "org-a", local.ID)
	if after.Active != before.Active {
		t.Fatal("failed patch must not mutate the user")
	}

	// Tenant guard on patch.
	if _, err := f.svc.PatchUser(ctx, "org-b", local.ID, disable); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("foreign-org patch must be 404-equivalent, got %v", err)
	}
}

func TestSCIMReplaceUserSemantics(t *testing.T) {
	f := newScimFixture(t)
	ctx := context.Background()
	f.seedOrg("org-a", "Org A")
	local := f.seedLocalUser("org-a", "emp@orga.test", "s3cretpass")

	// PUT with the same userName (case differs) and active=false.
	replaced, err := f.svc.ReplaceUser(ctx, "org-a", local.ID, UserRequest{UserName: "EMP@ORGA.TEST", Active: boolPtr(false)})
	if err != nil {
		t.Fatalf("ReplaceUser: %v", err)
	}
	if replaced.Active {
		t.Fatal("PUT must honor active=false")
	}
	if _, err := f.authSvc.LoginCtx(ctx, "emp@orga.test", "s3cretpass"); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("PUT-disabled user must not password-login, got %v", err)
	}

	// Full-replace semantics: PUT without active resets to the default true.
	reset, err := f.svc.ReplaceUser(ctx, "org-a", local.ID, UserRequest{UserName: "emp@orga.test"})
	if err != nil {
		t.Fatalf("ReplaceUser without active: %v", err)
	}
	if !reset.Active {
		t.Fatal("PUT without active must apply full-replace default true")
	}

	// userName is immutable: a directory rename must not hijack the account.
	if _, err := f.svc.ReplaceUser(ctx, "org-a", local.ID, UserRequest{UserName: "renamed@orga.test"}); !errors.Is(err, ErrUserNameImmutable) {
		t.Fatalf("expected ErrUserNameImmutable, got %v", err)
	}
	unchanged, _ := f.svc.GetUser(ctx, "org-a", local.ID)
	if unchanged.UserName != "emp@orga.test" {
		t.Fatalf("userName changed unexpectedly: %q", unchanged.UserName)
	}

	// Foreign org and unknown id stay 404-equivalent.
	if _, err := f.svc.ReplaceUser(ctx, "org-b", local.ID, UserRequest{UserName: "emp@orga.test"}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("foreign-org replace must be 404-equivalent, got %v", err)
	}
}

// TestRequireSCIMTokenMiddleware exercises the dedicated-bearer guard.
func TestRequireSCIMTokenMiddleware(t *testing.T) {
	f := newScimFixture(t)
	ctx := context.Background()
	_, secret, err := f.svc.CreateToken(ctx, "org-1", "owner-1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	var seenOrg string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		org, err := OrgFromContext(r.Context())
		if err != nil {
			t.Errorf("OrgFromContext: %v", err)
			http.Error(w, "no org", http.StatusInternalServerError)
			return
		}
		seenOrg = org
		w.WriteHeader(http.StatusOK)
	})
	h := RequireSCIMToken(f.svc)(inner)

	// Missing / malformed / wrong-scheme headers are 401 SCIM errors.
	for _, header := range []string{"", "Bearer", "Basic abc", "Token " + secret, "Bearer "} {
		req := httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: expected 401, got %d", header, rr.Code)
		}
		var envelope map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("401 body is not JSON: %s", rr.Body.String())
		}
		if envelope["status"] != "401" {
			t.Fatalf("expected SCIM error envelope, got %s", rr.Body.String())
		}
	}

	// A live token authenticates and injects its org.
	req := httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || seenOrg != "org-1" {
		t.Fatalf("valid token: expected 200 + org, got %d org=%q", rr.Code, seenOrg)
	}

	// Session tokens and API keys are NOT honored on this surface:
	// any bearer secret that is not a live scim_ credential is 401.
	req = httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil)
	req.Header.Set("Authorization", "Bearer not-a-scim-token")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatal("non-SCIM bearer credentials must be rejected")
	}
}

func TestSCIMUserResourceNeverCarriesCredentialMaterial(t *testing.T) {
	resource := UserResourceFrom(&auth.User{
		ID: "u1", Organization: "org-1", Email: "u@x.test",
		PasswordHash: "$2a$10$supersecret", Role: "MEMBER", Active: true,
	})
	payload, err := json.Marshal(resource)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"password", "passwordHash", "password_hash", "$2a$10$", "role"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("SCIM resource leaked %q: %s", forbidden, payload)
		}
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, required := range []string{"schemas", "id", "userName", "active", "meta"} {
		if _, ok := body[required]; !ok {
			t.Fatalf("SCIM resource missing %q: %s", required, payload)
		}
	}
}

func TestSCIMPostgresTokenStoreNilDB(t *testing.T) {
	if _, err := NewPostgresTokenStore(nil); !errors.Is(err, ErrNilDB) {
		t.Fatalf("expected ErrNilDB, got %v", err)
	}
}

// seedLocalUser creates a password-enabled local identity inside orgID on
// the SHARED identity table, so auth.Service.LoginCtx (password path) and
// the SCIM Service (provisioning path) observe the same row.
func (f *scimFixture) seedLocalUser(orgID, email, password string) *auth.User {
	f.t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		f.t.Fatalf("HashPassword: %v", err)
	}
	user := &auth.User{
		ID:           "local-" + email,
		Organization: orgID,
		Email:        email,
		PasswordHash: hash,
		Role:         "MEMBER",
		Active:       true,
		CreatedAt:    time.Now().UTC(),
	}
	if err := f.identities.CreateUser(context.Background(), user); err != nil {
		f.t.Fatalf("CreateUser: %v", err)
	}
	return user
}

// captureArg is a sqlmock.Argument that accepts and records the bind value
// so tests can assert on exactly what the store sent to the database.
type captureArg struct {
	value driver.Value
}

// Match records the observed bind value and always accepts it.
func (c *captureArg) Match(v driver.Value) bool {
	c.value = v
	return true
}

func boolPtr(b bool) *bool {
	return &b
}
