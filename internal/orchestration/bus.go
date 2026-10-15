package orchestration

// bus.go — issue #108: the agent communication bus.
//
// AgentCommBus is the transport seam for agent-to-agent messaging. This
// round ships an IN-PROCESS implementation (buffered channels per agent, no
// external broker); a NATS-backed implementation can slot in later behind
// the same interface (the interface deliberately avoids channel-typed
// extras so a distributed implementation only has to honor Send / Receive /
// Broadcast semantics).
//
// Delivery semantics (all deterministic and race-free):
//   - Send is NON-BLOCKING: delivery is atomic per message — the message is
//     appended to the subscriber's retained inbox ring AND pushed into the
//     live channel, or neither happens and ErrBusFull is returned.
//   - The per-subscriber ring is the INBOX OF RECORD (readable via
//     RecentMessages even with no active channel consumer); the channel is
//     the live-notification handle handed out by Receive.
//   - Broadcast fans out to every OTHER subscriber of the same organization
//     (sender excluded — no self-echo; org scoping is the topic isolation).
//     Fan-out is best-effort per target: a target whose channel is full is
//     skipped (never partially delivered), and Broadcast itself never fails
//     because one subscriber is saturated.
//   - Subscriber lifecycle is explicit: Receive registers (idempotently),
//     Unregister closes the channel and drops the subscriber, Close tears
//     down the whole bus. The bus spawns NO goroutines of its own.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Message types carried in AgentMessage.MessageType.
const (
	MessageTypeRequest   = "request"   // A asks B to do something
	MessageTypeResponse  = "response"  // B answers A (delegation results ride this type)
	MessageTypeDelegate  = "delegate"  // notification that a sub-run was delegated
	MessageTypeBroadcast = "broadcast" // fan-out to every other agent of the org
)

// Typed bus errors. Use errors.Is to classify.
var (
	// ErrInvalidMessage is returned for messages missing required fields or
	// carrying an unknown MessageType.
	ErrInvalidMessage = errors.New("orchestration: invalid agent message")
	// ErrNoSubscriber is returned when no inbox is registered for the
	// target (org, agent) pair.
	ErrNoSubscriber = errors.New("orchestration: no subscriber for agent")
	// ErrBusFull is returned by Send when the target's buffered channel is
	// full; the message is NOT delivered (atomic semantics).
	ErrBusFull = errors.New("orchestration: agent inbox is full")
	// ErrBusClosed is returned once the bus (or the whole process scope) has
	// been closed.
	ErrBusClosed = errors.New("orchestration: bus is closed")
)

// AgentMessage is one point-to-point (or broadcast) agent communication.
// OrgID scopes the topic: agents of different organizations never see each
// other's traffic (per-org topic isolation).
type AgentMessage struct {
	OrgID       string          `json:"org_id"`
	FromAgentID string          `json:"from_agent_id"`
	ToAgentID   string          `json:"to_agent_id"` // "*" for broadcast deliveries
	RunID       string          `json:"run_id,omitempty"`
	MessageType string          `json:"message_type"` // request | response | delegate | broadcast
	Payload     json.RawMessage `json:"payload,omitempty"`
	Timestamp   time.Time       `json:"timestamp"`
	MessageID   string          `json:"message_id"`
}

// AgentCommBus is the agent-to-agent transport seam. Implementations must be
// safe for concurrent use.
type AgentCommBus interface {
	// Send delivers one message to the subscriber for (msg.OrgID,
	// msg.ToAgentID). Non-blocking: a saturated inbox returns ErrBusFull and
	// delivers nothing.
	Send(ctx context.Context, msg AgentMessage) error
	// Receive registers (idempotently) the inbox for (orgID, agentID) and
	// returns its live channel. The channel is closed by Unregister/Close.
	Receive(ctx context.Context, orgID, agentID string) (<-chan AgentMessage, error)
	// Broadcast fans payload out to every OTHER registered agent of orgID.
	// Per-target delivery is best-effort; the call never fails on a
	// saturated peer.
	Broadcast(ctx context.Context, orgID, fromAgentID string, payload json.RawMessage) error
}

// Default bus capacities (configurable via BusOptions).
const (
	// DefaultChannelCapacity is the buffered-channel size per subscriber
	// (live notifications before a consumer drains them).
	DefaultChannelCapacity = 64
	// DefaultInboxRing is the number of messages retained per subscriber for
	// the recent-inbox read API (the inbox of record).
	DefaultInboxRing = 128
)

// BusOptions tunes an InProcessCommBus. Zero values select the defaults;
// Now/NewID are overridable clocks/id generators for deterministic tests.
type BusOptions struct {
	ChannelCapacity int
	InboxRing       int
	Now             func() time.Time
	NewID           func() string
}

// subscriber is one agent's inbox: the live channel plus the retained ring.
type subscriber struct {
	orgID   string
	agentID string
	ch      chan AgentMessage
	ring    []AgentMessage // bounded, oldest evicted first, newest last
}

// append adds msg to the ring, evicting the oldest entry once full.
func (s *subscriber) append(msg AgentMessage, ringSize int) {
	if ringSize <= 0 {
		return
	}
	if len(s.ring) >= ringSize {
		// Drop the oldest entry (index 0) and keep order.
		copy(s.ring, s.ring[1:])
		s.ring[len(s.ring)-1] = msg
		return
	}
	s.ring = append(s.ring, msg)
}

// popLast rolls back the most recent append (used to keep delivery atomic
// when the live channel push fails).
func (s *subscriber) popLast() {
	if len(s.ring) == 0 {
		return
	}
	s.ring = s.ring[:len(s.ring)-1]
}

// recent returns up to limit retained messages, newest first (limit <= 0
// returns the whole ring).
func (s *subscriber) recent(limit int) []AgentMessage {
	out := make([]AgentMessage, 0, len(s.ring))
	for i := len(s.ring) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		out = append(out, s.ring[i])
	}
	return out
}

// InProcessCommBus is the in-memory AgentCommBus: buffered channels per
// (org, agent) subscriber with a bounded retained ring per subscriber. Safe
// for concurrent use; spawns no goroutines.
type InProcessCommBus struct {
	mu       sync.Mutex
	subs     map[string]*subscriber // key: subKey(orgID, agentID)
	capacity int
	ringSize int
	closed   bool
	now      func() time.Time
	newID    func() string
}

func subKey(orgID, agentID string) string { return orgID + "\x00" + agentID }

// NewInProcessCommBus builds the bus with the given options (zero values ->
// defaults).
func NewInProcessCommBus(opts BusOptions) *InProcessCommBus {
	capacity := opts.ChannelCapacity
	if capacity <= 0 {
		capacity = DefaultChannelCapacity
	}
	ring := opts.InboxRing
	if ring <= 0 {
		ring = DefaultInboxRing
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newID := opts.NewID
	if newID == nil {
		newID = func() string { return uuid.NewString() }
	}
	return &InProcessCommBus{
		subs:     make(map[string]*subscriber),
		capacity: capacity,
		ringSize: ring,
		now:      now,
		newID:    newID,
	}
}

// validMessageType reports whether t is one of the four protocol types.
func validMessageType(t string) bool {
	switch t {
	case MessageTypeRequest, MessageTypeResponse, MessageTypeDelegate, MessageTypeBroadcast:
		return true
	}
	return false
}

// normalize fills in defaults (timestamp, message id) and validates the
// required fields of an outgoing message.
func (b *InProcessCommBus) normalize(msg *AgentMessage) error {
	if msg == nil {
		return fmt.Errorf("%w: message is required", ErrInvalidMessage)
	}
	if msg.OrgID == "" || msg.FromAgentID == "" || msg.ToAgentID == "" {
		return fmt.Errorf("%w: org_id, from_agent_id and to_agent_id are required", ErrInvalidMessage)
	}
	if !validMessageType(msg.MessageType) {
		return fmt.Errorf("%w: unknown message_type %q", ErrInvalidMessage, msg.MessageType)
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = b.now()
	}
	if msg.MessageID == "" {
		msg.MessageID = b.newID()
	}
	return nil
}

// Send implements AgentCommBus. Delivery is atomic: either the message lands
// in the target's ring AND channel, or nothing is delivered and ErrBusFull /
// ErrNoSubscriber / ErrBusClosed is returned.
func (b *InProcessCommBus) Send(ctx context.Context, msg AgentMessage) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBusClosed
	}
	if err := b.normalize(&msg); err != nil {
		return err
	}
	sub, ok := b.subs[subKey(msg.OrgID, msg.ToAgentID)]
	if !ok {
		return fmt.Errorf("%w: org=%s agent=%s", ErrNoSubscriber, msg.OrgID, msg.ToAgentID)
	}
	// Ring append first, then the non-blocking live push; roll back the ring
	// on a full channel so a failed send never leaves a trace (the caller
	// sees ErrBusFull and can retry).
	sub.append(msg, b.ringSize)
	select {
	case sub.ch <- msg:
		return nil
	default:
		sub.popLast()
		return ErrBusFull
	}
}

// Receive implements AgentCommBus. It registers the subscriber when absent
// (idempotent: the same channel is returned for repeat calls) and returns
// its live channel. Close/Unregister close the channel.
func (b *InProcessCommBus) Receive(ctx context.Context, orgID, agentID string) (<-chan AgentMessage, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if orgID == "" || agentID == "" {
		return nil, fmt.Errorf("%w: org_id and agent_id are required to receive", ErrInvalidMessage)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrBusClosed
	}
	key := subKey(orgID, agentID)
	if sub, ok := b.subs[key]; ok {
		return sub.ch, nil
	}
	sub := &subscriber{
		orgID:   orgID,
		agentID: agentID,
		ch:      make(chan AgentMessage, b.capacity),
	}
	b.subs[key] = sub
	return sub.ch, nil
}

// Broadcast implements AgentCommBus: fan-out to every OTHER subscriber of
// orgID (the sender is excluded to avoid self-echo). Per-target delivery is
// best-effort — a saturated peer is skipped; the call never fails because of
// one subscriber. Broadcasting into an organization with no other
// subscribers is a no-op success.
func (b *InProcessCommBus) Broadcast(ctx context.Context, orgID, fromAgentID string, payload json.RawMessage) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if orgID == "" || fromAgentID == "" {
		return fmt.Errorf("%w: org_id and from_agent_id are required to broadcast", ErrInvalidMessage)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBusClosed
	}
	base := AgentMessage{
		OrgID:       orgID,
		FromAgentID: fromAgentID,
		ToAgentID:   "*",
		MessageType: MessageTypeBroadcast,
		Payload:     payload,
	}
	for key, sub := range b.subs {
		if sub.orgID != orgID || sub.agentID == fromAgentID {
			continue
		}
		msg := base
		msg.ToAgentID = sub.agentID
		if msg.Timestamp.IsZero() {
			msg.Timestamp = b.now()
		}
		if msg.MessageID == "" {
			msg.MessageID = b.newID()
		}
		_ = key // iteration key unused; delivery is via the subscriber
		sub.append(msg, b.ringSize)
		select {
		case sub.ch <- msg:
		default:
			// Saturated peer: skip (best-effort fan-out); roll back the ring
			// so undelivered messages never appear in the inbox of record.
			sub.popLast()
		}
	}
	return nil
}

// Unregister closes the subscriber's channel and drops it from the bus.
// Returns ErrNoSubscriber when the pair is not registered (not idempotent —
// double-unregister is a lifecycle bug the caller should see).
func (b *InProcessCommBus) Unregister(orgID, agentID string) error {
	if orgID == "" || agentID == "" {
		return fmt.Errorf("%w: org_id and agent_id are required to unregister", ErrInvalidMessage)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBusClosed
	}
	key := subKey(orgID, agentID)
	sub, ok := b.subs[key]
	if !ok {
		return fmt.Errorf("%w: org=%s agent=%s", ErrNoSubscriber, orgID, agentID)
	}
	delete(b.subs, key)
	close(sub.ch)
	return nil
}

// Close tears down the whole bus: every subscriber channel is closed and the
// bus rejects further traffic with ErrBusClosed.
func (b *InProcessCommBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for _, sub := range b.subs {
		close(sub.ch)
	}
	b.subs = make(map[string]*subscriber)
	return nil
}

// RecentMessages returns up to limit retained messages for (orgID, agentID),
// newest first. limit <= 0 returns everything currently retained.
func (b *InProcessCommBus) RecentMessages(orgID, agentID string, limit int) ([]AgentMessage, error) {
	if orgID == "" || agentID == "" {
		return nil, fmt.Errorf("%w: org_id and agent_id are required to read the inbox", ErrInvalidMessage)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sub, ok := b.subs[subKey(orgID, agentID)]
	if !ok {
		return nil, fmt.Errorf("%w: org=%s agent=%s", ErrNoSubscriber, orgID, agentID)
	}
	return sub.recent(limit), nil
}

// SubscriberCount reports how many inboxes are currently registered. Test
// and observability helper.
func (b *InProcessCommBus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
