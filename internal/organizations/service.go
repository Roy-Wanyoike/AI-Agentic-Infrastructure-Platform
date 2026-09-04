package organizations

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Organization struct {
	ID        string
	Name      string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Membership struct {
	UserID         string
	OrganizationID string
	Role           string
	// CreatedAt is the joined_at timestamp of the membership row. The
	// zero value is only ever observed for rows created before the
	// column was projected (the durable store always persists it).
	CreatedAt time.Time
}

// Platform RBAC roles (mirrors the internal/auth rolePermissions keys).
// Membership rows carry the same enum so dashboards can render one badge
// per member regardless of which record a permission check consulted.
const (
	RoleOwner  = "OWNER"
	RoleAdmin  = "ADMIN"
	RoleMember = "MEMBER"
	RoleViewer = "VIEWER"
)

// NormalizeRole canonicalizes a role string (trim + upper). Unknown roles
// are returned as-is so callers can reject them with IsValidRole.
func NormalizeRole(role string) string {
	return strings.ToUpper(strings.TrimSpace(role))
}

// IsValidRole reports whether the role is one of the four platform roles.
func IsValidRole(role string) bool {
	switch NormalizeRole(role) {
	case RoleOwner, RoleAdmin, RoleMember, RoleViewer:
		return true
	}
	return false
}

var (
	ErrOrgNotFound = errors.New("organization not found")

	// ErrMembershipNotFound: the user has no membership row in the
	// tenant. Unknown AND foreign-organization user ids surface as this
	// one error so the transport can answer 404 with no existence leak.
	ErrMembershipNotFound = errors.New("membership not found")
	// ErrAlreadyMember: the user already has a membership row in the
	// tenant (POST /organization/members duplicate).
	ErrAlreadyMember = errors.New("user is already a member")
	// ErrLastOwner guards role demotions and removals that would leave
	// the tenant without any OWNER membership row. Registered owners
	// whose identity predates the members API have no membership row,
	// so the guard counts explicit OWNER rows only.
	ErrLastOwner = errors.New("cannot change the last owner of the organization")
	// ErrInvalidRole: role outside the OWNER/ADMIN/MEMBER/VIEWER enum.
	ErrInvalidRole = errors.New("role must be one of OWNER, ADMIN, MEMBER, VIEWER")
)

// Store abstracts durable organization storage. Every method takes a context;
// membership queries are guarded by organization_id so a tenant can only ever
// see its own rows (organizations are the tenant root).
type Store interface {
	// CreateOrganization inserts the tenant root row.
	CreateOrganization(ctx context.Context, org *Organization) error
	// GetOrganization fetches a tenant by primary key.
	GetOrganization(ctx context.Context, id string) (*Organization, error)
	// GetOrganizationByName resolves a tenant by name (registration uniqueness check).
	GetOrganizationByName(ctx context.Context, name string) (*Organization, error)
	// CreateMembership inserts a membership row scoped to one organization_id.
	CreateMembership(ctx context.Context, membership *Membership) error
	// ListMemberships returns the members of exactly one tenant (organization_id guard).
	ListMemberships(ctx context.Context, orgID string) ([]Membership, error)
	// UpdateMembershipRole changes the role of exactly one (organization_id,
	// user_id) membership row; RowsAffected == 0 (unknown user or foreign
	// tenant) returns ErrMembershipNotFound.
	UpdateMembershipRole(ctx context.Context, orgID, userID, role string) error
	// DeleteMembership removes exactly one (organization_id, user_id)
	// membership row with the same RowsAffected semantics.
	DeleteMembership(ctx context.Context, orgID, userID string) error
}

type Service struct {
	mu      sync.Mutex
	orgs    map[string]*Organization
	members map[string][]Membership
	store   Store
}

func NewService() *Service {
	return &Service{
		orgs:    make(map[string]*Organization),
		members: make(map[string][]Membership),
	}
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store; the in-memory maps act as a write-through cache for legacy callers.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	return s
}

func (s *Service) SetStore(store Store) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
}

// Create is the legacy context-free entry point kept for compatibility.
func (s *Service) Create(name string) (*Organization, error) {
	return s.CreateCtx(context.Background(), name)
}

// CreateCtx persists a new organization (tenant root).
func (s *Service) CreateCtx(ctx context.Context, name string) (*Organization, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("organization name is required")
	}
	if _, exists := s.FindByNameCtx(ctx, name); exists {
		return nil, errors.New("organization already exists")
	}

	now := time.Now().UTC()
	org := &Organization{
		ID:        uuid.NewString(),
		Name:      name,
		Status:    "ACTIVE",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if s.store != nil {
		if err := s.store.CreateOrganization(ctx, org); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orgs[org.ID] = org
	return org, nil
}

// FindByName is the legacy context-free lookup; see FindByNameCtx.
func (s *Service) FindByName(name string) (*Organization, bool) {
	return s.FindByNameCtx(context.Background(), name)
}

// FindByNameCtx resolves a tenant by name.
func (s *Service) FindByNameCtx(ctx context.Context, name string) (*Organization, bool) {
	if s.store != nil {
		org, err := s.store.GetOrganizationByName(ctx, strings.TrimSpace(name))
		if err == nil && org != nil {
			return org, true
		}
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, org := range s.orgs {
		if strings.EqualFold(org.Name, strings.TrimSpace(name)) {
			return org, true
		}
	}
	return nil, false
}

// Get is the legacy context-free lookup; see GetCtx.
func (s *Service) Get(id string) (*Organization, bool) {
	org, err := s.GetCtx(context.Background(), id)
	if err != nil {
		return nil, false
	}
	return org, true
}

// GetCtx fetches a tenant by primary key.
func (s *Service) GetCtx(ctx context.Context, id string) (*Organization, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrOrgNotFound
	}
	if s.store != nil {
		return s.store.GetOrganization(ctx, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	org, ok := s.orgs[id]
	if !ok {
		return nil, ErrOrgNotFound
	}
	return org, nil
}

// AddMember is the legacy context-free entry point; see AddMemberCtx.
func (s *Service) AddMember(orgID, userID, role string) error {
	return s.AddMemberCtx(context.Background(), orgID, userID, role)
}

// AddMemberCtx inserts a membership for one tenant (organization_id guard).
func (s *Service) AddMemberCtx(ctx context.Context, orgID, userID, role string) error {
	if strings.TrimSpace(orgID) == "" {
		return errors.New("organization id is required")
	}
	if strings.TrimSpace(userID) == "" {
		return errors.New("user id is required")
	}
	if strings.TrimSpace(role) == "" {
		return errors.New("role is required")
	}

	if _, err := s.GetCtx(ctx, orgID); err != nil {
		return ErrOrgNotFound
	}
	normalized := NormalizeRole(role)
	if !IsValidRole(normalized) {
		return ErrInvalidRole
	}
	members, err := s.MembersCtx(ctx, orgID)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.UserID == userID {
			return ErrAlreadyMember
		}
	}
	membership := Membership{UserID: userID, OrganizationID: orgID, Role: normalized, CreatedAt: time.Now().UTC()}
	if s.store != nil {
		if err := s.store.CreateMembership(ctx, &membership); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members[orgID] = append(s.members[orgID], membership)
	return nil
}

// UpdateMemberRoleCtx changes the role of one membership row inside one
// tenant. Demoting the last OWNER membership row is rejected with
// ErrLastOwner BEFORE anything is written, so the tenant can never lose its
// last owner-level member through this path. Unknown AND foreign-organization
// user ids surface as ErrMembershipNotFound (no existence leak).
func (s *Service) UpdateMemberRoleCtx(ctx context.Context, orgID, userID, role string) error {
	if strings.TrimSpace(orgID) == "" {
		return errors.New("organization id is required")
	}
	if strings.TrimSpace(userID) == "" {
		return errors.New("user id is required")
	}
	normalized := NormalizeRole(role)
	if !IsValidRole(normalized) {
		return ErrInvalidRole
	}
	members, err := s.MembersCtx(ctx, orgID)
	if err != nil {
		return err
	}
	target, found := findMembership(members, userID)
	if !found {
		return ErrMembershipNotFound
	}
	if strings.EqualFold(target.Role, RoleOwner) && normalized != RoleOwner && countOwners(members) <= 1 {
		return ErrLastOwner
	}
	if s.store != nil {
		if err := s.store.UpdateMembershipRole(ctx, orgID, target.UserID, normalized); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.members[orgID] {
		if s.members[orgID][i].UserID == target.UserID {
			s.members[orgID][i].Role = normalized
			break
		}
	}
	return nil
}

// RemoveMemberCtx deletes one membership row inside one tenant. Removing
// the last OWNER membership row is rejected with ErrLastOwner. Callers are
// responsible for the account-lifecycle half of a removal (deprovisioning):
// the HTTP layer pairs this with the same org-guarded identity deactivation
// the SCIM service uses, so a removed member cannot log in afterwards.
func (s *Service) RemoveMemberCtx(ctx context.Context, orgID, userID string) error {
	if strings.TrimSpace(orgID) == "" {
		return errors.New("organization id is required")
	}
	if strings.TrimSpace(userID) == "" {
		return errors.New("user id is required")
	}
	members, err := s.MembersCtx(ctx, orgID)
	if err != nil {
		return err
	}
	target, found := findMembership(members, userID)
	if !found {
		return ErrMembershipNotFound
	}
	if strings.EqualFold(target.Role, RoleOwner) && countOwners(members) <= 1 {
		return ErrLastOwner
	}
	if s.store != nil {
		if err := s.store.DeleteMembership(ctx, orgID, target.UserID); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	remaining := make([]Membership, 0, len(s.members[orgID]))
	for _, member := range s.members[orgID] {
		if member.UserID != target.UserID {
			remaining = append(remaining, member)
		}
	}
	s.members[orgID] = remaining
	return nil
}

// findMembership resolves one membership by user id (exact match).
func findMembership(members []Membership, userID string) (Membership, bool) {
	for _, member := range members {
		if member.UserID == userID {
			return member, true
		}
	}
	return Membership{}, false
}

// countOwners counts OWNER membership rows; the last-owner guard compares
// case-insensitively so legacy rows never slip past the protection.
func countOwners(members []Membership) int {
	owners := 0
	for _, member := range members {
		if strings.EqualFold(member.Role, RoleOwner) {
			owners++
		}
	}
	return owners
}

// Members is the legacy context-free entry point; see MembersCtx.
func (s *Service) Members(orgID string) []Membership {
	members, err := s.MembersCtx(context.Background(), orgID)
	if err != nil {
		return []Membership{}
	}
	return members
}

// MembersCtx lists the members of exactly one tenant (organization_id guard).
func (s *Service) MembersCtx(ctx context.Context, orgID string) ([]Membership, error) {
	if s.store != nil {
		return s.store.ListMemberships(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Membership, len(s.members[orgID]))
	copy(out, s.members[orgID])
	return out, nil
}
