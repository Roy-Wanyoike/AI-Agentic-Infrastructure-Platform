// Package sso implements OIDC single sign-on (issue #29): authorization-code
// login against a tenant-configured identity provider, manual stdlib-only
// JOSE verification of the RS256 ID token (crypto/rsa + crypto/sha256 +
// base64rawurl), and just-in-time provisioning/linking of local identities
// followed by issuance of the platform's existing HMAC session token.
//
// Security posture:
//   - state and nonce are generated server-side, stored in a single-use
//     in-memory TTL cache (10 minutes) and consumed exactly once, so replayed
//     callback URLs are rejected;
//   - the ID token signature is verified with the JWKS published by the
//     issuer (discovery document fetched + cached), with issuer, audience,
//     expiry and nonce claims validated on every callback;
//   - the IdP client secret is stored by REFERENCE (client_secret_ref naming
//     a row in the platform secrets store) and resolved per-org at exchange
//     time; an inline plaintext secret is accepted only on the in-memory/dev
//     config path and is rejected by the Postgres store;
//   - only the platform's existing session token format is issued — this
//     package never mints a second token scheme.
package sso

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/auth"
)

// Sentinel errors mapped to HTTP statuses by the transport layer (cmd/api).
var (
	// ErrOrgNotFound: no organization matches the login slug.
	ErrOrgNotFound = errors.New("organization not found")
	// ErrNotConfigured: the organization exists but has no sso_config.
	ErrNotConfigured = errors.New("sso is not configured for this organization")
	// ErrStateInvalid: unknown, expired or already-consumed state (replay).
	ErrStateInvalid = errors.New("invalid or expired sso state")
	// ErrIDTokenInvalid: malformed/unsigned/claim-invalid ID token.
	ErrIDTokenInvalid = errors.New("id token validation failed")
	// ErrEmailClaimMissing: the ID token carries no email claim.
	ErrEmailClaimMissing = errors.New("id token has no email claim")
	// ErrEmailForeignOrg: the email already belongs to another tenant
	// (users.email is globally unique) and cannot be provisioned here.
	ErrEmailForeignOrg = errors.New("email already registered to another organization")
	// ErrSubjectMismatch: the account is already linked to a different
	// IdP subject — a potential account-takeover attempt.
	ErrSubjectMismatch = errors.New("account linked to a different sso subject")
)

// TokenIssuer issues the platform's EXISTING session token. *auth.Service
// satisfies this interface directly (GenerateToken) — no second token format
// exists anywhere in the SSO flow.
type TokenIssuer interface {
	GenerateToken(user *auth.User) (string, error)
}

// SecretResolver resolves a client_secret_ref against the platform secrets
// store for one tenant. internal/secrets.Service.Resolve matches this shape.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, orgID, name string) (string, error)
}

// SecretResolverFunc adapts a plain function (and, transitively, any resolver
// with a compatible signature such as secrets.Service.Resolve).
type SecretResolverFunc func(ctx context.Context, orgID, name string) (string, error)

// ResolveSecret implements SecretResolver.
func (f SecretResolverFunc) ResolveSecret(ctx context.Context, orgID, name string) (string, error) {
	return f(ctx, orgID, name)
}

// IdentityStore is the slice of the auth identity table the SSO flow needs:
// email lookup, JIT creation and subject linking. Both auth.MemoryStore and
// auth.pgStore (via auth.ProvisioningStore) satisfy it.
type IdentityStore interface {
	GetUserByEmail(ctx context.Context, email string) (*auth.User, error)
	CreateUser(ctx context.Context, user *auth.User) error
	LinkSSOSubject(ctx context.Context, orgID, userID, subject string) error
}

// Service orchestrates the OIDC authorization-code flow. It is safe for
// concurrent use.
type Service struct {
	configs    ConfigStore
	identities IdentityStore
	issuer     TokenIssuer
	secrets    SecretResolver

	httpClient *http.Client
	states     *stateStore
	now        func() time.Time

	mu       sync.Mutex
	discover map[string]cacheEntry[discoveryDocument]
	jwks     map[string]cacheEntry[jwksDocument]
}

// Option configures a Service (test seams and optional integrations).
type Option func(*Service)

// WithHTTPClient overrides the client used for discovery/JWKS/token calls.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) { s.httpClient = c }
}

// WithSecretResolver attaches the secrets-store resolver for
// client_secret_ref lookups. Without it, only inline client secrets work.
func WithSecretResolver(r SecretResolver) Option {
	return func(s *Service) { s.secrets = r }
}

// WithClock overrides the wall clock (expiry tests). The state store shares
// the same clock so a shifted clock cannot smuggle stale states through.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		s.now = now
		s.states = newStateStore(defaultStateTTL, now)
	}
}

const (
	defaultStateTTL    = 10 * time.Minute
	discoveryCacheTTL  = 10 * time.Minute
	defaultHTTPTimeout = 10 * time.Second
)

// NewService wires the OIDC flow over the given stores. In-memory dual-mode
// callers pass auth.NewMemoryStore-backed stores; Postgres mode passes the
// pg implementations.
func NewService(configs ConfigStore, identities IdentityStore, issuer TokenIssuer, opts ...Option) *Service {
	s := &Service{
		configs:    configs,
		identities: identities,
		issuer:     issuer,
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
		states:     newStateStore(defaultStateTTL, time.Now),
		now:        time.Now,
		discover:   make(map[string]cacheEntry[discoveryDocument]),
		jwks:       make(map[string]cacheEntry[jwksDocument]),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// BeginLogin resolves the tenant by slug, validates its OIDC configuration
// against the live discovery document, stores a single-use state + nonce pair
// and returns the IdP authorization URL to redirect to.
func (s *Service) BeginLogin(ctx context.Context, orgSlug, redirectURI string) (string, error) {
	if strings.TrimSpace(orgSlug) == "" {
		return "", ErrOrgNotFound
	}
	if strings.TrimSpace(redirectURI) == "" {
		return "", errors.New("redirect uri is required")
	}
	org, err := s.configs.GetConfigByOrgSlug(ctx, orgSlug)
	if err != nil {
		return "", err // ErrOrgNotFound | ErrNotConfigured
	}
	cfg := org.Config
	doc, err := s.fetchDiscovery(ctx, cfg.Issuer)
	if err != nil {
		return "", fmt.Errorf("oidc discovery failed: %w", err)
	}
	if doc.AuthorizationEndpoint == "" {
		return "", errors.New("issuer discovery doc has no authorization_endpoint")
	}

	state, err := randomToken()
	if err != nil {
		return "", err
	}
	nonce, err := randomToken()
	if err != nil {
		return "", err
	}
	s.states.Put(state, stateEntry{
		OrgID:       org.OrgID,
		Nonce:       nonce,
		RedirectURI: redirectURI,
		CreatedAt:   s.now(),
	})

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	return doc.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// CompleteLogin consumes the single-use state, exchanges the authorization
// code at the token endpoint (client_secret_post form), verifies the RS256 ID
// token end-to-end (signature via JWKS, issuer, audience, expiry, nonce) and
// JIT-provisions or links the local identity by the email claim. It returns
// the provisioned user and a session token in the platform's existing HMAC
// format.
func (s *Service) CompleteLogin(ctx context.Context, code, state string) (*auth.User, string, error) {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(state) == "" {
		return nil, "", ErrStateInvalid
	}
	// Take is single-use: a replayed state finds an empty slot.
	entry, ok := s.states.Take(state, s.now())
	if !ok {
		return nil, "", ErrStateInvalid
	}
	org, err := s.configs.GetConfig(ctx, entry.OrgID)
	if err != nil {
		return nil, "", err
	}
	cfg := org.Config
	doc, err := s.fetchDiscovery(ctx, cfg.Issuer)
	if err != nil {
		return nil, "", fmt.Errorf("oidc discovery failed: %w", err)
	}
	clientSecret, err := s.clientSecret(ctx, org.OrgID, cfg)
	if err != nil {
		return nil, "", err
	}
	rawIDToken, err := s.exchangeCode(ctx, doc.TokenEndpoint, code, entry.RedirectURI, cfg.ClientID, clientSecret)
	if err != nil {
		return nil, "", err
	}
	claims, err := s.verifyIDToken(ctx, rawIDToken, doc, cfg)
	if err != nil {
		return nil, "", err
	}
	if claims.Nonce != entry.Nonce {
		return nil, "", fmt.Errorf("%w: nonce mismatch", ErrIDTokenInvalid)
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" {
		return nil, "", ErrEmailClaimMissing
	}
	user, err := s.provision(ctx, org.OrgID, cfg, email, claims.Subject)
	if err != nil {
		return nil, "", err
	}
	token, err := s.issuer.GenerateToken(user)
	if err != nil {
		return nil, "", err
	}
	return user, token, nil
}

// clientSecret resolves the exchange credential: client_secret_ref through
// the secrets resolver when configured, otherwise the inline value (dev only).
func (s *Service) clientSecret(ctx context.Context, orgID string, cfg *SSOConfig) (string, error) {
	if ref := strings.TrimSpace(cfg.ClientSecretRef); ref != "" {
		if s.secrets == nil {
			return "", errors.New("sso client_secret_ref configured but no secret resolver wired")
		}
		value, err := s.secrets.ResolveSecret(ctx, orgID, ref)
		if err != nil {
			return "", fmt.Errorf("resolve sso client secret: %w", err)
		}
		if value == "" {
			return "", errors.New("sso client secret reference resolves to an empty value")
		}
		return value, nil
	}
	if cfg.ClientSecret != "" {
		return cfg.ClientSecret, nil
	}
	return "", errors.New("sso config has no client secret or client_secret_ref")
}

// provision JIT-creates the local identity for a first-time SSO user or
// links/validates the existing one. Created users carry no password hash
// (they cannot password-login until an invite sets a credential) and the
// config's default role (MEMBER unless overridden).
func (s *Service) provision(ctx context.Context, orgID string, cfg *SSOConfig, email, subject string) (*auth.User, error) {
	user, err := s.identities.GetUserByEmail(ctx, email)
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		role := strings.TrimSpace(cfg.DefaultRole)
		if role == "" {
			role = "MEMBER"
		}
		created := &auth.User{
			ID:           uuid.NewString(),
			Organization: orgID,
			Email:        email,
			PasswordHash: "", // invite-pending: no local credential
			Role:         role,
			SSOSubject:   subject,
			Active:       true,
			CreatedAt:    s.now().UTC(),
		}
		if err := s.identities.CreateUser(ctx, created); err != nil {
			return nil, err
		}
		return created, nil
	case err != nil:
		return nil, err
	}
	if user.Organization != orgID {
		return nil, ErrEmailForeignOrg
	}
	if !user.Active {
		return nil, auth.ErrAccountDisabled
	}
	if user.SSOSubject == "" {
		if err := s.identities.LinkSSOSubject(ctx, orgID, user.ID, subject); err != nil {
			return nil, err
		}
		user.SSOSubject = subject
	} else if user.SSOSubject != subject {
		return nil, ErrSubjectMismatch
	}
	return user, nil
}

// randomToken returns 32 hex characters of crypto/rand entropy.
func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
