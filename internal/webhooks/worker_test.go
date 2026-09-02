package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/events"
)

// recordingClient is the fake HTTP client for delivery tests: it captures
// requests and replies with a scripted sequence of responses/errors. No live
// HTTP infrastructure is needed.
type recordingClient struct {
	mu          sync.Mutex
	calls       int
	requests    []*http.Request
	bodies      [][]byte
	statuses    []int // per-call status; ignored when respondNext/failErrs apply
	failErrs    []error
	respStatus  int
	respondNext func(call int) (int, error)
}

func (c *recordingClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	body, _ := io.ReadAll(req.Body)
	c.requests = append(c.requests, req)
	c.bodies = append(c.bodies, body)
	status := c.respStatus
	var err error
	if c.respondNext != nil {
		status, err = c.respondNext(call)
	} else if call <= len(c.failErrs) && c.failErrs[call-1] != nil {
		err = c.failErrs[call-1]
	} else if call <= len(c.statuses) {
		status = c.statuses[call-1]
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func (c *recordingClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// startWorker runs worker.Run on the test publisher and returns a cancel func
// plus a channel closed when Run returns. It waits deterministically until the
// worker's subscription is active so early publishes are never dropped.
func startWorker(svc *Service, pub *events.MemoryPublisher, client Doer, opts ...WorkerOption) (cancel context.CancelFunc, done <-chan struct{}) {
	worker := NewWorker(svc, pub, client, nil, opts...)
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_ = worker.Run(ctx)
	}()
	waitForSubscribed(pub)
	return cancel, doneCh
}

func waitForSubscribed(pub *events.MemoryPublisher) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && pub.Subscribers() == 0 {
		time.Sleep(time.Millisecond)
	}
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

// --- success path ---------------------------------------------------------

func TestWorkerDeliversSignedPayload(t *testing.T) {
	svc := NewService()
	wh, secret, err := svc.CreateWebhook(context.Background(), "org-1", "http://hook.example/callback", []string{events.EventRunCompleted})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := &recordingClient{respStatus: 200}
	pub := events.NewMemoryPublisher()
	cancel, done := startWorker(svc, pub, fake, WithBackoff([]time.Duration{time.Millisecond}))
	defer cancel()

	if err := pub.Publish(context.Background(), events.Event{
		ID:        "evt-123",
		Type:      events.EventRunCompleted,
		TenantID:  "org-1",
		Timestamp: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		Payload:   map[string]any{"run_id": "run-9", "status": "completed"},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		list, _ := svc.ListDeliveries(context.Background(), "org-1", wh.ID, 50)
		return len(list) == 1
	})
	cancel()
	<-done

	if fake.callCount() != 1 {
		t.Fatalf("expected exactly 1 HTTP attempt, got %d", fake.callCount())
	}
	fake.mu.Lock()
	req, body := fake.requests[0], fake.bodies[0]
	fake.mu.Unlock()

	if req.Method != http.MethodPost || req.URL.String() != "http://hook.example/callback" {
		t.Errorf("wrong request: %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := req.Header.Get("X-AgentOS-Event-Id"); got != "evt-123" {
		t.Errorf("X-AgentOS-Event-Id = %q", got)
	}
	wantSig := SignPayload(secret, body)
	if got := req.Header.Get("X-AgentOS-Signature"); got != wantSig {
		t.Errorf("signature header = %q, want HMAC over body %q", got, wantSig)
	}
	if !VerifyPayload(secret, body, got(req)) {
		t.Error("signature must verify with hmac scheme sha256=<hex>")
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	for _, key := range []string{"id", "type", "timestamp", "payload"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("delivery body missing %q: %s", key, body)
		}
	}
	if decoded["id"] != "evt-123" || decoded["type"] != events.EventRunCompleted {
		t.Errorf("wrong identity fields: %s", body)
	}
	if decoded["payload"].(map[string]any)["run_id"] != "run-9" {
		t.Errorf("payload not carried: %s", body)
	}
	if _, hasExtra := decoded["tenant_id"]; hasExtra {
		t.Error("delivery body must contain exactly id/type/timestamp/payload (no tenant leak)")
	}

	list, _ := svc.ListDeliveries(context.Background(), "org-1", wh.ID, 50)
	if len(list) != 1 {
		t.Fatalf("expected 1 delivery record, got %d", len(list))
	}
	d := list[0]
	if d.Status != DeliveryDelivered || d.Attempts != 1 || d.LastStatusCode != 200 {
		t.Errorf("delivery record wrong: %+v", d)
	}
	if d.EventID != "evt-123" || d.EventType != events.EventRunCompleted || d.WebhookID != wh.ID || d.OrganizationID != "org-1" {
		t.Errorf("delivery record identity wrong: %+v", d)
	}
	if d.LatencyMS < 0 {
		t.Errorf("latency should be recorded, got %d", d.LatencyMS)
	}
	if d.Error != "" {
		t.Errorf("successful delivery should have no error, got %q", d.Error)
	}
}

func got(req *http.Request) string { return req.Header.Get("X-AgentOS-Signature") }

// --- retry / backoff ------------------------------------------------------

func TestWorkerRetriesThenDelivers(t *testing.T) {
	svc := NewService()
	wh, _, err := svc.CreateWebhook(context.Background(), "org-1", "http://hook.example/flaky", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := &recordingClient{respondNext: func(call int) (int, error) {
		if call == 1 {
			return 0, errors.New("connection refused") // transport error
		}
		if call == 2 {
			return 500, nil // server error
		}
		return 200, nil // finally delivered
	}}
	pub := events.NewMemoryPublisher()
	cancel, done := startWorker(svc, pub, fake,
		WithBackoff([]time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}))
	defer cancel()

	if err := pub.Publish(context.Background(), events.NewEvent(events.EventRunFailed, "org-1", "run", "r-1", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		list, _ := svc.ListDeliveries(context.Background(), "org-1", wh.ID, 50)
		return len(list) == 1 && list[0].Status == DeliveryDelivered
	})
	cancel()
	<-done

	list, _ := svc.ListDeliveries(context.Background(), "org-1", wh.ID, 50)
	d := list[0]
	if d.Attempts != 3 || d.Status != DeliveryDelivered {
		t.Errorf("expected delivered after 3 attempts, got attempts=%d status=%s", d.Attempts, d.Status)
	}
	if d.LastStatusCode != 200 {
		t.Errorf("last status code should be 200, got %d", d.LastStatusCode)
	}
	if d.Error != "" {
		t.Errorf("successful delivery should clear error, got %q", d.Error)
	}
	if fake.callCount() != 3 {
		t.Errorf("expected 3 HTTP calls, got %d", fake.callCount())
	}
}

func TestWorkerGivesUpAfterMaxAttempts(t *testing.T) {
	svc := NewService()
	wh, _, err := svc.CreateWebhook(context.Background(), "org-1", "http://hook.example/always-down", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := &recordingClient{respStatus: 503}
	pub := events.NewMemoryPublisher()
	cancel, done := startWorker(svc, pub, fake,
		WithBackoff([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}),
		WithMaxAttempts(4)) // exercise backoff schedule clamping above contract default
	defer cancel()

	if err := pub.Publish(context.Background(), events.NewEvent(events.EventRunCancelled, "org-1", "run", "r-2", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		list, _ := svc.ListDeliveries(context.Background(), "org-1", wh.ID, 50)
		return len(list) == 1 && list[0].Status == DeliveryFailed
	})
	cancel()
	<-done

	list, _ := svc.ListDeliveries(context.Background(), "org-1", wh.ID, 50)
	d := list[0]
	if d.Status != DeliveryFailed || d.Attempts != 4 {
		t.Errorf("expected failed after 4 attempts, got %+v", d)
	}
	if d.LastStatusCode != 503 {
		t.Errorf("last status code should be 503, got %d", d.LastStatusCode)
	}
	if fake.callCount() != 4 {
		t.Errorf("expected 4 HTTP calls, got %d", fake.callCount())
	}
}

func TestWorkerFiltersByEventAndTenantAndStatus(t *testing.T) {
	svc := NewService()
	whFailedOnly, _, err := svc.CreateWebhook(context.Background(), "org-1", "http://hook.example/failed-only", []string{events.EventRunFailed})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.SetWebhookStatus(context.Background(), "org-1", whFailedOnly.ID, StatusDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}
	fake := &recordingClient{respStatus: 200}
	pub := events.NewMemoryPublisher()
	cancel, done := startWorker(svc, pub, fake, WithBackoff([]time.Duration{time.Millisecond}))
	defer cancel()

	// wrong tenant, wrong type, disabled webhook — none may hit the endpoint
	_ = pub.Publish(context.Background(), events.NewEvent(events.EventRunCompleted, "org-2", "run", "r", nil))
	_ = pub.Publish(context.Background(), events.NewEvent(events.EventRunFailed, "org-2", "run", "r", nil))
	_ = pub.Publish(context.Background(), events.NewEvent(events.EventRunFailed, "org-1", "run", "r", nil))

	time.Sleep(50 * time.Millisecond)
	if got := fake.callCount(); got != 0 {
		t.Errorf("no HTTP call expected (filtered/disabled), got %d", got)
	}
	list, _ := svc.ListDeliveries(context.Background(), "org-1", whFailedOnly.ID, 50)
	if len(list) != 0 {
		t.Errorf("no delivery record expected, got %d", len(list))
	}
	cancel()
	<-done
}

func TestWorkerStopsOnContextCancelMidBackoff(t *testing.T) {
	svc := NewService()
	if _, _, err := svc.CreateWebhook(context.Background(), "org-1", "http://hook.example/slow", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := &recordingClient{respondNext: func(call int) (int, error) {
		return 0, context.Canceled // attempts fail; worker sleeps in backoff
	}}
	pub := events.NewMemoryPublisher()
	cancel, done := startWorker(svc, pub, fake, WithBackoff([]time.Duration{5 * time.Second}))
	defer cancel()

	if err := pub.Publish(context.Background(), events.NewEvent(events.EventRunStarted, "org-1", "run", "r", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return fake.callCount() >= 1 })
	// cancel while the worker sits in the 5s backoff window
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancel")
	}
}

func TestWorkerRecordsTransportError(t *testing.T) {
	svc := NewService()
	wh, _, err := svc.CreateWebhook(context.Background(), "org-1", "http://hook.example/transport", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := &recordingClient{failErrs: []error{context.DeadlineExceeded}}
	pub := events.NewMemoryPublisher()
	cancel, done := startWorker(svc, pub, fake,
		WithBackoff([]time.Duration{time.Millisecond}),
		WithMaxAttempts(1))
	defer cancel()

	if err := pub.Publish(context.Background(), events.NewEvent(events.EventRunStarted, "org-1", "run", "r", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		list, _ := svc.ListDeliveries(context.Background(), "org-1", wh.ID, 50)
		return len(list) == 1
	})
	cancel()
	<-done

	list, _ := svc.ListDeliveries(context.Background(), "org-1", wh.ID, 50)
	d := list[0]
	if d.Status != DeliveryFailed || d.LastStatusCode != 0 {
		t.Errorf("transport failure should be failed with status 0, got %+v", d)
	}
	if !strings.Contains(d.Error, "context deadline exceeded") {
		t.Errorf("transport error should be recorded, got %q", d.Error)
	}
	if fake.callCount() != 1 {
		t.Errorf("maxAttempts=1 must stop after one call, got %d", fake.callCount())
	}
}
