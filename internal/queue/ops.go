package queue

// ops.go implements the task introspection surface (issue #77): org-scoped
// listing with keyset pagination, per-task detail, dead-letter requeue and
// per-status depth stats. It is part of the queue package because the queue
// itself is the only component that observes every state transition (the
// engine mutates tasks in place during Dequeue→MarkStarted→MarkFailed/Ack).
//
// DUAL-MODE: both backends implement the same four operations through the
// Inspector interface, and the config-driven *Queue returned by NewFromConfig
// dispatches to whichever backend it wraps (mirroring the Backend split in
// backend.go):
//
//   - in-memory: a dedicated RWMutex-guarded index of TaskRecord snapshots
//     (this file). Records are snapshot copies, never the live *Task, so the
//     heavily-mutex'd hot path is untouched and readers can never observe a
//     torn or racing task. The index is capped (DefaultTaskRecordLimit,
//     oldest evicted first) so the zero-infrastructure dev mode cannot leak.
//
//   - redis: per-task record strings + per-org ordering/status ZSETs
//     (redis_ops.go), namespaced under the existing queue list key.
//
// ORG SCOPING: Task.OrganizationID is stamped by Enqueue from the payload's
// "organization_id" (every producer — create-run, workflow engine, scheduler —
// already carries it). Tasks without a scope (e.g. encoded before the field
// existed) are indexed but never match an org-scoped query: they are invisible
// to GET /queue/tasks rather than leaking into some tenant.
//
// PROJECTION HONESTY: records mirror exactly what the engine models. A
// dequeued-but-not-yet-started task still reports "queued" (MarkStarted is
// what flips the state); the dev-only HTTP pull path (/queue/pull) dequeues
// without going through MarkStarted, so pulled tasks legitimately stay
// "queued" in the record — the API process has no visibility into the puller's
// execution and none is invented here.
//
// REQUEUE POLICY (documented per issue #77 "resets attempt bookkeeping"):
// only dead_letter tasks are requeueable (the engine's terminal failure state;
// transient failures already requeue themselves inside MarkFailed+Requeue).
// Requeueing: keeps the task ID and original enqueue sequence, restores the
// original CreatedAt, resets Attempts to 0 and clears LastError, increments
// the Requeues counter, stamps RequeuedAt, sets Status back to queued and
// appends the task to the work list so workers pick it up. Requeueing a task
// in any other state is ErrNotRequeueable (HTTP 409 in the API layer). The
// operation is serialized per backend (in-memory: index write lock; Redis:
// WATCH/MULTI on the record key), so two concurrent requeues of the same task
// yield exactly one work-list entry and one 409.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// TaskRecord is the introspection snapshot of one task. It is a value copy:
// independent of the live *Task the engine mutates, safe to hand to HTTP
// handlers and safe to encode into Redis.
type TaskRecord struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	OrganizationID string `json:"organization_id"`
	Status         string `json:"status"`
	Attempts       int    `json:"attempts"`
	// Requeues counts API-initiated requeues (issue #77); it survives the
	// attempt reset so operators can see a task's retry history at a glance.
	Requeues   int            `json:"requeues"`
	LastError  string         `json:"last_error,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	Sequence   int64          `json:"sequence"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	RequeuedAt *time.Time     `json:"requeued_at,omitempty"`
}

// cloneRecord deep-copies the parts of a record that callers could mutate
// (the payload map). Returned records are detached from the index.
func cloneRecord(rec *TaskRecord) *TaskRecord {
	if rec == nil {
		return nil
	}
	out := *rec
	out.Payload = maps.Clone(rec.Payload)
	out.RequeuedAt = nil
	if rec.RequeuedAt != nil {
		ts := *rec.RequeuedAt
		out.RequeuedAt = &ts
	}
	return &out
}

// recordFromTask snapshots a live task into a fresh record.
func recordFromTask(task *Task, sequence int64) *TaskRecord {
	return &TaskRecord{
		ID:             task.ID,
		Type:           task.Type,
		OrganizationID: task.OrganizationID,
		Status:         task.Status,
		Attempts:       task.Attempts,
		LastError:      task.LastError,
		Payload:        maps.Clone(task.Payload),
		Sequence:       sequence,
		CreatedAt:      task.CreatedAt,
		UpdatedAt:      task.UpdatedAt,
	}
}

// Typed errors surfaced by the introspection surface. The HTTP layer maps
// them onto the documented status codes (404/409/400/500).
var (
	// ErrTaskNotFound covers unknown AND foreign-organization task ids — no
	// existence leak across tenants.
	ErrTaskNotFound = errors.New("queue: task not found")
	// ErrNotRequeueable is returned when the target task is not dead_letter.
	ErrNotRequeueable = errors.New("queue: task is not requeueable from its current status")
	// ErrInvalidCursor is returned when a paging cursor cannot be decoded.
	ErrInvalidCursor = errors.New("queue: invalid cursor")
	// ErrOrgRequired guards every org-scoped read.
	ErrOrgRequired = errors.New("queue: organization scope is required")
	// ErrInvalidStatus is returned for a status filter outside the engine's
	// vocabulary.
	ErrInvalidStatus = errors.New("queue: invalid status filter")
)

// Listing bounds (mirrors the audit trail's contract: default 50, cap 200).
const (
	DefaultTaskListLimit = 50
	MaxTaskListLimit     = 200
	// DefaultTaskRecordLimit bounds the in-memory index (oldest records are
	// evicted first). Redis-mode records are not capped by the queue package;
	// retention there is documented in redis_ops.go.
	DefaultTaskRecordLimit = 10000
)

// NormalizeTaskListLimit clamps a requested page size into
// [1, MaxTaskListLimit]; values <= 0 fall back to DefaultTaskListLimit. The
// queue package — not the HTTP layer — is the point of truth for page bounds.
func NormalizeTaskListLimit(limit int) int {
	if limit <= 0 {
		return DefaultTaskListLimit
	}
	if limit > MaxTaskListLimit {
		return MaxTaskListLimit
	}
	return limit
}

// Inspector is the storage-agnostic introspection contract (issue #77). Both
// the in-memory queue and the Redis-backed queue implement it; the
// compile-time guards below keep them method-compatible the same way Backend
// does for the delivery surface.
type Inspector interface {
	// ListTasks returns one page of the org's task records, newest enqueue
	// first, plus the cursor for the next page ("" when exhausted). An empty
	// status lists every status; anything else must be one of the four
	// engine states.
	ListTasks(ctx context.Context, orgID, status string, limit int, cursor string) ([]*TaskRecord, string, error)
	// GetTask returns one org-scoped task record.
	GetTask(ctx context.Context, orgID, taskID string) (*TaskRecord, error)
	// RequeueTask requeues a dead_letter task (see the policy comment at the
	// top of this file) and returns the updated record.
	RequeueTask(ctx context.Context, orgID, taskID string) (*TaskRecord, error)
	// TaskStats returns the org's task depth per status (all four keys are
	// always present) plus the total number of tracked tasks.
	TaskStats(ctx context.Context, orgID string) (map[string]int64, int64, error)
}

var (
	_ Inspector = (*Queue)(nil)
	_ Inspector = (*RedisQueue)(nil)
)

// ---------------------------------------------------------------------------
// *Queue dispatch (memory vs redis) — the same pattern as the Backend methods.
// ---------------------------------------------------------------------------

// ListTasks implements Inspector for both backends.
func (q *Queue) ListTasks(ctx context.Context, orgID, status string, limit int, cursor string) ([]*TaskRecord, string, error) {
	if q == nil {
		return nil, "", ErrOrgRequired
	}
	if status != "" && !IsValidTaskStatus(status) {
		return nil, "", ErrInvalidStatus
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, "", ErrOrgRequired
	}
	if q.redis != nil {
		return q.redis.ListTasks(ctx, orgID, status, limit, cursor)
	}
	return q.listTasksMemory(orgID, status, limit, cursor)
}

// GetTask implements Inspector for both backends.
func (q *Queue) GetTask(ctx context.Context, orgID, taskID string) (*TaskRecord, error) {
	if q == nil {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if q.redis != nil {
		return q.redis.GetTask(ctx, orgID, taskID)
	}
	return q.getTaskMemory(orgID, taskID)
}

// RequeueTask implements Inspector for both backends.
func (q *Queue) RequeueTask(ctx context.Context, orgID, taskID string) (*TaskRecord, error) {
	if q == nil {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if q.redis != nil {
		return q.redis.RequeueTask(ctx, orgID, taskID)
	}
	return q.requeueTaskMemory(orgID, taskID)
}

// TaskStats implements Inspector for both backends.
func (q *Queue) TaskStats(ctx context.Context, orgID string) (map[string]int64, int64, error) {
	if q == nil {
		return nil, 0, ErrOrgRequired
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, 0, ErrOrgRequired
	}
	if q.redis != nil {
		return q.redis.TaskStats(ctx, orgID)
	}
	return q.taskStatsMemory(orgID)
}

// ---------------------------------------------------------------------------
// In-memory index
// ---------------------------------------------------------------------------

// trackNewTask registers a freshly enqueued task in the index. Called by
// (*Queue).Enqueue AFTER the work-list mutex is released (lock order: recMu
// is never taken while holding mu).
func (q *Queue) trackNewTask(task *Task) {
	if q == nil || task == nil {
		return
	}
	q.recMu.Lock()
	q.seq++
	rec := recordFromTask(task, q.seq)
	q.records[task.ID] = rec
	q.order = append(q.order, rec)
	q.evictOverflowLocked()
	q.recMu.Unlock()
}

// trackTaskSnapshot mirrors a state transition (MarkStarted/MarkFailed/Ack/
// Requeue) onto the index. Unknown tasks are registered defensively so a
// caller that drives the engine by hand still gets introspected.
func (q *Queue) trackTaskSnapshot(task *Task) {
	if q == nil || task == nil {
		return
	}
	q.recMu.Lock()
	existing, ok := q.records[task.ID]
	if !ok {
		q.seq++
		q.records[task.ID] = recordFromTask(task, q.seq)
		q.order = append(q.order, q.records[task.ID])
		q.evictOverflowLocked()
	} else {
		existing.Status = task.Status
		existing.Attempts = task.Attempts
		existing.LastError = task.LastError
		existing.UpdatedAt = task.UpdatedAt
		existing.Payload = maps.Clone(task.Payload)
	}
	q.recMu.Unlock()
}

// evictOverflowLocked trims the index to DefaultTaskRecordLimit, oldest
// sequence first. Caller holds recMu.Lock. Eviction is a dev-mode memory
// guard: the oldest records are the least actionable (they predate every
// newer dead letter), and the cap is documented on the constant.
func (q *Queue) evictOverflowLocked() {
	overflow := len(q.order) - DefaultTaskRecordLimit
	if overflow <= 0 {
		return
	}
	for i := 0; i < overflow; i++ {
		oldest := q.order[i]
		if current := q.records[oldest.ID]; current == oldest {
			delete(q.records, oldest.ID)
		}
	}
	copy(q.order, q.order[overflow:])
	tail := len(q.order) - overflow
	for i := tail; i < len(q.order); i++ {
		q.order[i] = nil // drop the reference so evicted payloads can be GCed
	}
	q.order = q.order[:tail]
}

func (q *Queue) listTasksMemory(orgID, status string, limit int, cursor string) ([]*TaskRecord, string, error) {
	limit = NormalizeTaskListLimit(limit)

	var cursorSeq int64
	var cursorID string
	if strings.TrimSpace(cursor) != "" {
		seq, id, err := decodeTaskCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		cursorSeq, cursorID = seq, id
	}

	q.recMu.RLock()
	defer q.recMu.RUnlock()

	page := make([]*TaskRecord, 0, limit)
	next := ""
	// Walk the append-ordered index backwards: ascending Sequence in the
	// slice means reverse iteration is the (Sequence DESC) keyset order.
	for i := len(q.order) - 1; i >= 0 && len(page) <= limit; i-- {
		rec := q.order[i]
		if rec == nil || rec.OrganizationID != orgID {
			continue
		}
		if status != "" && rec.Status != status {
			continue
		}
		if cursorSeq > 0 {
			if rec.Sequence > cursorSeq {
				continue // already returned on an earlier page
			}
			if rec.Sequence == cursorSeq {
				if rec.ID != cursorID {
					return nil, "", fmt.Errorf("%w: cursor does not match its task", ErrInvalidCursor)
				}
				continue // exclusive: the cursor's own task was already returned
			}
		}
		if len(page) == limit {
			// One extra match beyond the page proves more exist; the next
			// cursor is the last task actually returned.
			next = encodeTaskCursor(page[limit-1])
			break
		}
		page = append(page, cloneRecord(rec))
	}
	return page, next, nil
}

func (q *Queue) getTaskMemory(orgID, taskID string) (*TaskRecord, error) {
	q.recMu.RLock()
	defer q.recMu.RUnlock()
	rec, ok := q.records[taskID]
	if !ok || rec.OrganizationID != orgID {
		return nil, ErrTaskNotFound
	}
	return cloneRecord(rec), nil
}

func (q *Queue) requeueTaskMemory(orgID, taskID string) (*TaskRecord, error) {
	// recMu.Lock spans the whole check-and-act so two concurrent requeues of
	// the same task serialize (first wins, second observes StatusQueued and
	// gets ErrNotRequeueable). q.mu is taken INSIDE per the documented lock
	// order (recMu → mu); no other path holds mu while acquiring recMu.
	q.recMu.Lock()
	defer q.recMu.Unlock()

	rec, ok := q.records[taskID]
	if !ok || rec.OrganizationID != orgID {
		return nil, ErrTaskNotFound
	}
	if rec.Status != StatusDeadLetter {
		return nil, fmt.Errorf("%w: %s", ErrNotRequeueable, rec.Status)
	}

	now := time.Now().UTC()
	// Exactly one work-list entry for the task id: drop stale entries (a
	// dead-lettered task can linger in the slice when callers drive
	// MarkStarted/MarkFailed without dequeueing) and append the fresh copy.
	q.mu.Lock()
	for i := len(q.tasks) - 1; i >= 0; i-- {
		if q.tasks[i].ID == taskID {
			q.tasks = append(q.tasks[:i], q.tasks[i+1:]...)
		}
	}
	revived := &Task{
		ID:             rec.ID,
		Type:           rec.Type,
		Payload:        maps.Clone(rec.Payload),
		Status:         StatusQueued,
		Attempts:       0,
		CreatedAt:      rec.CreatedAt, // preserve the original enqueue time
		UpdatedAt:      now,
		OrganizationID: rec.OrganizationID,
	}
	q.tasks = append(q.tasks, revived)
	q.mu.Unlock()

	rec.Status = StatusQueued
	rec.Attempts = 0
	rec.Requeues++
	rec.LastError = ""
	rec.UpdatedAt = now
	rec.RequeuedAt = &now
	return cloneRecord(rec), nil
}

func (q *Queue) taskStatsMemory(orgID string) (map[string]int64, int64, error) {
	q.recMu.RLock()
	defer q.recMu.RUnlock()
	stats := map[string]int64{
		StatusQueued:     0,
		StatusRunning:    0,
		StatusDeadLetter: 0,
		StatusCompleted:  0,
	}
	var total int64
	for _, rec := range q.order {
		if rec == nil || rec.OrganizationID != orgID {
			continue
		}
		if _, known := stats[rec.Status]; !known {
			// Defensive: an unknown status (e.g. a caller-driven state) is
			// counted in the total but never fakes a known bucket.
			total++
			continue
		}
		stats[rec.Status]++
		total++
	}
	return stats, total, nil
}

// ---------------------------------------------------------------------------
// Keyset cursor (base64url("sequence|id"), mirroring the audit trail's
// created_at|id shape). The sequence is the stable sort key; the id segment
// is an integrity guard so a cursor can never be pointed at a different
// task's position.
// ---------------------------------------------------------------------------

func encodeTaskCursor(rec *TaskRecord) string {
	if rec == nil {
		return ""
	}
	raw := strconv.FormatInt(rec.Sequence, 10) + "|" + rec.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeTaskCursor(cursor string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return 0, "", fmt.Errorf("%w: not base64url", ErrInvalidCursor)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", fmt.Errorf("%w: missing id segment", ErrInvalidCursor)
	}
	seq, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || seq <= 0 {
		return 0, "", fmt.Errorf("%w: unparsable sequence", ErrInvalidCursor)
	}
	return seq, parts[1], nil
}
