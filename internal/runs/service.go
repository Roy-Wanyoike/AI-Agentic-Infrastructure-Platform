package runs

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"agentos/internal/streaming"
)

type RunStatus string

const (
	StatusQueued    RunStatus = "QUEUED"
	StatusRunning   RunStatus = "RUNNING"
	StatusCompleted RunStatus = "COMPLETED"
	StatusFailed    RunStatus = "FAILED"
)

type Run struct {
	ID             string
	OrganizationID string
	AgentID        string
	Input          string
	Output         string
	Status         RunStatus
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Service struct {
	mu       sync.Mutex
	runs     map[string]*Run
	streamer *streaming.Service
}

func NewService() *Service {
	return &Service{runs: make(map[string]*Run)}
}

func (s *Service) SetStreamer(st *streaming.Service) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamer = st
}

func (s *Service) Create(orgID, agentID, input string) (*Run, error) {
	if orgID == "" || agentID == "" {
		return nil, errors.New("organization and agent id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("run-%d", len(s.runs)+1)
	run := &Run{ID: id, OrganizationID: orgID, AgentID: agentID, Input: input, Status: StatusQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	s.runs[id] = run
	return run, nil
}

func (s *Service) Get(id string) (*Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	return r, ok
}

func (s *Service) UpdateStatus(id string, status RunStatus, output string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return errors.New("run not found")
	}
	r.Status = status
	if output != "" {
		r.Output = output
	}
	r.UpdatedAt = time.Now().UTC()
	// publish simple status event to streamer if available
	if s.streamer != nil {
		payload := map[string]any{"status": string(status)}
		if output != "" {
			payload["output"] = output
		}
		s.streamer.Publish(r.ID, "status", "status.changed", payload)
	}
	return nil
}
