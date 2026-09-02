package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// NATS defaults for the AgentOS event stream.
const (
	// NATSStreamName is the JetStream stream that durably retains events.
	NATSStreamName = "AGENTOS_EVENTS"
	// NATSSubjectPrefix is the subject namespace: agentos.events.<event.type>.
	NATSSubjectPrefix = "agentos.events"
	// natsConnectTimeout bounds the initial connection attempt so the caller
	// can fall back to the memory publisher quickly.
	natsConnectTimeout = 2 * time.Second
	// natsPublishWait bounds the JetStream publish ack wait.
	natsPublishWait = 5 * time.Second
	// natsSubscriberBuffer sizes the fan-in channel per subscription set.
	natsSubscriberBuffer = 128
)

// SubjectFor maps an event type to its NATS subject:
// "run.failed" -> "agentos.events.run.failed".
func SubjectFor(eventType string) string {
	return NATSSubjectPrefix + "." + eventType
}

// NATSPublisher publishes AgentOS events to NATS JetStream (durable stream
// AGENTOS_EVENTS, subjects agentos.events.<type>). The zero-downtime contract:
// the constructor returns an error when NATS is unreachable so the caller can
// fall back to the memory publisher — the platform never depends on NATS being
// up. It also implements Subscriber via ephemeral JetStream push consumers.
type NATSPublisher struct {
	conn *nats.Conn
	js   nats.JetStreamContext
}

// NewNATSPublisher connects to NATS at url, ensures the AGENTOS_EVENTS stream
// exists, and returns a Publisher/Subscriber. It returns an error when NATS is
// unreachable (connection refused / timeout / bad URL) — callers are expected
// to fall back to NewMemoryPublisher.
func NewNATSPublisher(url string) (*NATSPublisher, error) {
	if url == "" {
		return nil, fmt.Errorf("events: NATS url is required")
	}
	conn, err := nats.Connect(url,
		nats.Timeout(natsConnectTimeout),
		nats.NoReconnect(),
		nats.RetryOnFailedConnect(false),
	)
	if err != nil {
		return nil, fmt.Errorf("events: NATS connect failed: %w", err)
	}
	js, err := conn.JetStream(nats.PublishAsyncMaxPending(256))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("events: NATS JetStream unavailable: %w", err)
	}
	// Ensure the durable event stream exists; a stream that already exists
	// (from a previous run / another replica) is fine.
	if _, err = js.AddStream(&nats.StreamConfig{
		Name:     NATSStreamName,
		Subjects: []string{NATSSubjectPrefix + ".>"},
	}); err != nil && err != nats.ErrStreamNameAlreadyInUse {
		if _, existsErr := js.StreamInfo(NATSStreamName); existsErr != nil {
			conn.Close()
			return nil, fmt.Errorf("events: NATS stream setup failed: %w", err)
		}
	}
	return &NATSPublisher{conn: conn, js: js}, nil
}

// Publish marshals the event to JSON and publishes it (synchronously waiting
// for the JetStream ack) to agentos.events.<type>.
func (p *NATSPublisher) Publish(_ context.Context, event Event) error {
	if err := validate(&event); err != nil {
		return err
	}
	ensureDefaults(&event)
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("events: marshal event: %w", err)
	}
	if _, err := p.js.Publish(SubjectFor(event.Type), data, nats.AckWait(natsPublishWait)); err != nil {
		return fmt.Errorf("events: NATS publish %s: %w", event.Type, err)
	}
	return nil
}

// Subscribe creates one ephemeral JetStream push consumer per event type
// (empty types = a single consumer on agentos.events.>) fanning into one
// channel. The cancel func unsubscribes all consumers and closes the channel.
func (p *NATSPublisher) Subscribe(types []string) (<-chan Event, func(), error) {
	set := &natsSubscriptionSet{ch: make(chan Event, natsSubscriberBuffer)}
	subjects := make([]string, 0, len(types))
	if len(types) == 0 {
		subjects = append(subjects, NATSSubjectPrefix+".>")
	} else {
		for _, t := range types {
			if t != "" {
				subjects = append(subjects, SubjectFor(t))
			}
		}
	}

	handler := func(msg *nats.Msg) {
		var event Event
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			// Malformed payloads are skipped: the stream is for AgentOS events.
			return
		}
		set.deliver(event)
	}

	for _, subj := range subjects {
		sub, err := p.js.Subscribe(subj, handler,
			nats.BindStream(NATSStreamName),
			nats.DeliverNew(),
			nats.MaxAckPending(1024),
		)
		if err != nil {
			// roll back the subscriptions created so far
			for _, s := range set.subs {
				_ = s.Unsubscribe()
			}
			return nil, nil, fmt.Errorf("events: NATS subscribe %s: %w", subj, err)
		}
		set.subs = append(set.subs, sub)
	}

	return set.ch, set.cancel, nil
}

// natsSubscriptionSet guards one subscription set's channel: the NATS handler
// goroutine delivers under set.mu and cancel closes under the same lock, so a
// send on a closed channel is impossible.
type natsSubscriptionSet struct {
	mu     sync.Mutex
	closed bool
	ch     chan Event
	subs   []*nats.Subscription
}

func (s *natsSubscriptionSet) deliver(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- event:
	default:
		// slow consumer: drop rather than block the NATS dispatch loop
	}
}

func (s *natsSubscriptionSet) cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for _, sub := range s.subs {
		_ = sub.Unsubscribe()
	}
	close(s.ch)
}

// Close drains the connection. Cancelled subscriptions keep working until
// their own cancel func runs; Close is safe to defer in main.
func (p *NATSPublisher) Close() {
	if p == nil || p.conn == nil {
		return
	}
	p.conn.Close()
}
