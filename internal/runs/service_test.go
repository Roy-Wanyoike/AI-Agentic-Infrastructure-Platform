package runs

import (
	"testing"

	"agentos/internal/streaming"
)

func TestUpdateStatusPublishesEvent(t *testing.T) {
	svc := NewService()
	streamer := streaming.NewService()
	svc.SetStreamer(streamer)

	run, err := svc.Create("org-1", "agent-1", "input")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := svc.UpdateStatus(run.ID, StatusRunning, ""); err != nil {
		t.Fatalf("update status failed: %v", err)
	}

	history := streamer.History(run.ID)
	if len(history) == 0 {
		t.Fatalf("expected at least one published event, got 0")
	}
	ev := history[len(history)-1]
	if ev.Type != "status" || ev.Name != "status.changed" {
		t.Fatalf("unexpected event: %#v", ev)
	}
}
