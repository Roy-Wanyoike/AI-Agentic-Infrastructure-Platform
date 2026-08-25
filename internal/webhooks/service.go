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
    Events      []string
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
    endpoint := &Endpoint{ID: fmt.Sprintf("webhook-%d", len(s.endpoints)+1), URL: url, Secret: secret, Active: true, Events: []string{}, CreatedAt: time.Now().UTC()}
    s.endpoints[endpoint.ID] = endpoint
    return endpoint, nil
}

func (s *Service) Publish(eventType string, payload map[string]any) *Event {
    s.mu.Lock(); defer s.mu.Unlock()
    event := &Event{ID: fmt.Sprintf("event-%d", len(s.events)+1), Type: eventType, Payload: payload, CreatedAt: time.Now().UTC()}
    s.events = append(s.events, event)
    return event
}

func (s *Service) Dispatch(eventType string) []*Endpoint {
    s.mu.Lock(); defer s.mu.Unlock()
    if !s.hasPublished(eventType) {
        return []*Endpoint{}
    }
    matches := make([]*Endpoint, 0)
    for _, endpoint := range s.endpoints {
        if !endpoint.Active {
            continue
        }
        if len(endpoint.Events) == 0 || contains(endpoint.Events, eventType) {
            matches = append(matches, endpoint)
        }
    }
    return matches
}

func (s *Service) Snapshot() []*Event {
    s.mu.Lock(); defer s.mu.Unlock()
    out := make([]*Event, len(s.events))
    copy(out, s.events)
    return out
}

func (s *Service) hasPublished(eventType string) bool {
    for _, event := range s.events {
        if event != nil && event.Type == eventType {
            return true
        }
    }
    return false
}

func contains(items []string, item string) bool {
    for _, candidate := range items {
        if candidate == item {
            return true
        }
    }
    return false
}
