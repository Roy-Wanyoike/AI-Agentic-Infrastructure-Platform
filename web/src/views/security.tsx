// Security view (issue #81): the org audit trail over GET /audit-events
// (cmd/api/audit_events.go — audit.read, OWNER/ADMIN only; MEMBER/VIEWER are
// locked out of the security-sensitive trail and see a plain notice here).
//
//   GET /audit-events?limit=&cursor=(&action=&actor=)
//     -> {"events":[{id,actor,action,resource?,metadata?,created_at}],
//         "next_cursor":""}
//
// Keyset-paginated like the activity feed: "Load more" walks older pages and
// loaded pages accumulate (render-time id dedupe keeps pages coherent).
//
// Filters: issue #81 pins ?action= / ?actor= query params. The merged backend
// as of main @ 404c72b reads only limit/cursor, so the filter terms are ALSO
// applied to the loaded rows client-side (matchesAuditFilters) — the view is
// honest before the backend consumes the params and needs no change once it
// does. Both filters are EXACT matches (the contract's documented semantics),
// not substrings.

import { useMemo, useState } from 'react'
import { useAuditEvents } from '../lib/hooks'
import { matchesAuditFilters, type AuditEvent } from '../lib/api/auditEvents'
import { formatDateTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton } from './shared'

const AUDIT_PAGE_SIZE = 50

function MetadataCell({ event }: { event: AuditEvent }) {
  let text: string | null = null
  if (event.metadata && Object.keys(event.metadata).length > 0) {
    try {
      text = JSON.stringify(event.metadata)
    } catch {
      text = null
    }
  }
  return <code className="payload-cell">{text ?? '—'}</code>
}

export function SecurityView({ canReadAudit }: { canReadAudit: boolean }) {
  const [actionInput, setActionInput] = useState('')
  const [actorInput, setActorInput] = useState('')
  const [actionFilter, setActionFilter] = useState('')
  const [actorFilter, setActorFilter] = useState('')
  const [cursor, setCursor] = useState('')
  const [older, setOlder] = useState<AuditEvent[]>([])

  const trailQuery = useAuditEvents({ limit: AUDIT_PAGE_SIZE, cursor, action: actionFilter, actor: actorFilter })
  const pageItems = useMemo(() => trailQuery.data?.events ?? [], [trailQuery.data])
  const nextCursor = trailQuery.data?.nextCursor ?? ''

  // Server-side filter params (pinned contract) + client-side complement so
  // the results are accurate even while the backend ignores the params.
  const events = useMemo(() => {
    const merged = [...older, ...pageItems]
    const seen = new Set<string>()
    return merged.filter((event) => {
      if (!event.id || seen.has(event.id)) return false
      seen.add(event.id)
      return matchesAuditFilters(event, actionFilter, actorFilter)
    })
  }, [older, pageItems, actionFilter, actorFilter])

  const loadMore = () => {
    if (!nextCursor) return
    setOlder((prev) => [...prev, ...pageItems])
    setCursor(nextCursor)
  }

  const applyFilters = () => {
    setActionFilter(actionInput.trim())
    setActorFilter(actorInput.trim())
    setCursor('')
    setOlder([])
  }

  const clearFilters = () => {
    setActionInput('')
    setActorInput('')
    setActionFilter('')
    setActorFilter('')
    setCursor('')
    setOlder([])
  }

  if (!canReadAudit) {
    return (
      <>
        <PageHeader eyebrow="Security" title="Audit trail" />
        <EmptyState
          title="Available to Owner and Admin roles"
          hint="The audit trail (who did what, across the whole organization) is audit.read — deliberately locked to OWNER/ADMIN. The API enforces this for every caller."
        />
      </>
    )
  }

  return (
    <>
      <PageHeader
        eyebrow="Security"
        title="Audit trail"
        actions={<span className="form-note">GET /audit-events · append-only · OWNER/ADMIN</span>}
      />

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Filters</p>
            <h3>Narrow the trail</h3>
          </div>
          <span className="form-note">exact matches — sent as ?action=&amp;actor= and applied to loaded rows</span>
        </div>
        <form
          className="filter-row"
          onSubmit={(event) => {
            event.preventDefault()
            applyFilters()
          }}
        >
          <div className="field">
            <label htmlFor="audit-action">Action (e.g. agent.created)</label>
            <input id="audit-action" value={actionInput} onChange={(event) => setActionInput(event.target.value)} placeholder="deployment.canary_auto_promote" spellCheck={false} />
          </div>
          <div className="field">
            <label htmlFor="audit-actor">Actor (principal id)</label>
            <input id="audit-actor" value={actorInput} onChange={(event) => setActorInput(event.target.value)} placeholder="user-…" spellCheck={false} />
          </div>
          <div className="filter-actions">
            <button type="submit" className="primary-button small">
              Apply
            </button>
            <button type="button" className="ghost-button small" onClick={clearFilters}>
              Clear
            </button>
          </div>
        </form>
      </article>

      {trailQuery.isError ? <ErrorBanner error={trailQuery.error} onRetry={() => void trailQuery.refetch()} /> : null}

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Trail</p>
            <h3>Recorded actions</h3>
          </div>
          <span className="form-note">{events.length} loaded · newest first</span>
        </div>

        {trailQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : events.length === 0 ? (
          <EmptyState
            title="No audit events match"
            hint={
              actionFilter || actorFilter
                ? 'No recorded action matches these exact filters — clear them to see the full trail.'
                : 'Audit events appear here as privileged actions happen (secrets created, versions published, tokens minted, …).'
            }
          />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>When</th>
                  <th>Actor</th>
                  <th>Action</th>
                  <th>Resource</th>
                  <th>Metadata</th>
                </tr>
              </thead>
              <tbody>
                {events.map((event) => (
                  <tr key={event.id}>
                    <td>{formatDateTime(event.createdAt)}</td>
                    <td title={event.actor}>{shortenId(event.actor)}</td>
                    <td>
                      <code>{event.action}</code>
                    </td>
                    <td title={event.resource}>{event.resource ? shortenId(event.resource) : '—'}</td>
                    <td>
                      <MetadataCell event={event} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        {!trailQuery.isPending && nextCursor ? (
          <div className="form-actions">
            <span className="form-note">More events are available (keyset pagination).</span>
            <button type="button" className="ghost-button" onClick={loadMore}>
              Load more
            </button>
          </div>
        ) : null}
      </article>
    </>
  )
}
