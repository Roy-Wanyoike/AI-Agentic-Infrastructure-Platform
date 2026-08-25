package queue

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestRedisQueueLifecycle(t *testing.T) {
	server := miniredis.RunT(t)
	q, err := NewRedisQueue(server.Addr())
	if err != nil {
		t.Fatalf("NewRedisQueue returned error: %v", err)
	}
	if q == nil {
		t.Fatal("NewRedisQueue returned nil queue")
	}

	task := q.Enqueue("agent.run", map[string]any{"agent_id": "agent-1"})
	if task == nil {
		t.Fatal("Enqueue should return a task from Redis")
	}
	if q.Length() != 1 {
		t.Fatalf("expected Redis queue length 1, got %d", q.Length())
	}

	dequeued := q.Dequeue()
	if dequeued == nil {
		t.Fatal("Dequeue should return the queued task")
	}
	if dequeued.Type != "agent.run" {
		t.Fatalf("expected task type agent.run, got %q", dequeued.Type)
	}
	if q.Length() != 0 {
		t.Fatalf("expected Redis queue length 0 after dequeue, got %d", q.Length())
	}
}
