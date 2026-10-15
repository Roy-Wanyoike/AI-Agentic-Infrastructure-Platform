package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// jetstream_nats_test.go verifies the REAL-broker adapter (jetstream_nats.go)
// against an embedded nats-server with JetStream enabled — the same
// test-only dependency pattern internal/events uses (nothing in the shipped
// binaries imports nats-server; it is a go.mod test dependency). This is the
// end-to-end proof behind the adapter's doc comments: connect + stream setup,
// publish/ack, durable subscription, server-side delivery counts, nak
// redelivery, dead-lettering, and the admin Log surface (Range/Delete).

func startEmbeddedNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Port:      -1, // random free port, so parallel tests never collide
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("embedded NATS server: %v", err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("embedded NATS server did not become ready")
	}
	return srv
}

// waitForDeadline is the real-broker polling helper: correctness is
// guaranteed by the broker (ack nak redelivery, publish acks), the bound only
// fails the test when the guaranteed behavior does not happen.
func waitForDeadline(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNewNATSJetStreamUnreachable(t *testing.T) {
	_, err := NewNATSJetStream("nats://127.0.0.1:1")
	if !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("NewNATSJetStream(unreachable) = %v, want ErrBrokerUnavailable", err)
	}
	if _, err := NewNATSJetStream(""); err == nil {
		t.Fatal("empty url accepted")
	}
}

func TestNATSJetStreamPublishSubscribeRoundTrip(t *testing.T) {
	srv := startEmbeddedNATS(t)
	js, err := NewNATSJetStream(srv.ClientURL())
	if err != nil {
		t.Fatalf("NewNATSJetStream: %v", err)
	}
	defer js.Close()

	// Idempotent stream setup (constructor already ran it).
	if err := js.EnsureStream(MessagingStreamName, MessagingStreamSubjects); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	s1, err := js.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run","n":1}`))
	if err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	s2, err := js.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run","n":2}`))
	if err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	if s1 <= 0 || s2 <= s1 {
		t.Fatalf("stream sequences = %d, %d; want strictly increasing", s1, s2)
	}

	got := make(chan Message, 8)
	sub, err := js.Subscribe(SubjectRunsCreate, "durable-roundtrip", func(m Message) { got <- m })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	for want := 1; want <= 2; want++ {
		select {
		case m := <-got:
			if m.Deliveries() != 1 {
				t.Fatalf("fresh delivery count = %d, want 1", m.Deliveries())
			}
			if m.Subject() != SubjectRunsCreate || m.Sequence() == 0 {
				t.Fatalf("delivery = subject %q seq %d", m.Subject(), m.Sequence())
			}
			if err := m.Ack(); err != nil {
				t.Fatalf("ack: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("delivery %d missing", want)
		}
	}
	if !sub.Valid() {
		t.Fatal("subscription invalid while connected")
	}
}

func TestNATSJetStreamConsumerFrameworkAckNakDeadLetter(t *testing.T) {
	srv := startEmbeddedNATS(t)
	js, err := NewNATSJetStream(srv.ClientURL())
	if err != nil {
		t.Fatalf("NewNATSJetStream: %v", err)
	}
	defer js.Close()

	var mu sync.Mutex
	fail := true
	c := NewConsumer(js, "real-dlq",
		WithMaxDeliver(2),
		WithNakDelay(50*time.Millisecond),
		WithSupervision(5*time.Second, 100*time.Millisecond, time.Second),
	)
	if err := c.Subscribe(SubjectRunsCreate, func(_ context.Context, m Message) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return errors.New("real broker boom")
		}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()

	if _, err := js.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run","task_id":"real-1","run_id":"real-run-1","organization_id":"org-real"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Attempt 1 fails -> nak(50ms) -> server redelivers; attempt 2 (== cap)
	// dead-letters. Both transitions are broker-guaranteed.
	waitForDeadline(t, "first attempt", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return c.Stats().Redeliveries == 1
	})

	entries := func() []DLQEntry {
		es, err := c.InspectDLQ(10)
		if err != nil {
			t.Fatalf("InspectDLQ: %v", err)
		}
		return es
	}
	waitForDeadline(t, "dead letter on the real broker", func() bool { return len(entries()) == 1 })
	e := entries()[0]
	if e.Subject != SubjectRunsCreate || e.Error != "real broker boom" || e.Attempts != 2 {
		t.Fatalf("real-broker DLQ entry = %+v", e)
	}
	if e.TaskID != "real-1" || e.RunID != "real-run-1" || e.OrganizationID != "org-real" {
		t.Fatalf("real-broker DLQ identity = %+v", e)
	}

	// Requeue: payload republished verbatim, DLQ entry consumed, fresh
	// delivery succeeds end to end.
	mu.Lock()
	fail = false
	mu.Unlock()
	if _, err := c.RequeueDLQ(e.Sequence); err != nil {
		t.Fatalf("RequeueDLQ: %v", err)
	}
	waitForDeadline(t, "requeued run executed", func() bool { return c.Stats().Processed == 1 })
	waitForDeadline(t, "DLQ drained", func() bool { return len(entries()) == 0 })
}

func TestNATSJetStreamLogRangeAndDelete(t *testing.T) {
	srv := startEmbeddedNATS(t)
	js, err := NewNATSJetStream(srv.ClientURL())
	if err != nil {
		t.Fatalf("NewNATSJetStream: %v", err)
	}
	defer js.Close()

	if _, err := js.Publish(SubjectRunsCreate, []byte("runs-1")); err != nil {
		t.Fatalf("publish runs: %v", err)
	}
	if _, err := js.Publish(SubjectWorkflowNodeCompleted, []byte("node-1")); err != nil {
		t.Fatalf("publish nodes: %v", err)
	}
	if _, err := js.Publish(SubjectRunsCreate, []byte("runs-2")); err != nil {
		t.Fatalf("publish runs 2: %v", err)
	}

	var subjects []string
	err = js.Range(SubjectRunsCreate, func(seq uint64, ts time.Time, data []byte) bool {
		if string(data) != "runs-1" && string(data) != "runs-2" {
			t.Errorf("Range leaked a foreign subject payload: %q", data)
		}
		subjects = append(subjects, string(data))
		if ts.IsZero() {
			t.Error("retained entry carries a zero timestamp")
		}
		return true
	})
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(subjects) != 2 || subjects[0] != "runs-1" || subjects[1] != "runs-2" {
		t.Fatalf("Range order = %v, want oldest-first [runs-1 runs-2]", subjects)
	}
	if err := js.Range("agentos.never.published", func(uint64, time.Time, []byte) bool { return true }); !errors.Is(err, ErrNotRetained) {
		t.Fatalf("Range on empty subject = %v, want ErrNotRetained", err)
	}

	// Delete consumes one retained entry (the DLQ requeue primitive); an
	// unknown sequence is ErrEntryNotFound.
	if err := js.Delete(SubjectRunsCreate, 99999); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("Delete unknown seq = %v, want ErrEntryNotFound", err)
	}
	// The first runs entry: walk to find its stream sequence.
	var firstSeq uint64
	if err := js.Range(SubjectRunsCreate, func(seq uint64, _ time.Time, data []byte) bool {
		if string(data) == "runs-1" {
			firstSeq = seq
			return false
		}
		return true
	}); err != nil {
		t.Fatalf("Range lookup: %v", err)
	}
	if err := js.Delete(SubjectRunsCreate, firstSeq); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := js.Range(SubjectRunsCreate, func(uint64, time.Time, []byte) bool { return true }); err != nil {
		t.Fatalf("Range after delete: %v", err)
	}
}

func TestNATSJetStreamReplay(t *testing.T) {
	srv := startEmbeddedNATS(t)
	js, err := NewNATSJetStream(srv.ClientURL())
	if err != nil {
		t.Fatalf("NewNATSJetStream: %v", err)
	}
	defer js.Close()

	before := time.Now().UTC().Add(-time.Second)
	for i := 0; i < 3; i++ {
		if _, err := js.Publish(SubjectRunsCreate, []byte{byte(i)}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	c := NewConsumer(js, "real-replay") // Replay needs no Start
	n, err := c.Replay(SubjectRunsCreate, before, func(seq uint64, _ time.Time, data []byte) error {
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("Replay = (%d, %v), want (3, nil)", n, err)
	}
	n, err = c.Replay(SubjectRunsCreate, time.Now().UTC().Add(time.Hour), func(uint64, time.Time, []byte) error {
		return nil
	})
	if err != nil || n != 0 {
		t.Fatalf("future-bound Replay = (%d, %v), want (0, nil)", n, err)
	}
}
