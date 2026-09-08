// Queue operations (issue #77 backend, surfaced by issue #81 Ops view).
//
// Pinned contract (issue #77 — served under /v1 + /api/v1):
//
//   GET  /queue/stats                      -> depth by status
//   GET  /queue/tasks?status=&limit=&cursor= -> {"tasks":[…],"next_cursor"}
//   GET  /queue/tasks/{id}                 -> one task
//   POST /queue/tasks/{id}/requeue         -> OWNER/ADMIN; 409 when the task
//                                             is not in the dead-letter state
//
// The queue Task domain type (internal/queue) is {ID, Type, Payload, Status,
// Attempts, CreatedAt, UpdatedAt, LastError} with statuses queued / running /
// completed / dead_letter. The handlers are being built in parallel (wave-8
// queue-ops track), so every normalizer here accepts both PascalCase and
// snake_case projections, and the view treats a 404 from the whole /queue/*
// surface as "queue ops API not available on this deployment" (zero-infra
// mode), never as a crash.

import { apiFetch } from './client'
import { asNumber, asRecord, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export const QUEUE_TASK_STATUSES = ['queued', 'running', 'completed', 'dead_letter'] as const
export type QueueTaskStatus = (typeof QUEUE_TASK_STATUSES)[number]

export type QueueTask = {
  id: string
  type: string
  payload: Record<string, unknown> | null
  status: string
  attempts: number
  createdAt?: string
  updatedAt?: string
  lastError?: string
}

/**
 * Depth-by-status snapshot. Unknown keys from the backend are preserved in
 * `extra` so a new status never renders as missing data.
 */
export type QueueStats = {
  byStatus: Record<string, number>
  queued: number
  running: number
  completed: number
  deadLetter: number
  total: number
}

export type ListQueueTasksInput = {
  status?: string
  limit?: number
  cursor?: string
}

export type ListQueueTasksResult = {
  tasks: QueueTask[]
  nextCursor: string
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

export function normalizeQueueTask(raw: unknown): QueueTask {
  const payload = pickField(raw, 'payload')
  return {
    id: asString(pickField(raw, 'id', 'taskId')) ?? '',
    type: asString(pickField(raw, 'type', 'taskType')) ?? '',
    payload: asRecord(payload),
    status: (asString(pickField(raw, 'status', 'state')) ?? 'unknown').toLowerCase(),
    attempts: asNumber(pickField(raw, 'attempts', 'attemptCount')) ?? 0,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
    lastError: asString(pickField(raw, 'lastError', 'last_error', 'error')),
  }
}

/**
 * Depth-by-status: the pinned contract says "depth by status" without pinning
 * the envelope key, so the normalizer accepts the numeric status record from
 * any of the plausible wrappers (stats/depth/by_status/statuses/counts) or
 * from the top level itself, and always falls back to zeros — never NaN.
 */
export function normalizeQueueStats(raw: unknown): QueueStats {
  const wrappers = ['stats', 'depth', 'by_status', 'byStatus', 'statuses', 'counts']
  let source: Record<string, unknown> | null = null
  for (const key of wrappers) {
    const candidate = asRecord(pickField(raw, key))
    if (candidate) {
      source = candidate
      break
    }
  }
  if (!source) source = asRecord(raw)

  const byStatus: Record<string, number> = {}
  let total = 0
  if (source) {
    for (const [key, value] of Object.entries(source)) {
      const parsed = asNumber(value)
      if (parsed === undefined) continue
      byStatus[key.toLowerCase()] = parsed
      total += parsed
    }
  }
  const read = (...names: string[]): number => {
    for (const name of names) {
      const parsed = asNumber(pickField(byStatus, name))
      if (parsed !== undefined) return parsed
    }
    return 0
  }
  return {
    byStatus,
    queued: read('queued', 'queue'),
    running: read('running', 'in_flight', 'inflight'),
    completed: read('completed', 'complete', 'done'),
    deadLetter: read('dead_letter', 'deadletter', 'dead-letter', 'dlq'),
    total,
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

export async function getQueueStats(): Promise<QueueStats> {
  return normalizeQueueStats(await apiFetch<unknown>('/queue/stats'))
}

export async function listQueueTasks(input: ListQueueTasksInput = {}): Promise<ListQueueTasksResult> {
  const params = new URLSearchParams()
  if (input.status) params.set('status', input.status)
  if (input.limit && input.limit > 0) params.set('limit', String(input.limit))
  if (input.cursor) params.set('cursor', input.cursor)
  const query = params.toString()
  const raw = await apiFetch<unknown>(`/queue/tasks${query ? `?${query}` : ''}`)
  const list = pickField(raw, 'tasks')
  return {
    tasks: (Array.isArray(list) ? list : []).map(normalizeQueueTask),
    nextCursor: asString(pickField(raw, 'nextCursor', 'next_cursor')) ?? '',
  }
}

export async function getQueueTask(id: string): Promise<QueueTask> {
  const raw = await apiFetch<unknown>(`/queue/tasks/${encodeURIComponent(id)}`)
  return normalizeQueueTask(pickField(raw, 'task') ?? raw)
}

/** OWNER/ADMIN only; the API answers 409 when the task is not dead-lettered. */
export async function requeueTask(id: string): Promise<QueueTask> {
  const raw = await apiFetch<unknown>(`/queue/tasks/${encodeURIComponent(id)}/requeue`, { method: 'POST' })
  return normalizeQueueTask(pickField(raw, 'task') ?? raw)
}
