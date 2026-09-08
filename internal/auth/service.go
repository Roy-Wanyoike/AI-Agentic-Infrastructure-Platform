package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"golang.org/x/crypto/bcrypt"
)

const defaultRole = "OWNER"

type Organization struct {
	ID   string
	Name string
}

type User struct {
	ID           string
	Organization string
	Email        string
	PasswordHash string
	Role         string
	CreatedAt    time.Time
	// SSOSubject is the OIDC IdP subject (`sub` claim) linked to this
	// identity (issue #29). Empty for purely local users.
	SSOSubject string
	// Active is the SCIM 2.0 lifecycle flag: disabled accounts cannot
	// log in. The zero value is false, so every constructor must set it
	// to true explicitly for enabled users.
	Active bool
}

type Claims struct {
	UserID         string `json:"user_id"`
	OrganizationID string `json:"organization_id"`
	Email          string `json:"email"`
	Role           string `json:"role"`
	Exp            int64  `json:"exp"`
	// JTI (issue #76) is a random per-token id minted into every token
	// generated from now on; it is the token's revocation identity
	// (TokenReference). Legacy tokens without a jti still validate and are
	// revoked by the SHA-256 of the full token string instead.
	JTI string `json:"jti,omitempty"`
}

type Permission string

const (
	PermissionAgentsRead  Permission = "agents.read"
	PermissionAgentsWrite Permission = "agents.write"
	PermissionRunsRead    Permission = "runs.read"
	PermissionRunsExecute Permission = "runs.execute"
	PermissionUsersManage Permission = "users.manage"
	PermissionOrgManage   Permission = "organization.manage"
)

var rolePermissions = map[string][]Permission{
	"OWNER": {
		PermissionAgentsRead,
		PermissionAgentsWrite,
		PermissionRunsRead,
		PermissionRunsExecute,
		PermissionUsersManage,
		PermissionOrgManage,
	},
	"ADMIN": {
		PermissionAgentsRead,
		PermissionAgentsWrite,
		PermissionRunsRead,
		PermissionRunsExecute,
		PermissionUsersManage,
	},
	"MEMBER": {
		PermissionAgentsRead,
		PermissionRunsRead,
		PermissionRunsExecute,
	},
	"VIEWER": {
		PermissionAgentsRead,
		PermissionRunsRead,
	},
}

// Store persists users and their organizations. Email is globally unique
// (users.email UNIQUE in migration 001) so the credential itself resolves the
// tenant; every authorization decision still re-checks organization_id.
type Store interface {
	// CreateOrganization inserts the tenant root row for a new registration.
	CreateOrganization(ctx context.Context, org *Organization) error
	// CreateUser persists a user with its organization_id tenant scope.
	CreateUser(ctx context.Context, user *User) error
	// GetUserByEmail looks up the login identity by email (unique credential).
	GetUserByEmail(ctx context.Context, email string) (*User, error)
}

type Service struct {
	jwtSecret string
	orgs      map[string]*Organization
	users     map[string]*User
	store     Store
	// revocations is the token deny list consulted by ValidateToken
	// (issue #76). It is NEVER nil after construction: zero-infrastructure
	// mode gets an active in-memory store so logout/refresh work out of the
	// box; Postgres deployments replace it via SetRevocationStore. Passing
	// nil to SetRevocationStore explicitly degrades the feature.
	revocations RevocationStore
}

func NewService(jwtSecret string) *Service {
	return &Service{
		jwtSecret:   jwtSecret,
		orgs:        make(map[string]*Organization),
		users:       make(map[string]*User),
		revocations: NewMemoryRevocationStore(),
	}
}

// NewServiceWithStore returns a service whose user/org registrations are
// persisted through the given store; the in-memory maps remain a cache for
// RBAC middleware lookups.
func NewServiceWithStore(jwtSecret string, store Store) *Service {
	s := NewService(jwtSecret)
	s.store = store
	return s
}

func (s *Service) SetStore(store Store) {
	if s == nil {
		return
	}
	s.store = store
}

// SetRevocationStore replaces the token deny-list backend (issue #76). The
// constructor wires an active in-memory store; Postgres deployments pass
// NewPostgresRevocationStore(db) to share revocations across replicas, and
// passing nil degrades the feature (tokens can no longer be revoked
// server-side; validation keeps working). Mirrors the SetStore style.
func (s *Service) SetRevocationStore(store RevocationStore) {
	if s == nil {
		return
	}
	s.revocations = store
}

func HashPassword(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", errors.New("password cannot be empty")
	}
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func VerifyPassword(hash, password string) bool {
	if hash == "" || password == "" {
		return false
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return false
	}
	return true
}

// Register is the legacy context-free entry point; see RegisterCtx.
func (s *Service) Register(orgName, email, password string) (*Organization, *User, error) {
	return s.RegisterCtx(context.Background(), orgName, email, password)
}

// RegisterCtx persists a new organization + owner user. The bcrypt hashing and
// the token scheme are unchanged; only the storage backend is pluggable.
func (s *Service) RegisterCtx(ctx context.Context, orgName, email, password string) (*Organization, *User, error) {
	if strings.TrimSpace(orgName) == "" {
		return nil, nil, errors.New("organization name is required")
	}
	if strings.TrimSpace(email) == "" {
		return nil, nil, errors.New("email is required")
	}
	if strings.TrimSpace(password) == "" {
		return nil, nil, errors.New("password is required")
	}

	if s.store != nil {
		if _, err := s.store.GetUserByEmail(ctx, strings.TrimSpace(email)); err == nil {
			return nil, nil, errors.New("email already registered")
		} else if !errors.Is(err, ErrUserNotFound) {
			return nil, nil, err
		}
	} else if _, exists := s.findUserByEmail(email); exists {
		return nil, nil, errors.New("email already registered")
	}

	org := &Organization{ID: uuid.NewString(), Name: orgName}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, nil, err
	}
	user := &User{
		ID:           uuid.NewString(),
		Organization: org.ID,
		Email:        email,
		PasswordHash: hash,
		Role:         defaultRole,
		CreatedAt:    time.Now().UTC(),
		Active:       true,
	}

	if s.store != nil {
		if err := s.store.CreateOrganization(ctx, org); err != nil {
			return nil, nil, err
		}
		if err := s.store.CreateUser(ctx, user); err != nil {
			return nil, nil, err
		}
	}
	s.orgs[org.ID] = org
	s.users[user.ID] = user
	return org, user, nil
}

// Login is the legacy context-free entry point; see LoginCtx.
func (s *Service) Login(email, password string) (string, error) {
	return s.LoginCtx(context.Background(), email, password)
}

// ErrAccountDisabled is returned by LoginCtx when the credential is correct
// but the account has been deprovisioned (SCIM active=false, issue #29).
var ErrAccountDisabled = errors.New("account is disabled")

// ErrInvalidToken is returned by LogoutCtx when the presented credential is
// not a syntactically valid, correctly signed token at all. Logout of a
// merely expired or already-revoked token is an idempotent success, not an
// error (issue #76).
var ErrInvalidToken = errors.New("invalid token")

// LoginCtx verifies credentials against the store (when present) and issues
// the same HMAC token as before. The token scheme is unchanged; the only
// addition is the SCIM lifecycle check: a disabled account can never obtain
// a session even with valid credentials.
func (s *Service) LoginCtx(ctx context.Context, email, password string) (string, error) {
	user, err := s.lookupUser(ctx, email)
	if err != nil {
		return "", errors.New("invalid credentials")
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return "", errors.New("invalid credentials")
	}
	if !user.Active {
		return "", ErrAccountDisabled
	}
	return s.GenerateToken(user)
}

// lookupUser resolves a login identity: store first (Postgres), memory cache
// as fallback for users registered in this process.
func (s *Service) lookupUser(ctx context.Context, email string) (*User, error) {
	if s.store != nil && strings.TrimSpace(email) != "" {
		user, err := s.store.GetUserByEmail(ctx, strings.TrimSpace(email))
		if err == nil {
			return user, nil
		}
		if !errors.Is(err, ErrUserNotFound) {
			return nil, err
		}
	}
	if user, ok := s.findUserByEmail(email); ok {
		return user, nil
	}
	return nil, ErrUserNotFound
}

// findUserByEmailCtx is used by middleware where a request context exists.
func (s *Service) findUserByEmailCtx(ctx context.Context, email string) (*User, bool) {
	user, err := s.lookupUser(ctx, email)
	if err != nil || user == nil {
		return nil, false
	}
	return user, true
}

func signJWT(data []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(data)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (s *Service) GenerateToken(user *User) (string, error) {
	if user == nil {
		return "", errors.New("user is required")
	}
	claims := Claims{
		UserID:         user.ID,
		OrganizationID: user.Organization,
		Email:          user.Email,
		Role:           normalizeRole(user.Role),
		Exp:            time.Now().Add(24 * time.Hour).Unix(),
		JTI:            uuid.NewString(), // per-token revocation identity (issue #76)
	}
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	head64 := base64.RawURLEncoding.EncodeToString(header)
	payload64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := head64 + "." + payload64
	sig := signJWT([]byte(signingInput), s.jwtSecret)
	return signingInput + "." + sig, nil
}

// ParseToken verifies the token's HMAC signature and decodes its claims
// WITHOUT consulting expiry or the revocation deny list. Used by the logout
// transport (which must stay idempotent for already-revoked/expired tokens
// and still needs the actor identity for the audit trail) and internally by
// ValidateToken.
func (s *Service) ParseToken(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid token")
	}
	head64, payload64, sig := parts[0], parts[1], parts[2]
	expected := signJWT([]byte(head64+"."+payload64), s.jwtSecret)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return nil, errors.New("invalid token signature")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload64)
	if err != nil {
		return nil, errors.New("invalid token payload")
	}
	var claims Claims
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil, errors.New("invalid token payload")
	}
	return &claims, nil
}

// ValidateToken authenticates a presented token: signature check first,
// then natural expiry, then the revocation deny list (issue #76). The
// signature stayed identical to the pre-#76 scheme; the only functional
// addition is the deny-list consultation, so existing tokens (with or
// without a jti claim) keep validating unchanged.
func (s *Service) ValidateToken(token string) (*Claims, error) {
	claims, err := s.ParseToken(token)
	if err != nil {
		return nil, err
	}
	if time.Now().Unix() > claims.Exp {
		return nil, errors.New("token expired")
	}
	if s.revocations != nil {
		// Deny-list lookup without a request context: the ValidateToken
		// signature is frozen (middleware compatibility), matching the
		// legacy context-free entry points. Fail-closed: a deny-list backend
		// outage denies the request rather than bypassing revocation.
		revoked, err := s.revocations.Revoked(context.Background(), TokenReference(token, claims))
		if err != nil {
			return nil, err
		}
		if revoked {
			return nil, ErrTokenRevoked
		}
	}
	return claims, nil
}

// LogoutCtx revokes the presented token server-side (issue #76): its
// TokenReference lands on the deny list with a TTL equal to the token's
// remaining lifetime, so the entry self-expires when the token would have
// expired anyway. Idempotent by construction: logging out an already-revoked
// or already-expired token is a successful no-op; only a credential that is
// not a validly signed token at all returns ErrInvalidToken. With a nil
// revocation store (explicitly degraded mode) it is always a no-op success.
func (s *Service) LogoutCtx(ctx context.Context, token string) error {
	claims, err := s.ParseToken(token)
	if err != nil {
		return ErrInvalidToken
	}
	if s.revocations == nil {
		return nil // degraded mode: nothing to write, still a success
	}
	remaining := time.Until(time.Unix(claims.Exp, 0))
	if remaining <= 0 {
		return nil // already unusable; no deny row can outlive its purpose
	}
	return s.revocations.Revoke(ctx, TokenReference(token, claims), remaining)
}

// RefreshResult carries everything the refresh transport needs: the freshly
// rotated token, the current identity row it was derived from, and the
// deny-list references of both tokens (for the audit trail).
type RefreshResult struct {
	Token  string
	User   *User
	OldRef string
	NewRef string
}

// RefreshCtx exchanges a valid, unexpired, unrevoked token for a fresh one
// with a rotated jti (issue #76) — single-use refresh: the old token is
// revoked server-side BEFORE the new one is minted, so a rotation can never
// leave two live tokens, and replaying the old token against ValidateToken
// (or a second refresh) fails with ErrTokenRevoked. The fresh claims are
// re-derived from the user's CURRENT stored row, so SCIM deprovisioning
// (active=false) and role changes apply exactly as they do at password
// login: a deprovisioned user is refused with ErrAccountDisabled and an
// unresolvable identity is refused outright.
func (s *Service) RefreshCtx(ctx context.Context, token string) (*RefreshResult, error) {
	claims, err := s.ValidateToken(token) // signature + expiry + deny list
	if err != nil {
		return nil, err
	}
	user, err := s.lookupUser(ctx, claims.Email)
	if err != nil {
		return nil, fmt.Errorf("token identity is no longer resolvable: %w", err)
	}
	if !user.Active {
		return nil, ErrAccountDisabled
	}
	if s.revocations != nil {
		remaining := time.Until(time.Unix(claims.Exp, 0))
		if remaining > 0 {
			if err := s.revocations.Revoke(ctx, TokenReference(token, claims), remaining); err != nil {
				return nil, fmt.Errorf("refresh rotation failed: %w", err)
			}
		}
	}
	// GenerateToken re-derives every claim from the current row and mints a
	// new jti with a full 24h window (sliding session). It cannot fail after
	// the revoke above except on crypto/encoding errors, in which case the
	// caller's old token stays dead: rotation fails closed.
	fresh, err := s.GenerateToken(user)
	if err != nil {
		return nil, err
	}
	newRef := ""
	if newClaims, err := s.ParseToken(fresh); err == nil {
		newRef = TokenReference(fresh, newClaims)
	}
	return &RefreshResult{
		Token:  fresh,
		User:   user,
		OldRef: TokenReference(token, claims),
		NewRef: newRef,
	}, nil
}

func normalizeRole(role string) string {
	switch strings.ToUpper(strings.TrimSpace(role)) {
	case "OWNER", "ADMIN", "MEMBER", "VIEWER":
		return strings.ToUpper(strings.TrimSpace(role))
	default:
		return "VIEWER"
	}
}

func (s *Service) HasPermission(user *User, permission Permission) bool {
	if user == nil {
		return false
	}
	userRole := normalizeRole(user.Role)
	perms, ok := rolePermissions[userRole]
	if !ok {
		return false
	}
	for _, p := range perms {
		if p == permission {
			return true
		}
	}
	return false
}

func (s *Service) HasPermissionForOrg(user *User, orgID string, permission Permission) bool {
	if user == nil || strings.TrimSpace(orgID) == "" {
		return false
	}
	if user.Organization != orgID {
		return false
	}
	return s.HasPermission(user, permission)
}

func (s *Service) findUserByEmail(email string) (*User, bool) {
	for _, user := range s.users {
		if strings.EqualFold(user.Email, email) {
			return user, true
		}
	}
	return nil, false
}
