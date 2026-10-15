package queue

// jetstream.go defines the narrow broker seam the NATS core integration
// (issue #116) is built on. The messaging layer never touches the nats.go
// types directly: it programs against JetStream/Message/Subscription/Log, and
// two interchangeable implementations exist:
//
//   - SimJetStream (jetstream_sim.go): an embedded in-memory broker with
//     JetStream semantics (per-subject retained logs with sequence numbers,
//     durable consumers, ack/nak redelivery with delay). It backs
//     AGENTOS_MESSAGING=memory and every unit test — deterministic, zero
//     infrastructure, injectable clock.
//   - NewNATSJetStream (jetstream_nats.go): the production adapter over
//     github.com/nats-io/nats.go. The dependency already exists in go.mod
//     (the events publisher, internal/events/nats.go, uses it), so this
//     adapter is fully functional and is verified against an embedded
//     nats-server in tests. Swapping broker products later means adding
//     another file that satisfies this seam — nothing else changes.
//
// Semantics the seam guarantees (both implementations):
//
//   - Retention: every published message is retained per subject with a
//     1-based monotonically increasing sequence and a timestamp, until it is
//     explicitly deleted (the only deletion is the DLQ requeue path).
//   - Delivery: Subscribe with a durable name delivers retained + new
//     messages in publish order; redelivery of unacknowledged messages is
//     bounded by the framework's retry cap (see consumer.go) and lands in
//     the dead-letter subject agentos.dlq when exhausted.
//   - Attempts: Message.Deliveries reports the 1-based delivery attempt
//     count — server-side state for the real broker (nats.MsgMetadata
//     .NumDelivered), tracked per durable consumer for the simulator.

import (
	"errors"
	"time"
)

// Stream and subject constants for the AgentOS task/coordination messaging
// plane. The domain-event plane (agentos.events.>) keeps its own stream in
// internal/events — JetStream forbids overlapping subject sets across
// streams, so the two planes never collide.
const (
	// MessagingStreamName is the JetStream stream retaining the task
	// dispatch + coordination + dead-letter subjects.
	MessagingStreamName = "AGENTOS_TASKS"
)

// MessagingStreamSubjects is the AGENTOS_TASKS stream's subject set (declared
// once so EnsureStream is idempotent across every process). A slice cannot be
// a Go constant, hence the var.
var MessagingStreamSubjects = []string{
	"agentos.runs.*",
	"agentos.workflow.>",
	"agentos.dlq",
}

const (
	// SubjectRunsCreate is the task-dispatch subject: run creation
	// (agent.run / tool.run tasks) is published here in nats|hybrid
	// messaging modes instead of a direct queue put (issue #116 req 2).
	SubjectRunsCreate = "agentos.runs.create"
	// SubjectWorkflowNodeCompleted is the workflow coordination fan-out
	// subject (issue #116 req 3): the worker publishes one event per
	// finished workflow node.
	SubjectWorkflowNodeCompleted = "agentos.workflow.node.completed"
	// SubjectDLQ is the dead-letter subject (issue #116 req 4): messages
	// that exhaust their retry cap land here for inspection and requeue.
	SubjectDLQ = "agentos.dlq"
)

// ReplayingSubjects lists the subjects the admin replay endpoint accepts.
// Replaying the DLQ itself is refused (it would loop dead letters back into
// the DLQ); arbitrary subjects are refused so the admin surface can never be
// used to inject traffic outside the messaging plane.
func ReplayingSubjects() []string {
	return []string{SubjectRunsCreate, SubjectWorkflowNodeCompleted}
}

// IsReplayableSubject reports whether subject is accepted by the replay API.
func IsReplayableSubject(subject string) bool {
	for _, s := range ReplayingSubjects() {
		if s == subject {
			return true
		}
	}
	return false
}

// Message is one delivered broker message. Implementations must be safe to
// call from the handler goroutine.
type Message interface {
	// Subject the message was published to.
	Subject() string
	// Data is the published payload (the raw envelope bytes).
	Data() []byte
	// Sequence is the message's 1-based position in its subject's retained
	// log (stable across redeliveries; the DLQ requeue key).
	Sequence() uint64
	// Deliveries is the 1-based delivery attempt count for this message on
	// its durable consumer.
	Deliveries() uint64
	// Ack acknowledges successful processing: the message never redelivers.
	Ack() error
	// Nak signals a retryable failure: the message redelivers after the
	// subscription's default nak delay.
	Nak() error
	// NakWithDelay signals a retryable failure with an explicit delay.
	NakWithDelay(d time.Duration) error
}

// Subscription is one active broker subscription. Valid() reports false once
// the underlying subscription is gone (connection loss, unsubscribe) — the
// consumer framework's supervision loop uses it to re-establish delivery.
type Subscription interface {
	Unsubscribe() error
	Valid() bool
}

// JetStream is the narrow messaging-broker seam (package comment). All
// methods must be safe for concurrent use except where noted.
type JetStream interface {
	// EnsureStream idempotently creates the retention stream covering the
	// given subjects. Calling it for an existing compatible stream is a
	// no-op; an incompatible existing stream is an error.
	EnsureStream(name string, subjects []string) error
	// Publish appends one message to the subject's retained log and returns
	// its 1-based sequence. Publish blocks until the broker acknowledges
	// durability.
	Publish(subject string, data []byte) (uint64, error)
	// Subscribe starts durable delivery of subject (retained + new, in
	// publish order) to handler. The durable name is the consumer identity:
	// re-subscribing with the same name resumes that consumer's cursor and
	// delivery counts. handler runs on broker-owned goroutines and must not
	// block indefinitely (the consumer framework bounds its work with the
	// in-flight semaphore and returns fast).
	Subscribe(subject, durable string, handler func(Message)) (Subscription, error)
	// Close releases the connection. In-flight subscriptions become invalid.
	Close() error
}

// Log is the admin surface over the retained subjects (replay + DLQ
// operations, issue #116 req 4). It is implemented by both brokers.
type Log interface {
	// Range iterates one subject's retained log oldest-first, calling fn for
	// every entry. fn returns false to stop early (nil error). The log is a
	// point-in-time snapshot; entries appended during iteration may or may
	// not be visited.
	Range(subject string, fn func(seq uint64, ts time.Time, data []byte) bool) error
	// Delete removes one retained message. Used by the DLQ requeue path to
	// consume the dead-letter entry after republishing it (the real broker
	// maps this to JetStream message delete).
	Delete(subject string, seq uint64) error
}

// Messaging errors shared by both implementations.
var (
	// ErrBrokerUnavailable reports that the messaging broker could not be
	// reached (the dispatcher surfaces it to the hybrid fallback / the
	// nats-mode fail-fast path).
	ErrBrokerUnavailable = errors.New("queue: messaging broker unavailable")
	// ErrNotRetained is returned when a Log operation targets a subject with
	// no retained log (e.g. replaying a subject never published to).
	ErrNotRetained = errors.New("queue: subject has no retained log")
	// ErrEntryNotFound is returned by Log.Delete for a sequence that is not
	// retained anymore (e.g. an already-requeued DLQ entry).
	ErrEntryNotFound = errors.New("queue: retained entry not found")
)
