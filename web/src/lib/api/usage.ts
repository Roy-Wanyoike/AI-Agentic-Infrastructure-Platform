// Usage-costs resource (wave-3 cost report, cmd/api/usage_costs.go):
//
//   GET /usage/costs?from=&to=&group_by=day|agent|model   (usage.read)
//
// Contract response shape (snake_case, exact):
//   {"total_cost_cents": 0, "series": [{"bucket": "2026-09-03",
//     "agent_id": "…", "model": "…", "cost_cents": 0, "runs": 0}]}
//
// `bucket` is present for group_by=day; `agent_id`/`model` for the other
// groupings (omitted otherwise). The backend defaults to the last 30 days
// when from/to are omitted and caps windows at 366 days (INVALID_TIME_RANGE /
// INVALID_GROUP_BY error envelopes otherwise).

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

export type UsageCostGroupBy = 'day' | 'agent' | 'model'

export type UsageCostBucket = {
  bucket?: string
  agentId?: string
  model?: string
  costCents: number
  runs: number
}

export type UsageCostReport = {
  totalCostCents: number
  series: UsageCostBucket[]
}

export type UsageCostWindow = {
  from?: string
  to?: string
  groupBy?: UsageCostGroupBy
}

function normalizeCostBucket(raw: unknown): UsageCostBucket {
  return {
    bucket: asString(pickField(raw, 'bucket', 'day', 'date')),
    agentId: asString(pickField(raw, 'agentId', 'agent_id')),
    model: asString(pickField(raw, 'model')),
    costCents: asNumber(pickField(raw, 'costCents', 'cost_cents')) ?? 0,
    runs: asNumber(pickField(raw, 'runs', 'runCount', 'run_count')) ?? 0,
  }
}

/** Defensive list extraction: accepts {series:[]} or a bare array. */
function extractSeries(raw: unknown): unknown[] {
  const wrapped = pickField(raw, 'series', 'buckets')
  if (Array.isArray(wrapped)) return wrapped
  if (Array.isArray(raw)) return raw
  return []
}

export async function getUsageCosts(window: UsageCostWindow = {}): Promise<UsageCostReport> {
  const params = new URLSearchParams()
  if (window.from) params.set('from', window.from)
  if (window.to) params.set('to', window.to)
  if (window.groupBy) params.set('group_by', window.groupBy)
  const query = params.toString()
  const raw = await apiFetch<unknown>(`/usage/costs${query ? `?${query}` : ''}`)
  return {
    totalCostCents: asNumber(pickField(raw, 'totalCostCents', 'total_cost_cents', 'total')) ?? 0,
    series: extractSeries(raw).map(normalizeCostBucket),
  }
}
