package queue

import "testing"

func TestQueueLifecycle(t *testing.T) {
	q := NewQueue()
	if q.Length() != 0 {
		t.Fatalf("new queue should be empty, got %d", q.Length())
	}
	task := q.Enqueue("agent.run", map[string]any{"agent_id": "agent-1"})
	if task == nil {
		t.Fatal("Enqueue should return a task")
	}
	if q.Length() != 1 {
		t.Fatalf("expected queue length 1, got %d", q.Length())
	}
	if q.Peek() == nil || q.Peek().ID != task.ID {
		t.Fatal("Peek should return the first queued task")
	}
	q.Ack(task)
	if q.Length() != 0 {
		t.Fatalf("after ack queue should be empty, got %d", q.Length())
	}
}
