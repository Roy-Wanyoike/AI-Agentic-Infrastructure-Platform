package webhooks

import (
    "errors"
    "fmt"
    "strings"
    "sync"
    "time"
)

type Event struct {
    ID          string
    Type        string
    Payload     map[string]any
    CreatedAt   time.Time
}

type Endpoint struct {
    ID          string
    URL         string
    Secret      string
    Active      bool
    CreatedAt   time.Time
}

type Service struct {
    mu       sync.Mutex
    events   []*Event
    endpoints map[string]*Endpoint
}

func NewService() *Service {
    return &Service{events: make([]*Event, 0), endpoints: make(map[string]*Endpoint)}
}

func (s *Service) RegisterEndpoint(url, secret string) (*Endpoint, error) {
    if strings.TrimSpace(url) == "" { return nil, errors.New("url is required") }
    s.mu.Lock(); defer s.mu.Unlock()
    endpoint := &Endpoint{ID: fmt.Sprintf("webhook-%d", len(s.endpoints)+1), URL: url, Secret: secret, Active: true, CreatedAt: time.Now().UTC()}
    s.endpoints[endpoint.ID] = endpoint
    return endpoint, nil
}

func (s *Service) Publish(eventType string, payload map[string]any) *Event {
    s.mu.Lock(); defer s.mu.Unlock()
    event := &Event{ID: fmt.Sprintf("event-%d", len(s.events)+1), Type: eventType, Payload: payload, CreatedAt: time.Now().UTC()}
    s.events = append(s.events, event)
    return event
}

func (s *Service) Snapshot() []*Event {
    s.mu.Lock(); defer s.mu.Unlock()
    out := make([]*Event, len(s.events))
    copy(out, s.events)
    return out
}
