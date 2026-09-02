// Package streaming implements the in-memory pub/sub service backing the
// run-event SSE endpoint (GET /api/v1/runs/{id}/events).
//
// Hardening guarantees (Task 1-c):
//   - history is bounded: at most HistoryLimit events are retained per run;
//     the oldest are dropped first;
//   - slow subscribers never block the publisher: each subscriber has a
//     buffered channel and an event that cannot be enqueued is dropped for
//     that subscriber only, incrementing the service-wide dropped counter;
//   - Unsubscribe removes a subscriber and closes its channel exactly once
//     (guarded by the service mutex and a subscription index; never
//     double-closes, never sends on a closed channel);
//   - ServeSSE writes a proper text/event-stream response with periodic
//     keep-alive pings, flushing per event and terminating on request-context
//     cancellation (client disconnect).
//
// The original exported API (Subscribe / Publish / History) is unchanged.
package streaming

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event is a single execution event published for a run.
type Event struct {
	RunID     string
	Type      string
	Name      string
	Payload   map[string]any
	CreatedAt time.Time
}

const (
	// HistoryLimit caps retained events per run (bounded memory).
	HistoryLimit = 100
	// subscriberBuffer is the per-subscriber channel capacity.
	subscriberBuffer = 16
	// DefaultPingInterval is how often ServeSSE emits an SSE comment
	// keep-alive ("​: ping") when the stream is otherwise idle.
	DefaultPingInterval = 15 * time.Second
)

// Service is the in-memory streaming registry. It is safe for concurrent use.
type Service struct {
	mu           sync.Mutex
	streams      map[string][]chan Event
	history      map[string][]Event
	index        map[<-chan Event]chan Event // receive-only view -> owning channel
	dropped      uint64
	pingInterval time.Duration
}

// NewService creates a streaming service with the default history limit and
// ping interval.
func NewService() *Service {
	return &Service{
		streams:      make(map[string][]chan Event),
		history:      make(map[string][]Event),
		index:        make(map[<-chan Event]chan Event),
		pingInterval: DefaultPingInterval,
	}
}

// Subscribe registers a new subscriber for the given run and returns its
// receive-only event channel. The returned channel is closed exactly once by
// Unsubscribe (or never, if the subscriber never unsubscribes). Receivers
// must handle channel closure (`ev, ok := <-ch`).
func (s *Service) Subscribe(runID string) <-chan Event {
	if s == nil {
		return nil
	}
	ch := make(chan Event, subscriberBuffer)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil {
		s.streams = make(map[string][]chan Event)
	}
	if s.index == nil {
		s.index = make(map[<-chan Event]chan Event)
	}
	s.streams[runID] = append(s.streams[runID], ch)
	s.index[ch] = ch
	return ch
}

// Unsubscribe removes the subscriber associated with ch from the given run
// and closes its channel exactly once. It returns false when the subscriber
// is unknown for that run (already unsubscribed, subscribed under a different
// run ID, or never subscribed on this service) — in that case the channel is
// left untouched.
func (s *Service) Unsubscribe(runID string, ch <-chan Event) bool {
	if s == nil || ch == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	owner, ok := s.index[ch]
	if !ok {
		return false
	}
	subs := s.streams[runID]
	for i, c := range subs {
		if c == owner {
			s.streams[runID] = append(subs[:i], subs[i+1:]...)
			if len(s.streams[runID]) == 0 {
				delete(s.streams, runID)
			}
			delete(s.index, ch)
			close(owner)
			return true
		}
	}
	return false
}

// Publish appends the event to the run's bounded history and delivers it to
// every live subscriber. Delivery is non-blocking: a subscriber whose buffer
// is full misses the event (counted in DroppedTotal) instead of stalling the
// publisher. Sends happen under the service mutex so Unsubscribe can never
// close a channel mid-send.
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
	defer s.mu.Unlock()

	if s.history == nil {
		s.history = make(map[string][]Event)
	}
	s.history[runID] = append(s.history[runID], event)
	if limit := HistoryLimit; limit > 0 && len(s.history[runID]) > limit {
		overflow := len(s.history[runID]) - limit
		s.history[runID] = append(make([]Event, 0, limit), s.history[runID][overflow:]...)
	}

	for _, ch := range s.streams[runID] {
		select {
		case ch <- event:
		default:
			// Slow consumer: drop the event for this subscriber only.
			s.dropped++
		}
	}
}

// History returns a copy of the retained events for the run (bounded by
// HistoryLimit; oldest first).
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

// DroppedTotal reports how many event deliveries have been dropped because a
// subscriber's buffer was full (slow-consumer counter).
func (s *Service) DroppedTotal() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// ServeSSE writes the run event stream for runID as a Server-Sent Events
// response and blocks until the client disconnects (request context done) or
// the channel closes.
//
// Protocol details:
//   - Headers: Content-Type: text/event-stream, Cache-Control: no-cache,
//     Connection: keep-alive (plus X-Accel-Buffering: no so reverse proxies
//     do not buffer the stream).
//   - Buffered history is replayed first, then live events follow; the
//     subscriber is registered before the history snapshot so no event is
//     lost in between (events published during the replay are deduplicated
//     by their creation timestamp).
//   - Each event is a single frame: `data: {json}\n\n` where the JSON has
//     run_id, type, name, payload and created_at (RFC3339Nano) fields.
//   - A `: ping` comment keep-alive is written every DefaultPingInterval of
//     stream idleness.
func (s *Service) ServeSSE(w http.ResponseWriter, r *http.Request, runID string) {
	if s == nil {
		http.Error(w, "streaming service unavailable", http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe before snapshotting history so nothing is missed; duplicates
	// introduced by events published during the replay are filtered by
	// comparing CreatedAt below.
	sub := s.Subscribe(runID)
	defer func() { _ = s.Unsubscribe(runID, sub) }()

	writeFrame := func(body []byte) bool {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	writeEvent := func(ev Event) bool {
		b, err := json.Marshal(map[string]any{
			"run_id":     ev.RunID,
			"type":       ev.Type,
			"name":       ev.Name,
			"payload":    ev.Payload,
			"created_at": ev.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			b = []byte("{}")
		}
		return writeFrame(b)
	}

	var lastReplayed time.Time
	for _, ev := range s.History(runID) {
		if !writeEvent(ev) {
			return
		}
		if ev.CreatedAt.After(lastReplayed) {
			lastReplayed = ev.CreatedAt
		}
	}

	pingInterval := s.pingInterval
	if pingInterval <= 0 {
		pingInterval = DefaultPingInterval
	}
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case ev, open := <-sub:
			if !open {
				// Unsubscribed upstream: terminate the stream.
				return
			}
			if !ev.CreatedAt.After(lastReplayed) {
				continue // already replayed with history
			}
			if !writeEvent(ev) {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
