package main

// Issue #76 handler tests — POST /auth/logout and POST /auth/refresh through
// the registered routes, with a REAL RequireAuthOrAPIKey-protected endpoint
// in the mux to prove a token's real-world death/survival (the middleware
// consults the same deny list the lifecycle handlers write). Identity lives
// in ONE shared MemoryStore (scim_test harness pattern) so the SCIM
// deprovisioning seam blocks refresh exactly like it blocks login. Postgres
// parity is pinned with sqlmock at the bottom (revoke upsert + sweep +
// audit bind order).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
)

type authLifecycleEnv struct {
	mux        *http.ServeMux
	authSvc    *auth.Service
	identities *auth.MemoryStore
	auditSvc   *audit.Service
	ownerToken string
	betaToken  string
}

func newAuthLifecycleEnv(t *testing.T) *authLifecycleEnv {
	t.Helper()
	identities := auth.NewMemoryStore()
	authSvc := auth.NewServiceWithStore("test-secret", identities)
	auditSvc := audit.NewService()
	apiKeysSvc := apikeys.NewService()

	ctx := t.Context()
	for _, org := range []struct{ id, name string }{{"org-1", "Acme"}, {"org-2", "Beta"}} {
		if err := identities.CreateOrganization(ctx, &auth.Organization{ID: org.id, Name: org.name}); err != nil {
			t.Fatalf("CreateOrganization(%s): %v", org.id, err)
		}
	}
	hash, err := auth.HashPassword("secret123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := identities.CreateUser(ctx, &auth.User{ID: "user-1", Organization: "org-1", Email: "owner@acme.test", PasswordHash: hash, Role: "OWNER", Active: true}); err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	if err := identities.CreateUser(ctx, &auth.User{ID: "user-2", Organization: "org-2", Email: "owner@beta.test", PasswordHash: hash, Role: "OWNER", Active: true}); err != nil {
		t.Fatalf("CreateUser(beta): %v", err)
	}

	// Tokens obtained through the REAL login path (store lookup + bcrypt +
	// active check).
	ownerToken, err := authSvc.LoginCtx(ctx, "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("LoginCtx(owner): %v", err)
	}
	betaToken, err := authSvc.LoginCtx(ctx, "owner@beta.test", "secret123")
	if err != nil {
		t.Fatalf("LoginCtx(beta): %v", err)
	}

	mux := http.NewServeMux()
	registerAuthLifecycleRoutes(mux, authSvc, auditSvc)
	// A protected endpoint behind the REAL middleware chain: after logout or
	// rotation the old token must be rejected exactly like in production.
	mux.Handle("/protected", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	return &authLifecycleEnv{mux: mux, authSvc: authSvc, identities: identities, auditSvc: auditSvc, ownerToken: ownerToken, betaToken: betaToken}
}

func (e *authLifecycleEnv) do(t *testing.T, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// logoutBody decodes a structured error envelope (empty for 204s).
func logoutError(t *testing.T, rr *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body is not a structured error envelope: %q", rr.Body.String())
	}
	return decoded.Error.Code, decoded.Error.Message
}

func TestLogoutThenReusedTokenIsRejected(t *testing.T) {
	env := newAuthLifecycleEnv(t)

	rr := env.do(t, http.MethodPost, "/auth/logout", env.ownerToken)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("logout: expected 204, got %d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("204 must carry no body, got %q", rr.Body.String())
	}
	// Reuse through the REAL middleware chain: the deny list kills it.
	if rr := env.do(t, http.MethodGet, "/protected", env.ownerToken); rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token must fail the middleware chain, got %d", rr.Code)
	}
	// Reuse through refresh: single-use surface, distinct error code.
	rr = env.do(t, http.MethodPost, "/auth/refresh", env.ownerToken)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("refresh of a revoked token: expected 401, got %d", rr.Code)
	}
	if code, _ := logoutError(t, rr); code != "TOKEN_REVOKED" {
		t.Fatalf("expected TOKEN_REVOKED, got %q", code)
	}
	// The other tenant's token is untouched.
	if rr := env.do(t, http.MethodGet, "/protected", env.betaToken); rr.Code != http.StatusOK {
		t.Fatalf("foreign token must keep working after a logout, got %d", rr.Code)
	}
}

func TestLogoutIdempotentAndCredentialMatrix(t *testing.T) {
	env := newAuthLifecycleEnv(t)

	if rr := env.do(t, http.MethodPost, "/auth/logout", env.ownerToken); rr.Code != http.StatusNoContent {
		t.Fatalf("first logout: expected 204, got %d", rr.Code)
	}
	// Logout of an already-revoked token: idempotent 204 (issue contract).
	if rr := env.do(t, http.MethodPost, "/auth/logout", env.ownerToken); rr.Code != http.StatusNoContent {
		t.Fatalf("repeat logout: expected 204, got %d", rr.Code)
	}
	// Missing / malformed credentials.
	if rr := env.do(t, http.MethodPost, "/auth/logout", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer: expected 401, got %d", rr.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("non-bearer scheme: expected 401, got %d", rr.Code)
	}
	if code, _ := logoutError(t, rr); code != "UNAUTHORIZED" {
		t.Fatalf("missing credential must be UNAUTHORIZED, got %q", code)
	}
	// Garbage bearer: the only logout failure (invalid signature).
	if rr := env.do(t, http.MethodPost, "/auth/logout", "garbage.token.here"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("garbage token: expected 401, got %d", rr.Code)
	}
	// API keys are NOT session tokens: the lifecycle surface refuses them
	// even though they authenticate elsewhere.
	if err := env.identities.CreateOrganization(t.Context(), &auth.Organization{ID: "org-demo", Name: "Demo"}); err != nil {
		t.Fatalf("CreateOrganization(org-demo): %v", err)
	}
	key, err := apikeys.NewService().Create("org-demo", "dev-user", "dev-key")
	if err != nil {
		t.Fatalf("api key mint: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("X-API-Key", key.Value)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("API key must not authenticate the lifecycle surface, got %d", rr.Code)
	}
}

func TestRefreshRotationEndToEnd(t *testing.T) {
	env := newAuthLifecycleEnv(t)

	rr := env.do(t, http.MethodPost, "/auth/refresh", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var decoded struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("refresh body is not JSON: %s", rr.Body.String())
	}
	if decoded.Token == "" || decoded.Token == env.ownerToken {
		t.Fatal("refresh must return a NEW token")
	}
	// Old token dead in the middleware chain; replay refresh refused.
	if rr := env.do(t, http.MethodGet, "/protected", env.ownerToken); rr.Code != http.StatusUnauthorized {
		t.Fatalf("rotated-away token must fail the middleware chain, got %d", rr.Code)
	}
	if rr := env.do(t, http.MethodPost, "/auth/refresh", env.ownerToken); rr.Code != http.StatusUnauthorized {
		t.Fatalf("single-use replay: expected 401, got %d", rr.Code)
	}
	// New token works everywhere, keeps the same principal, and carries a
	// rotated jti with a full fresh window.
	if rr := env.do(t, http.MethodGet, "/protected", decoded.Token); rr.Code != http.StatusOK {
		t.Fatalf("fresh token must work, got %d", rr.Code)
	}
	newClaims, err := env.authSvc.ValidateToken(decoded.Token)
	if err != nil {
		t.Fatalf("ValidateToken(fresh): %v", err)
	}
	oldClaims, err := env.authSvc.ParseToken(env.ownerToken)
	if err != nil {
		t.Fatalf("ParseToken(old): %v", err)
	}
	if newClaims.UserID != oldClaims.UserID || newClaims.OrganizationID != "org-1" {
		t.Fatalf("fresh claims must keep the principal: %+v", newClaims)
	}
	if newClaims.JTI == "" || newClaims.JTI == oldClaims.JTI {
		t.Fatalf("jti must rotate: %q vs %q", oldClaims.JTI, newClaims.JTI)
	}
	if newClaims.Exp <= time.Now().Add(23*time.Hour).Unix() {
		t.Fatalf("fresh token must carry a full ~24h window, exp=%d", newClaims.Exp)
	}
	// Rotation chains: the fresh token can be refreshed again.
	if rr := env.do(t, http.MethodPost, "/auth/refresh", decoded.Token); rr.Code != http.StatusOK {
		t.Fatalf("chained refresh: expected 200, got %d", rr.Code)
	}
}

func TestRefreshBlockedForDeprovisionedUser(t *testing.T) {
	env := newAuthLifecycleEnv(t)

	// The SAME SCIM deprovisioning seam (SetUserActive) that blocks password
	// login must block refresh (issue #76 requirement).
	if err := env.identities.SetUserActive(t.Context(), "org-1", "user-1", false); err != nil {
		t.Fatalf("SetUserActive: %v", err)
	}
	if _, err := env.authSvc.LoginCtx(t.Context(), "owner@acme.test", "secret123"); err == nil {
		t.Fatal("parity check: deprovisioned login must fail")
	}
	rr := env.do(t, http.MethodPost, "/auth/refresh", env.ownerToken)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("deprovisioned refresh: expected 401, got %d", rr.Code)
	}
	if code, _ := logoutError(t, rr); code != "ACCOUNT_DISABLED" {
		t.Fatalf("expected ACCOUNT_DISABLED, got %q", code)
	}
	// Middleware semantics unchanged (issue #76 scope): the still-unexpired
	// token remains valid on ordinary endpoints until natural expiry or
	// logout — deprovisioning blocks login/refresh, exactly as before #76.
	if rr := env.do(t, http.MethodGet, "/protected", env.ownerToken); rr.Code != http.StatusOK {
		t.Fatalf("live token validity must be untouched by a failed refresh, got %d", rr.Code)
	}
}

func TestCrossTenantIsolation(t *testing.T) {
	env := newAuthLifecycleEnv(t)

	// Logout of the org-1 token must not affect the org-2 token.
	if rr := env.do(t, http.MethodPost, "/auth/logout", env.ownerToken); rr.Code != http.StatusNoContent {
		t.Fatalf("logout: expected 204, got %d", rr.Code)
	}
	if rr := env.do(t, http.MethodGet, "/protected", env.betaToken); rr.Code != http.StatusOK {
		t.Fatalf("org-2 token must survive org-1 logout, got %d", rr.Code)
	}
	// Refresh re-derives the tenant from the CURRENT identity row, never
	// from anything client-controlled.
	rr := env.do(t, http.MethodPost, "/auth/refresh", env.betaToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("org-2 refresh: expected 200, got %d", rr.Code)
	}
	var decoded struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("refresh body is not JSON: %s", rr.Body.String())
	}
	claims, err := env.authSvc.ValidateToken(decoded.Token)
	if err != nil {
		t.Fatalf("ValidateToken(fresh beta): %v", err)
	}
	if claims.OrganizationID != "org-2" || claims.UserID != "user-2" {
		t.Fatalf("refreshed token must keep its tenant: %+v", claims)
	}
	// Audit trails stay org-scoped.
	org1, err := env.auditSvc.ListCtx(t.Context(), "org-1")
	if err != nil {
		t.Fatalf("ListCtx(org-1): %v", err)
	}
	if len(org1) != 1 || org1[0].Action != "auth.logout" {
		t.Fatalf("org-1 trail must hold exactly the logout row, got %+v", org1)
	}
	org2, err := env.auditSvc.ListCtx(t.Context(), "org-2")
	if err != nil {
		t.Fatalf("ListCtx(org-2): %v", err)
	}
	if len(org2) != 1 || org2[0].Action != "auth.refresh" {
		t.Fatalf("org-2 trail must hold exactly the refresh row, got %+v", org2)
	}
}

func TestLifecycleAuditTrailShape(t *testing.T) {
	env := newAuthLifecycleEnv(t)

	if rr := env.do(t, http.MethodPost, "/auth/logout", env.ownerToken); rr.Code != http.StatusNoContent {
		t.Fatalf("logout: expected 204, got %d", rr.Code)
	}
	rr := env.do(t, http.MethodPost, "/auth/refresh", env.betaToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh: expected 200, got %d", rr.Code)
	}
	entries, err := env.auditSvc.ListCtx(t.Context(), "org-2")
	if err != nil {
		t.Fatalf("ListCtx: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one refresh audit row, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Action != "auth.refresh" || entry.Actor != "user-2" || entry.OrganizationID != "org-2" {
		t.Fatalf("refresh audit row mismatch: %+v", entry)
	}
	if !strings.HasPrefix(entry.Resource, "auth/tokens/") {
		t.Fatalf("refresh audit resource must reference the token, got %q", entry.Resource)
	}
	oldRef, _ := entry.Metadata["old_token_ref"].(string)
	newRef, _ := entry.Metadata["new_token_ref"].(string)
	if !strings.HasPrefix(oldRef, "jti:") || !strings.HasPrefix(newRef, "jti:") || oldRef == newRef {
		t.Fatalf("refresh audit must record distinct rotated refs, got %q -> %q", oldRef, newRef)
	}
	// No token material anywhere in the trail.
	for _, entry := range append(env.auditSvc.List(), entries...) {
		blob, _ := json.Marshal(entry)
		if strings.Contains(string(blob), env.betaToken) || strings.Contains(string(blob), env.ownerToken) {
			t.Fatalf("audit row leaks token material: %s", blob)
		}
	}
}

func TestLifecycleRoutesMethodMatrix(t *testing.T) {
	env := newAuthLifecycleEnv(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/auth/logout"},
		{http.MethodDelete, "/auth/logout"},
		{http.MethodGet, "/auth/refresh"},
		{http.MethodPut, "/auth/refresh"},
	} {
		if rr := env.do(t, tc.method, tc.path, env.ownerToken); rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: expected 405, got %d", tc.method, tc.path, rr.Code)
		}
	}
}

// TestLogoutPostgresMode pins the Postgres deny-list parity: the logout
// writes the revocation upsert + expiry sweep, then the audit row; the
// subsequent validation consults the deny list and refuses the token.
func TestLogoutPostgresMode(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	authSvc := auth.NewServiceWithStore("test-secret", auth.NewPostgresStore(db))
	authSvc.SetRevocationStore(auth.NewPostgresRevocationStore(db))
	auditSvc := audit.NewServiceWithStore(audit.NewPostgresStore(db))

	token, err := authSvc.GenerateToken(&auth.User{ID: "user-1", Organization: "org-1", Email: "owner@acme.test", Role: "OWNER", Active: true})
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	mux := http.NewServeMux()
	registerAuthLifecycleRoutes(mux, authSvc, auditSvc)

	// Order of statements: revoke upsert -> expired-row sweep -> audit row.
	mock.ExpectExec("INSERT INTO auth_revocations").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM auth_revocations WHERE expires_at").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO audit_logs").
		WithArgs(sqlmock.AnyArg(), "org-1", "user-1", "auth.logout", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PG logout: expected 204, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Validation now consults the durable deny list.
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	if _, err := authSvc.ValidateToken(token); err == nil {
		t.Fatal("PG deny list must refuse the logged-out token")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestRefreshPostgresMode pins the full refresh statement order in durable
// mode: deny-list lookup, CURRENT identity re-read, revoke upsert + sweep,
// audit row — and a rotated token on the wire.
func TestRefreshPostgresMode(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	authSvc := auth.NewServiceWithStore("test-secret", auth.NewPostgresStore(db))
	authSvc.SetRevocationStore(auth.NewPostgresRevocationStore(db))
	auditSvc := audit.NewServiceWithStore(audit.NewPostgresStore(db))

	token, err := authSvc.GenerateToken(&auth.User{ID: "user-1", Organization: "org-1", Email: "owner@acme.test", Role: "OWNER", Active: true})
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	mux := http.NewServeMux()
	registerAuthLifecycleRoutes(mux, authSvc, auditSvc)

	// ValidateToken consults the deny list first (not revoked).
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// The identity is re-read from the CURRENT row (org guard not needed:
	// the lookup is by globally-unique email, like login).
	mock.ExpectQuery("FROM users WHERE email").
		WithArgs("owner@acme.test").
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "email", "password_hash", "role", "created_at", "sso_subject", "active"}).
			AddRow("user-1", "org-1", "owner@acme.test", "irrelevant-hash", "OWNER", time.Now().UTC(), "", true))
	// Rotation revokes the old token, then sweeps expired rows.
	mock.ExpectExec("INSERT INTO auth_revocations").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM auth_revocations WHERE expires_at").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Org-scoped audit row for the rotation.
	mock.ExpectExec("INSERT INTO audit_logs").
		WithArgs(sqlmock.AnyArg(), "org-1", "user-1", "auth.refresh", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PG refresh: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var decoded struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("refresh body is not JSON: %s", rr.Body.String())
	}
	if decoded.Token == "" || decoded.Token == token {
		t.Fatal("PG refresh must rotate to a new token")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
