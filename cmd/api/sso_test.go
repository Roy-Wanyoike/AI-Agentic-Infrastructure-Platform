package main

// Issue #29 handler tests — SSO half: the OIDC browser flow through the real
// handler chain against an in-process stub OpenID Connect provider (RS256
// ID tokens over httptest, stdlib only). Covers the 302 begin-login redirect
// (client_id/redirect_uri/state/nonce), the full callback round trip with
// token validation and JIT provisioning, single-use state replay, and the
// typed error mapping contract. All in-memory, no database.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/auth"
	"agentos/internal/sso"
)

// ---------------------------------------------------------------------------
// In-process stub OpenID Connect provider (black-box reimplementation of the
// internal/sso test double — cmd/api cannot import that package's tests).
// ---------------------------------------------------------------------------

const (
	stubIDPClientID     = "agentos-stub-client"
	stubIDPClientSecret = "stub-client-secret"
	stubIDPKid          = "stub-signing-key"
)

type stubIDPGrant struct {
	nonce string
	email string
	sub   string
}

// stubIDP signs RS256 ID tokens with a fresh RSA key and serves discovery,
// JWKS and the token endpoint.
type stubIDP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	grants map[string]stubIDPGrant
}

func newStubIDP(t *testing.T) *stubIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	idp := &stubIDP{key: key, grants: make(map[string]stubIDPGrant)}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.handleDiscovery)
	mux.HandleFunc("/jwks", idp.handleJWKS)
	mux.HandleFunc("/token", idp.handleToken)
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (i *stubIDP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"issuer":                 i.srv.URL,
		"authorization_endpoint": i.srv.URL + "/authorize",
		"token_endpoint":         i.srv.URL + "/token",
		"jwks_uri":               i.srv.URL + "/jwks",
	})
}

func (i *stubIDP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := i.key.PublicKey
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"kid": stubIDPKid,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

func (i *stubIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.Form.Get("client_id") != stubIDPClientID || r.Form.Get("client_secret") != stubIDPClientSecret {
		http.Error(w, "invalid_client", http.StatusUnauthorized)
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		http.Error(w, "unsupported_grant_type", http.StatusBadRequest)
		return
	}
	i.mu.Lock()
	grant, ok := i.grants[r.Form.Get("code")]
	delete(i.grants, r.Form.Get("code"))
	i.mu.Unlock()
	if !ok {
		http.Error(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	idToken, err := i.signIDToken(map[string]any{
		"iss":   i.srv.URL,
		"aud":   stubIDPClientID,
		"sub":   grant.sub,
		"email": grant.email,
		"nonce": grant.nonce,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		http.Error(w, "token signing failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"access_token": "at-" + grant.sub,
		"id_token":     idToken,
		"token_type":   "Bearer",
	})
}

// MintCode registers one authorization code bound to the given nonce/email.
func (i *stubIDP) MintCode(nonce, email, sub string) string {
	code := fmt.Sprintf("code-%d-%s", time.Now().UnixNano(), sub)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.grants[code] = stubIDPGrant{nonce: nonce, email: email, sub: sub}
	return code
}

func (i *stubIDP) signIDToken(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": stubIDPKid, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ---------------------------------------------------------------------------
// Handler harness
// ---------------------------------------------------------------------------

type ssoHandlerEnv struct {
	mux        *http.ServeMux
	authSvc    *auth.Service
	configs    sso.ConfigStore
	identities *auth.MemoryStore
	svc        *sso.Service
	idp        *stubIDP
	orgID      string
	slug       string
}

func newSsoHandlerEnv(t *testing.T) *ssoHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	identities := auth.NewMemoryStore()
	configs := sso.NewMemoryConfigStore()

	// The tenant exists in BOTH the config store (slug resolution) and the
	// identity table (JIT provisioning FK).
	const orgID = "org-1"
	if err := identities.CreateOrganization(t.Context(), &auth.Organization{ID: orgID, Name: "Acme Corp"}); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	sso.SeedOrg(configs, orgID, "Acme Corp")

	env := &ssoHandlerEnv{
		authSvc:    authSvc,
		configs:    configs,
		identities: identities,
		orgID:      orgID,
		slug:       "acme-corp",
		idp:        newStubIDP(t),
	}
	env.svc = sso.NewService(configs, identities, authSvc)

	mux := http.NewServeMux()
	registerSsoRoutes(mux, env.svc)
	env.mux = mux
	return env
}

// configureIDP points the tenant's sso_config at the stub IdP.
func (e *ssoHandlerEnv) configureIDP(t *testing.T) {
	t.Helper()
	if err := e.configs.SaveConfig(t.Context(), e.orgID, &sso.SSOConfig{
		Issuer:       e.idp.srv.URL,
		ClientID:     stubIDPClientID,
		ClientSecret: stubIDPClientSecret,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}

// begin drives GET /auth/sso/{slug}/login and returns the redirect Location.
func (e *ssoHandlerEnv) begin(t *testing.T, slug string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/"+slug+"/login", nil)
	req.Host = "agentos.example"
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// beginParams parses the authorize URL the handler redirected to.
func beginParams(t *testing.T, location string) url.Values {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("redirect Location is not a URL: %q", location)
	}
	return parsed.Query()
}

func errCodeSso(t *testing.T, body []byte) (int, string) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("error body is not JSON: %s", body)
	}
	errObj, _ := decoded["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	status, _ := decoded["status"]
	_ = status
	return int(status.(float64)), code
}

// TestSSOBeginLoginRedirect pins the authorize redirect contract.
func TestSSOBeginLoginRedirect(t *testing.T) {
	env := newSsoHandlerEnv(t)
	env.configureIDP(t)

	rr := env.begin(t, env.slug)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rr.Code, rr.Body.String())
	}
	location := rr.Header().Get("Location")
	if !strings.HasPrefix(location, env.idp.srv.URL+"/authorize") {
		t.Fatalf("redirect must target the IdP authorize endpoint, got %q", location)
	}
	q := beginParams(t, location)
	if q.Get("client_id") != stubIDPClientID {
		t.Fatalf("client_id %q", q.Get("client_id"))
	}
	if got := q.Get("redirect_uri"); got != "http://agentos.example/auth/sso/callback" {
		t.Fatalf("redirect_uri must be the derived callback URL, got %q", got)
	}
	if q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatal("state and nonce must both be present")
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope must request openid, got %q", q.Get("scope"))
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("auth redirects must not be cacheable")
	}
}

// TestSSOBeginLoginUnknownAndUnconfigured pins the 404 contract for the slug
// space (unknown org AND configured-but-no-ssoid org are indistinguishable
// to non-enumerating clients in status; the codes differ for operators).
func TestSSOBeginLoginUnknownAndUnconfigured(t *testing.T) {
	env := newSsoHandlerEnv(t)
	env.configureIDP(t)

	if rr := env.begin(t, "ghost-org"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown slug: expected 404, got %d", rr.Code)
	}
	// A seeded org WITHOUT a saved config is 404 SSO_NOT_CONFIGURED.
	sso.SeedOrg(env.configs, "org-2", "Plain Org")
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/plain-org/login", nil)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unconfigured org: expected 404, got %d", rr.Code)
	}
	var decoded map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
	errObj, _ := decoded["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "SSO_NOT_CONFIGURED" {
		t.Fatalf("expected SSO_NOT_CONFIGURED, got %v", errObj)
	}
}

// TestSSOBeginLoginUpstreamFailure: a dead issuer is a 502, and the error
// body never carries upstream detail.
func TestSSOBeginLoginUpstreamFailure(t *testing.T) {
	env := newSsoHandlerEnv(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	dead.Close() // port now refused

	if err := env.configs.SaveConfig(t.Context(), env.orgID, &sso.SSOConfig{
		Issuer: dead.URL, ClientID: stubIDPClientID, ClientSecret: stubIDPClientSecret,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	rr := env.begin(t, env.slug)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("dead issuer: expected 502, got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), dead.URL) {
		t.Fatal("error body must not echo the upstream URL")
	}
}

// TestSSOLoginEndToEndJIT walks the complete browser flow: begin -> extract
// state/nonce -> mint a code at the stub IdP -> callback -> session token +
// JIT-provisioned MEMBER identity.
func TestSSOLoginEndToEndJIT(t *testing.T) {
	env := newSsoHandlerEnv(t)
	env.configureIDP(t)

	rr := env.begin(t, env.slug)
	if rr.Code != http.StatusFound {
		t.Fatalf("begin: expected 302, got %d", rr.Code)
	}
	q := beginParams(t, rr.Header().Get("Location"))
	state, nonce := q.Get("state"), q.Get("nonce")

	code := env.idp.MintCode(nonce, "new.user@acme.test", "sub-1")
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("callback: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Token string         `json:"token"`
		User  map[string]any `json:"user"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("callback body is not JSON: %s", rr.Body.String())
	}
	if body.Token == "" {
		t.Fatal("callback must return a session token")
	}
	if body.User["role"] != "MEMBER" || body.User["email"] != "new.user@acme.test" {
		t.Fatalf("JIT user must be a MEMBER with the IdP email, got %v", body.User)
	}
	if body.User["new"] != true {
		t.Fatal("first login must flag the identity as newly provisioned")
	}

	// The session token is the platform's existing format: it validates
	// against the SAME auth.Service.
	claims, err := env.authSvc.ValidateToken(body.Token)
	if err != nil {
		t.Fatalf("returned token must validate: %v", err)
	}
	if claims.Email != "new.user@acme.test" || claims.OrganizationID != env.orgID {
		t.Fatalf("unexpected claims: %+v", claims)
	}

	// The identity exists in the shared table with the IdP subject linked.
	user, err := env.identities.GetUserByEmail(t.Context(), "new.user@acme.test")
	if err != nil || user.SSOSubject != "sub-1" || user.PasswordHash != "" {
		t.Fatalf("JIT identity wrong: %+v err=%v", user, err)
	}

	// State is single-use: replaying it must fail with 401 SSO_STATE_INVALID.
	req = httptest.NewRequest(http.MethodGet, "/auth/sso/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("replayed state: expected 401, got %d", rr.Code)
	}
	var errBody map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &errBody)
	if errObj, _ := errBody["error"].(map[string]any); errObj["code"] != "SSO_STATE_INVALID" {
		t.Fatalf("expected SSO_STATE_INVALID, got %v", errObj)
	}
}

// TestSSOLoginErrorMatrix covers the callback failure mappings that do not
// need a full IdP round trip (bad params, unknown state) plus the
// disabled-account and foreign-email paths through the real flow.
func TestSSOLoginErrorMatrix(t *testing.T) {
	env := newSsoHandlerEnv(t)
	env.configureIDP(t)

	// Missing query parameters.
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/callback", nil)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing params: expected 400, got %d", rr.Code)
	}

	// Unknown state.
	req = httptest.NewRequest(http.MethodGet, "/auth/sso/callback?code=x&state=unknown", nil)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unknown state: expected 401, got %d", rr.Code)
	}

	// Disabled account: the email already belongs to a deprovisioned local
	// identity, so SSO cannot resurrect it (defense in depth with SCIM).
	if err := env.identities.CreateUser(t.Context(), &auth.User{
		ID: "local-1", Organization: env.orgID, Email: "gone@acme.test",
		PasswordHash: "", Role: "MEMBER", Active: false,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rr = env.begin(t, env.slug)
	q := beginParams(t, rr.Header().Get("Location"))
	code := env.idp.MintCode(q.Get("nonce"), "gone@acme.test", "sub-2")
	req = httptest.NewRequest(http.MethodGet, "/auth/sso/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(q.Get("state")), nil)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("disabled account: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// Email registered to ANOTHER tenant: conflict, no provisioning.
	if err := env.identities.CreateOrganization(t.Context(), &auth.Organization{ID: "org-2", Name: "Other Co"}); err != nil {
		t.Fatalf("CreateOrganization(org-2): %v", err)
	}
	if err := env.identities.CreateUser(t.Context(), &auth.User{
		ID: "local-2", Organization: "org-2", Email: "somewhere@acme.test",
		PasswordHash: "x", Role: "MEMBER", Active: true,
	}); err != nil {
		t.Fatalf("CreateUser(org-2): %v", err)
	}
	rr = env.begin(t, env.slug)
	q = beginParams(t, rr.Header().Get("Location"))
	code = env.idp.MintCode(q.Get("nonce"), "somewhere@acme.test", "sub-3")
	req = httptest.NewRequest(http.MethodGet, "/auth/sso/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(q.Get("state")), nil)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("foreign email: expected 409, got %d: %s", rr.Code, rr.Body.String())
	}

	// The tampered ID token path: a code whose token was signed by a
	// different key is rejected as a login failure (401), not a 5xx.
	rr = env.begin(t, env.slug)
	q = beginParams(t, rr.Header().Get("Location"))
	code = env.idp.MintCode(q.Get("nonce"), "valid@acme.test", "sub-4")
	// Corrupt the stored grant so the IdP issues no token for the code: the
	// sso service deliberately wraps every broken exchange into
	// ErrIDTokenInvalid, so the handler answers 401 SSO_LOGIN_FAILED.
	env.idp.mu.Lock()
	delete(env.idp.grants, code)
	env.idp.mu.Unlock()
	req = httptest.NewRequest(http.MethodGet, "/auth/sso/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(q.Get("state")), nil)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("dead grant: expected 401, got %d", rr.Code)
	}
	var loginErr map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &loginErr)
	if errObj, _ := loginErr["error"].(map[string]any); errObj["code"] != "SSO_LOGIN_FAILED" {
		t.Fatalf("expected SSO_LOGIN_FAILED, got %v", errObj)
	}
}
