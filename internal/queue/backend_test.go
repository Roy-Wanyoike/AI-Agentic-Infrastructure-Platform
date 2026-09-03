package queue

import (
	"errors"
	"strings"
	"testing"

	"agentos/internal/config"
	"github.com/alicebob/miniredis/v2"
)

func redisCfg(t *testing.T, addr, key string) config.Config {
	t.Helper()
	return config.Config{Queue: config.QueueConfig{
		Mode: config.QueueModeRedis,
		Redis: config.RedisConfig{
			Addr:     addr,
			QueueKey: key,
		},
	}}
}

func TestNewFromConfigDefaultsToMemory(t *testing.T) {
	// Zero-value config: Mode unset behaves exactly like AGENTOS_QUEUE unset.
	q, err := NewFromConfig(config.Config{})
	if err != nil {
		t.Fatalf("NewFromConfig with unset mode returned error: %v", err)
	}
	if q == nil {
		t.Fatal("NewFromConfig returned nil queue")
	}
	if q.redis != nil {
		t.Error("unset mode must select the in-memory queue")
	}

	// Explicit memory mode.
	q, err = NewFromConfig(config.Config{Queue: config.QueueConfig{Mode: config.QueueModeMemory}})
	if err != nil {
		t.Fatalf("NewFromConfig(memory) returned error: %v", err)
	}
	if q.redis != nil {
		t.Error("explicit memory mode must select the in-memory queue")
	}

	// Round-trip one task through the queue interface in memory mode.
	var bq Backend = q
	task := bq.Enqueue("agent.run", map[string]any{"agent_id": "agent-1"})
	if task == nil || bq.Length() != 1 {
		t.Fatalf("Enqueue failed: task=%v length=%d", task, bq.Length())
	}
	if got := bq.Dequeue(); got == nil || got.ID != task.ID || got.Payload["agent_id"] != "agent-1" {
		t.Fatalf("Dequeue returned %#v, want the enqueued task", got)
	}
	if bq.Length() != 0 {
		t.Errorf("queue should be drained, length=%d", bq.Length())
	}
	if err := bq.Close(); err != nil {
		t.Errorf("memory queue Close should be a no-op, got %v", err)
	}
}

func TestNewFromConfigRedisModeRoundTrip(t *testing.T) {
	server := miniredis.RunT(t)
	const key = "agentos:test:tasks"
	cfg := redisCfg(t, server.Addr(), key)

	var bq Backend
	bq, err := NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromConfig(redis) returned error: %v", err)
	}
	backend, ok := bq.(*Queue)
	if !ok {
		t.Fatalf("NewFromConfig should return *Queue, got %T", bq)
	}
	if backend.redis == nil {
		t.Fatal("redis mode must back the queue with Redis")
	}

	// Round-trip one task through the queue interface against miniredis.
	sent := bq.Enqueue("agent.run", map[string]any{"run_id": "run-1", "input": "hello"})
	if sent == nil {
		t.Fatal("Enqueue returned nil task")
	}
	if got := bq.Length(); got != 1 {
		t.Fatalf("queue length = %d, want 1", got)
	}
	if !server.Exists(key) {
		t.Fatalf("task must be stored under REDIS_QUEUE_KEY %q", key)
	}
	if head := bq.Peek(); head == nil || head.ID != sent.ID {
		t.Fatalf("Peek = %#v, want the queued task", head)
	}

	got := bq.Dequeue()
	if got == nil {
		t.Fatal("Dequeue returned nil")
	}
	if got.ID != sent.ID || got.Type != "agent.run" {
		t.Errorf("dequeued task = id %q type %q, want id %q type agent.run", got.ID, got.Type, sent.ID)
	}
	if got.Payload["run_id"] != "run-1" || got.Payload["input"] != "hello" {
		t.Errorf("payload did not survive the redis round trip: %#v", got.Payload)
	}

	bq.MarkStarted(got)
	if got.Status != "running" || got.Attempts != 1 {
		t.Errorf("MarkStarted: status=%q attempts=%d, want running/1", got.Status, got.Attempts)
	}
	bq.Ack(got)
	if got.Status != "completed" {
		t.Errorf("Ack: status=%q, want completed", got.Status)
	}
	if bq.Length() != 0 {
		t.Errorf("queue length = %d after dequeue, want 0", bq.Length())
	}
	if err := bq.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

func TestNewFromConfigRedisModeDefaultsToStandardKey(t *testing.T) {
	server := miniredis.RunT(t)
	bq, err := NewFromConfig(redisCfg(t, server.Addr(), ""))
	if err != nil {
		t.Fatalf("NewFromConfig returned error: %v", err)
	}
	defer bq.Close()
	if bq.Enqueue("agent.run", map[string]any{"run_id": "run-2"}) == nil {
		t.Fatal("Enqueue returned nil task")
	}
	if !server.Exists(DefaultQueueKey) {
		t.Errorf("unset REDIS_QUEUE_KEY must default to %q", DefaultQueueKey)
	}
}

func TestNewFromConfigRedisModeNormalizesMode(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := redisCfg(t, server.Addr(), "")
	cfg.Queue.Mode = "  Redis " // operator-friendly: trim + case-insensitive
	bq, err := NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("mixed-case mode should be accepted, got error: %v", err)
	}
	defer bq.Close()
	if bq.redis == nil {
		t.Error("redis mode (normalized) must back the queue with Redis")
	}
}

func TestNewFromConfigRejectsInvalidMode(t *testing.T) {
	cfg := config.Config{Queue: config.QueueConfig{Mode: "beanstalk"}}
	q, err := NewFromConfig(cfg)
	if err == nil {
		t.Fatal("invalid mode must return an error")
	}
	if q != nil {
		t.Errorf("invalid mode must not return a queue, got %#v", q)
	}
	if !strings.Contains(err.Error(), "memory") || !strings.Contains(err.Error(), "redis") {
		t.Errorf("error should name the supported modes, got: %v", err)
	}
}

func TestNewFromConfigRedisModeRequiresAddr(t *testing.T) {
	bq, err := NewFromConfig(redisCfg(t, "", ""))
	if err == nil {
		t.Fatal("redis mode without REDIS_ADDR must fail fast")
	}
	if bq != nil {
		t.Errorf("no queue should be returned, got %#v", bq)
	}
	if !strings.Contains(err.Error(), "redis address is required") {
		t.Errorf("error should be actionable, got: %v", err)
	}
}

func TestNewFromConfigRedisModeFailsFastWhenRedisDown(t *testing.T) {
	dead := miniredis.RunT(t)
	addr := dead.Addr()
	dead.Close() // free the port: nothing listens there anymore

	bq, err := NewFromConfig(redisCfg(t, addr, ""))
	if err == nil {
		t.Fatal("unreachable Redis must fail the constructor (no silent memory fallback)")
	}
	if bq != nil {
		t.Errorf("no queue should be returned, got %#v", bq)
	}
}

func TestWorkerProcessesRedisBackedTask(t *testing.T) {
	server := miniredis.RunT(t)
	bq, err := NewFromConfig(redisCfg(t, server.Addr(), ""))
	if err != nil {
		t.Fatalf("NewFromConfig returned error: %v", err)
	}
	defer bq.Close()

	var processed []*Task
	worker := NewWorker(bq, func(task *Task) error {
		processed = append(processed, task)
		return nil
	})

	bq.Enqueue("agent.run", map[string]any{"run_id": "run-9"})
	if err := worker.ProcessNext(); err != nil {
		t.Fatalf("ProcessNext returned error: %v", err)
	}
	if len(processed) != 1 || processed[0].Payload["run_id"] != "run-9" {
		t.Fatalf("handler received %#v, want the redis-backed task", processed)
	}
	if processed[0].Status != "completed" {
		t.Errorf("acknowledged task status = %q, want completed", processed[0].Status)
	}
	if bq.Length() != 0 {
		t.Errorf("queue length = %d after processing, want 0", bq.Length())
	}
}

func TestWorkerRetriesFailedTaskThroughRedis(t *testing.T) {
	server := miniredis.RunT(t)
	bq, err := NewFromConfig(redisCfg(t, server.Addr(), ""))
	if err != nil {
		t.Fatalf("NewFromConfig returned error: %v", err)
	}
	defer bq.Close()

	worker := NewWorker(bq, func(task *Task) error {
		return errTransient
	})

	sent := bq.Enqueue("agent.run", map[string]any{"run_id": "run-10"})
	if err := worker.ProcessNext(); err == nil {
		t.Fatal("ProcessNext must surface the handler error")
	}
	if bq.Length() != 1 {
		t.Fatalf("failed task must be requeued into Redis, length=%d", bq.Length())
	}
	// In redis mode Dequeue hands back a decoded copy, so re-read the
	// requeued task instead of asserting on the enqueue-time pointer.
	got := bq.Dequeue()
	if got == nil || got.ID != sent.ID {
		t.Fatalf("requeued task mismatch: got %#v want id %q", got, sent.ID)
	}
	if got.Status != "queued" || got.Attempts != 1 {
		t.Errorf("failed task should be marked for retry: status=%q attempts=%d", got.Status, got.Attempts)
	}
}

// errTransient is the handler failure used by the retry test.
var errTransient = errors.New("transient failure")
