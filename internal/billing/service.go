package billing

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

type Plan struct {
	Name      string
	Quota     int
	Rate      int
	Consumed  int
	Enabled   bool
}

type Service struct {
	mu    sync.Mutex
	plans map[string]*Plan
}

func NewService() *Service {
	return &Service{plans: make(map[string]*Plan)}
}

func (s *Service) CreatePlan(name string, quota, rate int) (*Plan, error) {
	if s == nil {
		return nil, errors.New("billing service is nil")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("plan name is required")
	}
	if quota <= 0 {
		return nil, errors.New("quota must be positive")
	}
	if rate <= 0 {
		return nil, errors.New("rate must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.plans[strings.TrimSpace(name)]; exists {
		return nil, fmt.Errorf("plan %q already exists", name)
	}
	plan := &Plan{Name: strings.TrimSpace(name), Quota: quota, Rate: rate, Consumed: 0, Enabled: true}
	s.plans[plan.Name] = plan
	return plan, nil
}

func (s *Service) Consume(name string, amount int) (*Plan, error) {
	if s == nil {
		return nil, errors.New("billing service is nil")
	}
	if amount <= 0 {
		return nil, errors.New("amount must be positive")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("plan name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.plans[strings.TrimSpace(name)]
	if !ok || plan == nil {
		return nil, fmt.Errorf("plan %q not found", name)
	}
	if plan.Consumed+amount > plan.Quota {
		return nil, errors.New("quota exceeded")
	}
	plan.Consumed += amount
	return plan, nil
}

func (s *Service) Usage(name string) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.plans[strings.TrimSpace(name)]
	if !ok || plan == nil {
		return 0
	}
	return plan.Consumed
}
