package agents

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Agent struct {
	ID               string
	OrganizationID   string
	Name             string
	Description      string
	Instructions     string
	Model            string
	Status           string
	CurrentVersionID string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type AgentVersion struct {
	ID           string
	AgentID      string
	Version      int
	Instructions string
	Model        string
	CreatedAt    time.Time
}

var ErrAgentNotFound = errors.New("agent not found")

// Store abstracts durable agent storage. Implementations MUST scope every
// query by organization_id (tenant guard); the only exception is
// GetAgentByID, an explicit trusted internal path for workers which callers
// must treat as unscoped until the agent's OrganizationID is enforced.
type Store interface {
	// CreateAgent inserts the agent row within one tenant (organization_id guard).
	CreateAgent(ctx context.Context, orgID string, agent *Agent) error
	// GetAgent fetches an agent strictly within one tenant (organization_id guard).
	GetAgent(ctx context.Context, orgID, id string) (*Agent, error)
	// GetAgentByID fetches by primary key (trusted internal worker path).
	GetAgentByID(ctx context.Context, id string) (*Agent, error)
	// ListAgents returns the agents of exactly one tenant (organization_id guard).
	ListAgents(ctx context.Context, orgID string) ([]*Agent, error)
	// UpdateAgent updates an agent within one tenant (organization_id guard).
	UpdateAgent(ctx context.Context, orgID string, agent *Agent) error
	// DeleteAgent removes an agent within one tenant (organization_id guard).
	DeleteAgent(ctx context.Context, orgID, id string) error
	// CreateAgentVersion inserts a version row within one tenant
	// (organization_id enforced via the agents join).
	CreateAgentVersion(ctx context.Context, orgID string, version *AgentVersion) error
	// ListAgentVersions returns versions of one agent within one tenant
	// (organization_id guard via the agents join).
	ListAgentVersions(ctx context.Context, orgID, agentID string) ([]*AgentVersion, error)
	// NextAgentVersionNumber computes the next version number within one tenant.
	NextAgentVersionNumber(ctx context.Context, orgID, agentID string) (int, error)
}

type Service struct {
	mu       sync.Mutex
	agents   map[string]*Agent
	versions map[string][]*AgentVersion
	store    Store
}

func NewService() *Service {
	return &Service{agents: make(map[string]*Agent), versions: make(map[string][]*AgentVersion)}
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

// Create is the legacy context-free entry point; see CreateAgentCtx.
func (s *Service) Create(orgID, name, description, instructions, model string) (*Agent, error) {
	return s.CreateAgentCtx(context.Background(), orgID, name, description, instructions, model)
}

// CreateAgentCtx persists a new agent plus its initial version (v1).
func (s *Service) CreateAgentCtx(ctx context.Context, orgID, name, description, instructions, model string) (*Agent, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("agent name is required")
	}
	if strings.TrimSpace(instructions) == "" {
		return nil, errors.New("instructions are required")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model is required")
	}

	now := time.Now().UTC()
	agent := &Agent{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Name:           name,
		Description:    description,
		Instructions:   instructions,
		Model:          model,
		Status:         "DRAFT",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	version := &AgentVersion{
		ID:           uuid.NewString(),
		AgentID:      agent.ID,
		Version:      1,
		Instructions: instructions,
		Model:        model,
		CreatedAt:    now,
	}
	agent.CurrentVersionID = version.ID

	if s.store != nil {
		if err := s.store.CreateAgent(ctx, orgID, agent); err != nil {
			return nil, err
		}
		if err := s.store.CreateAgentVersion(ctx, orgID, version); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.agents[agent.ID] = agent
	s.versions[agent.ID] = append(s.versions[agent.ID], version)
	s.mu.Unlock()
	return agent, nil
}

// Get is the legacy context-free primary-key lookup used by trusted internal
// workers; tenant-scoped API callers must use GetAgentCtx.
func (s *Service) Get(id string) (*Agent, bool) {
	agent, err := s.GetAgentByIDCtx(context.Background(), id)
	if err != nil {
		return nil, false
	}
	return agent, true
}

// GetAgentByIDCtx resolves an agent by primary key. Trusted internal path
// (the returned agent carries OrganizationID so callers can enforce tenancy).
func (s *Service) GetAgentByIDCtx(ctx context.Context, id string) (*Agent, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrAgentNotFound
	}
	if s.store != nil {
		return s.store.GetAgentByID(ctx, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	agent, ok := s.agents[id]
	if !ok {
		return nil, ErrAgentNotFound
	}
	return agent, nil
}

// GetAgentCtx resolves an agent strictly within one tenant
// (organization_id guard) - the API-facing path.
func (s *Service) GetAgentCtx(ctx context.Context, orgID, id string) (*Agent, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrAgentNotFound
	}
	if s.store != nil {
		return s.store.GetAgent(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	agent, ok := s.agents[id]
	if !ok || agent.OrganizationID != orgID {
		return nil, ErrAgentNotFound
	}
	return agent, nil
}

// List is the legacy context-free entry point; see ListAgentsCtx.
func (s *Service) List(orgID string) []*Agent {
	list, err := s.ListAgentsCtx(context.Background(), orgID)
	if err != nil {
		return []*Agent{}
	}
	return list
}

// ListAgentsCtx returns all agents of exactly one tenant (organization_id guard).
func (s *Service) ListAgentsCtx(ctx context.Context, orgID string) ([]*Agent, error) {
	if strings.TrimSpace(orgID) == "" {
		return []*Agent{}, errors.New("organization id is required")
	}
	if s.store != nil {
		return s.store.ListAgents(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Agent, 0)
	for _, agent := range s.agents {
		if agent.OrganizationID == orgID {
			out = append(out, agent)
		}
	}
	return out, nil
}

// UpdateAgentCtx updates name/description/instructions/model/status within one
// tenant (organization_id guard).
func (s *Service) UpdateAgentCtx(ctx context.Context, orgID string, agent *Agent) error {
	if agent == nil || strings.TrimSpace(agent.ID) == "" {
		return ErrAgentNotFound
	}
	if strings.TrimSpace(orgID) == "" {
		return errors.New("organization id is required")
	}
	if s.store != nil {
		if err := s.store.UpdateAgent(ctx, orgID, agent); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if cached, ok := s.agents[agent.ID]; ok && cached.OrganizationID == orgID {
		*cached = *agent
	}
	s.mu.Unlock()
	return nil
}

// DeleteAgentCtx removes an agent within one tenant (organization_id guard).
func (s *Service) DeleteAgentCtx(ctx context.Context, orgID, id string) error {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return ErrAgentNotFound
	}
	if s.store != nil {
		if err := s.store.DeleteAgent(ctx, orgID, id); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if agent, ok := s.agents[id]; ok && agent.OrganizationID == orgID {
		delete(s.agents, id)
		delete(s.versions, id)
	}
	return nil
}

// CreateVersion is the legacy context-free entry point; the tenant is resolved
// from the agent row before the scoped writes happen.
func (s *Service) CreateVersion(agentID, instructions, model string) (*AgentVersion, error) {
	return s.CreateVersionCtx(context.Background(), "", agentID, instructions, model)
}

// CreateVersionCtx creates a new agent version. orgID may be empty only for
// trusted internal callers (it is then resolved from the agent row); when
// provided and mismatched, the agent is treated as not found (tenant guard).
func (s *Service) CreateVersionCtx(ctx context.Context, orgID, agentID, instructions, model string) (*AgentVersion, error) {
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("agent id is required")
	}
	if strings.TrimSpace(instructions) == "" {
		return nil, errors.New("instructions are required")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model is required")
	}

	agent, err := s.GetAgentByIDCtx(ctx, agentID)
	if err != nil {
		return nil, ErrAgentNotFound
	}
	if strings.TrimSpace(orgID) == "" {
		orgID = agent.OrganizationID
	}
	if agent.OrganizationID != orgID {
		return nil, ErrAgentNotFound
	}

	now := time.Now().UTC()
	if s.store != nil {
		number, err := s.store.NextAgentVersionNumber(ctx, orgID, agentID)
		if err != nil {
			return nil, err
		}
		version := &AgentVersion{ID: uuid.NewString(), AgentID: agentID, Version: number, Instructions: instructions, Model: model, CreatedAt: now}
		if err := s.store.CreateAgentVersion(ctx, orgID, version); err != nil {
			return nil, err
		}
		agent.Instructions = instructions
		agent.Model = model
		agent.CurrentVersionID = version.ID
		agent.UpdatedAt = now
		if err := s.store.UpdateAgent(ctx, orgID, agent); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if cached, ok := s.agents[agentID]; ok {
			*cached = *agent
		}
		s.versions[agentID] = append(s.versions[agentID], version)
		s.mu.Unlock()
		return version, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cached, ok := s.agents[agentID]
	if !ok {
		return nil, ErrAgentNotFound
	}
	current := len(s.versions[agentID]) + 1
	version := &AgentVersion{ID: uuid.NewString(), AgentID: agentID, Version: current, Instructions: instructions, Model: model, CreatedAt: now}
	s.versions[agentID] = append(s.versions[agentID], version)
	cached.CurrentVersionID = version.ID
	cached.Instructions = instructions
	cached.Model = model
	cached.UpdatedAt = now
	return version, nil
}

// Versions is the legacy context-free entry point; the tenant is resolved from
// the agent row before the scoped read happens.
func (s *Service) Versions(agentID string) []*AgentVersion {
	agent, err := s.GetAgentByIDCtx(context.Background(), agentID)
	if err != nil {
		return nil
	}
	versions, err := s.ListVersionsCtx(context.Background(), agent.OrganizationID, agentID)
	if err != nil {
		return nil
	}
	return versions
}

// ListVersionsCtx returns the versions of one agent within one tenant
// (organization_id guard).
func (s *Service) ListVersionsCtx(ctx context.Context, orgID, agentID string) ([]*AgentVersion, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(agentID) == "" {
		return nil, ErrAgentNotFound
	}
	if s.store != nil {
		return s.store.ListAgentVersions(ctx, orgID, agentID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if agent, ok := s.agents[agentID]; !ok || agent.OrganizationID != orgID {
		return nil, ErrAgentNotFound
	}
	return append([]*AgentVersion(nil), s.versions[agentID]...), nil
}
