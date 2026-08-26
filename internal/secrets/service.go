package secrets

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

type Secret struct {
	Name  string
	Value string
}

type Service struct {
	mu      sync.Mutex
	secrets map[string]*Secret
}

func NewService() *Service {
	return &Service{secrets: make(map[string]*Secret)}
}

func (s *Service) Store(name, value string) (*Secret, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("secret name is required")
	}
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("secret value is required")
	}
	if s == nil {
		return nil, errors.New("secret service is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	secret := &Secret{Name: strings.TrimSpace(name), Value: value}
	s.secrets[secret.Name] = secret
	return secret, nil
}

func (s *Service) Get(name string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	secret, ok := s.secrets[strings.TrimSpace(name)]
	if !ok || secret == nil {
		return "", false
	}
	return secret.Value, true
}

func (s *Service) Rotate(name, value string) (*Secret, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("secret name is required")
	}
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("secret value is required")
	}
	if s == nil {
		return nil, errors.New("secret service is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	secret, ok := s.secrets[strings.TrimSpace(name)]
	if !ok || secret == nil {
		return nil, fmt.Errorf("secret %q not found", strings.TrimSpace(name))
	}
	secret.Value = value
	return secret, nil
}
