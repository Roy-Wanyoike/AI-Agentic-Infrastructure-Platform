package queue

import (
	"fmt"
	"strings"

	"agentos/internal/config"
)

// DefaultQueueKey is the Redis list key used when REDIS_QUEUE_KEY is unset.
const DefaultQueueKey = "agentos:queue"

// Backend is the storage-agnostic contract every queue implementation shares:
// the in-process *Queue, the Redis-backed *RedisQueue, and therefore the
// config-driven *Queue returned by NewFromConfig (which delegates to one of
// them). It is the compile-time guard keeping the two implementations
// method-compatible and the seam for future backends (e.g. a JetStream work
// queue).
type Backend interface {
	Enqueue(taskType string, payload map[string]any) *Task
	Dequeue() *Task
	Peek() *Task
	Length() int
	MarkStarted(task *Task)
	MarkFailed(task *Task, errMsg string)
	Ack(task *Task)
	Requeue(task *Task)
	// Close releases underlying resources (Redis connections); the in-memory
	// queue's Close is a no-op. Safe to defer in main.
	Close() error
}

var (
	_ Backend = (*Queue)(nil)
	_ Backend = (*RedisQueue)(nil)
)

// NewFromConfig builds the platform task queue from cfg.Queue:
//
//   - AGENTOS_QUEUE=memory (default, also when unset) -> the in-process queue
//     (NewQueue); workers use the HTTP pull endpoint.
//   - AGENTOS_QUEUE=redis -> a redis-backed queue that shares the
//     REDIS_ADDR list REDIS_QUEUE_KEY across every API/worker process. The
//     constructor pings Redis and FAILS when it is unreachable: an explicit
//     redis request that silently degraded to memory would split the task
//     flow (producers enqueueing in memory, consumers reading Redis), so
//     startup fails fast instead.
//
// The concrete *Queue return type is deliberate: every existing call site
// (HTTP handlers, workflows.Engine, scheduler worker, queue.Worker) consumes
// *queue.Queue, so both binaries wire the backend in one line without any
// other file changing. The returned value also satisfies Backend, and
// cmd/worker/main.go passes it straight to queue.NewWorker unchanged.
func NewFromConfig(cfg config.Config) (*Queue, error) {
	switch mode := strings.ToLower(strings.TrimSpace(cfg.Queue.Mode)); mode {
	case "", config.QueueModeMemory:
		return NewQueue(), nil
	case config.QueueModeRedis:
		rq, err := NewRedisQueue(cfg.Queue.Redis.Addr)
		if err != nil {
			return nil, err
		}
		rq.WithKey(cfg.Queue.Redis.QueueKey)
		return NewRedisBackedQueue(rq), nil
	default:
		return nil, fmt.Errorf("queue: invalid AGENTOS_QUEUE mode %q (supported: %q, %q)",
			cfg.Queue.Mode, config.QueueModeMemory, config.QueueModeRedis)
	}
}

// NewRedisBackedQueue returns the platform *Queue whose operations are served
// by rq (Redis) instead of the in-process slice: Enqueue/Dequeue/Length/Peek/
// Requeue hit the Redis list, Ack/MarkStarted/MarkFailed behave like the
// RedisQueue equivalents, and Close closes the Redis client. A nil rq yields
// a plain in-memory queue.
func NewRedisBackedQueue(rq *RedisQueue) *Queue {
	if rq == nil {
		return NewQueue()
	}
	return &Queue{redis: rq}
}

// WithKey overrides the Redis list key (REDIS_QUEUE_KEY). An empty key keeps
// DefaultQueueKey. Call before first use; returns the receiver for chaining.
func (q *RedisQueue) WithKey(key string) *RedisQueue {
	if q == nil || key == "" {
		return q
	}
	q.key = key
	return q
}

// Close closes the underlying Redis client, draining pending commands. Safe
// to defer at startup; the task list itself lives in Redis and survives the
// process.
func (q *RedisQueue) Close() error {
	if q == nil || q.client == nil {
		return nil
	}
	return q.client.Close()
}
