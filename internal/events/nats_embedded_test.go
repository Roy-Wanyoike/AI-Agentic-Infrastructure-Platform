package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startEmbeddedNATS boots an in-process NATS server with JetStream enabled so
// the NATS publish/subscribe wiring is verified for real — no docker, no
// external broker. The nats-server/v2 module is a TEST-ONLY dependency
// (documented in docs/wiring/redis-queue.md); nothing in the shipped binaries
// imports it.
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

// TestNATSPublisherEmbeddedRoundTrip is the end-to-end verification of the
// NATS path the wave-2 contract only allowed to be compile-checked so far:
// connect + stream setup, JetStream publish with ack, filtered subscription,
// subject naming (SubjectFor) on the wire, and the standard envelope shape.
func TestNATSPublisherEmbeddedRoundTrip(t *testing.T) {
	srv := startEmbeddedNATS(t)

	pub, err := NewNATSPublisher(srv.ClientURL())
	if err != nil {
		t.Fatalf("NewNATSPublisher against embedded server: %v", err)
	}
	defer pub.Close()

	ch, cancel, err := pub.Subscribe([]string{EventRunFailed})
	if err != nil {
		t.Fatalf("Subscribe(run.failed): %v", err)
	}
	defer cancel()

	// Independent core-NATS tap on the pinned subject: proves the subject
	// naming and the exact JSON envelope on the wire, not just the round trip
	// through our own unmarshaler.
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("core NATS connect: %v", err)
	}
	defer nc.Close()
	raw := make(chan *nats.Msg, 8)
	if _, err := nc.ChanSubscribe(SubjectFor(EventRunFailed), raw); err != nil {
		t.Fatalf("core subscribe %s: %v", SubjectFor(EventRunFailed), err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	event := NewEvent(EventRunFailed, "org-1", "run", "run-1",
		map[string]any{"reason": "timeout"})
	if err := pub.Publish(context.Background(), event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// 1. JetStream subscriber receives the event, envelope fields intact.
	got := waitForEvent(t, ch, 5*time.Second)
	if got.ID != event.ID || got.Type != EventRunFailed || got.TenantID != "org-1" {
		t.Errorf("delivered event = id %q type %q tenant %q, want id %q run.failed org-1",
			got.ID, got.Type, got.TenantID, event.ID)
	}
	if got.Payload["reason"] != "timeout" || got.Resource.ID != "run-1" {
		t.Errorf("payload/resource did not survive the round trip: %#v", got)
	}

	// 2. Raw message: exact subject + standard JSON envelope keys.
	select {
	case msg := <-raw:
		if msg.Subject != "agentos.events.run.failed" {
			t.Errorf("wire subject = %q, want agentos.events.run.failed", msg.Subject)
		}
		var wire Event
		if err := json.Unmarshal(msg.Data, &wire); err != nil {
			t.Fatalf("published bytes are not the standard event envelope: %v", err)
		}
		for _, key := range []string{"id", "type", "tenant_id", "timestamp"} {
			var probe map[string]any
			_ = json.Unmarshal(msg.Data, &probe)
			if _, ok := probe[key]; !ok {
				t.Errorf("published envelope is missing key %q: %s", key, msg.Data)
			}
		}
		if wire.ID != event.ID || wire.Type != EventRunFailed {
			t.Errorf("wire envelope = id %q type %q, want id %q run.failed", wire.ID, wire.Type, event.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("core subscriber saw no message on agentos.events.run.failed")
	}

	// 3. Resilience: malformed JSON on the subscribed subject is skipped
	// (handler drops it), foreign event types are filtered out by subject.
	if err := nc.Publish(SubjectFor(EventRunFailed), []byte(`{"broken`)); err != nil {
		t.Fatalf("publish malformed: %v", err)
	}
	if err := pub.Publish(context.Background(), NewEvent(EventAgentCreated, "org-1", "agent", "a-1", nil)); err != nil {
		t.Fatalf("publish foreign type: %v", err)
	}
	nc.Flush()
	select {
	case ev := <-ch:
		t.Errorf("run.failed subscriber must not receive %q", ev.Type)
	case <-time.After(300 * time.Millisecond):
		// nothing delivered — malformed + filtered messages correctly dropped
	}
}

// TestNATSPublisherEmbeddedSubscribeAllAndCancel covers the empty-types
// subscription (single consumer on agentos.events.>), delivery of multiple
// event types over it, and the cancel contract (unsubscribes + closes the
// channel, idempotent).
func TestNATSPublisherEmbeddedSubscribeAllAndCancel(t *testing.T) {
	srv := startEmbeddedNATS(t)

	pub, err := NewNATSPublisher(srv.ClientURL())
	if err != nil {
		t.Fatalf("NewNATSPublisher against embedded server: %v", err)
	}
	defer pub.Close()

	ch, cancel, err := pub.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe(all): %v", err)
	}

	types := []string{EventRunStarted, EventRunCompleted, EventAgentCreated}
	for _, typ := range types {
		if err := pub.Publish(context.Background(), NewEvent(typ, "org-2", "run", "r-1", nil)); err != nil {
			t.Fatalf("publish %s: %v", typ, err)
		}
	}

	seen := map[string]bool{}
	for i := 0; i < len(types); i++ {
		seen[waitForEvent(t, ch, 5*time.Second).Type] = true
	}
	for _, typ := range types {
		if !seen[typ] {
			t.Errorf("all-events subscriber did not receive %q (got %v)", typ, seen)
		}
	}

	cancel()
	cancel() // idempotent
	if _, ok := <-ch; ok {
		t.Error("cancel must close the delivery channel")
	}
	// Publishing after the subscriber cancelled must not panic the publisher.
	if err := pub.Publish(context.Background(), NewEvent(EventRunFailed, "org-2", "run", "r-2", nil)); err != nil {
		t.Errorf("publish after subscriber cancel: %v", err)
	}
}
