// Package scim implements the SCIM 2.0 user-provisioning surface (issue #29):
// an org-scoped CRUD lifecycle over the platform identity table plus the
// dedicated bearer-token scheme that guards it.
//
// Split of responsibilities (mirrors internal/sso):
//
//   - this file: the Service (token minting / authentication / revocation),
//     the RequireSCIMToken middleware and the tenant context helpers;
//   - protocol.go: SCIM 2.0 wire types (RFC 7643 core user projection,
//     ListResponse, PatchOp, Error) and the userName-eq filter parser;
//   - service.go: the user lifecycle operations (list / create / get /
//     replace / patch) over the IdentityStore;
//   - store.go: TokenStore with in-memory and Postgres implementations
//     (scim_tokens, migration 019).
//
// Security posture:
//
//   - SCIM tokens are secrets of the form scim_<64 hex>; ONLY their SHA-256
//     hex hash is persisted (api_keys.key_hash pattern). The plaintext is
//     returned exactly once by the minting endpoint and never stored or
//     logged;
//   - the protocol endpoints accept ONLY a SCIM bearer token — session
//     tokens and API keys are rejected, so a directory credential can never
//     double as a user login and vice versa;
//   - every identity operation is org-guarded: the tenant comes from the
//     presented token, never from the request, and foreign users surface as
//     404 without an existence leak;
//   - directory sync can never mint privilege: SCIM-created users are pinned
//     to the MEMBER role and disabling (active=false) blocks password login
//     through the shared auth.Service.LoginCtx lifecycle check.
package scim

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"agentos/internal/auth"
)

// SCIM 2.0 schema URIs (RFC 7643 / RFC 7644).
const (
	SchemaUser         = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaPatchOp      = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	SchemaError        = "urn:ietf:params:scim:api:messages:2.0:Error"
	SchemaListResponse = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
)

// TokenPrefix marks a SCIM bearer secret (the apikeys "ak_" pattern).
const TokenPrefix = "scim_"

// Sentinel errors mapped to HTTP statuses by the transport layer (cmd/api).
var (
	// ErrTokenInvalid: unknown, malformed or revoked bearer secret. One
	// sentinel for all three so responses leak no distinctions.
	ErrTokenInvalid = errors.New("invalid or revoked scim token")
	// ErrTokenNotFound: the token store has no row for a lookup (internal
	// distinction; Authenticate collapses it into ErrTokenInvalid).
	ErrTokenNotFound = errors.New("scim token not found")
	// ErrNilDB: pg constructor handed a nil *sql.DB.
	ErrNilDB = errors.New("scim: database is nil")
	// ErrOrgRequired / ErrCreatorRequired: token minting validation.
	ErrOrgRequired     = errors.New("organization id is required")
	ErrCreatorRequired = errors.New("created_by is required")
	// User lifecycle errors (HTTP: 404 / 409 / 400).
	ErrUserNotFound      = errors.New("scim user not found")
	ErrDuplicateUser     = errors.New("user already exists")
	ErrInvalidUserName   = errors.New("userName must be a valid email address")
	ErrUserNameImmutable = errors.New("userName is immutable in this deployment (it is the login credential)")
	ErrInvalidFilter     = errors.New("unsupported filter (only: userName eq \"...\")")
	ErrInvalidPatch      = errors.New("invalid patch operations")
)

// IdentityStore is the slice of the auth identity table SCIM needs. Both
// auth.MemoryStore and auth.pgStore (via auth.ProvisioningStore) satisfy it;
// the compile-time pin below keeps that guarantee load-bearing.
type IdentityStore interface {
	GetUserByEmail(ctx context.Context, email string) (*auth.User, error)
	GetUserByID(ctx context.Context, userID string) (*auth.User, error)
	CreateUser(ctx context.Context, user *auth.User) error
	SetUserActive(ctx context.Context, orgID, userID string, active bool) error
	ListUsersByOrg(ctx context.Context, orgID string) ([]*auth.User, error)
}

// auth.ProvisioningStore must always remain a superset of IdentityStore: the
// dual-mode wiring hands ONE store to both packages.
var _ IdentityStore = auth.ProvisioningStore(nil)

// Token is the metadata row of a SCIM bearer credential. The plaintext
// secret is NEVER a field here — only its SHA-256 hex hash (TokenHash).
type Token struct {
	ID        string
	OrgID     string
	TokenHash string
	CreatedBy string
	CreatedAt time.Time
	RevokedAt time.Time // zero while the token is live
}

// TokenStore persists SCIM bearer credentials. Lookup by hash is the
// authentication path (the hash is globally UNIQUE in migration 019, so the
// presented credential resolves to exactly one tenant row).
type TokenStore interface {
	CreateToken(ctx context.Context, token *Token) error
	GetTokenByHash(ctx context.Context, hash string) (*Token, error)
	RevokeToken(ctx context.Context, orgID, id string) error
}

// Service is the SCIM 2.0 surface over one tenant's identities. Safe for
// concurrent use.
type Service struct {
	identities IdentityStore
	tokens     TokenStore
}

// NewService returns the zero-infrastructure Service: identities resolve
// through the shared auth.MemoryStore and tokens live in a process-local
// map (no database required for tests and in-memory mode).
func NewService(identities IdentityStore) *Service {
	return &Service{identities: identities, tokens: newMemoryTokenStore()}
}

// NewServiceWithStore returns a Service whose tokens are persisted through
// the given store. A nil store falls back to the process-local map (the
// in-memory wiring never persists credentials anyway).
func NewServiceWithStore(tokens TokenStore, identities IdentityStore) *Service {
	s := NewService(identities)
	if tokens != nil {
		s.tokens = tokens
	}
	return s
}

// CreateToken mints a SCIM bearer credential for one tenant. It returns the
// stored metadata and the plaintext secret; the secret is shown EXACTLY ONCE
// (the caller is the only place it ever exists) and only its SHA-256 hex
// hash is persisted.
func (s *Service) CreateToken(ctx context.Context, orgID, createdBy string) (*Token, string, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, "", ErrOrgRequired
	}
	if strings.TrimSpace(createdBy) == "" {
		return nil, "", ErrCreatorRequired
	}
	secret, err := newSCIMSecret()
	if err != nil {
		return nil, "", err
	}
	token := &Token{
		ID:        uuid.NewString(),
		OrgID:     orgID,
		TokenHash: HashToken(secret),
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.tokens.CreateToken(ctx, token); err != nil {
		return nil, "", err
	}
	return token, secret, nil
}

// Authenticate resolves a presented bearer secret to its tenant token.
// Unknown, malformed and revoked secrets all collapse into ErrTokenInvalid
// so a probing client learns nothing.
func (s *Service) Authenticate(ctx context.Context, secret string) (*Token, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, ErrTokenInvalid
	}
	token, err := s.tokens.GetTokenByHash(ctx, HashToken(secret))
	if errors.Is(err, ErrTokenNotFound) || token == nil {
		return nil, ErrTokenInvalid
	}
	if err != nil {
		return nil, err // store failure: let the transport answer 500
	}
	if !token.RevokedAt.IsZero() {
		return nil, ErrTokenInvalid
	}
	return token, nil
}

// RevokeToken retires one credential within one tenant (organization_id
// guard; unknown or foreign id surfaces as ErrTokenNotFound). Revoked tokens
// authenticate as nothing at all.
func (s *Service) RevokeToken(ctx context.Context, orgID, id string) error {
	return s.tokens.RevokeToken(ctx, orgID, id)
}

// HashToken is the at-rest digest for SCIM secrets: SHA-256 hex of the
// plaintext, byte-for-byte the api_keys.key_hash pattern.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])
}

// newSCIMSecret returns a scim_<64 hex> bearer secret from crypto/rand.
func newSCIMSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate scim token: %w", err)
	}
	return TokenPrefix + hex.EncodeToString(buf), nil
}

// ctxKey is the unexported context key for the tenant bound to the presented
// SCIM token.
type ctxKey struct{}

var errNoSCIMContext = errors.New("missing scim token context")

// OrgFromContext returns the tenant resolved from the presented SCIM token.
// Handlers must take the tenant from here and NEVER from the request body or
// path.
func OrgFromContext(ctx context.Context) (string, error) {
	org, ok := ctx.Value(ctxKey{}).(string)
	if !ok || strings.TrimSpace(org) == "" {
		return "", errNoSCIMContext
	}
	return org, nil
}

// RequireSCIMToken guards the SCIM 2.0 protocol endpoints. It accepts ONLY
// the dedicated scim_ bearer token — session tokens and API keys are
// deliberately not honored here, so directory automation cannot be confused
// with user login and a leaked SCIM token cannot read the rest of the API.
// On success the tenant is injected into the request context.
func RequireSCIMToken(svc *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			parts := strings.SplitN(header, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
				WriteError(w, http.StatusUnauthorized, "missing scim bearer token")
				return
			}
			token, err := svc.Authenticate(r.Context(), parts[1])
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
				WriteError(w, http.StatusUnauthorized, "invalid or revoked scim token")
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, token.OrgID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WriteError emits the standard SCIM 2.0 error envelope
// (RFC 7644 section 3.12) with application/scim+json semantics.
func WriteError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schemas": []string{SchemaError},
		"status":  strconv.Itoa(status),
		"detail":  detail,
	})
}
