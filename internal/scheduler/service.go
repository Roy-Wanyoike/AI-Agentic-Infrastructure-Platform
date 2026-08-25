package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Schedule struct {
	ID        string
	Name      string
	Cron      string
	Enabled   bool
	NextRunAt time.Time
	CreatedAt time.Time
}

type Service struct {
	mu        sync.Mutex
	schedules map[string]*Schedule
}

func NewService() *Service {
	return &Service{ schedules: make(map[string]*Schedule) }
}

func (s *Service) Create(name, cron string) (*Schedule, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("schedule name is required")
	}
	if strings.TrimSpace(cron) == "" {
		return nil, errors.New("cron expression is required")
	}
	s.mu.Lock(); defer s.mu.Unlock()
	schedule := &Schedule{
		ID:        fmt.Sprintf("schedule-%d", len(s.schedules)+1),
		Name:      name,
		Cron:      cron,
		Enabled:   true,
		NextRunAt: time.Now().UTC().Add(5 * time.Minute),
		CreatedAt: time.Now().UTC(),
	}
	s.schedules[schedule.ID] = schedule
	return schedule, nil
}

func (s *Service) Get(id string) (*Schedule, bool) {
	s.mu.Lock(); defer s.mu.Unlock()
	schedule, ok := s.schedules[id]
	return schedule, ok
}

func (s *Service) Toggle(id string, enabled bool) error {
	schedule, ok := s.Get(id)
	if !ok {
		return errors.New("schedule not found")
	}
	schedule.Enabled = enabled
	return nil
}
