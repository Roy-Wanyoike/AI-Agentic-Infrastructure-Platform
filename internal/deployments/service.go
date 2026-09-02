package deployments

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Environment struct {
	ID        string
	Name      string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Release struct {
	ID        string
	Version   string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Service struct {
	mu           sync.Mutex
	environments map[string]*Environment
	releases     map[string][]*Release
}

func NewService() *Service {
	return &Service{
		environments: make(map[string]*Environment),
		releases:     make(map[string][]*Release),
	}
}

func (s *Service) CreateEnvironment(name string) (*Environment, error) {
	if s == nil {
		return nil, errors.New("deployment service is nil")
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, errors.New("environment name is required")
	}
	if _, exists := s.environments[trimmed]; exists {
		return nil, fmt.Errorf("environment %q already exists", trimmed)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	env := &Environment{
		ID:        fmt.Sprintf("env-%d", len(s.environments)+1),
		Name:      trimmed,
		Status:    "ACTIVE",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	s.environments[trimmed] = env
	return env, nil
}

func (s *Service) AddRelease(envName, version string) (*Release, error) {
	if s == nil {
		return nil, errors.New("deployment service is nil")
	}
	envKey := strings.TrimSpace(envName)
	trimmed := strings.TrimSpace(version)
	if envKey == "" {
		return nil, errors.New("environment name is required")
	}
	if trimmed == "" {
		return nil, errors.New("release version is required")
	}
	if _, ok := s.environments[envKey]; !ok {
		return nil, fmt.Errorf("environment %q not found", envKey)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	release := &Release{
		ID:        fmt.Sprintf("rel-%d", len(s.releases[envKey])+1),
		Version:   trimmed,
		Status:    "PENDING",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	s.releases[envKey] = append(s.releases[envKey], release)
	return release, nil
}

func (s *Service) Promote(envName, version string) (*Release, error) {
	if s == nil {
		return nil, errors.New("deployment service is nil")
	}
	envKey := strings.TrimSpace(envName)
	trimmed := strings.TrimSpace(version)
	if envKey == "" {
		return nil, errors.New("environment name is required")
	}
	if trimmed == "" {
		return nil, errors.New("release version is required")
	}
	if _, ok := s.environments[envKey]; !ok {
		return nil, fmt.Errorf("environment %q not found", envKey)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, release := range s.releases[envKey] {
		if release.Version == trimmed {
			release.Status = "ACTIVE"
			release.UpdatedAt = time.Now().UTC()
			if env, ok := s.environments[envKey]; ok {
				env.Status = "ACTIVE"
				env.UpdatedAt = time.Now().UTC()
			}
			return release, nil
		}
	}
	return nil, fmt.Errorf("release %q not found in environment %q", trimmed, envKey)
}

func (s *Service) History(envName string) []*Release {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Release, len(s.releases[strings.TrimSpace(envName)]))
	copy(out, s.releases[strings.TrimSpace(envName)])
	return out
}
