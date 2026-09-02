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
}

var ErrOrgNotFound = errors.New("organization not found")

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
		return errors.New("organization not found")
	}
	members, err := s.MembersCtx(ctx, orgID)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.UserID == userID {
			return errors.New("user is already a member")
		}
	}
	membership := Membership{UserID: userID, OrganizationID: orgID, Role: role}
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
