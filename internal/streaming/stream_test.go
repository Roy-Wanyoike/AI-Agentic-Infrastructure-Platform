package streaming

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStreamingServicePublishesAndStoresRunEvents(t *testing.T) {
	service := NewService()
	ch := service.Subscribe("run-1")
	if ch == nil {
		t.Fatal("Subscribe should return a channel")
	}

	service.Publish("run-1", "status", "queued", map[string]any{"status": "queued"})
	select {
	case event := <-ch:
		if event.RunID != "run-1" {
			t.Fatalf("expected run-1, got %q", event.RunID)
		}
		if event.Type != "status" {
			t.Fatalf("expected status event, got %q", event.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for streamed run event")
	}

	history := service.History("run-1")
	if len(history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(history))
	}
}

func TestStreamingServiceAllowsMultipleSubscribers(t *testing.T) {
	service := NewService()
	first := service.Subscribe("run-2")
	second := service.Subscribe("run-2")
	if first == nil || second == nil {
		t.Fatal("multiple subscribers should each receive channels")
	}

	service.Publish("run-2", "status", "running", nil)
	for _, ch := range []<-chan Event{first, second} {
		select {
		case event := <-ch:
			if event.RunID != "run-2" {
				t.Fatalf("expected run-2, got %q", event.RunID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for second subscription to receive event")
		}
	}
}

// --- Task 1-c hardening tests -------------------------------------------------

func TestHistoryIsBoundedAndDropsOldest(t *testing.T) {
	service := NewService()
	for i := 0; i < HistoryLimit+50; i++ {
		service.Publish("run-bounded", "status", "status.changed", map[string]any{"seq": i})
	}
	history := service.History("run-bounded")
	if len(history) != HistoryLimit {
		t.Fatalf("expected history capped at %d, got %d", HistoryLimit, len(history))
	}
	// Oldest events must have been dropped: the first retained event is
	// sequence number 50.
	seq, _ := history[0].Payload["seq"].(int)
	if seq != 50 {
		t.Fatalf("expected first retained event to be seq 50, got %v", seq)
	}
	seqLast, _ := history[len(history)-1].Payload["seq"].(int)
	if seqLast != HistoryLimit+49 {
		t.Fatalf("expected last retained event to be seq %d, got %v", HistoryLimit+49, seqLast)
	}
}

func TestSlowConsumerDropsEventsInsteadOfBlocking(t *testing.T) {
	service := NewService()
	ch := service.Subscribe("run-slow")

	// Fill the subscriber buffer (16) and overflow it without reading.
	for i := 0; i < subscriberBuffer+10; i++ {
		service.Publish("run-slow", "status", "status.changed", map[string]any{"seq": i})
	}
	if dropped := service.DroppedTotal(); dropped < 10 {
		t.Fatalf("expected at least 10 dropped deliveries, got %d", dropped)
	}

	// The subscriber still received exactly the buffered prefix.
	buffered := 0
draining:
	for {
		select {
		case <-ch:
			buffered++
		default:
			break draining
		}
	}
	if buffered != subscriberBuffer {
		t.Fatalf("expected %d buffered events, got %d", subscriberBuffer, buffered)
	}
}

func TestUnsubscribeClosesChannelExactlyOnce(t *testing.T) {
	service := NewService()
	ch := service.Subscribe("run-unsub")
	service.Publish("run-unsub", "status", "status.changed", nil)

	if !service.Unsubscribe("run-unsub", ch) {
		t.Fatal("first Unsubscribe should succeed")
	}
	// The channel is closed. Events buffered before the close are still
	// delivered first; after draining them a receive reports ok == false.
	closed := false
	for i := 0; i < 128 && !closed; i++ {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for channel close")
		}
	}
	if !closed {
		t.Fatal("channel was never closed")
	}
	// A second Unsubscribe must be a safe no-op (no double-close panic).
	if service.Unsubscribe("run-unsub", ch) {
		t.Fatal("second Unsubscribe should report false")
	}
}

func TestUnsubscribeUnknownSubscriberReturnsFalse(t *testing.T) {
	service := NewService()
	if service.Unsubscribe("run-none", nil) {
		t.Fatal("nil channel should report false")
	}
	ch := service.Subscribe("run-other")
	if service.Unsubscribe("run-different", ch) {
		t.Fatal("unknown run id should report false")
	}
}

func TestUnsubscribedSubscriberStopsReceiving(t *testing.T) {
	service := NewService()
	ch := service.Subscribe("run-stop")
	if !service.Unsubscribe("run-stop", ch) {
		t.Fatal("expected Unsubscribe to succeed")
	}
	service.Publish("run-stop", "status", "status.changed", map[string]any{"after": true})
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("expected no delivery after unsubscribe, got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected closed channel after unsubscribe")
	}
}

func TestConcurrentSubscribeUnsubscribePublishIsRaceFree(t *testing.T) {
	service := NewService()
	done := make(chan struct{})

	// Publishers hammer a shared run while subscribers churn.
	for p := 0; p < 4; p++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 300; i++ {
				service.Publish("run-race", "status", "status.changed", map[string]any{"i": i})
			}
		}()
	}
	for sub := 0; sub < 8; sub++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for round := 0; round < 25; round++ {
				ch := service.Subscribe("run-race")
				// Drain briefly with a timeout instead of blocking forever.
				timer := time.NewTimer(2 * time.Millisecond)
			draining:
				for {
					select {
					case _, ok := <-ch:
						if !ok {
							break draining
						}
					case <-timer.C:
						break draining
					}
				}
				timer.Stop()
				_ = service.Unsubscribe("run-race", ch)
			}
		}()
	}

	for i := 0; i < 12; i++ {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for concurrent workers")
		}
	}
}

// syncRecorder is a race-safe httptest.ResponseRecorder stand-in so tests can
// read the SSE body while ServeSSE writes to it under -race.
type syncRecorder struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	status  int
	flushed bool
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{header: make(http.Header)}
}

func (s *syncRecorder) Header() http.Header { return s.header }

func (s *syncRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(p)
}

func (s *syncRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == 0 {
		s.status = code
	}
}

func (s *syncRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushed = true
}

func (s *syncRecorder) bodyString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String()
}

func (s *syncRecorder) dataFrames() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Count(s.body.String(), "data: ")
}

func (s *syncRecorder) pingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Count(s.body.String(), ": ping")
}

func TestServeSSEStreamsHistoryThenLiveEvents(t *testing.T) {
	service := NewService()
	service.pingInterval = 20 * time.Millisecond // speed up keep-alives for the test
	service.Publish("run-sse", "status", "status.changed", map[string]any{"status": "RUNNING"})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/run-sse/events", nil).WithContext(ctx)
	rec := newSyncRecorder()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		service.ServeSSE(rec, req, "run-sse")
	}()

	// History frame should appear shortly after the stream starts.
	waitForBody(t, rec, `"status":"RUNNING"`)

	// A live event published mid-stream must be forwarded exactly once
	// (deduplicated against the replayed history).
	service.Publish("run-sse", "status", "status.changed", map[string]any{"status": "COMPLETED"})
	waitForBody(t, rec, `"status":"COMPLETED"`)

	// Keep the stream open briefly so an idle ping fires.
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeSSE did not terminate after context cancellation")
	}

	header := rec.Header()
	if got := header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", got)
	}
	if got := header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("expected Cache-Control no-cache, got %q", got)
	}
	if got := header.Get("Connection"); got != "keep-alive" {
		t.Fatalf("expected Connection keep-alive, got %q", got)
	}

	if frames := rec.dataFrames(); frames != 2 {
		t.Fatalf("expected exactly 2 data frames (history + live), got %d in body:\n%s", frames, rec.bodyString())
	}
	if pings := rec.pingCount(); pings < 1 {
		t.Fatalf("expected at least one keep-alive ping, body:\n%s", rec.bodyString())
	}
}

func TestServeSSEUnsubscribesOnReturn(t *testing.T) {
	service := NewService()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/run-cleanup/events", nil).WithContext(ctx)
	rec := newSyncRecorder()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		service.ServeSSE(rec, req, "run-cleanup")
	}()
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeSSE did not terminate after client disconnect")
	}

	// After ServeSSE returns it must have unsubscribed: publishing now
	// reaches nobody (nothing is dropped — there are no subscribers) and
	// history keeps growing.
	service.Publish("run-cleanup", "status", "status.changed", nil)
	if dropped := service.DroppedTotal(); dropped != 0 {
		t.Fatalf("expected no dropped deliveries (no subscribers), got %d", dropped)
	}
	if got := len(service.History("run-cleanup")); got != 1 {
		t.Fatalf("expected history to keep accepting events, got %d", got)
	}

	// The run is back to a clean state: a fresh subscriber works normally.
	ch := service.Subscribe("run-cleanup")
	service.Publish("run-cleanup", "status", "status.changed", map[string]any{"status": "RUNNING"})
	select {
	case ev := <-ch:
		if ev.Payload["status"] != "RUNNING" {
			t.Fatalf("unexpected event payload: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fresh subscriber did not receive event after stream cleanup")
	}
	_ = service.Unsubscribe("run-cleanup", ch)
}

// waitForBody polls the race-safe recorder until the body contains want.
func waitForBody(t *testing.T, rec *syncRecorder, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.bodyString(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in SSE body; body so far:\n%s", want, rec.bodyString())
}
