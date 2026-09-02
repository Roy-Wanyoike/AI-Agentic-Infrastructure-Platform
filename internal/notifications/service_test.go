package notifications

import "testing"

func TestNotificationsLifecycle(t *testing.T) {
	service := NewService()
	msg := service.Send("user-1", "email", "Run failed", "The run timed out.")
	if msg == nil || msg.ID == "" {
		t.Fatal("notification should be created")
	}
	if len(service.List()) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(service.List()))
	}
}
