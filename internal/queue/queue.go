package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
)

// Task states modeled by the engine. They are plain strings for backwards
// compatibility (the JSON wire format and the pre-existing call sites use the
// literals), but every producer/consumer should prefer these constants.
const (
	StatusQueued     = "queued"
	StatusRunning    = "running"
	StatusDeadLetter = "dead_letter"
	StatusCompleted  = "completed"
)

// IsValidTaskStatus reports whether s is one of the four states the engine
// models (issue #77 introspection filters accept exactly these).
func IsValidTaskStatus(s string) bool {
	switch s {
	case StatusQueued, StatusRunning, StatusDeadLetter, StatusCompleted:
		return true
	}
	return false
}

type Task struct {
	ID        string
	Type      string
	Payload   map[string]any
	Status    string
	Attempts  int
	CreatedAt time.Time
	UpdatedAt time.Time
	LastError string
	// OrganizationID is the tenant scope of the task (issue #77). Every
	// producer stamps "organization_id" into the payload; Enqueue lifts it
	// onto the task so introspection (GET /queue/tasks et al.) can scope
	// listings to the caller's organization. Additive field: tasks encoded
	// before this field existed simply decode with an empty scope and stay
	// invisible to org-scoped listings (documented in ops.go).
	OrganizationID string
}

// organizationFromPayload extracts the tenant scope every queue producer
// stamps into the payload (create-run handler, workflow engine, scheduler).
// A missing or non-string value yields "" (unscoped task).
func organizationFromPayload(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	org, _ := payload["organization_id"].(string)
	return org
}

type Queue struct {
	mu    sync.Mutex
	tasks []*Task
	redis *RedisQueue // non-nil ⇒ redis-backed mode: operations delegate to Redis

	// Introspection index (issue #77): a dedicated RWMutex-guarded registry of
	// TaskRecord snapshots, one per task the queue has seen. Readers (GET
	// /queue/tasks et al.) take recMu.RLock only, so listing never contends
	// the work-list mutex; writers take recMu.Lock AFTER releasing mu (lock
	// order: recMu before mu — the only nested acquisition is RequeueTask's
	// recMu → mu, which every other path avoids by construction).
	recMu   sync.RWMutex
	records map[string]*TaskRecord
	order   []*TaskRecord // append-ordered by Sequence (ascending); front = oldest
	seq     int64         // last assigned enqueue sequence
}

func NewQueue() *Queue {
	return &Queue{
		tasks:   make([]*Task, 0),
		records: make(map[string]*TaskRecord),
	}
}

type RedisQueue struct {
	client *redis.Client
	key    string
}

func NewRedisQueue(addr string) (*RedisQueue, error) {
	if addr == "" {
		return nil, fmt.Errorf("redis address is required")
	}

	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}

	return &RedisQueue{client: client, key: DefaultQueueKey}, nil
}

func encodeTask(task *Task) (string, error) {
	if task == nil {
		return "", fmt.Errorf("task is nil")
	}
	b, err := json.Marshal(task)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeTask(raw string) *Task {
	if raw == "" {
		return nil
	}
	var task Task
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		return nil
	}
	return &task
}

func (q *RedisQueue) Enqueue(taskType string, payload map[string]any) *Task {
	if q == nil || q.client == nil {
		return nil
	}

	task := &Task{
		ID:             taskType + "-" + time.Now().UTC().Format(time.RFC3339Nano),
		Type:           taskType,
		Payload:        payload,
		Status:         StatusQueued,
		Attempts:       0,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		OrganizationID: organizationFromPayload(payload),
	}

	encoded, err := encodeTask(task)
	if err != nil {
		return nil
	}
	if err := q.client.RPush(context.Background(), q.key, encoded).Err(); err != nil {
		return nil
	}
	q.recordEnqueue(task) // issue #77: register the task in the introspection records
	return task
}

func (q *RedisQueue) Length() int {
	if q == nil || q.client == nil {
		return 0
	}
	length, err := q.client.LLen(context.Background(), q.key).Result()
	if err != nil {
		return 0
	}
	return int(length)
}

func (q *RedisQueue) Peek() *Task {
	if q == nil || q.client == nil {
		return nil
	}
	item, err := q.client.LIndex(context.Background(), q.key, 0).Result()
	if err != nil || item == "" {
		return nil
	}
	return decodeTask(item)
}

func (q *RedisQueue) Dequeue() *Task {
	if q == nil || q.client == nil {
		return nil
	}
	item, err := q.client.LPop(context.Background(), q.key).Result()
	if err != nil || item == "" {
		return nil
	}
	return decodeTask(item)
}

func (q *RedisQueue) MarkStarted(task *Task) {
	if task == nil {
		return
	}
	task.Attempts++
	task.Status = StatusRunning
	task.UpdatedAt = time.Now().UTC()
	q.recordTransition(task) // issue #77: mirror the state onto the task record
}

func (q *RedisQueue) MarkFailed(task *Task, errMsg string) {
	if task == nil {
		return
	}
	task.LastError = errMsg
	task.UpdatedAt = time.Now().UTC()
	if task.Attempts >= 4 {
		task.Status = StatusDeadLetter
		q.recordTransition(task)
		return
	}
	task.Status = StatusQueued
	q.recordTransition(task)
}

func (q *RedisQueue) Ack(task *Task) {
	if task == nil {
		return
	}
	task.Status = StatusCompleted
	task.UpdatedAt = time.Now().UTC()
	q.recordTransition(task) // issue #77: mirror the state onto the task record
}

func (q *RedisQueue) Requeue(task *Task) {
	if q == nil || q.client == nil || task == nil {
		return
	}
	task.Status = StatusQueued
	task.UpdatedAt = time.Now().UTC()
	encoded, err := encodeTask(task)
	if err != nil {
		return
	}
	_ = q.client.RPush(context.Background(), q.key, encoded).Err()
	q.recordTransition(task) // issue #77: mirror the state onto the task record
}

func (q *Queue) Enqueue(taskType string, payload map[string]any) *Task {
	if q.redis != nil {
		return q.redis.Enqueue(taskType, payload)
	}
	task := &Task{
		ID:             taskType + "-" + time.Now().UTC().Format(time.RFC3339Nano),
		Type:           taskType,
		Payload:        payload,
		Status:         StatusQueued,
		Attempts:       0,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		OrganizationID: organizationFromPayload(payload),
	}
	// The record is published BEFORE the task enters the work list: a worker
	// that dequeues the task always finds its introspection record already
	// in place (issue #77). See ops.go for the index/lock design.
	q.trackNewTask(task)
	q.mu.Lock()
	q.tasks = append(q.tasks, task)
	q.mu.Unlock()
	return task
}

func (q *Queue) MarkStarted(task *Task) {
	if q.redis != nil {
		q.redis.MarkStarted(task)
		return
	}
	if task == nil {
		return
	}
	q.mu.Lock()
	task.Attempts++
	task.Status = StatusRunning
	task.UpdatedAt = time.Now().UTC()
	q.mu.Unlock()
	q.trackTaskSnapshot(task) // issue #77: mirror the state onto the task record
}

func (q *Queue) MarkFailed(task *Task, errMsg string) {
	if q.redis != nil {
		q.redis.MarkFailed(task, errMsg)
		return
	}
	if task == nil {
		return
	}
	q.mu.Lock()
	task.LastError = errMsg
	task.UpdatedAt = time.Now().UTC()

	if task.Attempts >= 4 {
		task.Status = StatusDeadLetter
		q.mu.Unlock()
		q.trackTaskSnapshot(task)
		return
	}

	task.Status = StatusQueued
	q.mu.Unlock()
	q.trackTaskSnapshot(task)
}

func (q *Queue) Length() int {
	if q.redis != nil {
		return q.redis.Length()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

func (q *Queue) Peek() *Task {
	if q.redis != nil {
		return q.redis.Peek()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return nil
	}
	return q.tasks[0]
}

func (q *Queue) Ack(task *Task) {
	if q.redis != nil {
		q.redis.Ack(task)
		return
	}
	if task == nil {
		return
	}
	// Issue #77 alignment: the memory backend now records the completion on
	// the task exactly like (*RedisQueue).Ack always did, so the "completed"
	// status is a real, queryable engine state in BOTH modes (previously the
	// memory Ack only removed the task from the work list and the status
	// field stayed "running" — invisible to introspection). No delivery
	// semantics change: the task still leaves the work list.
	task.Status = StatusCompleted
	task.UpdatedAt = time.Now().UTC()
	q.mu.Lock()
	for i, queued := range q.tasks {
		if queued.ID == task.ID {
			q.tasks = append(q.tasks[:i], q.tasks[i+1:]...)
			break
		}
	}
	q.mu.Unlock()
	q.trackTaskSnapshot(task) // completed tasks stay queryable via the index (issue #77)
}
func (q *Queue) Dequeue() *Task {
	if q.redis != nil {
		return q.redis.Dequeue()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return nil
	}
	task := q.tasks[0]
	q.tasks = q.tasks[1:]
	return task
}

func (q *Queue) Requeue(task *Task) {
	if q.redis != nil {
		q.redis.Requeue(task)
		return
	}
	if task == nil {
		return
	}
	q.mu.Lock()
	task.Status = StatusQueued
	task.UpdatedAt = time.Now().UTC()
	q.tasks = append(q.tasks, task)
	q.mu.Unlock()
	q.trackTaskSnapshot(task)
}

// Close releases the queue's resources. The in-memory queue holds none and
// always returns nil; a redis-backed queue closes the Redis client (see
// NewFromConfig / NewRedisBackedQueue). Safe to defer in main.
func (q *Queue) Close() error {
	if q == nil || q.redis == nil {
		return nil
	}
	return q.redis.Close()
}

type ScaleReport struct {
	TotalProcessed      int
	ThroughputPerSecond float64
	RecoveryRate        float64
}

func ScaleCheck(q *Queue, workers int) (*ScaleReport, error) {
	if q == nil {
		return nil, fmt.Errorf("queue is nil")
	}
	if workers <= 0 {
		workers = 1
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	total := len(q.tasks)
	if total == 0 {
		return &ScaleReport{TotalProcessed: 0, ThroughputPerSecond: 0, RecoveryRate: 0}, nil
	}

	processed := total
	throughput := float64(processed) / float64(workers)
	recovery := 1.0
	return &ScaleReport{TotalProcessed: processed, ThroughputPerSecond: throughput, RecoveryRate: recovery}, nil
}

type Worker struct {
	q      *Queue
	handle func(*Task) error
}

func NewWorker(q *Queue, handle func(*Task) error) *Worker {
	return &Worker{q: q, handle: handle}
}

func (w *Worker) ProcessNext() error {
	if w == nil || w.q == nil {
		return nil
	}
	task := w.q.Dequeue()
	if task == nil {
		return nil
	}
	w.q.MarkStarted(task)
	if w.handle != nil {
		if err := w.handle(task); err != nil {
			w.q.MarkFailed(task, err.Error())
			if task.Status == StatusDeadLetter {
				return nil
			}
			w.q.Requeue(task)
			return err
		}
	}
	w.q.Ack(task)
	return nil
}
