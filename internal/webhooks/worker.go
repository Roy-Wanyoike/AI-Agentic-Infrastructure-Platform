package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/events"
)

// Delivery behavior defaults from the wave-2 contract (track 2-e):
// POST with 10s HTTP timeout, retries with 1s/5s/30s exponential-ish backoff,
// at most 3 attempts per delivery.
const (
	DefaultHTTPTimeout  = 10 * time.Second
	maxAttemptsContract = 3
)

// DefaultBackoff is the inter-attempt sleep schedule (attempt 1 fails -> wait
// 1s -> attempt 2 fails -> wait 5s -> attempt 3 -> give up). The 30s entry
// applies when maxAttempts is raised above the contract default of 3.
var DefaultBackoff = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

// Doer is the injectable HTTP transport (http.Client satisfies it; tests pass
// fakes so no live infrastructure is needed).
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// deliveryPayload is the exact outbound JSON body pinned by the contract.
type deliveryPayload struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Payload   map[string]any `json:"payload"`
}

// Worker subscribes to the event publisher and delivers matching events to the
// tenant's active webhooks, recording every attempt. Each (event, webhook)
// delivery runs on its own goroutine so one slow endpoint cannot block others.
type Worker struct {
	svc         *Service
	subscriber  events.Subscriber
	client      Doer
	logr        *slog.Logger
	backoff     []time.Duration
	maxAttempts int
	wg          sync.WaitGroup
}

// WorkerOption tunes the worker (mainly for tests: tiny backoffs, custom
// attempt caps).
type WorkerOption func(*Worker)

// WithBackoff overrides the inter-attempt backoff schedule.
func WithBackoff(b []time.Duration) WorkerOption {
	return func(w *Worker) {
		if len(b) > 0 {
			w.backoff = append([]time.Duration(nil), b...)
		}
	}
}

// WithMaxAttempts overrides the per-delivery attempt cap (contract: 3).
func WithMaxAttempts(n int) WorkerOption {
	return func(w *Worker) {
		if n >= 1 {
			w.maxAttempts = n
		}
	}
}

// NewWorker builds the delivery worker. client may be nil to use
// http.Client{Timeout: 10s}; logr may be nil.
func NewWorker(svc *Service, subscriber events.Subscriber, client Doer, logr *slog.Logger, opts ...WorkerOption) *Worker {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if logr == nil {
		logr = slog.New(slog.DiscardHandler)
	}
	w := &Worker{
		svc:         svc,
		subscriber:  subscriber,
		client:      client,
		logr:        logr,
		backoff:     DefaultBackoff,
		maxAttempts: maxAttemptsContract,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Run subscribes to ALL event types and delivers until ctx is cancelled or the
// subscription channel closes. Returns ctx.Err() on shutdown.
func (w *Worker) Run(ctx context.Context) error {
	ch, cancel, err := w.subscriber.Subscribe(nil) // nil = every event type
	if err != nil {
		return fmt.Errorf("webhooks: worker subscribe: %w", err)
	}
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			w.wg.Wait()
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				w.wg.Wait()
				return nil
			}
			w.processEvent(ctx, ev)
		}
	}
}

// processEvent fans the event out to every active matching webhook of the
// event's tenant. Deliveries run asynchronously (tracked by wg) so the event
// loop never blocks on HTTP.
func (w *Worker) processEvent(ctx context.Context, ev events.Event) {
	if ev.TenantID == "" || ev.Type == "" {
		return
	}
	webhooks, err := w.svc.WebhooksForEvent(ctx, ev.TenantID, ev.Type)
	if err != nil {
		w.logr.Error("webhooks: match webhooks failed", "error", err, "event_id", ev.ID)
		return
	}
	if len(webhooks) == 0 {
		return
	}
	for _, wh := range webhooks {
		w.wg.Add(1)
		go func(wh *Webhook) {
			defer w.wg.Done()
			w.deliverWithRetries(ctx, wh, ev)
		}(wh)
	}
}

// deliverWithRetries POSTs the event to one webhook, retrying with backoff and
// upserting the delivery record after EVERY attempt.
func (w *Worker) deliverWithRetries(ctx context.Context, wh *Webhook, ev events.Event) {
	body, err := json.Marshal(deliveryPayload{
		ID:        ev.ID,
		Type:      ev.Type,
		Timestamp: ev.Timestamp,
		Payload:   ev.Payload,
	})
	if err != nil {
		w.logr.Error("webhooks: marshal delivery payload", "error", err, "event_id", ev.ID)
		return
	}

	delivery := &Delivery{
		ID:             uuid.NewString(),
		WebhookID:      wh.ID,
		OrganizationID: wh.OrganizationID,
		EventID:        ev.ID,
		EventType:      ev.Type,
		Status:         DeliveryRetrying,
		Attempts:       0,
	}
	secret := w.svc.secretForDelivery(wh)

	for attempt := 1; attempt <= w.maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			delivery.Status = DeliveryRetrying
			delivery.Error = "delivery cancelled"
			_ = w.svc.UpsertDelivery(ctx, delivery)
			return
		default:
		}

		start := time.Now().UTC()
		statusCode, attemptErr := w.post(ctx, wh.URL, body, secret, ev.ID)
		delivery.Attempts = attempt
		delivery.LastStatusCode = statusCode
		delivery.LatencyMS = time.Since(start).Milliseconds()
		if attemptErr != nil {
			delivery.Error = attemptErr.Error()
		} else {
			delivery.Error = ""
		}

		switch {
		case attemptErr == nil && statusCode >= 200 && statusCode < 300:
			delivery.Status = DeliveryDelivered
		case attempt < w.maxAttempts:
			delivery.Status = DeliveryRetrying
		default:
			delivery.Status = DeliveryFailed
		}
		// record every attempt
		if err := w.svc.UpsertDelivery(ctx, delivery); err != nil {
			w.logr.Error("webhooks: record delivery", "error", err, "delivery_id", delivery.ID)
		}

		if delivery.Status != DeliveryRetrying {
			return
		}

		backoffIdx := attempt - 1
		if backoffIdx >= len(w.backoff) {
			backoffIdx = len(w.backoff) - 1
		}
		if !sleep(ctx, w.backoff[backoffIdx]) {
			return
		}
	}
}

// post performs one HTTP attempt. It returns the response status code (0 when
// the request never got a response) and any error.
func (w *Worker) post(ctx context.Context, rawURL string, body []byte, secret, eventID string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agentos-webhook/1.0")
	req.Header.Set("X-AgentOS-Signature", SignPayload(secret, body))
	req.Header.Set("X-AgentOS-Event-Id", eventID)

	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}

// sleep waits for d, returning false when ctx was cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
