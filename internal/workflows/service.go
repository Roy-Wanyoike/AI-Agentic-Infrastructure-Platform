package workflows

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type StepType string

const (
	StepAgent     StepType = "agent"
	StepTool      StepType = "tool"
	StepDelay     StepType = "delay"
	StepCondition StepType = "condition"
	StepEnd       StepType = "end"
)

type Step struct {
	ID     string
	Type   StepType
	Name   string
	Config map[string]any
}

type Workflow struct {
	ID        string
	Name      string
	Status    string
	Steps     []Step
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Service struct {
	mu         sync.Mutex
	workflows map[string]*Workflow
}

func NewService() *Service {
	return &Service{workflows: make(map[string]*Workflow)}
}

func (s *Service) Create(name string, steps []Step) (*Workflow, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("workflow name is required")
	}
	if len(steps) == 0 {
		return nil, errors.New("workflow requires at least one step")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wf := &Workflow{
		ID:        fmt.Sprintf("wf-%d", len(s.workflows)+1),
		Name:      name,
		Status:    "DRAFT",
		Steps:     steps,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	s.workflows[wf.ID] = wf
	return wf, nil
}

func (s *Service) Get(id string) (*Workflow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wf, ok := s.workflows[id]
	return wf, ok
}

func (s *Service) Execute(id string) (string, error) {
	wf, ok := s.Get(id)
	if !ok {
		return "", errors.New("workflow not found")
	}
	trace := make([]string, 0, len(wf.Steps))
	for _, step := range wf.Steps {
		trace = append(trace, fmt.Sprintf("%s:%s", step.Type, step.Name))
	}
	wf.Status = "RUNNING"
	wf.UpdatedAt = time.Now().UTC()
	wf.Status = "COMPLETED"
	return strings.Join(trace, " -> "), nil
}
