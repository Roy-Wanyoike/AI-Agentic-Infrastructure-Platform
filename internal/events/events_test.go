package events

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// --- event model ---------------------------------------------------------

func TestEventTypeConstants(t *testing.T) {
	expected := map[string]string{
		EventAgentCreated:        "agent.created",
		EventAgentUpdated:        "agent.updated",
		EventRunStarted:          "run.started",
		EventRunCompleted:        "run.completed",
		EventRunFailed:           "run.failed",
		EventRunCancelled:        "run.cancelled",
		EventStepStarted:         "step.started",
		EventStepCompleted:       "step.completed",
		EventApprovalRequested:   "approval.requested",
		EventApprovalDecided:     "approval.decided",
		EventDeploymentCompleted: "deployment.completed",
		EventDeploymentFailed:    "deployment.failed",
		EventWebhookReceived:     "webhook.received",
	}
	for constant, want := range expected {
		if constant != want {
			t.Errorf("event constant mismatch: got %q want %q", constant, want)
		}
	}
	if got := len(AllEventTypes()); got != 13 {
		t.Errorf("expected 13 event types, got %d", got)
	}
	if !IsValidEventType("run.failed") || IsValidEventType("execution.completed") {
		t.Error("IsValidEventType should accept exactly the pinned contract types")
	}
}

func TestEventJSONShape(t *testing.T) {
	ts := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	event := Event{
		ID:          "evt-1",
		Type:        EventRunFailed,
		TenantID:    "org-1",
		ProjectID:   "proj-1",
		Timestamp:   ts,
		Resource:    Resource{Type: "run", ID: "run-1"},
		ExecutionID: "exec-1",
		TraceID:     "trace-1",
		Payload:     map[string]any{"reason": "timeout"},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"id", "type", "tenant_id", "project_id", "timestamp", "resource", "execution_id", "trace_id", "payload"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("expected key %q in event JSON", key)
		}
	}
	if !strings.Contains(string(data), `"timestamp":"2025-01-02T03:04:05Z"`) {
		t.Errorf("timestamp should marshal as RFC3339 UTC, got %s", data)
	}
	resource, ok := decoded["resource"].(map[string]any)
	if !ok || resource["type"] != "run" || resource["id"] != "run-1" {
		t.Errorf("resource shape wrong: %v", decoded["resource"])
	}
}

func TestNewEventFillsIdentity(t *testing.T) {
	event := NewEvent(EventRunStarted, "org-1", "run", "run-1", map[string]any{"input": "hi"})
	if event.ID == "" {
		t.Error("NewEvent should assign a UUID id")
	}
	if event.Timestamp.IsZero() || event.Timestamp.Location() != time.UTC {
		t.Error("NewEvent should assign a UTC timestamp")
	}
	if event.Payload["input"] != "hi" || event.Resource.ID != "run-1" {
		t.Error("NewEvent should carry resource + payload")
	}
}

// --- noop publisher -------------------------------------------------------

func TestNoopPublisherPublishesAndSubscribes(t *testing.T) {
	pub := NewNoopPublisher()
	if err := pub.Publish(context.Background(), NewEvent(EventRunCompleted, "org-1", "run", "r1", nil)); err != nil {
		t.Fatalf("noop publish should succeed: %v", err)
	}
	if err := pub.Publish(context.Background(), Event{Type: "bogus", TenantID: "org-1"}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("invalid event type should be rejected, got %v", err)
	}
	ch, cancel, err := pub.Subscribe([]string{EventRunCompleted})
	if err != nil {
		t.Fatalf("noop subscribe: %v", err)
	}
	cancel()
	cancel() // idempotent
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("noop subscription should never deliver a real event")
		}
	default:
		// nothing pending — fine
	}
}

// --- memory publisher -----------------------------------------------------

func waitForEvent(t *testing.T, ch <-chan Event, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for event")
		return Event{}
	}
}

func TestMemoryPublisherFanOutAndFilter(t *testing.T) {
	pub := NewMemoryPublisher()

	all, cancelAll, err := pub.Subscribe(nil)
	if err != nil {
		t.Fatalf("subscribe all: %v", err)
	}
	defer cancelAll()

	failedOnly, cancelFailed, err := pub.Subscribe([]string{EventRunFailed})
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}
	defer cancelFailed()

	if err := pub.Publish(context.Background(), NewEvent(EventRunCompleted, "org-1", "run", "r1", nil)); err != nil {
		t.Fatalf("publish completed: %v", err)
	}
	if err := pub.Publish(context.Background(), NewEvent(EventRunFailed, "org-1", "run", "r2", nil)); err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	got1 := waitForEvent(t, all, time.Second)
	got2 := waitForEvent(t, all, time.Second)
	if !((got1.Type == EventRunCompleted && got2.Type == EventRunFailed) || (got1.Type == EventRunFailed && got2.Type == EventRunCompleted)) {
		t.Errorf("all-subscriber should get both events, got %s then %s", got1.Type, got2.Type)
	}
	only := waitForEvent(t, failedOnly, time.Second)
	if only.Type != EventRunFailed {
		t.Errorf("filtered subscriber got wrong event: %s", only.Type)
	}
	select {
	case ev := <-failedOnly:
		t.Errorf("filtered subscriber must not receive %s", ev.Type)
	default:
	}
	if len(pub.Snapshot()) != 2 {
		t.Errorf("ring buffer should hold 2 events, got %d", len(pub.Snapshot()))
	}
	if pub.Dropped() != 0 {
		t.Errorf("no drops expected, got %d", pub.Dropped())
	}
}

func TestMemoryPublisherRingBufferCap(t *testing.T) {
	pub := NewMemoryPublisher()
	for i := 0; i < MemoryRingCapacity+50; i++ {
		event := NewEvent(EventStepCompleted, "org-1", "step", string(rune('a'+i%26)), nil)
		event.Payload = map[string]any{"i": i}
		if err := pub.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	snap := pub.Snapshot()
	if len(snap) != MemoryRingCapacity {
		t.Fatalf("ring should be capped at %d, got %d", MemoryRingCapacity, len(snap))
	}
	first, ok := snap[0].Payload["i"].(int)
	if !ok || first != 50 {
		t.Errorf("oldest 50 events should have been overwritten, first i=%v (ok=%v)", snap[0].Payload["i"], ok)
	}
}

func TestMemoryPublisherCancelStopsDelivery(t *testing.T) {
	pub := NewMemoryPublisher()
	ch, cancel, err := pub.Subscribe([]string{EventAgentCreated})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	cancel()
	cancel() // idempotent
	if err := pub.Publish(context.Background(), NewEvent(EventAgentCreated, "org-1", "agent", "a1", nil)); err != nil {
		t.Fatalf("publish after cancel: %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("cancelled subscription channel should be closed (no deliveries)")
		}
	default:
		// no event delivered — acceptable; cancel must stop future fan-out
	}
	pub.Close()
	if err := pub.Publish(context.Background(), NewEvent(EventAgentCreated, "org-1", "agent", "a1", nil)); !errors.Is(err, ErrSubscribeClosed) {
		t.Errorf("publish after close: %v", err)
	}
}

func TestMemoryPublisherConcurrentPublish(t *testing.T) {
	pub := NewMemoryPublisher()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 70; j++ {
				_ = pub.Publish(context.Background(), NewEvent(EventRunStarted, "org-1", "run", "r", nil))
			}
		}(i)
	}
	wg.Wait()
	if got := len(pub.Snapshot()); got != MemoryRingCapacity {
		t.Errorf("ring should be full (%d), got %d", MemoryRingCapacity, got)
	}
}

func TestMemoryPublisherRejectsInvalidEvents(t *testing.T) {
	pub := NewMemoryPublisher()
	if err := pub.Publish(context.Background(), Event{Type: "nope", TenantID: "org-1"}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("unknown type should be rejected: %v", err)
	}
	if err := pub.Publish(context.Background(), Event{Type: EventRunStarted}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("missing tenant should be rejected: %v", err)
	}
}

func TestMemoryPublisherClose(t *testing.T) {
	pub := NewMemoryPublisher()
	ch, cancel, _ := pub.Subscribe(nil)
	defer cancel()
	pub.Close()
	if err := pub.Publish(context.Background(), NewEvent(EventRunStarted, "org-1", "run", "r", nil)); !errors.Is(err, ErrSubscribeClosed) {
		t.Errorf("publish after close should fail: %v", err)
	}
	if _, _, err := pub.Subscribe(nil); !errors.Is(err, ErrSubscribeClosed) {
		t.Errorf("subscribe after close should fail: %v", err)
	}
	// the pre-existing subscription channel is closed by Close
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed")
		}
	default:
		t.Error("channel should be closed and readable")
	}
}

// --- NewFromEnv -----------------------------------------------------------

func TestNewFromEnvDefaults(t *testing.T) {
	t.Setenv(EnvNATSURL, "")
	if _, ok := NewFromEnv().(*NoopPublisher); !ok {
		t.Error("empty AGENTOS_NATS_URL should select the noop publisher")
	}
}

func TestNewFromEnvFallsBackToMemoryWhenNATSDown(t *testing.T) {
	// port 1 on localhost: nothing listens there; connect must fail fast
	t.Setenv(EnvNATSURL, "nats://127.0.0.1:1")
	pub := NewFromEnv()
	if _, ok := pub.(*MemoryPublisher); !ok {
		t.Errorf("unreachable NATS should fall back to memory publisher, got %T", pub)
	}
}

// --- NATS publisher (compile + connection-error path only; no live NATS) ---

func TestNATSPublisherUnreachableReturnsError(t *testing.T) {
	if _, err := NewNATSPublisher(""); err == nil {
		t.Error("empty url should error")
	}
	if _, err := NewNATSPublisher("nats://127.0.0.1:1"); err == nil {
		t.Error("unreachable NATS should return an error so the caller can fall back")
	}
}

func TestSubjectFor(t *testing.T) {
	cases := map[string]string{
		EventRunFailed:    "agentos.events.run.failed",
		EventAgentCreated: "agentos.events.agent.created",
	}
	for eventType, want := range cases {
		if got := SubjectFor(eventType); got != want {
			t.Errorf("SubjectFor(%s) = %s, want %s", eventType, got, want)
		}
	}
}

func TestNATSPublisherInterfaces(t *testing.T) {
	var _ Publisher = (*NATSPublisher)(nil)
	var _ Subscriber = (*NATSPublisher)(nil)
	var _ Publisher = (*MemoryPublisher)(nil)
	var _ Subscriber = (*MemoryPublisher)(nil)
	var _ Publisher = (*NoopPublisher)(nil)
	var _ Subscriber = (*NoopPublisher)(nil)
	var _ Publisher = (*AuditPublisher)(nil)
}

// --- append-only audit store (sqlmock) ------------------------------------

func TestAuditPublisherPersistsThenForwards(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO events").
		WithArgs(sqlmock.AnyArg(), "org-1", EventRunCompleted, "", "run", "run-1", "", "", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	inner := NewMemoryPublisher()
	pub := NewAuditPublisher(NewPostgresStore(db), inner)

	event := NewEvent(EventRunCompleted, "org-1", "run", "run-1", nil)
	if err := pub.Publish(context.Background(), event); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("audit insert expectations: %v", err)
	}
	snap := inner.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("event should be forwarded to the inner publisher")
	}
	if snap[0].ID != event.ID {
		t.Errorf("audit should normalize identity once; forwarded id changed: %s vs %s", snap[0].ID, event.ID)
	}
}

func TestAuditPublisherFailsClosedOnStoreError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO events").WillReturnError(errors.New("db down"))

	inner := NewMemoryPublisher()
	pub := NewAuditPublisher(NewPostgresStore(db), inner)

	err = pub.Publish(context.Background(), NewEvent(EventRunFailed, "org-1", "run", "r", nil))
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("audit failure should fail the publish, got %v", err)
	}
	if len(inner.Snapshot()) != 0 {
		t.Error("event must not be forwarded when the audit write fails")
	}
}

func TestAuditPublisherPassThroughWithoutStore(t *testing.T) {
	inner := NewMemoryPublisher()
	pub := NewAuditPublisher(nil, inner)
	if err := pub.Publish(context.Background(), NewEvent(EventRunStarted, "org-1", "run", "r", nil)); err != nil {
		t.Fatalf("pass-through publish: %v", err)
	}
	if len(inner.Snapshot()) != 1 {
		t.Error("nil store should behave as pass-through")
	}
}

func TestPgStoreAppendEventMarshal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	payload := map[string]any{"reason": "timeout"}
	mock.ExpectExec("INSERT INTO events").
		WithArgs("evt-1", "org-1", EventRunFailed, "proj-1", "run", "run-1", "exec-1", "trace-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	store := NewPostgresStore(db)
	event := Event{
		ID:          "evt-1",
		Type:        EventRunFailed,
		TenantID:    "org-1",
		ProjectID:   "proj-1",
		Resource:    Resource{Type: "run", ID: "run-1"},
		ExecutionID: "exec-1",
		TraceID:     "trace-1",
		Payload:     payload,
	}
	if err := store.AppendEvent(context.Background(), &event); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestReflectEventFieldsContract(t *testing.T) {
	// guard the wire contract: field names pinned by docs/wave2-api-contract.md
	eventType := reflect.TypeOf(Event{})
	want := map[string]string{
		"ID": "id", "Type": "type", "TenantID": "tenant_id", "ProjectID": "project_id",
		"Timestamp": "timestamp", "Resource": "resource", "ExecutionID": "execution_id",
		"TraceID": "trace_id", "Payload": "payload",
	}
	for field, tag := range want {
		f, ok := eventType.FieldByName(field)
		if !ok {
			t.Fatalf("field %s missing", field)
		}
		if !strings.Contains(f.Tag.Get("json"), tag) {
			t.Errorf("field %s json tag = %q, want to contain %q", field, f.Tag.Get("json"), tag)
		}
	}
}
