package queue

// redis_ops.go implements the introspection surface (issue #77) for the
// Redis-backed queue, namespaced under the same REDIS_QUEUE_KEY the delivery
// path already uses so one REDIS_QUEUE_KEY configures both:
//
//      <key>                     the work list (existing, untouched)
//      <key>:seq                 global enqueue-sequence counter (INCR)
//      <key>:task:{org}:{id}     one JSON TaskRecord per task
//      <key>:tasks:{org}:{status}  ZSET id -> enqueue sequence, per status
//      <key>:tasks:{org}:any     ZSET id -> enqueue sequence, all statuses
//
// Every write goes through the record+index pipeline; the work list keeps its
// exact existing semantics (RPUSH on enqueue/requeue, LPOP on dequeue) and a
// record-write failure never blocks delivery — introspection degrades, the
// task flow does not (errors on the read paths are surfaced, write-path
// mirror failures are swallowed exactly like the historical `_ = RPush` in
// (*RedisQueue).Requeue).
//
// RETENTION: records are kept without a TTL. A silent expiry of a dead-letter
// record would recreate the exact blindness issue #77 exists to fix, so
// retention is explicit: operators clean up with DEL/UNLINK on the
// <key>:task:* and <key>:tasks:* keyspaces (or a Redis maxmemory policy) when
// tasks should age out. The in-memory backend caps its index
// (DefaultTaskRecordLimit) because dev mode has no operator.
//
// LEGACY TASKS: entries pushed before this file existed decode without an
// organization scope; they are invisible to org-scoped listings (the empty
// org matches no caller) and gain a record with a fresh sequence the first
// time the engine touches them (MarkStarted/MarkFailed/Ack/Requeue).
//
// ATOMICITY: RequeueTask is the only operation that must be check-and-act
// (rejecting a second requeue of the same task), so it runs inside
// WATCH/MULTI on the record key. A racing requeue aborts the transaction;
// the retry re-reads the now-queued status and maps to ErrNotRequeueable —
// exactly one work-list entry survives.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	redis "github.com/redis/go-redis/v9"
)

// requeueWatchRetries bounds the optimistic-locking retry loop for
// RequeueTask. The first retry always observes the winner's write, so the
// loop converges after one iteration in practice.
const requeueWatchRetries = 3

func (q *RedisQueue) taskRecordKey(orgID, taskID string) string {
	return q.key + ":task:" + orgID + ":" + taskID
}

func (q *RedisQueue) taskStatusIndexKey(orgID, status string) string {
	return q.key + ":tasks:" + orgID + ":" + status
}

func (q *RedisQueue) taskAnyIndexKey(orgID string) string {
	return q.key + ":tasks:" + orgID + ":any"
}

func (q *RedisQueue) taskSeqKey() string {
	return q.key + ":seq"
}

// encodeTaskRecord serializes a record for the <key>:task:{org}:{id} string.
func encodeTaskRecord(rec *TaskRecord) (string, error) {
	if rec == nil {
		return "", fmt.Errorf("queue: task record is nil")
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeTaskRecord(raw string) (*TaskRecord, error) {
	var rec TaskRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, fmt.Errorf("queue: undecodable task record: %w", err)
	}
	return &rec, nil
}

// recordEnqueue registers a freshly enqueued task (called by Enqueue after
// the work-list push). Best-effort: delivery already happened, so a mirror
// failure degrades introspection only.
func (q *RedisQueue) recordEnqueue(task *Task) {
	if q == nil || q.client == nil || task == nil {
		return
	}
	ctx := context.Background()
	seq, err := q.client.Incr(ctx, q.taskSeqKey()).Result()
	if err != nil {
		return
	}
	rec := recordFromTask(task, seq)
	encoded, err := encodeTaskRecord(rec)
	if err != nil {
		return
	}
	pipe := q.client.TxPipeline()
	pipe.Set(ctx, q.taskRecordKey(task.OrganizationID, task.ID), encoded, 0)
	pipe.ZAdd(ctx, q.taskStatusIndexKey(task.OrganizationID, rec.Status), redis.Z{Score: float64(seq), Member: task.ID})
	pipe.ZAdd(ctx, q.taskAnyIndexKey(task.OrganizationID), redis.Z{Score: float64(seq), Member: task.ID})
	_, _ = pipe.Exec(ctx)
}

// recordTransition mirrors a state transition onto the task record. Best-effort
// (same contract as recordEnqueue). Tasks without an existing record (legacy
// entries) are registered with a fresh sequence at first touch.
func (q *RedisQueue) recordTransition(task *Task) {
	if q == nil || q.client == nil || task == nil {
		return
	}
	ctx := context.Background()
	recordKey := q.taskRecordKey(task.OrganizationID, task.ID)

	raw, err := q.client.Get(ctx, recordKey).Result()
	switch {
	case errors.Is(err, redis.Nil):
		// First touch of a task that predates the record store: register it
		// with a fresh sequence and full index membership.
		seq, serr := q.client.Incr(ctx, q.taskSeqKey()).Result()
		if serr != nil {
			return
		}
		rec := recordFromTask(task, seq)
		encoded, eerr := encodeTaskRecord(rec)
		if eerr != nil {
			return
		}
		pipe := q.client.TxPipeline()
		pipe.Set(ctx, recordKey, encoded, 0)
		pipe.ZAdd(ctx, q.taskStatusIndexKey(task.OrganizationID, rec.Status), redis.Z{Score: float64(rec.Sequence), Member: task.ID})
		pipe.ZAdd(ctx, q.taskAnyIndexKey(task.OrganizationID), redis.Z{Score: float64(rec.Sequence), Member: task.ID})
		_, _ = pipe.Exec(ctx)
	case err != nil:
		return
	default:
		rec, derr := decodeTaskRecord(raw)
		if derr != nil {
			return
		}
		previousStatus := rec.Status
		rec.Status = task.Status
		rec.Attempts = task.Attempts
		rec.LastError = task.LastError
		rec.UpdatedAt = task.UpdatedAt
		rec.Payload = maps.Clone(task.Payload)

		encoded, eerr := encodeTaskRecord(rec)
		if eerr != nil {
			return
		}
		pipe := q.client.TxPipeline()
		pipe.Set(ctx, recordKey, encoded, 0)
		if previousStatus != rec.Status {
			pipe.ZRem(ctx, q.taskStatusIndexKey(task.OrganizationID, previousStatus), task.ID)
			pipe.ZAdd(ctx, q.taskStatusIndexKey(task.OrganizationID, rec.Status), redis.Z{Score: float64(rec.Sequence), Member: task.ID})
		}
		_, _ = pipe.Exec(ctx)
	}
}

// ListTasks implements Inspector for the Redis backend: one page of the org's
// task records, newest enqueue sequence first.
func (q *RedisQueue) ListTasks(ctx context.Context, orgID, status string, limit int, cursor string) ([]*TaskRecord, string, error) {
	if q == nil || q.client == nil {
		return nil, "", ErrOrgRequired
	}
	limit = NormalizeTaskListLimit(limit)

	indexKey := q.taskAnyIndexKey(orgID)
	if status != "" {
		if !IsValidTaskStatus(status) {
			return nil, "", ErrInvalidStatus
		}
		indexKey = q.taskStatusIndexKey(orgID, status)
	}

	zrange := &redis.ZRangeBy{Max: "+inf", Min: "-inf", Count: int64(limit + 1)}
	if strings.TrimSpace(cursor) != "" {
		cursorSeq, cursorID, err := decodeTaskCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		// Integrity guard: the cursor's id must still own its sequence.
		members, err := q.client.ZRangeByScore(ctx, indexKey, &redis.ZRangeBy{
			Min: strconv.FormatInt(cursorSeq, 10), Max: strconv.FormatInt(cursorSeq, 10),
		}).Result()
		if err != nil {
			return nil, "", err
		}
		owned := false
		for _, member := range members {
			if member == cursorID {
				owned = true
				break
			}
		}
		if !owned {
			return nil, "", fmt.Errorf("%w: cursor does not match its task", ErrInvalidCursor)
		}
		zrange.Min = "(" + strconv.FormatInt(cursorSeq, 10) // exclusive lower bound
	}

	pairs, err := q.client.ZRevRangeByScoreWithScores(ctx, indexKey, zrange).Result()
	if err != nil {
		return nil, "", err
	}

	page := make([]*TaskRecord, 0, limit)
	var stale []any
	next := ""
	for _, pair := range pairs {
		raw, err := q.client.Get(ctx, q.taskRecordKey(orgID, pair.Member.(string))).Result()
		if errors.Is(err, redis.Nil) {
			// The record vanished underneath the index (external cleanup):
			// drop the orphaned index entries lazily and skip the task.
			stale = append(stale, pair.Member)
			continue
		}
		if err != nil {
			return nil, "", err
		}
		rec, err := decodeTaskRecord(raw)
		if err != nil {
			return nil, "", err
		}
		if len(page) == limit {
			next = encodeTaskCursor(page[limit-1])
			break
		}
		page = append(page, cloneRecord(rec))
	}
	if len(stale) > 0 {
		pipe := q.client.TxPipeline()
		pipe.ZRem(ctx, indexKey, stale...)
		if status == "" {
			// The any-set listing removes orphans from every status set too.
			for _, s := range []string{StatusQueued, StatusRunning, StatusDeadLetter, StatusCompleted} {
				pipe.ZRem(ctx, q.taskStatusIndexKey(orgID, s), stale...)
			}
		} else {
			pipe.ZRem(ctx, q.taskAnyIndexKey(orgID), stale...)
		}
		_, _ = pipe.Exec(ctx)
	}
	return page, next, nil
}

// GetTask implements Inspector for the Redis backend.
func (q *RedisQueue) GetTask(ctx context.Context, orgID, taskID string) (*TaskRecord, error) {
	if q == nil || q.client == nil {
		return nil, ErrOrgRequired
	}
	raw, err := q.client.Get(ctx, q.taskRecordKey(orgID, taskID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	rec, err := decodeTaskRecord(raw)
	if err != nil {
		return nil, err
	}
	if rec.OrganizationID != orgID {
		// Defense in depth: the key itself is org-scoped, so a mismatch can
		// only come from tampered data — answer with the same not-found as a
		// foreign id (no existence leak).
		return nil, ErrTaskNotFound
	}
	return cloneRecord(rec), nil
}

// RequeueTask implements Inspector for the Redis backend. The status check,
// the record rewrite and the work-list push all run inside one WATCH/MULTI
// transaction on the record key, so concurrent requeues of the same task
// serialize: exactly one wins, the loser re-reads the queued status and gets
// ErrNotRequeueable.
func (q *RedisQueue) RequeueTask(ctx context.Context, orgID, taskID string) (*TaskRecord, error) {
	if q == nil || q.client == nil {
		return nil, ErrOrgRequired
	}
	recordKey := q.taskRecordKey(orgID, taskID)

	var out *TaskRecord
	var lastErr error
	for attempt := 0; attempt < requeueWatchRetries; attempt++ {
		werr := q.client.Watch(ctx, func(tx *redis.Tx) error {
			raw, err := tx.Get(ctx, recordKey).Result()
			if errors.Is(err, redis.Nil) {
				return ErrTaskNotFound
			}
			if err != nil {
				return err
			}
			rec, err := decodeTaskRecord(raw)
			if err != nil {
				return err
			}
			if rec.OrganizationID != orgID {
				return ErrTaskNotFound
			}
			if rec.Status != StatusDeadLetter {
				return fmt.Errorf("%w: %s", ErrNotRequeueable, rec.Status)
			}

			now := time.Now().UTC()
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
			encodedTask, err := encodeTask(revived)
			if err != nil {
				return err
			}

			updated := *rec
			updated.Status = StatusQueued
			updated.Attempts = 0
			updated.Requeues++
			updated.LastError = ""
			updated.UpdatedAt = now
			updated.RequeuedAt = &now
			encodedRecord, err := encodeTaskRecord(&updated)
			if err != nil {
				return err
			}

			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, recordKey, encodedRecord, 0)
				pipe.ZRem(ctx, q.taskStatusIndexKey(orgID, StatusDeadLetter), rec.ID)
				pipe.ZAdd(ctx, q.taskStatusIndexKey(orgID, StatusQueued), redis.Z{Score: float64(rec.Sequence), Member: rec.ID})
				pipe.RPush(ctx, q.key, encodedTask)
				return nil
			})
			if err != nil {
				return err
			}
			out = &updated
			return nil
		}, recordKey)

		if werr == nil {
			return out, nil
		}
		if errors.Is(werr, redis.TxFailedErr) {
			// Another requeue won the race: re-read inside the next iteration
			// and let its status decide the outcome (queued → 409).
			lastErr = werr
			continue
		}
		if errors.Is(werr, ErrTaskNotFound) || errors.Is(werr, ErrNotRequeueable) {
			return nil, werr
		}
		return nil, werr
	}
	// Exhausted retries without a winner's write becoming visible.
	if lastErr != nil {
		return nil, fmt.Errorf("queue: requeue contention on task %s: %w", taskID, lastErr)
	}
	return nil, ErrNotRequeueable
}

// TaskStats implements Inspector for the Redis backend: exact per-status
// depth from the per-org status indexes (all four keys always present).
func (q *RedisQueue) TaskStats(ctx context.Context, orgID string) (map[string]int64, int64, error) {
	if q == nil || q.client == nil {
		return nil, 0, ErrOrgRequired
	}
	stats := map[string]int64{
		StatusQueued:     0,
		StatusRunning:    0,
		StatusDeadLetter: 0,
		StatusCompleted:  0,
	}
	pipe := q.client.Pipeline()
	cmds := make(map[string]*redis.IntCmd, len(stats))
	for status := range stats {
		cmds[status] = pipe.ZCard(ctx, q.taskStatusIndexKey(orgID, status))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, 0, err
	}
	var total int64
	for status, cmd := range cmds {
		count := cmd.Val()
		stats[status] = count
		total += count
	}
	return stats, total, nil
}
