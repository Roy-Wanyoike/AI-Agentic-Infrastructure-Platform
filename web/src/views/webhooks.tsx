// Webhooks view (track 2-e contract): list, create (signing secret shown
// exactly once), delete, deliveries with status.
//
// Wired endpoints (no mocks):
// - GET    /webhooks                   -> {"webhooks":[…]}   (webhooks.read)
// - POST   /webhooks/create            -> {"webhook":{…},"secret":…} (webhooks.write)
// - DELETE /webhooks/{id}              -> {"deleted":true}   (webhooks.write)
// - GET    /webhooks/{id}/deliveries   -> {"deliveries":[…]} (webhooks.read)
//
// The backend stores only a SHA-256 hash of the secret and never returns it
// again — the create response is the only chance to copy it, so the banner
// stays until explicitly dismissed.

import { useMemo, useState, type FormEvent } from 'react'
import { useCreateWebhook, useDeleteWebhook, useWebhookDeliveries, useWebhooks } from '../lib/hooks'
import { WEBHOOK_EVENT_TYPES, type Webhook } from '../lib/api/webhooks'
import { formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

function SecretOnceBanner({ secret, onDismiss }: { secret: string; onDismiss: () => void }) {
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(secret)
      setCopied(true)
    } catch {
      // Clipboard unavailable (permissions/insecure context) — the secret
      // stays selectable in the banner, nothing is faked.
      setCopied(false)
    }
  }

  return (
    <div className="secret-banner" role="alert">
      <div>
        <strong>Signing secret — shown only once</strong>
        <span>
          Store it now: the API keeps only a SHA-256 hash and <em>never returns this value again</em>. Verify deliveries
          with an HMAC-SHA256 signature over the raw body.
        </span>
        <code className="secret-value">{secret}</code>
      </div>
      <div className="topbar-actions">
        <button type="button" className="ghost-button small" onClick={() => void copy()}>
          {copied ? 'Copied ✓' : 'Copy secret'}
        </button>
        <button type="button" className="ghost-button small" onClick={onDismiss}>
          I saved it — dismiss
        </button>
      </div>
    </div>
  )
}

function CreateWebhookForm({ onCreated }: { onCreated: (secret?: string) => void }) {
  const createWebhook = useCreateWebhook()
  const [url, setUrl] = useState('')
  const [events, setEvents] = useState<string[]>([])
  const [parseNote, setParseNote] = useState<string | null>(null)

  const toggleEvent = (eventType: string) => {
    setEvents((current) =>
      current.includes(eventType) ? current.filter((candidate) => candidate !== eventType) : [...current, eventType],
    )
  }

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (createWebhook.isPending) return
    setParseNote(null)
    // Empty selection = subscribe to all event types (backend semantics).
    createWebhook.mutate(
      { url: url.trim(), events },
      {
        onSuccess: (result) => {
          setUrl('')
          setEvents([])
          onCreated(result.secret)
        },
        onError: (error) => setParseNote(describeError(error)),
      },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">New webhook</p>
          <h3>Create webhook</h3>
        </div>
        <span className="form-note">POST /webhooks/create</span>
      </div>

      {parseNote ? <div className="form-error">{parseNote}</div> : null}

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field span-2">
            <label htmlFor="webhook-url">Endpoint URL</label>
            <input
              id="webhook-url"
              type="url"
              value={url}
              onChange={(event) => setUrl(event.target.value)}
              placeholder="https://example.com/hooks/agentos"
              required
            />
          </div>
          <div className="field span-2">
            <label>Events ({events.length === 0 ? 'none selected = all events' : `${events.length} selected`})</label>
            <div className="event-grid">
              {WEBHOOK_EVENT_TYPES.map((eventType) => (
                <label key={eventType} className="event-option">
                  <input
                    type="checkbox"
                    checked={events.includes(eventType)}
                    onChange={() => toggleEvent(eventType)}
                  />
                  <code>{eventType}</code>
                </label>
              ))}
            </div>
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">A signing secret is generated server-side and shown once after creation.</span>
          <button type="submit" className="primary-button" disabled={createWebhook.isPending}>
            {createWebhook.isPending ? 'Creating…' : 'Create webhook'}
          </button>
        </div>
      </form>
    </article>
  )
}

function WebhookRowActions({ webhook, canWrite }: { webhook: Webhook; canWrite: boolean }) {
  const deleteWebhook = useDeleteWebhook()
  if (!canWrite) return <span className="form-note">write needs MEMBER+</span>
  return (
    <button
      type="button"
      className="ghost-button small"
      disabled={deleteWebhook.isPending}
      onClick={() => {
        if (window.confirm(`Delete webhook ${webhook.url}? Deliveries will stop immediately.`)) {
          deleteWebhook.mutate(webhook.id)
        }
      }}
    >
      {deleteWebhook.isPending ? 'Deleting…' : 'Delete'}
    </button>
  )
}

function DeliveriesPanel({ webhooks }: { webhooks: Webhook[] }) {
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const selectedHook = selectedId ?? webhooks[0]?.id ?? null
  const deliveriesQuery = useWebhookDeliveries(selectedHook)
  const deliveries = deliveriesQuery.data ?? []

  if (webhooks.length === 0) {
    return (
      <EmptyState
        title="No webhook selected"
        hint="Deliveries appear per webhook once matching platform events fire."
      />
    )
  }

  return (
    <>
      <div className="panel-header">
        <div>
          <p className="eyebrow">Delivery log</p>
          <h3>Recent deliveries</h3>
        </div>
        <label className="inline-label" htmlFor="deliveries-webhook">
          Webhook
          <select id="deliveries-webhook" value={selectedHook ?? ''} onChange={(event) => setSelectedId(event.target.value)}>
            {webhooks.map((webhook) => (
              <option key={webhook.id} value={webhook.id}>
                {shortenId(webhook.url)}
              </option>
            ))}
          </select>
        </label>
      </div>

      <p className="form-note">GET /webhooks/{selectedHook ? shortenId(selectedHook) : '…'}/deliveries?limit=50 · refreshes every 10s</p>

      {deliveriesQuery.isError ? (
        <ErrorBanner error={deliveriesQuery.error} onRetry={() => void deliveriesQuery.refetch()} />
      ) : null}

      {deliveriesQuery.isPending ? (
        <div className="stack-gap">
          <Skeleton height={16} />
          <Skeleton height={16} />
        </div>
      ) : deliveries.length === 0 ? (
        <EmptyState
          title="No deliveries recorded yet"
          hint="Deliveries show up when an event matching this webhook's subscription fires (attempt, status code, latency)."
        />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Event</th>
                <th>Status</th>
                <th>Attempts</th>
                <th>Last status code</th>
                <th>Latency</th>
                <th>Error</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {deliveries.map((delivery) => (
                <tr key={delivery.id}>
                  <td>
                    <code>{delivery.eventType}</code>
                  </td>
                  <td>
                    <StatusPill status={delivery.status} />
                  </td>
                  <td>{delivery.attempts}</td>
                  <td>{delivery.lastStatusCode ?? '—'}</td>
                  <td>{delivery.latencyMs !== undefined ? `${delivery.latencyMs}ms` : '—'}</td>
                  <td className="table-error-cell">{delivery.error || '—'}</td>
                  <td>{formatRelativeTime(delivery.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}

export function WebhooksView({ canWrite }: { canWrite: boolean }) {
  const webhooksQuery = useWebhooks()
  const [showCreate, setShowCreate] = useState(false)
  const [createdSecret, setCreatedSecret] = useState<string | null>(null)
  const webhooks = useMemo(() => webhooksQuery.data ?? [], [webhooksQuery.data])
  const activeHooks = webhooks.filter((webhook) => webhook.status === 'active').length

  return (
    <>
      <PageHeader
        eyebrow="Events"
        title="Webhooks"
        actions={
          canWrite ? (
            <button type="button" className="primary-button" onClick={() => setShowCreate((open) => !open)}>
              New webhook
            </button>
          ) : (
            <span className="form-note">Viewer role — creating webhooks needs MEMBER and above</span>
          )
        }
      />

      {createdSecret ? <SecretOnceBanner secret={createdSecret} onDismiss={() => setCreatedSecret(null)} /> : null}

      {showCreate && canWrite ? (
        <CreateWebhookForm
          onCreated={(secret) => {
            setShowCreate(false)
            setCreatedSecret(secret ?? null)
          }}
        />
      ) : null}

      {webhooksQuery.isError ? <ErrorBanner error={webhooksQuery.error} onRetry={() => void webhooksQuery.refetch()} /> : null}

      <section className="summary-grid">
        {webhooksQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Total webhooks" value={String(webhooks.length)} />
            <SummaryStat label="Active" value={String(activeHooks)} accent="success" />
            <SummaryStat label="Events available" value={String(WEBHOOK_EVENT_TYPES.length)} accent="info" />
            <SummaryStat label="API" value="/webhooks" accent="warning" />
          </>
        )}
      </section>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Endpoints</p>
            <h3>All webhooks</h3>
          </div>
          <span className="form-note">GET /webhooks</span>
        </div>

        {webhooksQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : webhooks.length === 0 ? (
          <EmptyState
            title="No webhooks registered"
            hint="Webhooks POST signed payloads to your endpoints when platform events fire."
            action={
              canWrite ? (
                <button type="button" className="primary-button" onClick={() => setShowCreate(true)}>
                  Create webhook
                </button>
              ) : undefined
            }
          />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>URL</th>
                  <th>Events</th>
                  <th>Status</th>
                  <th>Secret set</th>
                  <th>Created</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {webhooks.map((webhook) => (
                  <tr key={webhook.id}>
                    <td>
                      <strong>{webhook.url}</strong>
                      <div className="form-note">{shortenId(webhook.id)}</div>
                    </td>
                    <td>
                      {webhook.events.length === 0
                        ? 'all events'
                        : webhook.events.map((event) => <code key={event} className="event-chip">{event}</code>)}
                    </td>
                    <td>
                      <StatusPill status={webhook.status} />
                    </td>
                    <td>{webhook.secretSet ? 'yes (hash only)' : 'no'}</td>
                    <td>{formatRelativeTime(webhook.createdAt)}</td>
                    <td>
                      <WebhookRowActions webhook={webhook} canWrite={canWrite} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </article>

      <article className="panel wide">
        <DeliveriesPanel webhooks={webhooks} />
      </article>
    </>
  )
}
