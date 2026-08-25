package organizations

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
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

type Service struct {
	mu      sync.Mutex
	orgs    map[string]*Organization
	members map[string][]Membership
}

func NewService() *Service {
	return &Service{
		orgs:    make(map[string]*Organization),
		members: make(map[string][]Membership),
	}
}

func (s *Service) Create(name string) (*Organization, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("organization name is required")
	}
	if _, exists := s.FindByName(name); exists {
		return nil, errors.New("organization already exists")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	org := &Organization{
		ID:        fmt.Sprintf("org-%d", len(s.orgs)+1),
		Name:      name,
		Status:    "ACTIVE",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	s.orgs[org.ID] = org
	return org, nil
}

func (s *Service) FindByName(name string) (*Organization, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, org := range s.orgs {
		if strings.EqualFold(org.Name, strings.TrimSpace(name)) {
			return org, true
		}
	}
	return nil, false
}

func (s *Service) Get(id string) (*Organization, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	org, ok := s.orgs[id]
	return org, ok
}

func (s *Service) AddMember(orgID, userID, role string) error {
	if strings.TrimSpace(orgID) == "" { return errors.New("organization id is required") }
	if strings.TrimSpace(userID) == "" { return errors.New("user id is required") }
	if strings.TrimSpace(role) == "" { return errors.New("role is required") }

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[orgID]; !ok {
		return errors.New("organization not found")
	}
	members := s.members[orgID]
	for _, member := range members {
		if member.UserID == userID {
			return errors.New("user is already a member")
		}
	}
	s.members[orgID] = append(members, Membership{UserID: userID, OrganizationID: orgID, Role: role})
	return nil
}

func (s *Service) Members(orgID string) []Membership {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Membership, len(s.members[orgID]))
	copy(out, s.members[orgID])
	return out
}
