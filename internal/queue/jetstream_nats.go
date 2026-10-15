package queue

// jetstream_nats.go is the production adapter from the narrow JetStream seam
// (jetstream.go) onto github.com/nats-io/nats.go. The dependency already
// exists in go.mod — the domain-event publisher (internal/events/nats.go) uses
// it — so this adapter is fully functional, env-gated by AGENTOS_MESSAGING
// (memory stays the default), and verified against an embedded nats-server in
// the queue package tests. It deliberately implements the SAME seam as the
// in-memory simulator: adding a different broker later (or upgrading to the
// newer jetstream package API) means writing another adapter file — the
// consumer framework, dispatcher and admin endpoints are untouched.
//
// Semantics mapping:
//
//   - EnsureStream  -> JetStream AddStream (idempotent; an already-existing
//     compatible stream is fine, mirroring internal/events).
//   - Publish       -> js.Publish (synchronous server ack; PublishAckWait 5s,
//     matching the events publisher).
//   - Subscribe     -> js.Subscribe with nats.Durable (server-side cursor +
//     delivery counts), nats.DeliverAll, nats.ManualAck,
//     nats.BindStream(MessagingStreamName), MaxAckPending 1024. The nats.go
//     connection keeps its DEFAULT reconnect behavior (unlimited reconnect
//     attempts with backoff): a broker restart transparently re-attaches
//     subscriptions, and the consumer framework's supervision loop
//     re-establishes any subscription that reports invalid.
//   - Deliveries    -> nats.MsgMetadata.NumDelivered (server-side attempt
//     count — survives process restarts, which the in-memory simulator
//     approximates per durable consumer).
//   - Log.Range     -> js.GetMsg walking the stream sequence space and
//     filtering by subject (admin-path only; linear in stream size).
//   - Log.Delete    -> js.DeleteMsg (JetStream message delete; the DLQ
//     requeue consumes the entry after republishing it).

import (
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// natsConnectTimeout bounds the initial connection attempt so a wrong
// AGENTOS_MESSAGING selection fails fast instead of hanging startup
// (mirrors internal/events).
const natsConnectTimeout = 2 * time.Second

// natsPublishAckWait bounds the JetStream publish ack wait.
const natsPublishAckWait = 5 * time.Second

// natsMaxAckPending caps unacknowledged in-flight messages per durable
// consumer; combined with the consumer framework's in-flight semaphore this
// is the broker-side backpressure bound.
const natsMaxAckPending = 1024

// NATSJetStream is the real-broker implementation of JetStream + Log.
type NATSJetStream struct {
	conn   *nats.Conn
	js     nats.JetStreamContext
	stream string
}

// Compile-time guards: the adapter satisfies both broker surfaces.
var (
	_ JetStream = (*NATSJetStream)(nil)
	_ Log       = (*NATSJetStream)(nil)
)

// NewNATSJetStream connects to the NATS server at url and ensures the
// AGENTOS_TASKS stream exists. It returns ErrBrokerUnavailable-classified
// errors when the broker is unreachable — the messaging constructor decides
// between fail-fast (nats mode) and fallback (hybrid mode).
func NewNATSJetStream(url string) (*NATSJetStream, error) {
	if url == "" {
		return nil, fmt.Errorf("%w: messaging NATS url is required", ErrBrokerUnavailable)
	}
	conn, err := nats.Connect(url,
		nats.Timeout(natsConnectTimeout),
		// Reconnect stays ON (library defaults: unlimited attempts,
		// exponential backoff): a broker restart must not lose the worker's
		// subscriptions. Publishes during the reconnect window fail with an
		// error the dispatcher surfaces (hybrid falls back, nats errors).
		nats.RetryOnFailedConnect(false),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: NATS connect failed: %v", ErrBrokerUnavailable, err)
	}
	js, err := conn.JetStream(nats.PublishAsyncMaxPending(256))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: NATS JetStream unavailable: %v", ErrBrokerUnavailable, err)
	}
	jsa := &NATSJetStream{conn: conn, js: js}
	if err := jsa.EnsureStream(MessagingStreamName, MessagingStreamSubjects); err != nil {
		conn.Close()
		return nil, fmt.Errorf("queue: NATS stream setup failed: %v", err)
	}
	return jsa, nil
}

// EnsureStream implements JetStream.
func (a *NATSJetStream) EnsureStream(name string, subjects []string) error {
	if name == "" {
		return fmt.Errorf("queue: stream name is required")
	}
	if _, err := a.js.AddStream(&nats.StreamConfig{
		Name:     name,
		Subjects: subjects,
	}); err != nil && err != nats.ErrStreamNameAlreadyInUse {
		// ErrStreamNameAlreadyInUse also fires when the stream exists with a
		// different config; verify it is actually present and usable.
		if _, existsErr := a.js.StreamInfo(name); existsErr != nil {
			return fmt.Errorf("queue: stream %s setup: %v", name, err)
		}
	}
	a.stream = name
	return nil
}

// Publish implements JetStream.
func (a *NATSJetStream) Publish(subject string, data []byte) (uint64, error) {
	ack, err := a.js.Publish(subject, data, nats.AckWait(natsPublishAckWait))
	if err != nil {
		return 0, err
	}
	return ack.Sequence, nil
}

// Subscribe implements JetStream.
func (a *NATSJetStream) Subscribe(subject, durable string, handler func(Message)) (Subscription, error) {
	if a.stream == "" {
		// EnsureStream always runs in the constructor; a programmatic
		// zero-value adapter would break the BindStream below.
		a.stream = MessagingStreamName
	}
	sub, err := a.js.Subscribe(subject, func(m *nats.Msg) { handler(&natsMessage{m: m}) },
		nats.Durable(durable),
		nats.BindStream(a.stream),
		nats.DeliverAll(),
		nats.ManualAck(),
		nats.MaxAckPending(natsMaxAckPending),
	)
	if err != nil {
		return nil, fmt.Errorf("queue: NATS subscribe %s (durable %s): %v", subject, durable, err)
	}
	return &natsSubscription{sub: sub}, nil
}

// Close implements JetStream. Draining is skipped deliberately: Close is the
// process-exit path, and the durable consumers keep their server-side state
// for the next process.
func (a *NATSJetStream) Close() error {
	if a == nil || a.conn == nil {
		return nil
	}
	a.conn.Close()
	return nil
}

// Range implements Log. The admin path walks the stream's sequence space via
// js.GetMsg and filters by subject (linear in stream size — acceptable for
// the bounded DLQ/replay admin surface, and documented on the interface).
func (a *NATSJetStream) Range(subject string, fn func(seq uint64, ts time.Time, data []byte) bool) error {
	info, err := a.js.StreamInfo(a.stream)
	if err != nil {
		return err
	}
	if info.State.FirstSeq == 0 && info.State.LastSeq == 0 {
		return ErrNotRetained
	}
	found := false
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		raw, err := a.js.GetMsg(a.stream, seq)
		if err != nil {
			if errors.Is(err, nats.ErrMsgNotFound) {
				continue // purged/deleted window
			}
			return err
		}
		if raw.Subject != subject {
			continue
		}
		found = true
		if !fn(raw.Sequence, raw.Time.UTC(), raw.Data) {
			return nil
		}
	}
	if !found {
		return ErrNotRetained
	}
	return nil
}

// Delete implements Log.
func (a *NATSJetStream) Delete(subject string, seq uint64) error {
	// DeleteMsg is stream-scoped; verifying the subject keeps the operation
	// honest (a foreign seq for the right subject would otherwise delete
	// another subject's retained message).
	raw, err := a.js.GetMsg(a.stream, seq)
	if err != nil {
		return ErrEntryNotFound
	}
	if raw.Subject != subject {
		return ErrEntryNotFound
	}
	if err := a.js.DeleteMsg(a.stream, seq); err != nil {
		return err
	}
	return nil
}

// natsMessage adapts nats.Msg to the Message seam.
type natsMessage struct{ m *nats.Msg }

func (m *natsMessage) Subject() string { return m.m.Subject }
func (m *natsMessage) Data() []byte    { return m.m.Data }

func (m *natsMessage) Sequence() uint64 {
	meta, err := m.m.Metadata()
	if err != nil {
		return 0
	}
	return meta.Sequence.Stream
}

func (m *natsMessage) Deliveries() uint64 {
	meta, err := m.m.Metadata()
	if err != nil {
		return 0
	}
	return meta.NumDelivered
}

func (m *natsMessage) Ack() error                         { return m.m.Ack() }
func (m *natsMessage) Nak() error                         { return m.m.Nak() }
func (m *natsMessage) NakWithDelay(d time.Duration) error { return m.m.NakWithDelay(d) }

// natsSubscription adapts nats.Subscription to the Subscription seam.
type natsSubscription struct{ sub *nats.Subscription }

func (s *natsSubscription) Unsubscribe() error { return s.sub.Unsubscribe() }
func (s *natsSubscription) Valid() bool        { return s.sub.IsValid() }
