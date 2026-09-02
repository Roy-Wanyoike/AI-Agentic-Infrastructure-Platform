package webhooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Event struct {
	ID        string
	Type      string
	Payload   map[string]any
	CreatedAt time.Time
}

type Endpoint struct {
	ID        string
	URL       string
	Secret    string
	Active    bool
	Events    []string
	CreatedAt time.Time
}

type Service struct {
	mu        sync.Mutex
	events    []*Event
	endpoints map[string]*Endpoint
}

func NewService() *Service {
	return &Service{events: make([]*Event, 0), endpoints: make(map[string]*Endpoint)}
}

func (s *Service) RegisterEndpoint(url, secret string) (*Endpoint, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("url is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	endpoint := &Endpoint{ID: fmt.Sprintf("webhook-%d", len(s.endpoints)+1), URL: url, Secret: secret, Active: true, Events: []string{}, CreatedAt: time.Now().UTC()}
	s.endpoints[endpoint.ID] = endpoint
	return endpoint, nil
}

func (s *Service) Publish(eventType string, payload map[string]any) *Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	event := &Event{ID: fmt.Sprintf("event-%d", len(s.events)+1), Type: eventType, Payload: payload, CreatedAt: time.Now().UTC()}
	s.events = append(s.events, event)
	return event
}

func (s *Service) Dispatch(eventType string) []*Endpoint {
	s.mu.Lock()
	if !s.hasPublished(eventType) {
		s.mu.Unlock()
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
	event := s.lastEvent(eventType)
	s.mu.Unlock()

	for _, endpoint := range matches {
		s.deliver(endpoint, event)
	}
	return matches
}

func (s *Service) deliver(endpoint *Endpoint, event *Event) {
	if endpoint == nil || strings.TrimSpace(endpoint.URL) == "" || event == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"event":      event.Type,
		"payload":    event.Payload,
		"created_at": event.CreatedAt,
	})
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, endpoint.URL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agentos-webhook/1.0")
	if endpoint.Secret != "" {
		mac := hmac.New(sha256.New, []byte(endpoint.Secret))
		_, _ = mac.Write(payload)
		req.Header.Set("X-AgentOS-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}

func (s *Service) lastEvent(eventType string) *Event {
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i] != nil && s.events[i].Type == eventType {
			return s.events[i]
		}
	}
	return nil
}

func (s *Service) Snapshot() []*Event {
	s.mu.Lock()
	defer s.mu.Unlock()
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
