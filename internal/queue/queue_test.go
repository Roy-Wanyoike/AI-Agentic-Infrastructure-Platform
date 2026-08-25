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

func TestQueueRetryAndDeadLetter(t *testing.T) {
	q := NewQueue()
	task := q.Enqueue("agent.run", map[string]any{"agent_id": "agent-2"})
	if task == nil {
		t.Fatal("Enqueue should return a task")
	}
	q.MarkStarted(task)
	q.MarkFailed(task, "transient error")
	if task.Status != "queued" {
		t.Fatalf("after transient failure task should be retried, got %q", task.Status)
	}
	if task.Attempts != 1 {
		t.Fatalf("expected 1 attempt after first retry, got %d", task.Attempts)
	}
	for i := 0; i < 4; i++ {
		q.MarkStarted(task)
		q.MarkFailed(task, "transient error")
	}
	if task.Status != "dead_letter" {
		t.Fatalf("task should be dead-lettered after max retries, got %q", task.Status)
	}
	if q.Length() != 1 {
		t.Fatalf("dead-letter task should still remain in queue for visibility, got %d", q.Length())
	}
}
