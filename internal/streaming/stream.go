package streaming

import (
	"sync"
	"time"
)

type Event struct {
	RunID    string
	Type     string
	Name     string
	Payload  map[string]any
	CreatedAt time.Time
}

type Service struct {
	mu       sync.Mutex
	streams  map[string][]chan Event
	history  map[string][]Event
}

func NewService() *Service {
	return &Service{
		streams: make(map[string][]chan Event),
		history: make(map[string][]Event),
	}
}

func (s *Service) Subscribe(runID string) <-chan Event {
	if s == nil {
		return nil
	}
	ch := make(chan Event, 16)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams[runID] = append(s.streams[runID], ch)
	return ch
}

func (s *Service) Publish(runID, eventType, name string, payload map[string]any) {
	if s == nil {
		return
	}
	event := Event{
		RunID:     runID,
		Type:      eventType,
		Name:      name,
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	s.history[runID] = append(s.history[runID], event)
	listeners := append([]chan Event(nil), s.streams[runID]...)
	s.mu.Unlock()

	for _, ch := range listeners {
		select {
		case ch <- event:
		default:
		}
	}
}

func (s *Service) History(runID string) []Event {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.history[runID]))
	copy(out, s.history[runID])
	return out
}
