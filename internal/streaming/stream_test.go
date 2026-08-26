package streaming

import (
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
