package events

import (
	"context"
	"os"
	"strings"
	"sync"
)

// EnvNATSURL is the environment variable NewFromEnv consults. Empty or unset
// selects the NoopPublisher (zero-infrastructure mode).
const EnvNATSURL = "AGENTOS_NATS_URL"

// NoopPublisher discards every event. It exists so the platform keeps running
// (and every call site keeps compiling) with zero infrastructure. Subscribing
// yields a channel that never receives and a no-op cancel func.
type NoopPublisher struct{}

// NewNoopPublisher returns the discard-everything publisher.
func NewNoopPublisher() *NoopPublisher { return &NoopPublisher{} }

// Publish discards the event; it never fails for well-formed events.
func (n *NoopPublisher) Publish(_ context.Context, event Event) error {
	return validate(&event)
}

// Subscribe returns a channel that never receives (noop mode has no events in
// flight). cancel is safe to call any number of times.
func (n *NoopPublisher) Subscribe(_ []string) (<-chan Event, func(), error) {
	ch := make(chan Event)
	var once sync.Once
	cancel := func() {
		once.Do(func() { close(ch) })
	}
	return ch, cancel, nil
}

// MemoryPublisher is the in-process publisher: a bounded ring buffer (1000
// events, oldest overwritten) plus per-subscriber channels with non-blocking
// fan-out (a subscriber that stops draining gets its events dropped and a
// dropped counter bump — never blocks producers).
type MemoryPublisher struct {
	mu      sync.Mutex
	buffer  []Event
	ringCap int
	subs    map[int]*memSubscriber
	nextID  int
	closed  bool
	dropped uint64
}

type memSubscriber struct {
	types map[string]struct{} // empty set = all event types
	ch    chan Event
}

// MemoryRingCapacity is the default ring buffer size.
const MemoryRingCapacity = 1000

// subscriberChannelBuffer sizes each subscriber's channel; slower consumers
// than this lag behind and start dropping (never blocking the publisher).
const subscriberChannelBuffer = 128

// NewMemoryPublisher returns an in-process publisher with a 1000-event ring.
func NewMemoryPublisher() *MemoryPublisher {
	return &MemoryPublisher{
		buffer:  make([]Event, 0, MemoryRingCapacity),
		ringCap: MemoryRingCapacity,
		subs:    make(map[int]*memSubscriber),
	}
}

// Publish validates the event, normalizes identity fields, appends to the ring
// buffer and fans out to matching subscribers. It never blocks on a consumer.
func (m *MemoryPublisher) Publish(_ context.Context, event Event) error {
	if err := validate(&event); err != nil {
		return err
	}
	ensureDefaults(&event)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrSubscribeClosed
	}
	if len(m.buffer) == m.ringCap {
		copy(m.buffer, m.buffer[1:])
		m.buffer[len(m.buffer)-1] = event
	} else {
		m.buffer = append(m.buffer, event)
	}
	for _, sub := range m.subs {
		if !sub.matches(event.Type) {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			m.dropped++
		}
	}
	return nil
}

// Subscribe registers a channel for the given event types (empty = all). The
// cancel func unsubscribes and closes the channel; it is idempotent.
func (m *MemoryPublisher) Subscribe(types []string) (<-chan Event, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, nil, ErrSubscribeClosed
	}
	id := m.nextID
	m.nextID++
	sub := &memSubscriber{types: make(map[string]struct{}, len(types)), ch: make(chan Event, subscriberChannelBuffer)}
	for _, t := range types {
		if t = strings.TrimSpace(t); t != "" {
			sub.types[t] = struct{}{}
		}
	}
	m.subs[id] = sub
	cancel := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if cur, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(cur.ch)
		}
	}
	return sub.ch, cancel, nil
}

// Close tears the publisher down: further Publish/Subscribe calls fail and all
// subscriber channels are closed.
func (m *MemoryPublisher) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	for id, sub := range m.subs {
		close(sub.ch)
		delete(m.subs, id)
	}
}

// Snapshot returns a copy of the ring buffer contents (oldest first). Useful
// for tests and diagnostics.
func (m *MemoryPublisher) Snapshot() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.buffer))
	copy(out, m.buffer)
	return out
}

// Dropped reports how many events were dropped because a subscriber channel
// was full.
func (m *MemoryPublisher) Dropped() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropped
}

// Subscribers reports the number of active subscriptions (diagnostics helper;
// tests use it to wait until a worker has actually subscribed).
func (m *MemoryPublisher) Subscribers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

func (s *memSubscriber) matches(eventType string) bool {
	if len(s.types) == 0 {
		return true
	}
	_, ok := s.types[eventType]
	return ok
}

// NewFromEnv builds a publisher from AGENTOS_NATS_URL:
//
//   - unset/empty  -> NoopPublisher (zero-infrastructure mode)
//   - set, healthy -> NATSPublisher (JetStream)
//   - set, down    -> MemoryPublisher fallback (platform keeps running)
//
// This is the constructor cmd/api should call at startup.
func NewFromEnv() Publisher {
	url := strings.TrimSpace(os.Getenv(EnvNATSURL))
	if url == "" {
		return NewNoopPublisher()
	}
	pub, err := NewNATSPublisher(url)
	if err != nil {
		return NewMemoryPublisher()
	}
	return pub
}
