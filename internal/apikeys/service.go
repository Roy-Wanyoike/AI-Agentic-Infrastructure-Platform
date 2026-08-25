package apikeys

import (
    "errors"
    "fmt"
    "strings"
    "sync"
    "time"
)

type APIKey struct {
    ID        string
    Name      string
    Value     string
    OrgID     string
    UserID    string
    CreatedAt time.Time
    Revoked   bool
}

type Service struct {
    mu    sync.Mutex
    keys  map[string]*APIKey
}

func NewService() *Service {
    return &Service{keys: make(map[string]*APIKey)}
}

func (s *Service) Create(orgID, userID, name string) (*APIKey, error) {
    if strings.TrimSpace(orgID) == "" { return nil, errors.New("organization id is required") }
    if strings.TrimSpace(userID) == "" { return nil, errors.New("user id is required") }
    if strings.TrimSpace(name) == "" { return nil, errors.New("key name is required") }
    s.mu.Lock(); defer s.mu.Unlock()
    key := &APIKey{
        ID:        fmt.Sprintf("key-%d", len(s.keys)+1),
        Name:      name,
        Value:     fmt.Sprintf("ak_%s_%s", orgID, time.Now().UTC().Format(time.RFC3339Nano)),
        OrgID:     orgID,
        UserID:    userID,
        CreatedAt: time.Now().UTC(),
        Revoked:   false,
    }
    s.keys[key.ID] = key
    return key, nil
}

func (s *Service) Revoke(id string) error {
    s.mu.Lock(); defer s.mu.Unlock()
    key, ok := s.keys[id]
    if !ok { return errors.New("api key not found") }
    key.Revoked = true
    return nil
}

func (s *Service) Validate(value string) (*APIKey, bool) {
    s.mu.Lock(); defer s.mu.Unlock()
    for _, key := range s.keys {
        if !key.Revoked && key.Value == value {
            return key, true
        }
    }
    return nil, false
}
