package queue

// ops_test.go — issue #77 introspection surface, in-memory backend: org
// stamping, the dead-letter → requeue → picked-up loop, requeue guards,
// cross-tenant isolation, pagination, stats, index eviction and concurrency
// (race-detector covered by `go test -race ./internal/queue/...`).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// collectHandler returns a worker handler that succeeds and records every
// processed task.
func collectHandler(processed *[]*Task) func(*Task) error {
	return func(task *Task) error {
		*processed = append(*processed, task)
		return nil
	}
}

// deadLetterThroughEngine drives the real worker loop with a failing handler
// until the task dead-letters (4 failed attempts) and returns the task.
func deadLetterThroughEngine(t *testing.T, q *Queue, taskType string, payload map[string]any) *Task {
	t.Helper()
	sent := q.Enqueue(taskType, payload)
	if sent == nil {
		t.Fatal("Enqueue returned nil task")
	}
	worker := NewWorker(q, func(*Task) error { return errors.New("transient failure") })
	for i := 0; i < 10; i++ {
		_ = worker.ProcessNext() // transient failures surface as errors; the loop just keeps walking
		if sent.Status == StatusDeadLetter {
			return sent
		}
	}
	t.Fatal("task never reached dead_letter through the engine loop")
	return nil
}

func TestOrgStampingFromPayload(t *testing.T) {
	q := NewQueue()
	withOrg := q.Enqueue("agent.run", map[string]any{"organization_id": "org-1", "run_id": "r-1"})
	if withOrg.OrganizationID != "org-1" {
		t.Fatalf("Enqueue must lift organization_id onto the task, got %q", withOrg.OrganizationID)
	}
	withoutOrg := q.Enqueue("agent.run", map[string]any{"run_id": "r-2"})
	if withoutOrg.OrganizationID != "" {
		t.Fatalf("task without organization_id payload must stay unscoped, got %q", withoutOrg.OrganizationID)
	}
	if nilPayload := q.Enqueue("agent.run", nil); nilPayload.OrganizationID != "" {
		t.Fatalf("nil payload must not panic or scope, got %q", nilPayload.OrganizationID)
	}
}

func TestDeadLetterRequeueFlowMemory(t *testing.T) {
	const org = "org-1"
	q := NewQueue()
	sent := deadLetterThroughEngine(t, q, "agent.run", map[string]any{"organization_id": org, "run_id": "r-9", "input": "hi"})
	createdAt := sent.CreatedAt

	// Dead letter is visible with the right status and error bookkeeping.
	dead, _, err := q.ListTasks(context.Background(), org, StatusDeadLetter, 10, "")
	if err != nil {
		t.Fatalf("ListTasks(dead_letter) returned error: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != sent.ID {
		t.Fatalf("dead-letter listing = %#v, want exactly task %s", dead, sent.ID)
	}
	if dead[0].Attempts != 4 || dead[0].LastError != "transient failure" {
		t.Fatalf("dead-letter record bookkeeping wrong: attempts=%d last_error=%q", dead[0].Attempts, dead[0].LastError)
	}
	if dead[0].Payload["run_id"] != "r-9" {
		t.Fatalf("dead-letter record must preserve the payload, got %#v", dead[0].Payload)
	}

	// Requeue: attempts reset, requeues counted, original enqueue time kept.
	rec, err := q.RequeueTask(context.Background(), org, sent.ID)
	if err != nil {
		t.Fatalf("RequeueTask returned error: %v", err)
	}
	if rec.Status != StatusQueued || rec.Attempts != 0 || rec.Requeues != 1 {
		t.Fatalf("requeued record = status %q attempts %d requeues %d", rec.Status, rec.Attempts, rec.Requeues)
	}
	if rec.LastError != "" {
		t.Fatalf("requeue must clear last_error, got %q", rec.LastError)
	}
	if rec.RequeuedAt == nil {
		t.Fatal("requeue must stamp requeued_at")
	}
	if !rec.CreatedAt.Equal(createdAt) {
		t.Fatalf("requeue must preserve created_at: got %v want %v", rec.CreatedAt, createdAt)
	}
	if q.Length() != 1 {
		t.Fatalf("requeued task must be back in the work list, length=%d", q.Length())
	}

	// Second requeue is rejected: the task is queued again, not dead-lettered.
	if _, err := q.RequeueTask(context.Background(), org, sent.ID); !errors.Is(err, ErrNotRequeueable) {
		t.Fatalf("second requeue must be ErrNotRequeueable, got %v", err)
	}

	// A worker picks the revived task up and completes it; the attempt
	// bookkeeping restarts from zero.
	var processed []*Task
	worker := NewWorker(q, collectHandler(&processed))
	if err := worker.ProcessNext(); err != nil {
		t.Fatalf("worker failed to pick up the requeued task: %v", err)
	}
	if len(processed) != 1 || processed[0].ID != sent.ID {
		t.Fatalf("worker processed %#v, want the revived task %s", processed, sent.ID)
	}
	if processed[0].Attempts != 1 {
		t.Fatalf("revived task must restart attempt bookkeeping, got attempts=%d", processed[0].Attempts)
	}
	final, err := q.GetTask(context.Background(), org, sent.ID)
	if err != nil {
		t.Fatalf("GetTask after completion returned error: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("completed task must be queryable, got status %q", final.Status)
	}
	if final.Requeues != 1 {
		t.Fatalf("requeues counter must survive later transitions, got %d", final.Requeues)
	}
}

func TestRequeueNonDeadLetterRejectedMemory(t *testing.T) {
	const org = "org-1"
	q := NewQueue()
	task := q.Enqueue("agent.run", map[string]any{"organization_id": org})

	// queued → 409 equivalent.
	if _, err := q.RequeueTask(context.Background(), org, task.ID); !errors.Is(err, ErrNotRequeueable) {
		t.Fatalf("requeue of a queued task must be ErrNotRequeueable, got %v", err)
	}
	// running → 409 equivalent.
	q.MarkStarted(task)
	if _, err := q.RequeueTask(context.Background(), org, task.ID); !errors.Is(err, ErrNotRequeueable) {
		t.Fatalf("requeue of a running task must be ErrNotRequeueable, got %v", err)
	}
	// completed → 409 equivalent.
	q.Ack(task)
	if _, err := q.RequeueTask(context.Background(), org, task.ID); !errors.Is(err, ErrNotRequeueable) {
		t.Fatalf("requeue of a completed task must be ErrNotRequeueable, got %v", err)
	}
	// unknown id → not found (never a leak).
	if _, err := q.RequeueTask(context.Background(), org, "no-such-task"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("requeue of an unknown task must be ErrTaskNotFound, got %v", err)
	}
}

func TestCrossTenantTaskIsolationMemory(t *testing.T) {
	q := NewQueue()
	a := q.Enqueue("agent.run", map[string]any{"organization_id": "org-a", "run_id": "r-a"})
	q.Enqueue("agent.run", map[string]any{"organization_id": "org-b", "run_id": "r-b"})

	listA, _, err := q.ListTasks(context.Background(), "org-a", "", 10, "")
	if err != nil {
		t.Fatalf("ListTasks(org-a) returned error: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != a.ID {
		t.Fatalf("org-a listing must contain only its own task, got %#v", listA)
	}
	if _, err := q.GetTask(context.Background(), "org-b", a.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("foreign GetTask must be ErrTaskNotFound, got %v", err)
	}
	if _, err := q.RequeueTask(context.Background(), "org-b", a.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("foreign requeue must be ErrTaskNotFound, got %v", err)
	}
	if _, _, err := q.ListTasks(context.Background(), "", "", 10, ""); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("empty org scope must be ErrOrgRequired, got %v", err)
	}
}

func TestTaskStatsMemory(t *testing.T) {
	q := NewQueue()
	// The dead-letter walk happens on an otherwise empty queue so the
	// failing worker cannot grind unrelated tasks through retries first.
	dead := deadLetterThroughEngine(t, q, "agent.run", map[string]any{"organization_id": "org-1", "input": "x"})
	_ = dead
	q.Enqueue("agent.run", map[string]any{"organization_id": "org-1"})
	queued := q.Enqueue("agent.run", map[string]any{"organization_id": "org-1"})
	running := q.Enqueue("agent.run", map[string]any{"organization_id": "org-1"})
	q.MarkStarted(running)
	q.Ack(queued)
	q.Enqueue("agent.run", map[string]any{"organization_id": "org-other"})

	stats, total, err := q.TaskStats(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("TaskStats returned error: %v", err)
	}
	if stats[StatusQueued] != 1 || stats[StatusRunning] != 1 || stats[StatusDeadLetter] != 1 || stats[StatusCompleted] != 1 {
		t.Fatalf("stats = %#v, want one of each status", stats)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4 (the other org's task must not count)", total)
	}
}

func TestTaskListPaginationMemory(t *testing.T) {
	const org = "org-1"
	q := NewQueue()
	ids := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		task := q.Enqueue("agent.run", map[string]any{"organization_id": org, "i": i})
		ids = append(ids, task.ID)
	}
	for i := 0; i < 5; i++ {
		q.Enqueue("agent.run", map[string]any{"organization_id": "org-2", "i": i})
	}

	// Newest enqueue first; walking pages must yield every task exactly once.
	seen := make(map[string]bool)
	cursor := ""
	pages := 0
	for {
		page, next, err := q.ListTasks(context.Background(), org, "", 10, cursor)
		if err != nil {
			t.Fatalf("ListTasks page %d returned error: %v", pages, err)
		}
		pages++
		for _, rec := range page {
			if seen[rec.ID] {
				t.Fatalf("task %s appeared on two pages", rec.ID)
			}
			seen[rec.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("pagination walk exceeded the expected page count")
		}
	}
	if len(seen) != 25 {
		t.Fatalf("pagination walked %d tasks, want 25 (foreign-org tasks excluded)", len(seen))
	}
	if pages != 3 {
		t.Fatalf("expected 3 pages of 10/10/5, got %d", pages)
	}

	// Status filter pages only the matching status.
	filtered, _, err := q.ListTasks(context.Background(), org, StatusQueued, 100, "")
	if err != nil {
		t.Fatalf("filtered ListTasks returned error: %v", err)
	}
	if len(filtered) != 25 {
		t.Fatalf("filtered listing = %d tasks, want 25", len(filtered))
	}

	// Limit normalization: 0 → default 50, over-cap → 200.
	if _, _, err := q.ListTasks(context.Background(), org, "", 0, ""); err != nil {
		t.Fatalf("limit=0 must fall back to the default, got error %v", err)
	}
	page, next, err := q.ListTasks(context.Background(), org, "", 500, "")
	if err != nil {
		t.Fatalf("limit=500 must clamp to 200, got error %v", err)
	}
	if len(page) != 25 || next != "" {
		t.Fatalf("single page must carry everything (25 tasks), got %d tasks next=%q", len(page), next)
	}

	// Malformed cursors are rejected, never silently restarted.
	if _, _, err := q.ListTasks(context.Background(), org, "", 10, "not-a-cursor"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("malformed cursor must be ErrInvalidCursor, got %v", err)
	}
	if _, _, err := q.ListTasks(context.Background(), org, "", 10, "bogus|bogus"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("unparsable cursor must be ErrInvalidCursor, got %v", err)
	}

	// A cursor pointing at a task id that never owned the sequence is rejected.
	page, next, err = q.ListTasks(context.Background(), org, "", 2, "")
	if err != nil || len(page) != 2 {
		t.Fatalf("baseline page failed: %v", err)
	}
	forged := encodeTaskCursor(&TaskRecord{Sequence: page[0].Sequence, ID: "somebody-elses-task"})
	if _, _, err := q.ListTasks(context.Background(), org, "", 2, forged); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("forged cursor must be ErrInvalidCursor, got %v", err)
	}

	// Invalid status filters are rejected before touching the index.
	if _, _, err := q.ListTasks(context.Background(), org, "zombie", 10, ""); !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("invalid status filter must be ErrInvalidStatus, got %v", err)
	}
}

func TestRecordIndexEviction(t *testing.T) {
	q := NewQueue()
	for i := 0; i < DefaultTaskRecordLimit+25; i++ {
		q.Enqueue("agent.run", map[string]any{"organization_id": "org-1", "i": i})
	}
	q.recMu.RLock()
	capped := len(q.records) == DefaultTaskRecordLimit
	oldestKept := len(q.order) > 0 && q.order[0].ID != ""
	q.recMu.RUnlock()
	if !capped {
		t.Fatalf("index must cap at DefaultTaskRecordLimit (%d)", DefaultTaskRecordLimit)
	}
	if !oldestKept {
		t.Fatal("order slice must retain its newest records after eviction")
	}
	list, _, err := q.ListTasks(context.Background(), "org-1", "", 1, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("listing after eviction failed: %v", err)
	}
}

func TestConcurrentProcessAndListRace(t *testing.T) {
	// Exercises the documented lock discipline (recMu → mu, record snapshots
	// outside mu) under the race detector: a worker drives transitions while
	// a reader lists/stats/gets concurrently.
	q := NewQueue()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			task := q.Enqueue("agent.run", map[string]any{"organization_id": "org-1", "i": i, "scratch": make([]string, 0, 1)})
			q.MarkStarted(task)
			task.Payload["result"] = fmt.Sprintf("out-%d", i) // the worker mutates payloads mid-flight
			q.Ack(task)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			_, _, _ = q.ListTasks(context.Background(), "org-1", "", 20, "")
			_, _, _ = q.TaskStats(context.Background(), "org-1")
			_, _ = q.GetTask(context.Background(), "org-1", "missing")
		}
	}()
	wg.Wait()

	stats, total, err := q.TaskStats(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("final TaskStats returned error: %v", err)
	}
	if stats[StatusCompleted] != 30 || total != 30 {
		t.Fatalf("final stats = %#v total %d, want 30 completed", stats, total)
	}
}
