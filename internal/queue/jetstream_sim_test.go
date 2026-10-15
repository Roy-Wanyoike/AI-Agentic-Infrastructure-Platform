package queue

import (
	"errors"
	"testing"
	"time"
)

// jetstream_sim_test.go verifies the embedded in-memory broker's JetStream
// semantics (issue #116): retained logs with stable sequences, durable
// consumers that resume their cursor and delivery counts, ack/nak redelivery
// driven by the injected clock, wildcard subjects, and the admin Log surface.
// Everything is deterministic via FakeClock — no real-time sleeps drive
// correctness.

func newTestSim(t *testing.T) (*SimJetStream, *FakeClock) {
	t.Helper()
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sim := NewSimJetStream(clock)
	t.Cleanup(func() { _ = sim.Close() })
	return sim, clock
}

// waitFor polls cond until it holds or the real-time deadline passes. It
// bounds failure only — correctness never depends on timing (deliveries are
// made deterministic by explicit FakeClock advances before every wait).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSimEnsureStreamIdempotentAndConflicting(t *testing.T) {
	sim, _ := newTestSim(t)
	if err := sim.EnsureStream(MessagingStreamName, MessagingStreamSubjects); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	if err := sim.EnsureStream(MessagingStreamName, MessagingStreamSubjects); err != nil {
		t.Fatalf("idempotent EnsureStream: %v", err)
	}
	if err := sim.EnsureStream("OTHER", []string{"x.>"}); err == nil {
		t.Fatal("conflicting stream name accepted")
	}
	if err := sim.EnsureStream("", nil); err == nil {
		t.Fatal("empty stream name accepted")
	}
}

func TestSimPublishRetainsWithStableSequences(t *testing.T) {
	sim, clock := newTestSim(t)
	s1, err := sim.Publish(SubjectRunsCreate, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	clock.Advance(time.Second)
	s2, err := sim.Publish(SubjectRunsCreate, []byte(`{"a":2}`))
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if s1 != 1 || s2 != 2 {
		t.Fatalf("sequences = %d, %d; want 1, 2", s1, s2)
	}
	if got := sim.RetainedCount(SubjectRunsCreate); got != 2 {
		t.Fatalf("RetainedCount = %d, want 2", got)
	}
	// Sequence + timestamp are per-subject; the timestamps come from the
	// injected clock.
	var seen []time.Time
	err = sim.Range(SubjectRunsCreate, func(seq uint64, ts time.Time, data []byte) bool {
		seen = append(seen, ts)
		return true
	})
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(seen) != 2 || seen[0].Equal(seen[1]) {
		t.Fatalf("Range timestamps = %v; want the two distinct injected instants", seen)
	}
	if err := sim.Range("agentos.nope", func(uint64, time.Time, []byte) bool { return true }); !errors.Is(err, ErrNotRetained) {
		t.Fatalf("Range on unknown subject = %v, want ErrNotRetained", err)
	}
}

func TestSimSubscribeDeliversRetainedAndNew(t *testing.T) {
	sim, _ := newTestSim(t)
	for i := 0; i < 3; i++ {
		if _, err := sim.Publish(SubjectRunsCreate, []byte{byte(i)}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	received := make(chan Message, 8)
	sub, err := sim.Subscribe(SubjectRunsCreate, "durable-rt", func(m Message) { received <- m })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	for want := 0; want < 3; want++ {
		select {
		case m := <-received:
			if m.Sequence() != uint64(want+1) {
				t.Fatalf("delivery sequence = %d, want %d", m.Sequence(), want+1)
			}
			if m.Deliveries() != 1 {
				t.Fatalf("fresh delivery count = %d, want 1", m.Deliveries())
			}
			if m.Subject() != SubjectRunsCreate {
				t.Fatalf("subject = %q", m.Subject())
			}
			_ = m.Ack()
		case <-time.After(2 * time.Second):
			t.Fatalf("retained delivery %d missing", want)
		}
	}
	// New publishes fan out to the live subscription too.
	if _, err := sim.Publish(SubjectRunsCreate, []byte{3}); err != nil {
		t.Fatalf("publish after subscribe: %v", err)
	}
	select {
	case m := <-received:
		if m.Sequence() != 4 {
			t.Fatalf("live delivery sequence = %d, want 4", m.Sequence())
		}
		_ = m.Ack()
	case <-time.After(2 * time.Second):
		t.Fatal("live delivery missing")
	}
	_ = sub.Unsubscribe()
	if sub.Valid() {
		t.Fatal("subscription still valid after Unsubscribe")
	}
}

func TestSimDurableResumeRedeliversUnackedAndKeepsCounts(t *testing.T) {
	sim, _ := newTestSim(t)
	for i := 0; i < 2; i++ {
		if _, err := sim.Publish(SubjectRunsCreate, []byte{byte(i)}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	first := make(chan Message, 8)
	sub, err := sim.Subscribe(SubjectRunsCreate, "durable-resume", func(m Message) { first <- m })
	if err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	var m1, m2 Message
	select {
	case m1 = <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery 1 missing")
	}
	select {
	case m2 = <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery 2 missing")
	}
	// Ack the first, leave the second unacknowledged, then drop the
	// connection: exactly the second message must redeliver, with its
	// delivery count incremented (server-side durable state).
	if err := m1.Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	sim.DisconnectSubscription(sub)
	if sub.Valid() {
		t.Fatal("subscription still valid after disconnect")
	}

	second := make(chan Message, 8)
	if _, err := sim.Subscribe(SubjectRunsCreate, "durable-resume", func(m Message) { second <- m }); err != nil {
		t.Fatalf("re-Subscribe: %v", err)
	}
	select {
	case m := <-second:
		if m.Sequence() != m2.Sequence() {
			t.Fatalf("redelivered sequence = %d, want %d", m.Sequence(), m2.Sequence())
		}
		if m.Deliveries() != 2 {
			t.Fatalf("redelivery count = %d, want 2", m.Deliveries())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unacked message was not redelivered on re-subscribe")
	}
	// The acked message must NOT redeliver — drain briefly via RetainedCount
	// stability (no second message arrives; the channel stays empty).
	select {
	case extra := <-second:
		t.Fatalf("acked message redelivered: seq %d", extra.Sequence())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSimNakRedeliversOnlyWhenClockAdvances(t *testing.T) {
	sim, clock := newTestSim(t)
	if _, err := sim.Publish(SubjectRunsCreate, []byte("x")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got := make(chan Message, 8)
	if _, err := sim.Subscribe(SubjectRunsCreate, "durable-nak", func(m Message) { got <- m }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	var msg Message
	select {
	case msg = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("initial delivery missing")
	}
	if err := msg.NakWithDelay(5 * time.Second); err != nil {
		t.Fatalf("NakWithDelay: %v", err)
	}
	if got := clock.PendingTimers(); got != 1 {
		t.Fatalf("PendingTimers after nak = %d, want 1 (redelivery armed on the injected clock)", got)
	}
	// No wall-clock waiting: before the advance nothing may redeliver.
	select {
	case m := <-got:
		t.Fatalf("redelivery happened before the clock advanced: seq %d", m.Sequence())
	case <-time.After(100 * time.Millisecond):
	}

	clock.Advance(5 * time.Second)
	var redelivered Message
	select {
	case redelivered = <-got:
		if redelivered.Deliveries() != 2 {
			t.Fatalf("redelivery count = %d, want 2", redelivered.Deliveries())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("redelivery missing after Advance")
	}

	// Ack retires the message: a nak afterwards must not resurrect it.
	if err := redelivered.Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := redelivered.Nak(); err != nil {
		t.Fatalf("nak after ack: %v", err)
	}
	if got := clock.PendingTimers(); got != 0 {
		t.Fatalf("nak on acked message armed %d timers, want 0", got)
	}
	clock.Advance(time.Hour) // a resurrecting nak would fire here
	select {
	case m := <-got:
		t.Fatalf("acked message redelivered after nak: seq %d", m.Sequence())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSimWildcardSubjects(t *testing.T) {
	sim, _ := newTestSim(t)
	if err := sim.EnsureStream(MessagingStreamName, MessagingStreamSubjects); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	got := make(chan Message, 8)
	if _, err := sim.Subscribe("agentos.runs.*", "durable-star", func(m Message) { got <- m }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	publish := func(subject, body string) {
		t.Helper()
		if _, err := sim.Publish(subject, []byte(body)); err != nil {
			t.Fatalf("publish %s: %v", subject, err)
		}
	}
	publish(SubjectRunsCreate, "runs-create")               // matches agentos.runs.*
	publish(SubjectWorkflowNodeCompleted, "node-completed") // no match (extra token)
	publish("agentos.runs", "bare")                         // no match (* needs one token)
	select {
	case m := <-got:
		if m.Subject() != SubjectRunsCreate || string(m.Data()) != "runs-create" {
			t.Fatalf("wildcard delivery = %s %q", m.Subject(), m.Data())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wildcard delivery missing")
	}
	select {
	case m := <-got:
		t.Fatalf("unexpected wildcard delivery: %s %q", m.Subject(), m.Data())
	case <-time.After(100 * time.Millisecond):
	}
	// ">" matches the remaining tokens at any depth. The new durable also
	// replays the earlier retained node-completed publish (DeliverAll
	// semantics) before the fresh "deep" delivery.
	depth := make(chan Message, 8)
	if _, err := sim.Subscribe("agentos.workflow.>", "durable-gt", func(m Message) { depth <- m }); err != nil {
		t.Fatalf("Subscribe >: %v", err)
	}
	publish("agentos.workflow.node.completed", "deep")
	publish("agentos.workflow", "top") // ">" needs at least one remaining token
	for want := 0; want < 2; want++ {
		select {
		case m := <-depth:
			if m.Subject() != SubjectWorkflowNodeCompleted {
				t.Fatalf("> delivery = %s, want %s", m.Subject(), SubjectWorkflowNodeCompleted)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("> delivery %d missing", want)
		}
	}
	select {
	case m := <-depth:
		t.Fatalf("unexpected > delivery: %s %q", m.Subject(), m.Data())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSimLogDeleteAndErrors(t *testing.T) {
	sim, _ := newTestSim(t)
	seq, err := sim.Publish(SubjectDLQ, []byte(`{"subject":"x"}`))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := sim.Delete(SubjectDLQ, seq+1); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("Delete unknown seq = %v, want ErrEntryNotFound", err)
	}
	if err := sim.Delete(SubjectDLQ, seq); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := sim.RetainedCount(SubjectDLQ); got != 0 {
		t.Fatalf("RetainedCount after delete = %d, want 0", got)
	}
	if err := sim.Range(SubjectDLQ, func(uint64, time.Time, []byte) bool { return true }); !errors.Is(err, ErrNotRetained) {
		t.Fatalf("Range after delete = %v, want ErrNotRetained", err)
	}
}

func TestSimCloseInvalidatesAndRejects(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sim := NewSimJetStream(clock)
	if _, err := sim.Publish(SubjectRunsCreate, []byte("x")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	sub, err := sim.Subscribe(SubjectRunsCreate, "durable-close", func(Message) {})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := sim.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if sub.Valid() {
		t.Fatal("subscription valid after broker close")
	}
	if _, err := sim.Publish(SubjectRunsCreate, []byte("y")); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("publish after close = %v, want ErrBrokerUnavailable", err)
	}
	if _, err := sim.Subscribe(SubjectRunsCreate, "durable-close", func(Message) {}); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("subscribe after close = %v, want ErrBrokerUnavailable", err)
	}
}

func TestSimSubscribeValidation(t *testing.T) {
	sim, _ := newTestSim(t)
	if _, err := sim.Subscribe("", "d", func(Message) {}); err == nil {
		t.Fatal("empty subject accepted")
	}
	if _, err := sim.Subscribe(SubjectRunsCreate, "", func(Message) {}); err == nil {
		t.Fatal("empty durable accepted")
	}
	if _, err := sim.Subscribe(SubjectRunsCreate, "d", nil); err == nil {
		t.Fatal("nil handler accepted")
	}
	// The same durable may not switch subjects (durable identity is bound).
	if _, err := sim.Subscribe(SubjectRunsCreate, "d-bound", func(Message) {}); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if _, err := sim.Subscribe(SubjectWorkflowNodeCompleted, "d-bound", func(Message) {}); err == nil {
		t.Fatal("durable rebind to another subject accepted")
	}
}

func TestSubjectMatches(t *testing.T) {
	cases := []struct {
		pattern, subject string
		want             bool
	}{
		{"agentos.runs.create", "agentos.runs.create", true},
		{"agentos.runs.create", "agentos.runs.other", false},
		{"agentos.runs.*", "agentos.runs.create", true},
		{"agentos.runs.*", "agentos.runs", false},
		{"agentos.runs.*", "agentos.runs.create.more", false},
		{"agentos.workflow.>", "agentos.workflow.node.completed", true},
		{"agentos.workflow.>", "agentos.workflow", false},
		{"agentos.workflow.>", "agentos.runs.create", false},
		{">", "agentos.runs.create", true},
		{"*.runs.create", "agentos.runs.create", true},
		{"*.runs.create", "agentos.workflow.create", false},
	}
	for _, tc := range cases {
		if got := subjectMatches(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("subjectMatches(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
}
