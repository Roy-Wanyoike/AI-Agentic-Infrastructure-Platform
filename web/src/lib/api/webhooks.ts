// Webhooks resource (track 2-e contract): list, create (secret shown ONCE),
// delete, deliveries.
//
// - GET    /webhooks                     -> {"webhooks":[{id,url,events,status,secret_set,created_at}]}
// - POST   /webhooks/create              -> {"webhook":{…},"secret":"whsec_…"}  (secret returned exactly once)
// - DELETE /webhooks/{id}                -> {"deleted":true}
// - GET    /webhooks/{id}/deliveries     -> {"deliveries":[{id,event_type,status,attempts,last_status_code,latency_ms,error,created_at}]}

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type Webhook = {
  id: string
  url: string
  events: string[]
  status: string
  secretSet: boolean
  createdAt?: string
}

export type WebhookDelivery = {
  id: string
  eventType: string
  status: string
  attempts: number
  lastStatusCode?: number
  latencyMs?: number
  error?: string
  createdAt?: string
}

/** Event types accepted by POST /webhooks/create (mirrors internal/events/events.go). */
export const WEBHOOK_EVENT_TYPES = [
  'agent.created',
  'agent.updated',
  'run.started',
  'run.completed',
  'run.failed',
  'run.cancelled',
  'step.started',
  'step.completed',
  'approval.requested',
  'approval.decided',
  'deployment.completed',
  'deployment.failed',
  'webhook.received',
] as const

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeWebhook(raw: unknown): Webhook {
  const eventsRaw = pickField(raw, 'events')
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    url: asString(pickField(raw, 'url')) ?? '',
    events: Array.isArray(eventsRaw) ? eventsRaw.map((event) => asString(event) ?? '').filter(Boolean) : [],
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    secretSet: pickField(raw, 'secretSet', 'secret_set') === true,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
  }
}

function normalizeDelivery(raw: unknown): WebhookDelivery {
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    eventType: asString(pickField(raw, 'eventType', 'event_type')) ?? '',
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    attempts: asNumber(pickField(raw, 'attempts')) ?? 0,
    lastStatusCode: asNumber(pickField(raw, 'lastStatusCode', 'last_status_code')),
    latencyMs: asNumber(pickField(raw, 'latencyMs', 'latency_ms')),
    error: asString(pickField(raw, 'error')),
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

export async function listWebhooks(): Promise<Webhook[]> {
  const raw = await apiFetch<unknown>('/webhooks')
  const list = pickField(raw, 'webhooks')
  return (Array.isArray(list) ? list : []).map(normalizeWebhook)
}

export async function createWebhook(input: { url: string; events: string[] }): Promise<{ webhook: Webhook; secret?: string }> {
  const raw = await apiFetch<unknown>('/webhooks/create', {
    method: 'POST',
    body: JSON.stringify(input),
  })
  return {
    webhook: normalizeWebhook(pickField(raw, 'webhook') ?? raw),
    secret: asString(pickField(raw, 'secret')),
  }
}

export async function deleteWebhook(id: string): Promise<boolean> {
  const raw = await apiFetch<unknown>(`/webhooks/${encodeURIComponent(id)}`, { method: 'DELETE' })
  return pickField(raw, 'deleted') === true
}

export async function listWebhookDeliveries(id: string, limit = 50): Promise<WebhookDelivery[]> {
  const raw = await apiFetch<unknown>(`/webhooks/${encodeURIComponent(id)}/deliveries?limit=${limit}`)
  const list = pickField(raw, 'deliveries')
  return (Array.isArray(list) ? list : []).map(normalizeDelivery)
}
