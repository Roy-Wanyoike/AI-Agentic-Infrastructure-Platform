package queue

// jetstream_sim.go implements the in-memory JetStream simulator: an embedded,
// deterministic broker with the JetStream semantics the messaging layer relies
// on (package comment in jetstream.go).
//
// WHY A SIMULATOR: AGENTOS_MESSAGING=memory (the default) must keep working
// with zero infrastructure, and every unit test must be deterministic. The
// simulator models exactly the semantics the framework programs against —
// no more:
//
//   - Retention: one retained log per concrete subject, entries carry a
//     1-based per-subject monotonically increasing sequence and a timestamp
//     (from the injected Clock), until Delete removes them (the DLQ requeue
//     path).
//   - Durable consumers: Subscribe(subject, durable, handler) delivers
//     retained + new messages in publish order; the durable name is the
//     consumer identity — a re-subscribe (reconnect) resumes the cursor and
//     the per-message delivery counts, and redelivers unacknowledged
//     messages first. Wildcard patterns are accepted: delivery state is
//     keyed by (concrete subject, sequence), so one durable may observe
//     several concrete subjects.
//   - Ack/Nak: Ack retires a message; Nak/NakWithDelay schedules a
//     redelivery after the delay on the injected Clock (deterministic under
//     FakeClock.Advance).
//   - Disconnect simulation: DisconnectSubscription marks one subscription
//     invalid (connection loss) so the consumer framework's reconnect
//     supervision can be tested end to end.
//
// Known, documented deviations from a real JetStream server (kept minimal on
// purpose): no persistence across process restarts, no replication, no
// global cross-subject ordering (sequences are per subject; the framework
// subscribes to literal subjects where this is indistinguishable), and
// delivery to a full subscription mailbox reschedules on the clock instead
// of blocking the publisher (mailbox capacity 1024 far exceeds the
// framework's MaxAckPending-style in-flight bounds in tests).

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// simMailBuffer sizes each subscription's delivery mailbox.
const simMailBuffer = 1024

// simNakDefaultDelay is the redelivery delay used by Message.Nak when no
// explicit delay is given (mirrors a JetStream ack-wait fallback).
const simNakDefaultDelay = time.Second

// simDelivery identifies one retained message on its concrete subject (the
// durable's delivery-state key; sequences are per subject).
type simDelivery struct {
	subject string
	seq     uint64
}

// SimJetStream is the embedded in-memory JetStream simulator. Construct with
// NewSimJetStream. Safe for concurrent use.
type SimJetStream struct {
	mu     sync.Mutex
	clock  Clock
	closed bool

	streamName     string
	streamSubjects []string
	logs           map[string][]*simEntry // per-concrete-subject retained log (ascending sequence)
	seqs           map[string]uint64      // last assigned sequence per concrete subject

	durables  map[string]*simDurable
	subs      map[uint64]*simSubscription
	nextSubID uint64
}

type simEntry struct {
	seq  uint64
	ts   time.Time
	data []byte
}

// simDurable is the server-side state of one durable consumer.
type simDurable struct {
	name    string
	subject string            // the subscription pattern the durable is bound to
	cursors map[string]uint64 // highest delivered sequence per concrete subject
	state   map[simDelivery]*simDurableMsg
	sub     *simSubscription // currently bound subscription (nil while disconnected)
}

type simDurableMsg struct {
	deliveries  uint64
	acked       bool
	redeliverAt time.Time // zero when no redelivery is scheduled
}

// simSubscription is one active subscription instance.
type simSubscription struct {
	id        uint64
	subject   string
	durable   *simDurable
	handler   func(Message)
	mail      chan simDelivery // deliveries awaiting handler dispatch (state lives in the durable)
	done      chan struct{}
	closeOnce sync.Once
	js        *SimJetStream

	invalidMu sync.Mutex
	invalid   bool
}

// NewSimJetStream returns a simulator Clock-driven by clock (RealClock when
// nil). Every test constructs it with a FakeClock for determinism.
func NewSimJetStream(clock Clock) *SimJetStream {
	if clock == nil {
		clock = RealClock()
	}
	return &SimJetStream{
		clock:    clock,
		logs:     make(map[string][]*simEntry),
		seqs:     make(map[string]uint64),
		durables: make(map[string]*simDurable),
		subs:     make(map[uint64]*simSubscription),
	}
}

// EnsureStream implements JetStream. The simulator models one retention
// namespace per instance (the AGENTOS_TASKS plane): the first call registers
// the stream, later identical calls are no-ops, a different name is rejected
// exactly like a real broker rejects overlapping/renamed streams.
func (s *SimJetStream) EnsureStream(name string, subjects []string) error {
	if name == "" {
		return fmt.Errorf("queue/sim: stream name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrBrokerUnavailable
	}
	if s.streamName == "" {
		s.streamName = name
		s.streamSubjects = append([]string(nil), subjects...)
		return nil
	}
	if s.streamName != name {
		return fmt.Errorf("queue/sim: stream %q already exists as %q", name, s.streamName)
	}
	return nil
}

// Publish implements JetStream: append to the subject's log, fan out to
// matching subscriptions.
func (s *SimJetStream) Publish(subject string, data []byte) (uint64, error) {
	if subject == "" {
		return 0, fmt.Errorf("queue/sim: subject is required")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrBrokerUnavailable
	}
	s.seqs[subject]++
	seq := s.seqs[subject]
	entry := &simEntry{seq: seq, ts: s.clock.Now(), data: append([]byte(nil), data...)}
	s.logs[subject] = append(s.logs[subject], entry)

	// Snapshot the matching subscriptions and hand out fresh deliveries.
	// Delivery counting happens under the lock; the mailbox enqueue happens
	// after unlocking (never block a publisher on a consumer).
	type simTarget struct {
		sub *simSubscription
		key simDelivery
	}
	targets := make([]simTarget, 0, len(s.subs))
	for _, sub := range s.subs {
		if !subjectMatches(sub.subject, subject) {
			continue
		}
		d := sub.durable
		key := simDelivery{subject: subject, seq: seq}
		if st, ok := d.state[key]; ok {
			// Defensive: this (subject, seq) was delivered before and is still
			// unacked — count the publish as one more delivery attempt.
			if !st.acked {
				st.deliveries++
				targets = append(targets, simTarget{sub: sub, key: key})
			}
		} else {
			d.state[key] = &simDurableMsg{deliveries: 1}
			targets = append(targets, simTarget{sub: sub, key: key})
		}
		if seq > d.cursors[subject] {
			d.cursors[subject] = seq
		}
	}
	s.mu.Unlock()

	for _, t := range targets {
		s.enqueueMail(t.sub, t.key)
	}
	return seq, nil
}

// Subscribe implements JetStream. Re-subscribing an existing durable
// (reconnect) resumes its cursors, redelivers unacknowledged messages first
// and keeps the delivery counts.
func (s *SimJetStream) Subscribe(subject, durable string, handler func(Message)) (Subscription, error) {
	if subject == "" || durable == "" || handler == nil {
		return nil, fmt.Errorf("queue/sim: subject, durable and handler are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrBrokerUnavailable
	}

	d, ok := s.durables[durable]
	if ok && d.subject != subject {
		return nil, fmt.Errorf("queue/sim: durable consumer %q is bound to %q, not %q", durable, d.subject, subject)
	}
	s.nextSubID++
	sub := &simSubscription{
		id:      s.nextSubID,
		subject: subject,
		handler: handler,
		mail:    make(chan simDelivery, simMailBuffer),
		done:    make(chan struct{}),
		js:      s,
	}
	if !ok {
		d = &simDurable{name: durable, subject: subject, cursors: make(map[string]uint64), state: make(map[simDelivery]*simDurableMsg)}
		s.durables[durable] = d
	}
	sub.durable = d
	d.sub = sub
	s.subs[sub.id] = sub

	// Replay: unacknowledged messages first (redelivery, attempts bump),
	// then everything past the durable's per-subject cursors (fresh
	// delivery). Both walks are in (subject, sequence) order for
	// determinism.
	pending := make([]simDelivery, 0, len(d.state))
	for key, st := range d.state {
		if !st.acked {
			st.deliveries++
			pending = append(pending, key)
		}
	}
	sortSimDeliveries(pending)
	replay := make([]simDelivery, 0)
	for cs, last := range s.seqs {
		if !subjectMatches(subject, cs) {
			continue
		}
		for seq := d.cursors[cs] + 1; seq <= last; seq++ {
			key := simDelivery{subject: cs, seq: seq}
			if _, seen := d.state[key]; !seen {
				d.state[key] = &simDurableMsg{deliveries: 1}
				replay = append(replay, key)
			}
		}
		d.cursors[cs] = last
	}
	sortSimDeliveries(replay)

	go sub.run()
	// Enqueue after run() started (mailbox drained by its own goroutine).
	for _, key := range pending {
		s.enqueueMailLocked(sub, key)
	}
	for _, key := range replay {
		s.enqueueMailLocked(sub, key)
	}
	return sub, nil
}

// sortSimDeliveries orders deliveries by (subject, sequence) deterministically.
func sortSimDeliveries(keys []simDelivery) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0; j-- {
			a, b := keys[j-1], keys[j]
			if a.subject < b.subject || (a.subject == b.subject && a.seq < b.seq) {
				break
			}
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
}

// Close implements JetStream: every subscription becomes invalid; retained
// logs are discarded (an in-memory broker loses its state when it stops).
func (s *SimJetStream) Close() error {
	s.mu.Lock()
	s.closed = true
	subs := make([]*simSubscription, 0, len(s.subs))
	for _, sub := range s.subs {
		subs = append(subs, sub)
	}
	s.subs = make(map[uint64]*simSubscription)
	s.mu.Unlock()
	for _, sub := range subs {
		sub.invalidate()
	}
	return nil
}

// Range implements Log.
func (s *SimJetStream) Range(subject string, fn func(seq uint64, ts time.Time, data []byte) bool) error {
	s.mu.Lock()
	log := s.logs[subject]
	snapshot := make([]*simEntry, len(log))
	copy(snapshot, log)
	s.mu.Unlock()
	if len(snapshot) == 0 {
		return ErrNotRetained
	}
	for _, e := range snapshot {
		if !fn(e.seq, e.ts, e.data) {
			return nil
		}
	}
	return nil
}

// Delete implements Log (DLQ requeue consumes the entry).
func (s *SimJetStream) Delete(subject string, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.logs[subject]
	for i, e := range log {
		if e.seq == seq {
			s.logs[subject] = append(log[:i:i], log[i+1:]...)
			return nil
		}
	}
	return ErrEntryNotFound
}

// ---------------------------------------------------------------------------
// Test/supervision hooks (simulator-only; the real adapter has no equivalent —
// connection loss there is observed through Subscription.Valid()).
// ---------------------------------------------------------------------------

// DisconnectSubscription simulates a connection drop for one subscription: it
// becomes invalid (supervision loop reconnects) and unacknowledged messages
// redeliver on the durable's next Subscribe.
func (s *SimJetStream) DisconnectSubscription(sub Subscription) {
	sim, ok := sub.(*simSubscription)
	if !ok {
		return
	}
	s.mu.Lock()
	if cur, ok := s.subs[sim.id]; ok && cur == sim {
		delete(s.subs, sim.id)
	}
	if sim.durable != nil && sim.durable.sub == sim {
		sim.durable.sub = nil
	}
	s.mu.Unlock()
	sim.invalidate()
}

// RetainedCount reports how many messages are retained for one subject
// (test helper).
func (s *SimJetStream) RetainedCount(subject string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.logs[subject])
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// ack retires one message on its durable consumer.
func (s *SimJetStream) ack(d *simDurable, key simDelivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := d.state[key]; ok {
		st.acked = true
	}
}

// nak schedules a redelivery of key after delay on the injected Clock. The
// timer targets the durable (not the subscription instance): a reconnect
// between nak and firing still redelivers, on whichever subscription is
// bound when the timer fires.
func (s *SimJetStream) nak(d *simDurable, key simDelivery, delay time.Duration) {
	s.mu.Lock()
	st, ok := d.state[key]
	if !ok || st.acked {
		s.mu.Unlock()
		return
	}
	st.redeliverAt = s.clock.Now().Add(delay)
	s.mu.Unlock()

	timer := s.clock.NewTimer(delay)
	go func() {
		select {
		case <-timer.C():
		case <-d.subDone(s):
			timer.Stop()
			return
		}
		s.redeliver(d, key)
	}()
}

// redeliver fires a scheduled redelivery: skip when acked/disconnected.
func (s *SimJetStream) redeliver(d *simDurable, key simDelivery) {
	s.mu.Lock()
	st, ok := d.state[key]
	if !ok || st.acked || d.sub == nil {
		s.mu.Unlock()
		return
	}
	st.deliveries++
	sub := d.sub
	s.mu.Unlock()
	s.enqueueMail(sub, key)
}

// enqueueMail hands one delivery to the subscription's mailbox without
// blocking. A full mailbox reschedules via the clock (never blocks a
// publisher or the nak timer goroutine).
func (s *SimJetStream) enqueueMail(sub *simSubscription, key simDelivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueueMailLocked(sub, key)
}

func (s *SimJetStream) enqueueMailLocked(sub *simSubscription, key simDelivery) {
	select {
	case sub.mail <- key:
	default:
		// Mailbox full: retry after a tick on the injected clock. In tests
		// (FakeClock) the retry only fires on Advance, which keeps the
		// simulator deterministic.
		timer := s.clock.NewTimer(simNakDefaultDelay)
		go func() {
			select {
			case <-timer.C():
			case <-sub.done:
				timer.Stop()
				return
			}
			s.enqueueMail(sub, key)
		}()
	}
}

// run is the subscription's dispatch loop: broker-side goroutine invoking the
// framework handler in delivery order.
func (sub *simSubscription) run() {
	for {
		select {
		case key := <-sub.mail:
			sub.js.mu.Lock()
			st := sub.durable.state[key]
			var entry *simEntry
			for _, e := range sub.js.logs[key.subject] {
				if e.seq == key.seq {
					entry = e
					break
				}
			}
			deliveries := uint64(0)
			if st != nil {
				deliveries = st.deliveries
			}
			closed := sub.js.closed
			sub.js.mu.Unlock()
			if entry == nil || closed {
				continue // deleted (e.g. DLQ requeue) or broker closed
			}
			sub.handler(&simMessage{sub: sub, key: key, entry: entry, deliveries: deliveries})
		case <-sub.done:
			return
		}
	}
}

// invalidate marks the subscription invalid and stops its dispatch loop.
func (sub *simSubscription) invalidate() {
	sub.invalidMu.Lock()
	sub.invalid = true
	sub.invalidMu.Unlock()
	sub.closeOnce.Do(func() { close(sub.done) })
}

// Unsubscribe implements Subscription.
func (sub *simSubscription) Unsubscribe() error {
	js := sub.js
	js.mu.Lock()
	if cur, ok := js.subs[sub.id]; ok && cur == sub {
		delete(js.subs, sub.id)
	}
	if sub.durable != nil && sub.durable.sub == sub {
		sub.durable.sub = nil
	}
	js.mu.Unlock()
	sub.invalidate()
	return nil
}

// Valid implements Subscription: false once unsubscribed, disconnected or the
// broker closed.
func (sub *simSubscription) Valid() bool {
	sub.js.mu.Lock()
	closed := sub.js.closed
	sub.js.mu.Unlock()
	sub.invalidMu.Lock()
	defer sub.invalidMu.Unlock()
	return !sub.invalid && !closed
}

// subDone resolves the shutdown channel for nak timers: the durable's bound
// subscription's done channel, or a never-firing channel while disconnected
// (the redelivery then fires on the next bound subscription or is skipped).
func (d *simDurable) subDone(s *SimJetStream) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.sub != nil {
		return d.sub.done
	}
	return neverChan
}

var neverChan = make(chan struct{})

// simMessage implements Message for the simulator.
type simMessage struct {
	sub        *simSubscription
	key        simDelivery
	entry      *simEntry
	deliveries uint64
}

func (m *simMessage) Subject() string {
	if m == nil {
		return ""
	}
	return m.key.subject
}

func (m *simMessage) Data() []byte {
	if m == nil || m.entry == nil {
		return nil
	}
	return m.entry.data
}

func (m *simMessage) Sequence() uint64 {
	if m == nil || m.entry == nil {
		return 0
	}
	return m.entry.seq
}

func (m *simMessage) Deliveries() uint64 {
	if m == nil {
		return 0
	}
	return m.deliveries
}

func (m *simMessage) Ack() error {
	m.sub.js.ack(m.sub.durable, m.key)
	return nil
}

func (m *simMessage) Nak() error {
	return m.NakWithDelay(simNakDefaultDelay)
}

func (m *simMessage) NakWithDelay(d time.Duration) error {
	if d <= 0 {
		d = simNakDefaultDelay
	}
	m.sub.js.nak(m.sub.durable, m.key, d)
	return nil
}

// subjectMatches reports whether subject matches the subscription pattern.
// Supports the NATS wildcards "*" (exactly one token) and ">" (the remaining
// tokens).
func subjectMatches(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	pt := strings.Split(pattern, ".")
	st := strings.Split(subject, ".")
	for i, p := range pt {
		if p == ">" {
			return i < len(st)
		}
		if i >= len(st) {
			return false
		}
		if p != "*" && p != st[i] {
			return false
		}
	}
	return len(pt) == len(st)
}
