package webhooks

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebhooksLifecycle(t *testing.T) {
	service := NewService()
	endpoint, err := service.RegisterEndpoint("https://example.com/hook", "secret")
	if err != nil {
		t.Fatalf("RegisterEndpoint returned error: %v", err)
	}
	if endpoint.ID == "" {
		t.Fatal("endpoint ID should not be empty")
	}
	event := service.Publish("run.completed", map[string]any{"run_id": "run-1"})
	if event == nil || event.Type != "run.completed" {
		t.Fatal("publish should create an event")
	}
	if len(service.Snapshot()) != 1 {
		t.Fatalf("expected 1 event, got %d", len(service.Snapshot()))
	}
}

func TestWebhookDispatcherMatchesActiveEndpoints(t *testing.T) {
	service := NewService()
	_, err := service.RegisterEndpoint("https://example.com/hook", "secret")
	if err != nil {
		t.Fatalf("RegisterEndpoint returned error: %v", err)
	}
	event := service.Publish("run.completed", map[string]any{"run_id": "run-1"})
	if event == nil {
		t.Fatal("event should be published")
	}
	if len(service.Dispatch("run.completed")) != 1 {
		t.Fatal("dispatch should return active matching endpoints")
	}
	if len(service.Dispatch("run.failed")) != 0 {
		t.Fatal("dispatch should ignore unrelated events")
	}
}

func TestWebhookDispatcherSendsHTTPRequests(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("X-AgentOS-Signature"); got == "" {
			t.Fatal("expected signature header to be set")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	service := NewService()
	if _, err := service.RegisterEndpoint(server.URL, "secret"); err != nil {
		t.Fatalf("RegisterEndpoint returned error: %v", err)
	}
	service.Publish("run.completed", map[string]any{"run_id": "run-1"})

	if endpoints := service.Dispatch("run.completed"); len(endpoints) != 1 {
		t.Fatalf("expected 1 dispatched endpoint, got %d", len(endpoints))
	}
	if !called {
		t.Fatal("expected webhook HTTP delivery to fire")
	}
}
