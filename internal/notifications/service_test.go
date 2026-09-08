package notifications

// Tests for the subscription model (issue #83): CRUD with tenant guards and
// event-type validation, the events.Subscriber filtering proxy, and the
// end-to-end path publish run.failed -> signed webhook delivery recorded via
// the webhooks delivery pipeline (recording Doer, no live infrastructure).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/events"
	"agentos/internal/webhooks"
)

// newTestService wires the memory publisher + webhooks service together the
// way cmd/api does; the delivery seam is attached separately per test.
func newTestService(t *testing.T) (*Service, *webhooks.Service, *events.MemoryPublisher) {
	t.Helper()
	pub := events.NewMemoryPublisher()
	hooks := webhooks.NewService()
	svc := NewService(pub, hooks, nil)
	return svc, hooks, pub
}

func mustWebhook(t *testing.T, hooks *webhooks.Service, orgID string, eventTypes []string) *webhooks.Webhook {
	t.Helper()
	wh, _, err := hooks.CreateWebhook(context.Background(), orgID, "https://hooks.example.com/agent", eventTypes)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	return wh
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- compile-time interface conformance -------------------------------------

func TestServiceImplementsEventsSubscriber(t *testing.T) {
	var _ events.Subscriber = (*Service)(nil)
}

// --- CRUD: tenant guards + validation ----------------------------------------

func TestSubscriptionCRUD(t *testing.T) {
	svc, hooks, _ := newTestService(t)
	ctx := context.Background()
	wh := mustWebhook(t, hooks, "org-1", nil)

	sub, err := svc.CreateSubscription(ctx, "org-1", wh.ID, []string{" run.failed ", events.EventRunFailed, events.EventApprovalRequested})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if sub.ID == "" || sub.OrganizationID != "org-1" || sub.WebhookID != wh.ID {
		t.Fatalf("subscription identity wrong: %+v", sub)
	}
	if len(sub.EventTypes) != 2 || sub.EventTypes[0] != events.EventRunFailed || sub.EventTypes[1] != events.EventApprovalRequested {
		t.Fatalf("event types should be trimmed + deduped, got %v", sub.EventTypes)
	}
	if sub.CreatedAt.IsZero() {
		t.Fatal("created_at must be set")
	}

	got, err := svc.GetSubscription(ctx, "org-1", sub.ID)
	if err != nil || got.ID != sub.ID {
		t.Fatalf("GetSubscription: %v %+v", err, got)
	}

	list, err := svc.ListSubscriptions(ctx, "org-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSubscriptions: %v %d", err, len(list))
	}

	if err := svc.DeleteSubscription(ctx, "org-1", sub.ID); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	if _, err := svc.GetSubscription(ctx, "org-1", sub.ID); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("expected ErrSubscriptionNotFound after delete, got %v", err)
	}
	if err := svc.DeleteSubscription(ctx, "org-1", sub.ID); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("deleting twice should be ErrSubscriptionNotFound, got %v", err)
	}
}

func TestSubscriptionTenantGuards(t *testing.T) {
	svc, hooks, _ := newTestService(t)
	ctx := context.Background()
	wh := mustWebhook(t, hooks, "org-1", []string{events.EventRunFailed})

	sub, err := svc.CreateSubscription(ctx, "org-1", wh.ID, []string{events.EventRunFailed})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	// Foreign-tenant reads/writes surface as not-found (no existence leak).
	if _, err := svc.GetSubscription(ctx, "org-2", sub.ID); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("cross-tenant get should be ErrSubscriptionNotFound, got %v", err)
	}
	if err := svc.DeleteSubscription(ctx, "org-2", sub.ID); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("cross-tenant delete should be ErrSubscriptionNotFound, got %v", err)
	}
	list, err := svc.ListSubscriptions(ctx, "org-2")
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-tenant list should be empty, got %v %d", err, len(list))
	}
	// Empty org/id guards.
	if _, err := svc.GetSubscription(ctx, "", sub.ID); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("empty org get should be ErrSubscriptionNotFound, got %v", err)
	}
	if err := svc.DeleteSubscription(ctx, "org-1", ""); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("empty id delete should be ErrSubscriptionNotFound, got %v", err)
	}
	if _, err := svc.CreateSubscription(ctx, "", wh.ID, []string{events.EventRunFailed}); !errors.Is(err, ErrInvalidSubscription) {
		t.Fatalf("empty org create should be ErrInvalidSubscription, got %v", err)
	}
}

func TestCreateSubscriptionValidatesWebhookSameOrg(t *testing.T) {
	svc, hooks, _ := newTestService(t)
	ctx := context.Background()
	wh := mustWebhook(t, hooks, "org-1", nil)

	// Unknown id.
	_, err := svc.CreateSubscription(ctx, "org-1", "wh-missing", []string{events.EventRunFailed})
	if !errors.Is(err, webhooks.ErrWebhookNotFound) {
		t.Fatalf("unknown webhook should be webhooks.ErrWebhookNotFound, got %v", err)
	}
	// Existing id, wrong tenant: same not-found contract (FK-ish guard).
	_, err = svc.CreateSubscription(ctx, "org-2", wh.ID, []string{events.EventRunFailed})
	if !errors.Is(err, webhooks.ErrWebhookNotFound) {
		t.Fatalf("foreign webhook should be webhooks.ErrWebhookNotFound, got %v", err)
	}
}

func TestCreateSubscriptionValidatesEventTypes(t *testing.T) {
	svc, hooks, _ := newTestService(t)
	ctx := context.Background()
	wh := mustWebhook(t, hooks, "org-1", nil)

	_, err := svc.CreateSubscription(ctx, "org-1", wh.ID, []string{"not.an.event"})
	if !errors.Is(err, ErrInvalidSubscription) {
		t.Fatalf("unknown event type should be ErrInvalidSubscription, got %v", err)
	}
	// The message must list every valid type from AllEventTypes().
	for _, valid := range events.AllEventTypes() {
		if !strings.Contains(err.Error(), valid) {
			t.Errorf("validation error should list valid type %q: %v", valid, err)
		}
	}
	// Empty / whitespace-only selections are rejected.
	_, err = svc.CreateSubscription(ctx, "org-1", wh.ID, nil)
	if !errors.Is(err, ErrInvalidSubscription) || !strings.Contains(err.Error(), "at least one event type") {
		t.Fatalf("empty event types should be rejected with hint, got %v", err)
	}
	_, err = svc.CreateSubscription(ctx, "org-1", wh.ID, []string{"  ", ""})
	if !errors.Is(err, ErrInvalidSubscription) {
		t.Fatalf("whitespace-only event types should be rejected, got %v", err)
	}
	// Missing webhook id.
	_, err = svc.CreateSubscription(ctx, "org-1", "  ", []string{events.EventRunFailed})
	if !errors.Is(err, ErrInvalidSubscription) || !strings.Contains(err.Error(), "webhook_id") {
		t.Fatalf("missing webhook_id should be rejected, got %v", err)
	}
}

// --- events.Subscriber filtering proxy ---------------------------------------

func TestSubscribeFiltersToPinnedAlertTypes(t *testing.T) {
	svc, _, pub := newTestService(t)
	ctx := context.Background()

	ch, cancel, err := svc.Subscribe([]string{events.EventAgentCreated, events.EventRunFailed})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	// A pinned type flows through.
	if err := pub.Publish(ctx, events.NewEvent(events.EventRunFailed, "org-1", "run", "r-1", nil)); err != nil {
		t.Fatalf("Publish run.failed: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Type != events.EventRunFailed {
			t.Fatalf("expected run.failed, got %q", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("pinned subscribed type never received")
	}

	// The non-pinned type (explicitly requested) must NOT leak through.
	if err := pub.Publish(ctx, events.NewEvent(events.EventAgentCreated, "org-1", "agent", "a-1", nil)); err != nil {
		t.Fatalf("Publish agent.created: %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("non-pinned event leaked through the filter: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeNilSelectionIsExactlyThePinnedSet(t *testing.T) {
	svc, _, pub := newTestService(t)
	ctx := context.Background()

	ch, cancel, err := svc.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	// Every pinned type flows through.
	for _, alertType := range AlertEventTypes() {
		if err := pub.Publish(ctx, events.NewEvent(alertType, "org-1", "run", "r", nil)); err != nil {
			t.Fatalf("Publish %s: %v", alertType, err)
		}
	}
	for _, alertType := range AlertEventTypes() {
		select {
		case ev := <-ch:
			if ev.Type != alertType {
				t.Fatalf("expected %s, got %s", alertType, ev.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("pinned type %s never received", alertType)
		}
	}

	// A non-pinned type never reaches a nil (pinned) subscription.
	if err := pub.Publish(ctx, events.NewEvent(events.EventRunStarted, "org-1", "run", "r", nil)); err != nil {
		t.Fatalf("Publish run.started: %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("non-pinned event leaked: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeNoPinnedTypeNeverWidens(t *testing.T) {
	svc, _, pub := newTestService(t)

	// A selection containing zero pinned types must receive NOTHING, not all
	// events (the memory publisher treats an empty filter as "all types").
	ch, cancel, err := svc.Subscribe([]string{events.EventAgentCreated})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if err := pub.Publish(context.Background(), events.NewEvent(events.EventRunFailed, "org-1", "run", "r", nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("empty intersection widened to all events: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeWithoutSourceFails(t *testing.T) {
	svc := NewService(nil, webhooks.NewService(), nil)
	if _, _, err := svc.Subscribe(nil); err == nil {
		t.Fatal("Subscribe without a source should fail")
	}
}

// --- end-to-end: publish run.failed -> signed webhook delivery ---------------

// recordingDoer is the fake HTTP transport (webhooks.Doer) capturing every
// delivery attempt and replying with a fixed status. No live infrastructure.
type recordingDoer struct {
	mu       sync.Mutex
	calls    int
	captures []*httpCapture
	status   int
}

type httpCapture struct {
	Method  string
	URL     string
	Body    []byte
	Sign    string
	EventID string
	Type    string
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.calls++
	body, _ := io.ReadAll(req.Body)
	d.captures = append(d.captures, &httpCapture{
		Method:  req.Method,
		URL:     req.URL.String(),
		Body:    body,
		Sign:    req.Header.Get("X-AgentOS-Signature"),
		EventID: req.Header.Get("X-AgentOS-Event-Id"),
	})
	var decoded struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &decoded)
	// record the decoded type on the same capture under the lock
	if n := len(d.captures); n > 0 {
		d.captures[n-1].Type = decoded.Type
	}
	status := d.status
	d.mu.Unlock()
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func (d *recordingDoer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *recordingDoer) lastCapture() *httpCapture {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.captures) == 0 {
		return nil
	}
	return d.captures[len(d.captures)-1]
}

// waitForSubscribers waits until the publisher has n active subscriptions so
// early publishes are never dropped (mirrors the webhooks worker tests).
func waitForSubscribers(t *testing.T, pub *events.MemoryPublisher, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pub.Subscribers() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected %d subscribers, got %d", n, pub.Subscribers())
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestEndToEndRunFailedDeliveryIsSignedAndRecorded(t *testing.T) {
	ctx := context.Background()
	pub := events.NewMemoryPublisher()
	hooks := webhooks.NewService()
	// The webhook listens to run.completed ONLY: no webhook-level match for
	// run.failed, so any delivery below must have come from the subscription.
	wh, secret, err := hooks.CreateWebhook(ctx, "org-1", "https://hooks.example.com/alerts", []string{events.EventRunCompleted})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	fake := &recordingDoer{status: 200}
	worker := webhooks.NewWorker(hooks, pub, fake, nil, webhooks.WithBackoff([]time.Duration{time.Millisecond}))
	svc := NewService(pub, hooks, worker, WithLogger(discardLogger()))
	if _, err := svc.CreateSubscription(ctx, "org-1", wh.ID, []string{events.EventRunFailed}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	workerDone := make(chan struct{})
	go func() { _ = worker.Run(wctx); close(workerDone) }()
	svcDone := make(chan struct{})
	go func() { _ = svc.Run(wctx); close(svcDone) }()
	waitForSubscribers(t, pub, 2)

	if err := pub.Publish(ctx, events.NewEvent(events.EventRunFailed, "org-1", "run", "run-9", map[string]any{"error": "timeout"})); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return fake.callCount() == 1 })
	wcancel()
	<-workerDone
	<-svcDone

	cap := fake.lastCapture()
	if cap == nil {
		t.Fatal("no delivery attempt captured")
	}
	if cap.Method != http.MethodPost || cap.URL != wh.URL {
		t.Fatalf("wrong request: %s %s", cap.Method, cap.URL)
	}
	if cap.Type != events.EventRunFailed {
		t.Fatalf("delivery body type = %q, want run.failed", cap.Type)
	}
	if cap.EventID == "" {
		t.Fatal("X-AgentOS-Event-Id header must be present")
	}
	// The delivery must be signed exactly like ordinary webhook traffic.
	if want := webhooks.SignPayload(secret, cap.Body); cap.Sign != want || !webhooks.VerifyPayload(secret, cap.Body, cap.Sign) {
		t.Fatalf("signature header = %q, want HMAC over body %q", cap.Sign, want)
	}

	// Delivery recorded via the EXISTING webhook status fields.
	list, err := hooks.ListDeliveries(ctx, "org-1", wh.ID, 50)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListDeliveries: %v %d", err, len(list))
	}
	d := list[0]
	if d.Status != webhooks.DeliveryDelivered || d.Attempts != 1 || d.LastStatusCode != 200 {
		t.Fatalf("delivery record wrong: %+v", d)
	}
	if d.EventType != events.EventRunFailed || d.WebhookID != wh.ID || d.OrganizationID != "org-1" {
		t.Fatalf("delivery identity wrong: %+v", d)
	}
	if d.Error != "" {
		t.Fatalf("successful delivery should carry no error, got %q", d.Error)
	}
}

func TestEndToEndUnsubscribedEventsAreNotDelivered(t *testing.T) {
	ctx := context.Background()
	pub := events.NewMemoryPublisher()
	hooks := webhooks.NewService()
	// The webhook listens to deployment.failed ONLY (not an event this test
	// publishes): any delivery below could only come from the worker's own
	// path matching the webhook's events — which none of the publishes do.
	wh, _, err := hooks.CreateWebhook(ctx, "org-1", "https://hooks.example.com/alerts", []string{events.EventDeploymentFailed})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	fake := &recordingDoer{status: 200}
	worker := webhooks.NewWorker(hooks, pub, fake, nil, webhooks.WithBackoff([]time.Duration{time.Millisecond}))
	svc := NewService(pub, hooks, worker, WithLogger(discardLogger()))
	if _, err := svc.CreateSubscription(ctx, "org-1", wh.ID, []string{events.EventRunFailed}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	go func() { _ = worker.Run(wctx) }()
	go func() { _ = svc.Run(wctx) }()
	waitForSubscribers(t, pub, 2)

	// Foreign-tenant failure + non-subscribed types: none may deliver.
	_ = pub.Publish(ctx, events.NewEvent(events.EventRunFailed, "org-2", "run", "r-foreign", nil))
	_ = pub.Publish(ctx, events.NewEvent(events.EventApprovalRequested, "org-1", "approval", "ap-1", nil))
	_ = pub.Publish(ctx, events.NewEvent(events.EventRunStarted, "org-1", "run", "r-started", nil))
	time.Sleep(80 * time.Millisecond)
	wcancel()

	if got := fake.callCount(); got != 0 {
		t.Fatalf("no delivery expected, got %d", got)
	}
	list, _ := hooks.ListDeliveries(ctx, "org-1", wh.ID, 50)
	if len(list) != 0 {
		t.Fatalf("no delivery records expected, got %d", len(list))
	}
}

func TestEndToEndDeliveryFailureRecorded(t *testing.T) {
	ctx := context.Background()
	pub := events.NewMemoryPublisher()
	hooks := webhooks.NewService()
	// The webhook listens to run.completed ONLY: no webhook-level match for
	// run.failed, so the ONLY delivery path is the notification subscription.
	wh, _, err := hooks.CreateWebhook(ctx, "org-1", "https://hooks.example.com/down", []string{events.EventRunCompleted})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	// Endpoint always 500s: the notification path must not wedge and the
	// failure must surface through the existing delivery status fields.
	fake := &recordingDoer{status: 500}
	worker := webhooks.NewWorker(hooks, pub, fake, nil,
		webhooks.WithBackoff([]time.Duration{time.Millisecond}),
		webhooks.WithMaxAttempts(2))
	svc := NewService(pub, hooks, worker, WithLogger(discardLogger()))
	if _, err := svc.CreateSubscription(ctx, "org-1", wh.ID, []string{events.EventRunFailed}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	go func() { _ = worker.Run(wctx) }()
	go func() { _ = svc.Run(wctx) }()
	waitForSubscribers(t, pub, 2)

	if err := pub.Publish(ctx, events.NewEvent(events.EventRunFailed, "org-1", "run", "run-x", nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		list, _ := hooks.ListDeliveries(ctx, "org-1", wh.ID, 50)
		return len(list) == 1 && list[0].Status == webhooks.DeliveryFailed
	})
	wcancel()

	list, _ := hooks.ListDeliveries(ctx, "org-1", wh.ID, 50)
	d := list[0]
	if d.Status != webhooks.DeliveryFailed || d.Attempts != 2 || d.LastStatusCode != 500 {
		t.Fatalf("failed delivery record wrong: %+v", d)
	}
}

func TestSubscriberSkipsMissingAndDisabledWebhooks(t *testing.T) {
	ctx := context.Background()
	pub := events.NewMemoryPublisher()
	hooks := webhooks.NewService()
	// Both webhooks listen to run.completed ONLY: the notification
	// subscription (run.failed) is the only possible delivery path.
	whA, _, err := hooks.CreateWebhook(ctx, "org-1", "https://hooks.example.com/a", []string{events.EventRunCompleted})
	if err != nil {
		t.Fatalf("CreateWebhook A: %v", err)
	}
	whB, _, err := hooks.CreateWebhook(ctx, "org-1", "https://hooks.example.com/b", []string{events.EventRunCompleted})
	if err != nil {
		t.Fatalf("CreateWebhook B: %v", err)
	}
	fake := &recordingDoer{status: 200}
	worker := webhooks.NewWorker(hooks, pub, fake, nil, webhooks.WithBackoff([]time.Duration{time.Millisecond}))
	svc := NewService(pub, hooks, worker, WithLogger(discardLogger()))

	// Two subscriptions: one whose webhook will disappear, one live.
	if _, err := svc.CreateSubscription(ctx, "org-1", whA.ID, []string{events.EventRunFailed}); err != nil {
		t.Fatalf("CreateSubscription A: %v", err)
	}
	if _, err := svc.CreateSubscription(ctx, "org-1", whB.ID, []string{events.EventRunFailed}); err != nil {
		t.Fatalf("CreateSubscription B: %v", err)
	}
	// Delete webhook A: the stale subscription must be skipped without
	// erroring the run loop (delivery-failure resilience).
	if err := hooks.DeleteWebhook(ctx, "org-1", whA.ID); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}

	// runLoops starts the worker + subscriber loops and returns a cancel that
	// waits for BOTH goroutines to exit. Every webhook-state mutation below
	// happens only while the loops are quiesced: the in-memory webhooks
	// service is intentionally unsynchronized (single-goroutine access by
	// convention), so mutating it under a live reader would be a data race.
	runLoops := func(t *testing.T) context.CancelFunc {
		t.Helper()
		wctx, wcancel := context.WithCancel(context.Background())
		workerDone := make(chan struct{})
		svcDone := make(chan struct{})
		go func() { _ = worker.Run(wctx); close(workerDone) }()
		go func() { _ = svc.Run(wctx); close(svcDone) }()
		waitForSubscribers(t, pub, 2)
		return func() {
			wcancel()
			<-workerDone
			<-svcDone
		}
	}

	// Phase 1: the missing webhook (A) is skipped, the live one (B) delivers.
	cancelLoops := runLoops(t)
	if err := pub.Publish(ctx, events.NewEvent(events.EventRunFailed, "org-1", "run", "run-y", nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return fake.callCount() == 1 })
	if cap := fake.lastCapture(); cap == nil || cap.URL != whB.URL {
		t.Fatalf("delivery should hit the live webhook only: %+v", cap)
	}
	cancelLoops()

	// Phase 2: disabled webhooks are skipped at dispatch time.
	if err := hooks.SetWebhookStatus(ctx, "org-1", whB.ID, webhooks.StatusDisabled); err != nil {
		t.Fatalf("SetWebhookStatus: %v", err)
	}
	cancelLoops = runLoops(t)
	if err := pub.Publish(ctx, events.NewEvent(events.EventRunFailed, "org-1", "run", "run-z", nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if got := fake.callCount(); got != 1 {
		t.Fatalf("disabled webhook must not be delivered, got %d calls", got)
	}
	cancelLoops()
}

func TestDispatchWithoutSeamIsDropped(t *testing.T) {
	svc, hooks, _ := newTestService(t)
	ctx := context.Background()
	wh := mustWebhook(t, hooks, "org-1", nil)
	if _, err := svc.CreateSubscription(ctx, "org-1", wh.ID, []string{events.EventRunFailed}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	// deliver == nil: the run loop must survive and drop the event (logged).
	svc.processEvent(ctx, events.NewEvent(events.EventRunFailed, "org-1", "run", "r", nil))
	// Non-alert types and tenant-less events are ignored outright.
	svc.processEvent(ctx, events.NewEvent(events.EventAgentUpdated, "org-1", "agent", "a", nil))
	svc.processEvent(ctx, events.Event{Type: events.EventRunFailed})
}

func TestRunWithoutSourceReturnsError(t *testing.T) {
	svc := NewService(nil, webhooks.NewService(), nil)
	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("Run without a source should fail fast")
	}
}
