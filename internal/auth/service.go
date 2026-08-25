package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
}

type Claims struct {
	UserID         string `json:"user_id"`
	OrganizationID string `json:"organization_id"`
	Email          string `json:"email"`
	Role           string `json:"role"`
	Exp            int64  `json:"exp"`
}

type Permission string

const (
	PermissionAgentsRead   Permission = "agents.read"
	PermissionAgentsWrite  Permission = "agents.write"
	PermissionRunsRead     Permission = "runs.read"
	PermissionRunsExecute  Permission = "runs.execute"
	PermissionUsersManage  Permission = "users.manage"
	PermissionOrgManage    Permission = "organization.manage"
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

type Service struct {
	jwtSecret string
	orgs      map[string]*Organization
	users     map[string]*User
}

func NewService(jwtSecret string) *Service {
	return &Service{
		jwtSecret: jwtSecret,
		orgs:      make(map[string]*Organization),
		users:     make(map[string]*User),
	}
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

func (s *Service) Register(orgName, email, password string) (*Organization, *User, error) {
	if strings.TrimSpace(orgName) == "" {
		return nil, nil, errors.New("organization name is required")
	}
	if strings.TrimSpace(email) == "" {
		return nil, nil, errors.New("email is required")
	}
	if strings.TrimSpace(password) == "" {
		return nil, nil, errors.New("password is required")
	}
	if _, exists := s.findUserByEmail(email); exists {
		return nil, nil, errors.New("email already registered")
	}

	org := &Organization{ID: fmt.Sprintf("org-%d", len(s.orgs)+1), Name: orgName}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, nil, err
	}
	user := &User{
		ID:           fmt.Sprintf("user-%d", len(s.users)+1),
		Organization: org.ID,
		Email:        email,
		PasswordHash: hash,
		Role:         defaultRole,
		CreatedAt:    time.Now().UTC(),
	}

	s.orgs[org.ID] = org
	s.users[user.ID] = user
	return org, user, nil
}

func (s *Service) Login(email, password string) (string, error) {
	user, ok := s.findUserByEmail(email)
	if !ok {
		return "", errors.New("invalid credentials")
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return "", errors.New("invalid credentials")
	}
	return s.GenerateToken(user)
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

func (s *Service) ValidateToken(token string) (*Claims, error) {
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
	if time.Now().Unix() > claims.Exp {
		return nil, errors.New("token expired")
	}
	return &claims, nil
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
