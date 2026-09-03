package sso

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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
)

// ---------------------------------------------------------------------------
// In-process stub OpenID Connect provider (httptest): discovery document,
// JWKS and token endpoint over a generated RSA key. Everything is stdlib.
// ---------------------------------------------------------------------------

const (
	stubClientID     = "agentos-stub-client"
	stubClientSecret = "stub-client-secret"
	stubKid          = "stub-signing-key"
)

// stubIDP is a minimal IdP: it signs RS256 ID tokens with a fresh RSA key and
// serves the three endpoints the OIDC client consumes.
type stubIDP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu       sync.Mutex
	codes    map[string]codeGrant // authorization code -> grant
	sniffs   map[string]int       // path prefix -> fetch counter
	authHits int
}

type codeGrant struct {
	nonce   string
	email   string
	sub     string
	aud     string
	issuer  string
	expUnix int64
	noEmail bool
}

func newStubIDP(t *testing.T) *stubIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	idp := &stubIDP{
		t:      t,
		key:    key,
		codes:  make(map[string]codeGrant),
		sniffs: make(map[string]int),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.handleDiscovery)
	mux.HandleFunc("/jwks", idp.handleJWKS)
	mux.HandleFunc("/token", idp.handleToken)
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (i *stubIDP) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	i.sniff("/.well-known")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(discoveryDocument{
		Issuer:                i.srv.URL,
		AuthorizationEndpoint: i.srv.URL + "/authorize",
		TokenEndpoint:         i.srv.URL + "/token",
		JWKSURI:               i.srv.URL + "/jwks",
	})
}

func (i *stubIDP) handleJWKS(w http.ResponseWriter, r *http.Request) {
	i.sniff("/jwks")
	pub := i.key.PublicKey
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jwksDocument{Keys: []jsonWebKey{{
		Kty: "RSA",
		Kid: stubKid,
		Use: "sig",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (i *stubIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	i.sniff("/token")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.Form.Get("client_secret") != stubClientSecret || r.Form.Get("client_id") != stubClientID {
		http.Error(w, "invalid_client", http.StatusUnauthorized)
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		http.Error(w, "unsupported_grant_type", http.StatusBadRequest)
		return
	}
	i.mu.Lock()
	grant, ok := i.codes[r.Form.Get("code")]
	delete(i.codes, r.Form.Get("code"))
	i.mu.Unlock()
	if !ok {
		http.Error(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	issuer := grant.issuer
	if issuer == "" {
		issuer = i.srv.URL
	}
	aud := grant.aud
	if aud == "" {
		aud = stubClientID
	}
	exp := grant.expUnix
	if exp == 0 {
		exp = time.Now().Add(time.Hour).Unix()
	}
	claims := map[string]any{
		"iss":   issuer,
		"sub":   grant.sub,
		"aud":   aud,
		"exp":   exp,
		"iat":   time.Now().Unix(),
		"nonce": grant.nonce,
	}
	if !grant.noEmail {
		claims["email"] = grant.email
	}
	token := i.sign(claims)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"access_token": "at-" + grant.sub,
		"id_token":     token,
		"token_type":   "Bearer",
	})
}

func (i *stubIDP) sniff(path string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.sniffs[path]++
}

// issueCode simulates the user authenticating at the IdP: it binds a fresh
// authorization code to the nonce from the authorize URL (a real IdP echoes
// the nonce into the ID token).
func (i *stubIDP) issueCode(authorizeURL string, grant codeGrant) (code, state string) {
	i.t.Helper()
	u, err := url.Parse(authorizeURL)
	if err != nil {
		i.t.Fatalf("parse authorize URL: %v", err)
	}
	q := u.Query()
	if grant.nonce == "" {
		grant.nonce = q.Get("nonce")
	}
	state = q.Get("state")
	code = "code-" + grant.nonce[:8]
	i.mu.Lock()
	defer i.mu.Unlock()
	i.authHits++
	i.codes[code] = grant
	return code, state
}

// sign builds a compact RS256 JWS with the stub kid.
func (i *stubIDP) sign(claims map[string]any) string {
	i.t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": stubKid, "typ": "JWT"})
	if err != nil {
		i.t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		i.t.Fatalf("marshal claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		i.t.Fatalf("sign id token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// fetchCount returns how often an endpoint was fetched (cache assertions).
func (i *stubIDP) fetchCount(path string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.sniffs[path]
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const testRedirectURI = "https://app.example.test/api/v1/auth/sso/callback"

type fixture struct {
	t          *testing.T
	idp        *stubIDP
	configs    ConfigStore
	identities *auth.MemoryStore
	authSvc    *auth.Service
	svc        *Service
}

func newFixture(t *testing.T, clock func() time.Time, opts ...Option) *fixture {
	t.Helper()
	idp := newStubIDP(t)
	configs := NewMemoryConfigStore()
	identities := auth.NewMemoryStore()
	authSvc := auth.NewServiceWithStore("test-secret", identities)
	options := []Option{}
	if clock != nil {
		options = append(options, WithClock(clock))
	}
	options = append(options, opts...)
	svc := NewService(configs, identities, authSvc, options...)
	return &fixture{t: t, idp: idp, configs: configs, identities: identities, authSvc: authSvc, svc: svc}
}

func (f *fixture) seedOrg(orgID, name string) {
	f.t.Helper()
	SeedOrg(f.configs, orgID, name)
	// The identity store mirrors the users.organization_id FK: the org row
	// must exist before JIT provisioning can insert users into it.
	if err := f.identities.CreateOrganization(context.Background(), &auth.Organization{ID: orgID, Name: name}); err != nil {
		f.t.Fatalf("CreateOrganization: %v", err)
	}
	if err := f.configs.SaveConfig(context.Background(), orgID, &SSOConfig{
		Issuer:       f.idp.srv.URL,
		ClientID:     stubClientID,
		ClientSecret: stubClientSecret,
	}); err != nil {
		f.t.Fatalf("SaveConfig: %v", err)
	}
}

// begin drives BeginLogin and returns the parsed authorize URL query.
func (f *fixture) begin(t *testing.T, orgSlug string) url.Values {
	t.Helper()
	authorizeURL, err := f.svc.BeginLogin(context.Background(), orgSlug, testRedirectURI)
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	if !strings.HasPrefix(authorizeURL, f.idp.srv.URL+"/authorize?") {
		t.Fatalf("authorize URL does not point at the IdP: %s", authorizeURL)
	}
	return u.Query()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSSOHappyPathJITProvisionsAndIssuesPlatformToken(t *testing.T) {
	f := newFixture(t, nil)
	f.seedOrg("org-1", "Acme Corp")
	q := f.begin(t, "acme-corp")

	if got := q.Get("response_type"); got != "code" {
		t.Fatalf("response_type = %q", got)
	}
	if got := q.Get("client_id"); got != stubClientID {
		t.Fatalf("client_id = %q", got)
	}
	if got := q.Get("redirect_uri"); got != testRedirectURI {
		t.Fatalf("redirect_uri = %q", got)
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope missing openid: %q", q.Get("scope"))
	}

	// The user authenticates at the IdP; the IdP binds the code to the nonce
	// from the authorize request and issues the callback with code+state.
	code, state := f.idp.issueCode(
		f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(),
		codeGrant{email: "new.hire@acme.test", sub: "idp-sub-1", nonce: q.Get("nonce")})

	user, token, err := f.svc.CompleteLogin(context.Background(), code, state)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if user.Email != "new.hire@acme.test" {
		t.Fatalf("unexpected email %q", user.Email)
	}
	if user.Organization != "org-1" {
		t.Fatalf("JIT user must join the slug-resolved org, got %q", user.Organization)
	}
	if user.Role != "MEMBER" {
		t.Fatalf("default JIT role = %q, want MEMBER", user.Role)
	}
	if user.SSOSubject != "idp-sub-1" {
		t.Fatalf("sso_subject = %q", user.SSOSubject)
	}
	if user.PasswordHash != "" {
		t.Fatal("JIT user must be invite-pending (no password credential)")
	}
	if !user.Active {
		t.Fatal("JIT user must be active")
	}

	// The session token is the platform's EXISTING HMAC format.
	claims, err := f.authSvc.ValidateToken(token)
	if err != nil {
		t.Fatalf("platform token rejected by auth.Service: %v", err)
	}
	if claims.UserID != user.ID || claims.OrganizationID != "org-1" || claims.Email != user.Email {
		t.Fatalf("unexpected claims %+v", claims)
	}

	// Password login must remain unavailable for a JIT (passwordless) user
	// while SSO keeps working — dual-auth coexistence.
	if _, err := f.authSvc.LoginCtx(context.Background(), user.Email, "anything"); err == nil {
		t.Fatal("password login must fail for an SSO-provisioned user without credential")
	}
}

func TestSSOLinksExistingLocalUserByEmail(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	org, local, err := f.authSvc.RegisterCtx(ctx, "Zeta Inc", "owner@zeta.test", "s3cretpass")
	if err != nil {
		t.Fatalf("RegisterCtx: %v", err)
	}
	SeedOrg(f.configs, org.ID, org.Name)
	if err := f.configs.SaveConfig(ctx, org.ID, &SSOConfig{Issuer: f.idp.srv.URL, ClientID: stubClientID, ClientSecret: stubClientSecret}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	q := f.begin(t, "zeta-inc")
	grant := codeGrant{email: "owner@zeta.test", sub: "idp-sub-owner", nonce: q.Get("nonce")}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)

	user, token, err := f.svc.CompleteLogin(ctx, code, state)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if user.ID != local.ID {
		t.Fatalf("expected existing local user to be linked, got new user %s", user.ID)
	}
	if _, err := f.authSvc.ValidateToken(token); err != nil {
		t.Fatalf("token invalid: %v", err)
	}
	linked, _ := f.identities.GetUserByEmail(ctx, "owner@zeta.test")
	if linked.SSOSubject != "idp-sub-owner" {
		t.Fatalf("subject not linked: %q", linked.SSOSubject)
	}

	// Local password login keeps working after the SSO link (dual auth).
	if _, err := f.authSvc.LoginCtx(ctx, "owner@zeta.test", "s3cretpass"); err != nil {
		t.Fatalf("password login broke after SSO link: %v", err)
	}
}

func TestSSOStateReplayRejected(t *testing.T) {
	f := newFixture(t, nil)
	f.seedOrg("org-1", "Acme Corp")
	ctx := context.Background()
	q := f.begin(t, "acme-corp")
	grant := codeGrant{email: "replay@acme.test", sub: "sub-replay", nonce: q.Get("nonce")}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)

	if _, _, err := f.svc.CompleteLogin(ctx, code, state); err != nil {
		t.Fatalf("first login failed: %v", err)
	}
	// Second use of the same state (captured callback URL) must fail even
	// with a fresh code.
	code2, _ := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {state}, "nonce": {q.Get("nonce")}}.Encode(), grant)
	if _, _, err := f.svc.CompleteLogin(ctx, code2, state); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("expected ErrStateInvalid on replay, got %v", err)
	}
	// Unknown state fails the same way (no oracle).
	if _, _, err := f.svc.CompleteLogin(ctx, "some-code", "totally-unknown-state"); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("expected ErrStateInvalid on unknown state, got %v", err)
	}
}

func TestSSOStateExpiryRejected(t *testing.T) {
	now := time.Unix(1700000000, 0)
	f := newFixture(t, func() time.Time { return now })
	f.seedOrg("org-1", "Acme Corp")
	q := f.begin(t, "acme-corp")
	grant := codeGrant{email: "late@acme.test", sub: "sub-late", nonce: q.Get("nonce"), expUnix: now.Add(time.Hour).Unix()}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)

	// Advance the clock past the 10-minute state TTL.
	now = now.Add(11 * time.Minute)
	if _, _, err := f.svc.CompleteLogin(context.Background(), code, state); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("expected ErrStateInvalid after TTL, got %v", err)
	}
}

func TestSSOExpiredIDTokenRejected(t *testing.T) {
	f := newFixture(t, nil)
	f.seedOrg("org-1", "Acme Corp")
	ctx := context.Background()
	q := f.begin(t, "acme-corp")
	grant := codeGrant{email: "expired@acme.test", sub: "sub-expired", nonce: q.Get("nonce"), expUnix: time.Now().Add(-2 * time.Hour).Unix()}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)

	_, _, err := f.svc.CompleteLogin(ctx, code, state)
	if !errors.Is(err, ErrIDTokenInvalid) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired token rejection, got %v", err)
	}
}

func TestSSOAudienceMismatchRejected(t *testing.T) {
	f := newFixture(t, nil)
	f.seedOrg("org-1", "Acme Corp")
	ctx := context.Background()
	q := f.begin(t, "acme-corp")
	grant := codeGrant{email: "victim@acme.test", sub: "sub-aud", nonce: q.Get("nonce"), aud: "another-client"}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)

	_, _, err := f.svc.CompleteLogin(ctx, code, state)
	if !errors.Is(err, ErrIDTokenInvalid) || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("expected audience mismatch rejection, got %v", err)
	}
}

func TestSSOIssuerMismatchRejected(t *testing.T) {
	f := newFixture(t, nil)
	f.seedOrg("org-1", "Acme Corp")
	ctx := context.Background()
	q := f.begin(t, "acme-corp")
	grant := codeGrant{email: "rogue@acme.test", sub: "sub-iss", nonce: q.Get("nonce"), issuer: "https://evil.example.test"}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)

	_, _, err := f.svc.CompleteLogin(ctx, code, state)
	if !errors.Is(err, ErrIDTokenInvalid) || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("expected issuer mismatch rejection, got %v", err)
	}
}

func TestSSONonceMismatchRejected(t *testing.T) {
	f := newFixture(t, nil)
	f.seedOrg("org-1", "Acme Corp")
	ctx := context.Background()
	q := f.begin(t, "acme-corp")
	// The IdP (mis)issues a token bound to a DIFFERENT nonce than the one the
	// platform sent in the authorize request.
	grant := codeGrant{email: "nonce@acme.test", sub: "sub-nonce", nonce: "attacker-chosen-nonce", aud: stubClientID}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)

	_, _, err := f.svc.CompleteLogin(ctx, code, state)
	if !errors.Is(err, ErrIDTokenInvalid) || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("expected nonce mismatch rejection, got %v", err)
	}
}

func TestSSOTamperedIDTokenSignatureRejected(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	// A token signed by a foreign key (a rogue "IdP") must fail the JOSE
	// signature verification even when every claim looks right.
	attacker := newStubIDP(t)
	forged := attacker.sign(map[string]any{"iss": f.idp.srv.URL, "sub": "sub-evil", "aud": stubClientID, "exp": time.Now().Add(time.Hour).Unix(), "nonce": "n", "email": "evil@acme.test"})

	doc, err := f.svc.fetchDiscovery(ctx, f.idp.srv.URL)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	cfg := &SSOConfig{Issuer: f.idp.srv.URL, ClientID: stubClientID}
	if _, err := f.svc.verifyIDToken(ctx, forged, doc, cfg); !errors.Is(err, ErrIDTokenInvalid) {
		t.Fatalf("expected forged token rejection, got %v", err)
	}

	// Malformed tokens (two segments) are rejected structurally.
	if _, err := f.svc.verifyIDToken(ctx, strings.Split(forged, ".")[0]+".eyJpc3MiOiJ4In0.", doc, cfg); !errors.Is(err, ErrIDTokenInvalid) {
		t.Fatalf("expected malformed token rejection, got %v", err)
	}
}

func TestSSODisabledUserCannotSSOLogin(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	org, local, err := f.authSvc.RegisterCtx(ctx, "Acme Corp", "disabled@acme.test", "pw-123456")
	if err != nil {
		t.Fatalf("RegisterCtx: %v", err)
	}
	SeedOrg(f.configs, org.ID, org.Name)
	if err := f.configs.SaveConfig(ctx, org.ID, &SSOConfig{Issuer: f.idp.srv.URL, ClientID: stubClientID, ClientSecret: stubClientSecret}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := f.identities.SetUserActive(ctx, org.ID, local.ID, false); err != nil {
		t.Fatalf("SetUserActive: %v", err)
	}

	q := f.begin(t, "acme-corp")
	grant := codeGrant{email: "disabled@acme.test", sub: "sub-disabled", nonce: q.Get("nonce")}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)

	if _, _, err := f.svc.CompleteLogin(ctx, code, state); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("expected ErrAccountDisabled, got %v", err)
	}
}

func TestSSOForeignOrgEmailRejected(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	// seedOrg registers org-1 (Acme Corp); register a user in org-2 whose
	// email the org-1 IdP then asserts.
	f.seedOrg("org-1", "Acme Corp")
	f.seedOrg("org-2", "Beta LLC")
	if _, _, err := f.authSvc.RegisterCtx(ctx, "Beta LLC", "shared@acme.test", "pw-123456"); err != nil {
		t.Fatalf("RegisterCtx: %v", err)
	}

	q := f.begin(t, "acme-corp")
	grant := codeGrant{email: "shared@acme.test", sub: "sub-shared", nonce: q.Get("nonce")}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)

	if _, _, err := f.svc.CompleteLogin(ctx, code, state); !errors.Is(err, ErrEmailForeignOrg) {
		t.Fatalf("expected ErrEmailForeignOrg, got %v", err)
	}
}

func TestSSOSubjectMismatchRejected(t *testing.T) {
	f := newFixture(t, nil)
	f.seedOrg("org-1", "Acme Corp")
	ctx := context.Background()
	q := f.begin(t, "acme-corp")
	grant := codeGrant{email: "first@acme.test", sub: "sub-A", nonce: q.Get("nonce")}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)
	if _, _, err := f.svc.CompleteLogin(ctx, code, state); err != nil {
		t.Fatalf("first login: %v", err)
	}

	// Same email, different IdP subject: account-takeover guard.
	q2 := f.begin(t, "acme-corp")
	grant2 := codeGrant{email: "first@acme.test", sub: "sub-B", nonce: q2.Get("nonce")}
	code2, state2 := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q2.Get("state")}, "nonce": {q2.Get("nonce")}}.Encode(), grant2)
	if _, _, err := f.svc.CompleteLogin(ctx, code2, state2); !errors.Is(err, ErrSubjectMismatch) {
		t.Fatalf("expected ErrSubjectMismatch, got %v", err)
	}
}

func TestSSOMissingEmailClaimRejected(t *testing.T) {
	f := newFixture(t, nil)
	f.seedOrg("org-1", "Acme Corp")
	ctx := context.Background()
	q := f.begin(t, "acme-corp")
	grant := codeGrant{sub: "sub-noemail", nonce: q.Get("nonce"), noEmail: true}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)
	if _, _, err := f.svc.CompleteLogin(ctx, code, state); !errors.Is(err, ErrEmailClaimMissing) {
		t.Fatalf("expected ErrEmailClaimMissing, got %v", err)
	}
}

func TestSSODiscoveryAndJWKSCached(t *testing.T) {
	f := newFixture(t, nil)
	f.seedOrg("org-1", "Acme Corp")
	ctx := context.Background()
	// Two full flows against the same issuer.
	for i, email := range []string{"one@acme.test", "two@acme.test"} {
		q := f.begin(t, "acme-corp")
		grant := codeGrant{email: email, sub: fmt.Sprintf("sub-%d", i), nonce: q.Get("nonce")}
		code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)
		if _, _, err := f.svc.CompleteLogin(ctx, code, state); err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
	}
	if got := f.idp.fetchCount("/.well-known"); got != 1 {
		t.Fatalf("discovery fetched %d times, want 1 (cached)", got)
	}
	if got := f.idp.fetchCount("/jwks"); got != 1 {
		t.Fatalf("jwks fetched %d times, want 1 (cached)", got)
	}
}

func TestSSOUnknownSlugAndUnconfiguredOrg(t *testing.T) {
	f := newFixture(t, nil)
	SeedOrg(f.configs, "org-bare", "Bare Org")
	if _, err := f.svc.BeginLogin(context.Background(), "ghost-org", testRedirectURI); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("expected ErrOrgNotFound, got %v", err)
	}
	if _, err := f.svc.BeginLogin(context.Background(), "bare-org", testRedirectURI); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}

func TestSSOClientSecretRefResolution(t *testing.T) {
	// The resolver must be attached to the SAME Service that drives the
	// flow: the single-use state cache is per-Service, so begin and
	// complete cannot straddle two instances.
	resolved := ""
	f := newFixture(t, nil, WithSecretResolver(SecretResolverFunc(func(_ context.Context, orgID, name string) (string, error) {
		resolved = orgID + "/" + name
		return stubClientSecret, nil
	})))
	ctx := context.Background()
	f.seedOrg("org-1", "Acme Corp")
	// Reconfigure org-1: the inline secret is replaced by a reference
	// resolved (org-scoped) at exchange time.
	if err := f.configs.SaveConfig(ctx, "org-1", &SSOConfig{Issuer: f.idp.srv.URL, ClientID: stubClientID, ClientSecretRef: "sso/stub-secret"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	q := f.begin(t, "acme-corp")
	grant := codeGrant{email: "ref@acme.test", sub: "sub-ref", nonce: q.Get("nonce")}
	code, state := f.idp.issueCode(f.idp.srv.URL+"/authorize?"+url.Values{"state": {q.Get("state")}, "nonce": {q.Get("nonce")}}.Encode(), grant)
	if _, _, err := f.svc.CompleteLogin(ctx, code, state); err != nil {
		t.Fatalf("CompleteLogin with secret ref: %v", err)
	}
	if resolved != "org-1/sso/stub-secret" {
		t.Fatalf("secret resolver not consulted with org scope: %q", resolved)
	}
}

func TestSSOConfigValidationAndSlugify(t *testing.T) {
	if err := (&SSOConfig{}).Validate(); !errors.Is(err, ErrIssuerRequired) {
		t.Fatalf("expected ErrIssuerRequired, got %v", err)
	}
	if err := (&SSOConfig{Issuer: "https://idp"}).Validate(); !errors.Is(err, ErrClientIDRequired) {
		t.Fatalf("expected ErrClientIDRequired, got %v", err)
	}
	if err := (&SSOConfig{Issuer: "https://idp", ClientID: "c"}).Validate(); !errors.Is(err, ErrSecretRequired) {
		t.Fatalf("expected ErrSecretRequired, got %v", err)
	}
	if err := (&SSOConfig{Issuer: "https://idp", ClientID: "c", ClientSecretRef: "x"}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]string{
		"Acme Corp":       "acme-corp",
		"  Beta  LLC!!  ": "beta-llc",
		"acme":            "acme",
		// Non-ASCII collapses to a dash and the result is trimmed — a
		// leading '-' would be a malformed URL slug.
		"Über Org": "ber-org",
	}
	for name, want := range cases {
		if got := slugify(name); got != want {
			t.Fatalf("slugify(%q) = %q, want %q", name, got, want)
		}
	}
}
