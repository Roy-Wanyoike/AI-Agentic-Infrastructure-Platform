package auth

// Issue #76 service tests — logout, single-use refresh rotation, deny-list
// consultation in ValidateToken, and the backward-compatibility pin: tokens
// minted before issue #76 (no jti claim) validate and remain revocable under
// their full-token hash.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// legacyToken mints a token EXACTLY like the pre-#76 scheme: same claims,
// same signing, no jti.
func legacyToken(t *testing.T, s *Service, user *User, exp time.Time) string {
	t.Helper()
	claims := Claims{
		UserID:         user.ID,
		OrganizationID: user.Organization,
		Email:          user.Email,
		Role:           user.Role,
		Exp:            exp.Unix(),
	}
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	head64 := base64.RawURLEncoding.EncodeToString(header)
	payload64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := head64 + "." + payload64
	return signingInput + "." + signJWT([]byte(signingInput), s.jwtSecret)
}

// recordingRevocationStore captures deny-list writes so tests can pin the
// revoke key and the TTL (remaining token lifetime), and can simulate
// backend outages.
type recordingRevocationStore struct {
	mu          sync.Mutex
	revocations map[string]time.Duration
	live        map[string]bool
	revokeErr   error
	revokedErr  error
}

func newRecordingRevocationStore() *recordingRevocationStore {
	return &recordingRevocationStore{
		revocations: make(map[string]time.Duration),
		live:        make(map[string]bool),
	}
}

func (r *recordingRevocationStore) Revoke(_ context.Context, key string, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.revokeErr != nil {
		return r.revokeErr
	}
	r.revocations[key] = ttl
	r.live[key] = true
	return nil
}

func (r *recordingRevocationStore) Revoked(_ context.Context, key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.revokedErr != nil {
		return false, r.revokedErr
	}
	return r.live[key], nil
}

func TestGeneratedTokensCarryUniqueJTI(t *testing.T) {
	s := NewService("test-secret")
	user := &User{ID: "user-1", Organization: "org-1", Email: "a@acme.test", Role: "OWNER", Active: true}
	token, err := s.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	claims, err := s.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.JTI == "" {
		t.Fatal("freshly minted tokens must carry a jti claim (issue #76)")
	}
	other, err := s.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken(second): %v", err)
	}
	otherClaims, err := s.ValidateToken(other)
	if err != nil {
		t.Fatalf("ValidateToken(second): %v", err)
	}
	if otherClaims.JTI == claims.JTI {
		t.Fatalf("every token needs a unique jti, got %q twice", claims.JTI)
	}
}

func TestLegacyTokenWithoutJTIStillValidates(t *testing.T) {
	// Backward-compatibility pin (non-negotiable): pre-#76 tokens keep
	// validating untouched against the new ValidateToken.
	s := NewService("test-secret")
	user := &User{ID: "user-1", Organization: "org-1", Email: "legacy@acme.test", Role: "OWNER", Active: true}
	token := legacyToken(t, s, user, time.Now().Add(time.Hour))
	claims, err := s.ValidateToken(token)
	if err != nil {
		t.Fatalf("legacy token must still validate, got %v", err)
	}
	if claims.JTI != "" {
		t.Fatalf("legacy token must parse without a jti, got %q", claims.JTI)
	}
	if claims.UserID != user.ID || claims.OrganizationID != user.Organization {
		t.Fatalf("legacy claims mangled: %+v", claims)
	}
}

func TestValidateTokenConsultsDenyList(t *testing.T) {
	s := NewService("test-secret")
	user := &User{ID: "user-1", Organization: "org-1", Email: "deny@acme.test", Role: "OWNER", Active: true}
	token, err := s.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := s.ValidateToken(token); err != nil {
		t.Fatalf("pre-logout validation failed: %v", err)
	}
	if err := s.LogoutCtx(context.Background(), token); err != nil {
		t.Fatalf("LogoutCtx: %v", err)
	}
	if _, err := s.ValidateToken(token); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("revoked token must fail with ErrTokenRevoked, got %v", err)
	}
}

func TestValidateTokenChecksSignatureBeforeDenyList(t *testing.T) {
	s := NewService("test-secret")
	user := &User{ID: "user-1", Organization: "org-1", Email: "sig@acme.test", Role: "OWNER", Active: true}
	token, err := s.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	// Tamper with the payload: whatever the deny list says, the signature
	// check comes first and answers the plain signature error.
	tampered := token[:len(token)-4] + "beef"
	if _, err := s.ValidateToken(tampered); err == nil || err.Error() != "invalid token signature" {
		t.Fatalf("tampered token must fail the signature check first, got %v", err)
	}
}

func TestValidateTokenDenyListOutageFailsClosed(t *testing.T) {
	s := NewService("test-secret")
	s.SetRevocationStore(failingRevocationStore{})
	user := &User{ID: "user-1", Organization: "org-1", Email: "outage@acme.test", Role: "OWNER", Active: true}
	token, err := s.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := s.ValidateToken(token); err == nil {
		t.Fatal("a deny-list backend outage must deny the request (fail-closed), not bypass revocation")
	}
}

func TestSetRevocationStoreNilDegradedGracefully(t *testing.T) {
	s := NewService("test-secret")
	user := &User{ID: "user-1", Organization: "org-1", Email: "degraded@acme.test", Role: "OWNER", Active: true}
	token, err := s.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if err := s.LogoutCtx(context.Background(), token); err != nil {
		t.Fatalf("LogoutCtx before degradation: %v", err)
	}
	if _, err := s.ValidateToken(token); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("token should be revoked pre-degradation, got %v", err)
	}
	// Explicit degradation (mirrors SetStore(nil) optional-dependency style):
	// validation keeps working, logout stays a success no-op.
	s.SetRevocationStore(nil)
	if _, err := s.ValidateToken(token); err != nil {
		t.Fatalf("degraded mode must keep validating tokens, got %v", err)
	}
	if err := s.LogoutCtx(context.Background(), token); err != nil {
		t.Fatalf("degraded logout must stay a no-op success, got %v", err)
	}
}

func TestLogoutCtxIdempotentMatrix(t *testing.T) {
	s := NewService("test-secret")
	user := &User{ID: "user-1", Organization: "org-1", Email: "idem@acme.test", Role: "OWNER", Active: true}
	ctx := context.Background()

	token, err := s.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.LogoutCtx(ctx, token); err != nil {
			t.Fatalf("logout #%d must succeed (idempotent), got %v", i+1, err)
		}
	}
	// An expired token: already unusable, so logout is a no-op success.
	expired := legacyToken(t, s, user, time.Now().Add(-time.Minute))
	if err := s.LogoutCtx(ctx, expired); err != nil {
		t.Fatalf("logout of an expired token must be a no-op success, got %v", err)
	}
	// Garbage is the only logout failure.
	if err := s.LogoutCtx(ctx, "not.a.token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("logout of garbage must return ErrInvalidToken, got %v", err)
	}
	// The revoked token stays revoked after the repeated logouts.
	if _, err := s.ValidateToken(token); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("revoked token must stay revoked, got %v", err)
	}
}

func TestLogoutCtxTTLEqualsRemainingLifetime(t *testing.T) {
	store := newRecordingRevocationStore()
	s := NewService("test-secret")
	s.SetRevocationStore(store)
	user := &User{ID: "user-1", Organization: "org-1", Email: "ttl@acme.test", Role: "OWNER", Active: true}

	token := legacyToken(t, s, user, time.Now().Add(time.Hour)) // legacy: hash-key strategy end-to-end
	if err := s.LogoutCtx(context.Background(), token); err != nil {
		t.Fatalf("LogoutCtx: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.revocations) != 1 {
		t.Fatalf("expected exactly one deny-list write, got %d", len(store.revocations))
	}
	wantKey := TokenReference(token, &Claims{}) // legacy token: full-token hash key
	ttl, ok := store.revocations[wantKey]
	if !ok {
		t.Fatalf("deny-list write keyed %q missing (have %v)", wantKey, store.revocations)
	}
	if ttl > time.Hour || ttl < time.Hour-10*time.Second {
		t.Fatalf("deny TTL must equal the remaining token lifetime (~1h), got %v", ttl)
	}
}

func TestRefreshCtxRotatesAndKillsOldToken(t *testing.T) {
	ctx := context.Background()
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	identities := NewMemoryStore()
	s := NewServiceWithStore("test-secret", identities)
	if err := identities.CreateOrganization(ctx, &Organization{ID: "org-1", Name: "Acme"}); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if err := identities.CreateUser(ctx, &User{ID: "user-1", Organization: "org-1", Email: "rotate@acme.test", PasswordHash: hash, Role: "OWNER", Active: true}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	oldToken, err := s.LoginCtx(ctx, "rotate@acme.test", "secret123")
	if err != nil {
		t.Fatalf("LoginCtx: %v", err)
	}
	oldClaims, err := s.ValidateToken(oldToken)
	if err != nil {
		t.Fatalf("ValidateToken(old): %v", err)
	}

	before := time.Now()
	result, err := s.RefreshCtx(ctx, oldToken)
	if err != nil {
		t.Fatalf("RefreshCtx: %v", err)
	}
	if result.Token == "" || result.Token == oldToken {
		t.Fatal("refresh must return a NEW token")
	}
	newClaims, err := s.ValidateToken(result.Token)
	if err != nil {
		t.Fatalf("fresh token must validate: %v", err)
	}
	if newClaims.JTI == "" || newClaims.JTI == oldClaims.JTI {
		t.Fatalf("refresh must rotate the jti: old %q new %q", oldClaims.JTI, newClaims.JTI)
	}
	// Sliding session: a full fresh 24h window.
	if newClaims.Exp <= before.Add(23*time.Hour).Unix() || newClaims.Exp > time.Now().Add(24*time.Hour).Unix() {
		t.Fatalf("fresh token must carry a full ~24h window, exp=%d", newClaims.Exp)
	}
	// Old token is dead everywhere: validation and a second refresh.
	if _, err := s.ValidateToken(oldToken); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("old token must be revoked after refresh, got %v", err)
	}
	if _, err := s.RefreshCtx(ctx, oldToken); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("replaying the old token must fail single-use refresh, got %v", err)
	}
	// Same principal, rotated credential.
	if result.User == nil || result.User.ID != "user-1" || result.User.Organization != "org-1" {
		t.Fatalf("refresh result must carry the current identity row: %+v", result.User)
	}
	if result.OldRef == "" || result.NewRef == "" || result.OldRef == result.NewRef {
		t.Fatalf("refresh must report distinct old/new deny-list refs: %q vs %q", result.OldRef, result.NewRef)
	}
	// The fresh token can itself be refreshed (rotation chain).
	if _, err := s.RefreshCtx(ctx, result.Token); err != nil {
		t.Fatalf("chained refresh with the fresh token must work: %v", err)
	}
}

func TestRefreshCtxBlocksDeprovisionedUser(t *testing.T) {
	ctx := context.Background()
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	identities := NewMemoryStore()
	s := NewServiceWithStore("test-secret", identities)
	if err := identities.CreateOrganization(ctx, &Organization{ID: "org-1", Name: "Acme"}); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if err := identities.CreateUser(ctx, &User{ID: "user-1", Organization: "org-1", Email: "scim@acme.test", PasswordHash: hash, Role: "MEMBER", Active: true}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, err := s.LoginCtx(ctx, "scim@acme.test", "secret123")
	if err != nil {
		t.Fatalf("LoginCtx: %v", err)
	}
	// SCIM deprovisioning seam: the SAME SetUserActive path the SCIM
	// service uses (issue #29) must block refresh, not just login.
	if err := identities.SetUserActive(ctx, "org-1", "user-1", false); err != nil {
		t.Fatalf("SetUserActive: %v", err)
	}
	if _, err := s.LoginCtx(ctx, "scim@acme.test", "secret123"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("parity check: deprovisioned login must be ErrAccountDisabled, got %v", err)
	}
	if _, err := s.RefreshCtx(ctx, token); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("deprovisioned user must not refresh, got %v", err)
	}
	// And the refusal must not revoke the old token as a side effect of the
	// failed rotation... it stays valid until natural expiry, exactly like
	// any pre-#76 live token (deprovisioning blocks login/refresh).
	if _, err := s.ValidateToken(token); err != nil {
		t.Fatalf("failed refresh must leave the old token's validity untouched, got %v", err)
	}
}

func TestRefreshCtxReDerivesCurrentClaims(t *testing.T) {
	ctx := context.Background()
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	identities := NewMemoryStore()
	s := NewServiceWithStore("test-secret", identities)
	if err := identities.CreateOrganization(ctx, &Organization{ID: "org-1", Name: "Acme"}); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if err := identities.CreateUser(ctx, &User{ID: "user-1", Organization: "org-1", Email: "rbac@acme.test", PasswordHash: hash, Role: "OWNER", Active: true}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, err := s.LoginCtx(ctx, "rbac@acme.test", "secret123")
	if err != nil {
		t.Fatalf("LoginCtx: %v", err)
	}
	// Role was changed out-of-band (e.g. by an operator): refresh must NOT
	// carry the stale claim forward — it re-derives from the current row.
	identities.mu.Lock()
	identities.users["user-1"].Role = "VIEWER"
	identities.mu.Unlock()
	result, err := s.RefreshCtx(ctx, token)
	if err != nil {
		t.Fatalf("RefreshCtx: %v", err)
	}
	claims, err := s.ValidateToken(result.Token)
	if err != nil {
		t.Fatalf("fresh token must validate: %v", err)
	}
	if claims.Role != "VIEWER" {
		t.Fatalf("refresh must re-derive the role from the current row, got %q", claims.Role)
	}
}

func TestRefreshCtxUnknownIdentityRefused(t *testing.T) {
	s := NewService("test-secret")
	// No store, no in-memory user: the token's identity cannot be resolved,
	// so its status can never be re-checked. Refresh refuses (fail-closed).
	user := &User{ID: "ghost", Organization: "org-1", Email: "ghost@acme.test", Role: "OWNER", Active: true}
	token, err := s.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := s.RefreshCtx(context.Background(), token); err == nil {
		t.Fatal("refresh of an unresolvable identity must be refused")
	}
}

func TestRefreshCtxRevokeFailureFailsClosed(t *testing.T) {
	store := newRecordingRevocationStore()
	store.revokeErr = errors.New("deny-list write failed")
	s := NewService("test-secret")
	s.SetRevocationStore(store)
	ctx := context.Background()
	user := &User{ID: "user-1", Organization: "org-1", Email: "closed@acme.test", Role: "OWNER", Active: true}
	token, err := s.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := s.RefreshCtx(ctx, token); err == nil {
		t.Fatal("refresh must fail closed when the old token cannot be revoked (single-use guarantee)")
	}
	// No rotation happened: the old token was never deny-listed, so it is
	// still the one live credential (fail-closed, not fail-split).
	if _, err := s.ValidateToken(token); err != nil {
		t.Fatalf("failed rotation must leave the old token live, got %v", err)
	}
}

func TestRefreshCtxDegradedModeRotatesButCannotRevoke(t *testing.T) {
	identities := NewMemoryStore()
	s := NewServiceWithStore("test-secret", identities)
	s.SetRevocationStore(nil) // explicit degradation
	ctx := context.Background()
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := identities.CreateOrganization(ctx, &Organization{ID: "org-1", Name: "Acme"}); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if err := identities.CreateUser(ctx, &User{ID: "user-1", Organization: "org-1", Email: "degraded-refresh@acme.test", PasswordHash: hash, Role: "OWNER", Active: true}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	oldToken, err := s.LoginCtx(ctx, "degraded-refresh@acme.test", "secret123")
	if err != nil {
		t.Fatalf("LoginCtx: %v", err)
	}
	result, err := s.RefreshCtx(ctx, oldToken)
	if err != nil {
		t.Fatalf("degraded refresh must still rotate: %v", err)
	}
	if _, err := s.ValidateToken(result.Token); err != nil {
		t.Fatalf("fresh token must validate: %v", err)
	}
	// Documented degradation: without a deny list the old token cannot be
	// revoked server-side and stays valid until natural expiry.
	if _, err := s.ValidateToken(oldToken); err != nil {
		t.Fatalf("degraded mode cannot revoke the old token, got %v", err)
	}
}
