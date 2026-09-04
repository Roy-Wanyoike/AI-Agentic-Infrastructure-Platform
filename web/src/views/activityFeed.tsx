// Overview activity feed (issue #56): the latest events from GET /v1/events
// — the org's domain event stream (run/workflow lifecycle, agent lifecycle,
// approvals, deployments, webhook receipts). This is NOT the audit trail
// (audit-events is the OWNER/ADMIN actor log and lives in no dashboard view).
//
// Keyset-paginated like the marketplace browse: the first fetch shows the
// latest 20 events and "Load more" walks older pages via next_cursor. Loaded
// pages accumulate in component state so the feed grows like a timeline; a
// render-time id dedupe keeps pages coherent if page 1 is refetched while
// older pages are already accumulated.
//
// RBAC: the feed is MEMBER+ (runs.execute — the established MEMBER+ read
// grant; runs.read would also admit VIEWER). VIEWER roles get a plain empty
// state here instead of a permanent 403 banner; the API still enforces the
// real permission for every other caller.

import { useMemo, useState } from 'react'
import { useEvents } from '../lib/hooks'
import { eventIcon, type ActivityEvent } from '../lib/api/events'
import { formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, Skeleton } from './shared'

const FEED_PAGE_SIZE = 20

function entityLabel(event: ActivityEvent): string {
  if (!event.entityType && !event.entityId) return '—'
  const id = shortenId(event.entityId || event.entityType)
  return event.entityType ? `${event.entityType} ${id}` : id
}

export function ActivityFeedPanel({ canRead }: { canRead: boolean }) {
  const [cursor, setCursor] = useState('')
  const [older, setOlder] = useState<ActivityEvent[]>([])
  const feedQuery = useEvents({ cursor, limit: FEED_PAGE_SIZE }, { enabled: canRead })

  const pageItems = feedQuery.data?.events ?? []
  const nextCursor = feedQuery.data?.nextCursor ?? ''

  const items = useMemo(() => {
    const merged = [...older, ...pageItems]
    const seen = new Set<string>()
    return merged.filter((event) => {
      if (!event.id || seen.has(event.id)) return false
      seen.add(event.id)
      return true
    })
  }, [older, pageItems])

  const loadMore = () => {
    if (!nextCursor) return
    setOlder((prev) => [...prev, ...pageItems])
    setCursor(nextCursor)
  }

  if (!canRead) {
    return (
      <section className="panel" aria-label="Activity feed">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Event stream</p>
            <h3>Activity feed</h3>
          </div>
        </div>
        <EmptyState
          title="Available to Member+ roles"
          hint="The organization event stream (runs, agents, deployments, webhooks) requires the Member role."
        />
      </section>
    )
  }

  return (
    <section className="panel" aria-label="Activity feed">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Event stream</p>
          <h3>Activity feed</h3>
        </div>
        <span className="form-note">live from /events (latest {FEED_PAGE_SIZE} per page)</span>
      </div>

      {feedQuery.isPending ? (
        <div className="stack-gap">
          <Skeleton height={16} />
          <Skeleton height={16} />
          <Skeleton height={16} />
          <Skeleton height={16} />
          <Skeleton height={16} />
        </div>
      ) : feedQuery.error ? (
        <ErrorBanner error={feedQuery.error} onRetry={() => void feedQuery.refetch()} />
      ) : items.length === 0 ? (
        <EmptyState
          title="No events yet"
          hint="Platform activity (run.started, agent.created, deployment.completed, …) streams here as it is published."
        />
      ) : (
        <div className="activity-list">
          {items.map((event) => (
            <div key={event.id} className="activity-row" title={event.type}>
              <span className="activity-icon" aria-hidden="true">
                {eventIcon(event.type)}
              </span>
              <div className="activity-body">
                <span className="activity-type">{event.type}</span>
                <span className="activity-entity">{entityLabel(event)}</span>
              </div>
              <span className="activity-when">{formatRelativeTime(event.timestamp)}</span>
            </div>
          ))}
        </div>
      )}

      {canRead && !feedQuery.isPending && !feedQuery.error && nextCursor ? (
        <div className="form-actions">
          <span className="form-note">More activity is available (keyset pagination).</span>
          <button type="button" className="ghost-button" onClick={loadMore}>
            Load more
          </button>
        </div>
      ) : null}
    </section>
  )
}
