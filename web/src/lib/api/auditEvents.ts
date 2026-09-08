// Audit trail (issue #18 surface, cmd/api/audit_events.go + internal/audit/list.go):
//
//   GET /audit-events?limit=&cursor=(&action=&actor=)
//     -> {"events":[{"id","actor","action","resource"?,"metadata"?,"created_at"}],
//         "next_cursor":""}
//
// audit.read — OWNER/ADMIN only. Keyset-paginated newest first: pass the
// previous response's next_cursor as ?cursor=; "" means the trail is
// exhausted.
//
// Filters: issue #81 pins ?action= and ?actor= query params. The merged
// backend (as of main @ 404c72b) accepts but IGNORES them (the handler reads
// only limit/cursor), so the Security view additionally filters the loaded
// rows client-side — the UI stays honest before the backend consumes the
// params, and needs no change once it does.

import { apiFetch } from './client'
import { asRecord, asString, pickField } from './types'

export type AuditEvent = {
  id: string
  actor: string
  action: string
  resource: string
  metadata: Record<string, unknown> | null
  createdAt: string
}

export type ListAuditEventsInput = {
  limit?: number
  cursor?: string
  /** Exact action match (dotted name, e.g. "agent.created"). */
  action?: string
  /** Exact actor match (principal id). */
  actor?: string
}

export type ListAuditEventsResult = {
  events: AuditEvent[]
  nextCursor: string
}

function normalizeAuditEvent(raw: unknown): AuditEvent {
  const metadata = pickField(raw, 'metadata')
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    actor: asString(pickField(raw, 'actor')) ?? '',
    action: asString(pickField(raw, 'action')) ?? '',
    resource: asString(pickField(raw, 'resource')) ?? '',
    metadata: asRecord(metadata),
    createdAt: asString(pickField(raw, 'created_at', 'createdAt')) ?? '',
  }
}

export async function listAuditEvents(input: ListAuditEventsInput = {}): Promise<ListAuditEventsResult> {
  const params = new URLSearchParams()
  if (input.limit && input.limit > 0) params.set('limit', String(input.limit))
  if (input.cursor) params.set('cursor', input.cursor)
  if (input.action) params.set('action', input.action)
  if (input.actor) params.set('actor', input.actor)
  const query = params.toString()
  const raw = await apiFetch<unknown>(`/audit-events${query ? `?${query}` : ''}`)
  const list = pickField(raw, 'events')
  return {
    events: (Array.isArray(list) ? list : []).map(normalizeAuditEvent),
    nextCursor: asString(pickField(raw, 'nextCursor', 'next_cursor')) ?? '',
  }
}

/** Client-side complement to the ?action=/?actor= params (see header note). */
export function matchesAuditFilters(event: AuditEvent, action: string, actor: string): boolean {
  if (action && event.action !== action) return false
  if (actor && event.actor !== actor) return false
  return true
}
