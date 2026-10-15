// Showback resource (wave-8 issue C: showback/chargeback reports,
// cmd/api/showback.go — read-only, usage.read):
//
//   GET /showback?from=&to=&group_by=agent|team
//
// Contract response shape (snake_case, exact):
//   {"from":"…","to":"…","group_by":"agent",
//    "rows":[{"scope_ref":"…","runs":0,"tool_calls":0,"tokens_in":0,
//             "tokens_out":0,"cost_cents":0}],
//    "notes":["…"]}
//
// from/to accept RFC3339 timestamps or bare UTC dates (YYYY-MM-DD); the
// window is half-open [from, to) and capped at 92 days server-side
// (INVALID_TIME_RANGE otherwise; unknown group_by is INVALID_GROUP_BY).
// `scope_ref` is the agent id for group_by=agent (team grouping is a backend
// fallback that travels in "notes").

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

export type ShowbackGroupBy = 'agent' | 'team'

export type ShowbackRow = {
  scopeRef: string
  runs: number
  toolCalls: number
  tokensIn: number
  tokensOut: number
  costCents: number
}

export type ShowbackReport = {
  from: string
  to: string
  groupBy: string
  rows: ShowbackRow[]
  notes: string[]
}

export type ShowbackWindow = {
  from?: string
  to?: string
  groupBy?: ShowbackGroupBy
}

function normalizeShowbackRow(raw: unknown): ShowbackRow {
  return {
    scopeRef: asString(pickField(raw, 'scopeRef', 'scope_ref', 'agentId', 'agent_id')) ?? '',
    runs: asNumber(pickField(raw, 'runs', 'runCount', 'run_count')) ?? 0,
    toolCalls: asNumber(pickField(raw, 'toolCalls', 'tool_calls')) ?? 0,
    tokensIn: asNumber(pickField(raw, 'tokensIn', 'tokens_in')) ?? 0,
    tokensOut: asNumber(pickField(raw, 'tokensOut', 'tokens_out')) ?? 0,
    costCents: asNumber(pickField(raw, 'costCents', 'cost_cents')) ?? 0,
  }
}

/** Defensive list extraction: accepts {rows:[]} or a bare array. */
function extractRows(raw: unknown): unknown[] {
  const wrapped = pickField(raw, 'rows', 'items', 'data')
  if (Array.isArray(wrapped)) return wrapped
  if (Array.isArray(raw)) return raw
  return []
}

export async function getShowbackReport(window: ShowbackWindow = {}): Promise<ShowbackReport> {
  const params = new URLSearchParams()
  if (window.from) params.set('from', window.from)
  if (window.to) params.set('to', window.to)
  if (window.groupBy) params.set('group_by', window.groupBy)
  const query = params.toString()
  const raw = await apiFetch<unknown>(`/showback${query ? `?${query}` : ''}`)
  const notes = pickField(raw, 'notes')
  return {
    from: asString(pickField(raw, 'from')) ?? '',
    to: asString(pickField(raw, 'to')) ?? '',
    groupBy: asString(pickField(raw, 'groupBy', 'group_by')) ?? '',
    rows: extractRows(raw).map(normalizeShowbackRow),
    notes: Array.isArray(notes)
      ? notes.map((note) => asString(note) ?? '').filter((note) => note !== '')
      : [],
  }
}
