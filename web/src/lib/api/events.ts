// Events resource (issue #56, cmd/api/events.go) — the org-scoped domain
// event stream behind the dashboard activity feed.
//
//   GET /events?limit=&cursor=&type=&entity_type=&entity_id=&since=
//     -> {"events":[…],"next_cursor":…}   (runs.execute — MEMBER+)
//
// This is NOT the audit trail (GET /audit-events is the OWNER/ADMIN
// actor/action log); it is the run/workflow event stream the platform
// publishes on the event bus. Listing is keyset-paginated (newest first):
// pass `cursor` from the previous response's next_cursor. `since` is an
// inclusive RFC3339 lower bound.
//
// Note: the backend pins MEMBER+ (runs.execute) for this read — the natural
// runs.read permission would also admit VIEWER, which the issue contract
// explicitly forbids.

import { apiFetch } from './client'
import { asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type ActivityEvent = {
  id: string
  type: string
  entityType: string
  entityId: string
  projectId: string
  executionId: string
  traceId: string
  payload: Record<string, unknown> | null
  timestamp: string
}

export type ListEventsInput = {
  limit?: number
  cursor?: string
  /** Exact contract event type, e.g. run.failed. */
  type?: string
  /** Exact resource_type match (the domain object the event is about). */
  entityType?: string
  /** Exact resource_id match. */
  entityId?: string
  /** RFC3339 inclusive lower bound on the event timestamp. */
  since?: string
}

export type ListEventsResult = {
  events: ActivityEvent[]
  nextCursor: string
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeEvent(raw: unknown): ActivityEvent {
  const payload = pickField(raw, 'payload')
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    type: asString(pickField(raw, 'type')) ?? 'unknown',
    entityType: asString(pickField(raw, 'entity_type', 'entityType')) ?? '',
    entityId: asString(pickField(raw, 'entity_id', 'entityId')) ?? '',
    projectId: asString(pickField(raw, 'project_id', 'projectId')) ?? '',
    executionId: asString(pickField(raw, 'execution_id', 'executionId')) ?? '',
    traceId: asString(pickField(raw, 'trace_id', 'traceId')) ?? '',
    payload: payload && typeof payload === 'object' && !Array.isArray(payload)
      ? { ...(payload as Record<string, unknown>) }
      : null,
    timestamp: asString(pickField(raw, 'timestamp')) ?? '',
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

export async function listEvents(input: ListEventsInput = {}): Promise<ListEventsResult> {
  const params = new URLSearchParams()
  if (input.limit && input.limit > 0) params.set('limit', String(input.limit))
  if (input.cursor) params.set('cursor', input.cursor)
  if (input.type) params.set('type', input.type)
  if (input.entityType) params.set('entity_type', input.entityType)
  if (input.entityId) params.set('entity_id', input.entityId)
  if (input.since) params.set('since', input.since)
  const query = params.toString()
  const raw = await apiFetch<unknown>(`/events${query ? `?${query}` : ''}`)
  const list = pickField(raw, 'events')
  return {
    events: (Array.isArray(list) ? list : []).map(normalizeEvent),
    nextCursor: asString(pickField(raw, 'nextCursor', 'next_cursor')) ?? '',
  }
}

// ---------------------------------------------------------------------------
// Display helpers
// ---------------------------------------------------------------------------

/**
 * Monochrome glyph per event family, matching the sidebar icon language.
 * Falls back to a neutral dot for unknown types (the vocabulary is a pinned
 * backend contract, but never render "undefined").
 */
const EVENT_FAMILY_ICONS: ReadonlyArray<readonly [prefix: string, glyph: string]> = [
  ['run.', '▷'],
  ['step.', '⑂'],
  ['agent.', '⬡'],
  ['approval.', '✓'],
  ['deployment.', '⎌'],
  ['webhook.', '⇄'],
]

export function eventIcon(type: string): string {
  for (const [prefix, glyph] of EVENT_FAMILY_ICONS) {
    if (type.startsWith(prefix)) return glyph
  }
  return '•'
}
