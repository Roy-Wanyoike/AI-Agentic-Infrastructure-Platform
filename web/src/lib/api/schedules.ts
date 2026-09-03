// Schedules resource (track 2-f contract): list, create, pause, resume.
//
// - GET    /schedules               -> {"schedules":[…]}
// - POST   /schedules/create        -> {"schedule":{…}}        (422 VALIDATION_ERROR on bad cron/kind)
// - POST   /schedules/{id}/pause    -> {"schedule":{…}}
// - POST   /schedules/{id}/resume   -> {"schedule":{…}}
//
// Schedule JSON (snake_case, additive last_fired_at/updated_at beyond the
// wave-2 contract): id, agent_id, input, kind (once|recurring|cron), run_at,
// interval_seconds, cron_expr, timezone, status (active|paused|completed),
// next_run_at, last_run_id, last_fired_at, created_at, updated_at.

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type Schedule = {
  id: string
  agentId: string
  input: string
  kind: string
  runAt?: string | null
  intervalSeconds?: number
  cronExpr?: string
  timezone?: string
  status: string
  nextRunAt?: string | null
  lastRunId?: string
  lastFiredAt?: string | null
  createdAt?: string
  updatedAt?: string
}

export type CreateScheduleInput = {
  agent_id: string
  input: string
  kind: string
  run_at?: string
  interval_seconds?: number
  cron_expr?: string
  timezone?: string
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeSchedule(raw: unknown): Schedule {
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    agentId: asString(pickField(raw, 'agentId', 'agent_id')) ?? '',
    input: asString(pickField(raw, 'input')) ?? '',
    kind: (asString(pickField(raw, 'kind')) ?? 'unknown').toLowerCase(),
    runAt: asString(pickField(raw, 'runAt', 'run_at')) ?? null,
    intervalSeconds: asNumber(pickField(raw, 'intervalSeconds', 'interval_seconds')),
    cronExpr: asString(pickField(raw, 'cronExpr', 'cron_expr')),
    timezone: asString(pickField(raw, 'timezone')),
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    nextRunAt: asString(pickField(raw, 'nextRunAt', 'next_run_at')) ?? null,
    lastRunId: asString(pickField(raw, 'lastRunId', 'last_run_id')),
    lastFiredAt: asString(pickField(raw, 'lastFiredAt', 'last_fired_at')) ?? null,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

function extractSchedules(raw: unknown): Schedule[] {
  const list = pickField(raw, 'schedules')
  return (Array.isArray(list) ? list : []).map(normalizeSchedule)
}

export async function listSchedules(): Promise<Schedule[]> {
  return extractSchedules(await apiFetch<unknown>('/schedules'))
}

export async function createSchedule(input: CreateScheduleInput): Promise<Schedule> {
  const raw = await apiFetch<unknown>('/schedules/create', {
    method: 'POST',
    body: JSON.stringify(input),
  })
  return normalizeSchedule(pickField(raw, 'schedule') ?? raw)
}

export async function pauseSchedule(id: string): Promise<Schedule> {
  const raw = await apiFetch<unknown>(`/schedules/${encodeURIComponent(id)}/pause`, { method: 'POST' })
  return normalizeSchedule(pickField(raw, 'schedule') ?? raw)
}

export async function resumeSchedule(id: string): Promise<Schedule> {
  const raw = await apiFetch<unknown>(`/schedules/${encodeURIComponent(id)}/resume`, { method: 'POST' })
  return normalizeSchedule(pickField(raw, 'schedule') ?? raw)
}
