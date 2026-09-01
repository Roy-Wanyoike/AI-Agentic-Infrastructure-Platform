package runs
package runs

import (
    "errors"
    "fmt"
    "sync"
    "time"
)

type RunStatus string

const (
    StatusQueued RunStatus = "QUEUED"
    StatusRunning RunStatus = "RUNNING"
    StatusCompleted RunStatus = "COMPLETED"
    StatusFailed RunStatus = "FAILED"
)

type Run struct {
    ID string
    OrganizationID string
    AgentID string
    Input string
    Output string
    Status RunStatus
    CreatedAt time.Time
    UpdatedAt time.Time
}

type Service struct {
    mu sync.Mutex
    runs map[string]*Run
}

func NewService() *Service {
    return &Service{runs: make(map[string]*Run)}
}

func (s *Service) Create(orgID, agentID, input string) (*Run, error) {
    if orgID == "" || agentID == "" {
        return nil, errors.New("organization and agent id required")
    }
    s.mu.Lock(); defer s.mu.Unlock()
    id := fmt.Sprintf("run-%d", len(s.runs)+1)
    run := &Run{ID: id, OrganizationID: orgID, AgentID: agentID, Input: input, Status: StatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
    s.runs[id] = run
    return run, nil
}

func (s *Service) Get(id string) (*Run, bool) {
    s.mu.Lock(); defer s.mu.Unlock()
    r, ok := s.runs[id]
    return r, ok
}

func (s *Service) UpdateStatus(id string, status RunStatus, output string) error {
    s.mu.Lock(); defer s.mu.Unlock()
    r, ok := s.runs[id]
    if !ok { return errors.New("run not found") }
    r.Status = status
    if output != "" {
        r.Output = output
    }
    r.UpdatedAt = time.Now().UTC()
    return nil
}
