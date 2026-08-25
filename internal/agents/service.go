package agents

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
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

type Service struct {
	mu       sync.Mutex
	agents  map[string]*Agent
	versions map[string][]*AgentVersion
}

func NewService() *Service {
	return &Service{agents: make(map[string]*Agent), versions: make(map[string][]*AgentVersion)}
}

func (s *Service) Create(orgID, name, description, instructions, model string) (*Agent, error) {
	if strings.TrimSpace(orgID) == "" { return nil, errors.New("organization id is required") }
	if strings.TrimSpace(name) == "" { return nil, errors.New("agent name is required") }
	if strings.TrimSpace(instructions) == "" { return nil, errors.New("instructions are required") }
	if strings.TrimSpace(model) == "" { return nil, errors.New("model is required") }

	s.mu.Lock()
	defer s.mu.Unlock()
	agent := &Agent{ID: fmt.Sprintf("agent-%d", len(s.agents)+1), OrganizationID: orgID, Name: name, Description: description, Instructions: instructions, Model: model, Status: "DRAFT", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	s.agents[agent.ID] = agent
	if _, err := s.CreateVersion(agent.ID, instructions, model); err != nil {
		return nil, err
	}
	return agent, nil
}

func (s *Service) Get(id string) (*Agent, bool) {
	s.mu.Lock(); defer s.mu.Unlock()
	agent, ok := s.agents[id]
	return agent, ok
}

func (s *Service) List(orgID string) []*Agent {
	s.mu.Lock(); defer s.mu.Unlock()
	out := make([]*Agent, 0)
	for _, agent := range s.agents {
		if agent.OrganizationID == orgID {
			out = append(out, agent)
		}
	}
	return out
}

func (s *Service) CreateVersion(agentID, instructions, model string) (*AgentVersion, error) {
	if strings.TrimSpace(agentID) == "" { return nil, errors.New("agent id is required") }
	if strings.TrimSpace(instructions) == "" { return nil, errors.New("instructions are required") }
	if strings.TrimSpace(model) == "" { return nil, errors.New("model is required") }

	s.mu.Lock(); defer s.mu.Unlock()
	agent, ok := s.agents[agentID]
	if !ok { return nil, errors.New("agent not found") }
	current := len(s.versions[agentID]) + 1
	version := &AgentVersion{ID: fmt.Sprintf("version-%s-%d", agentID, current), AgentID: agentID, Version: current, Instructions: instructions, Model: model, CreatedAt: time.Now().UTC()}
	s.versions[agentID] = append(s.versions[agentID], version)
	agent.CurrentVersionID = version.ID
	agent.Instructions = instructions
	agent.Model = model
	agent.UpdatedAt = time.Now().UTC()
	return version, nil
}

func (s *Service) Versions(agentID string) []*AgentVersion {
	s.mu.Lock(); defer s.mu.Unlock()
	return append([]*AgentVersion(nil), s.versions[agentID]...)
}
