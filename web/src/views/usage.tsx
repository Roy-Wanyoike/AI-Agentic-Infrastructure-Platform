// Usage view (Priority 5) — honest aggregates built from GET /metrics?format=json
// and GET /runs. Widgets show real platform telemetry; when the API does not
// expose a metric (tokens, cost, latency percentiles) the widget says so
// instead of inventing numbers.

import { useMemo } from 'react'
import { useMetrics, useRuns } from '../lib/hooks'
import { formatCents, formatDurationMs, formatNumber, formatRelativeTime } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, SummaryStat } from './shared'
import { countByStatus, sortRunsDesc } from './uiHelpers'

const HTTP_COUNT_PREFIX = 'http_requests_total{'
const HTTP_LATENCY_PREFIX = 'http_request_duration_seconds{'

type TrafficRow = {
  route: string
  method: string
  status: string
  count: number
}

/** Parses keys shaped like `http_requests_total{route="/api/v1/agents",method="GET",status="200"}`. */
function parseTrafficCounts(counts: Record<string, number>): TrafficRow[] {
  const rows: TrafficRow[] = []
  for (const [key, value] of Object.entries(counts)) {
    if (!key.startsWith(HTTP_COUNT_PREFIX) || !key.endsWith('}')) continue
    const inner = key.slice(HTTP_COUNT_PREFIX.length, -1)
    const parts = new Map<string, string>()
    for (const piece of inner.split(',')) {
      const eq = piece.indexOf('=')
      if (eq === -1) continue
      const label = piece.slice(0, eq).trim()
      const raw = piece.slice(eq + 1).trim().replace(/^"|"$/g, '')
      parts.set(label, raw)
    }
    rows.push({
      route: parts.get('route') ?? 'unknown',
      method: parts.get('method') ?? '—',
      status: parts.get('status') ?? '—',
      count: value,
    })
  }
  return rows.sort((a, b) => b.count - a.count)
}

function sumLatencySamples(latency: Record<string, number>): number {
  return Object.entries(latency)
    .filter(([key]) => key.startsWith(HTTP_LATENCY_PREFIX))
    .reduce((sum, [, value]) => sum + (Number.isFinite(value) ? value : 0), 0)
}

function findMetricValue(counts: Record<string, number>, patterns: RegExp[]): number | undefined {
  let total: number | undefined
  for (const [key, value] of Object.entries(counts)) {
    if (patterns.some((pattern) => pattern.test(key))) {
      total = (total ?? 0) + value
    }
  }
  return total
}

export function UsageView() {
  const metricsQuery = useMetrics()
  const runsQuery = useRuns()
  const metrics = metricsQuery.data
  const runs = useMemo(() => sortRunsDesc(runsQuery.data ?? []), [runsQuery.data])
  const statusCounts = useMemo(() => countByStatus(runs), [runs])

  const loading = metricsQuery.isPending || runsQuery.isPending
  const loadError = metricsQuery.error ?? runsQuery.error
  const retryAll = () => {
    void metricsQuery.refetch()
    void runsQuery.refetch()
  }

  const traffic = useMemo(() => parseTrafficCounts(metrics?.counts ?? {}), [metrics?.counts])
  const totalRequests = useMemo(() => traffic.reduce((sum, row) => sum + row.count, 0), [traffic])
  const latestLatencySeconds = useMemo(() => sumLatencySamples(metrics?.latency ?? {}), [metrics?.latency])

  const otherRuns = useMemo(() => {
    const known = statusCounts.QUEUED + statusCounts.RUNNING + statusCounts.COMPLETED + statusCounts.FAILED
    return Math.max(runs.length - known, 0)
  }, [runs.length, statusCounts])

  const tokensTotal = useMemo(() => findMetricValue(metrics?.counts ?? {}, [/token/i]), [metrics?.counts])
  const costCentsTotal = useMemo(() => findMetricValue(metrics?.counts ?? {}, [/cost/i]), [metrics?.counts])

  const inFlight = statusCounts.QUEUED + statusCounts.RUNNING + otherRuns
  const completionRate = runs.length ? Math.round((statusCounts.COMPLETED / runs.length) * 100) : null
  const failureRate = runs.length ? Math.round((statusCounts.FAILED / runs.length) * 100) : null

  const lastRun = runs[0]

  return (
    <>
      <PageHeader
        eyebrow="Telemetry"
        title="Usage"
        actions={<span className="form-note">GET /metrics?format=json · GET /runs</span>}
      />

      {loadError && !loading ? <ErrorBanner error={loadError} onRetry={retryAll} /> : null}

      <section className="kpi-grid" aria-label="Usage aggregates">
        <article className="kpi-card">
          <p>Total runs</p>
          <div className="kpi-row">
            {loading ? <Skeleton height={30} width={90} /> : <strong>{formatNumber(runs.length)}</strong>}
            {!loading ? <span className="delta neutral">{lastRun ? `last run ${formatRelativeTime(lastRun.createdAt)}` : 'no runs yet'}</span> : null}
          </div>
        </article>
        <article className="kpi-card">
          <p>Completed / failed</p>
          <div className="kpi-row">
            {loading ? <Skeleton height={30} width={90} /> : <strong>{formatNumber(statusCounts.COMPLETED)} / {formatNumber(statusCounts.FAILED)}</strong>}
            {!loading && runs.length > 0 ? (
              <span className="delta positive">
                {completionRate}% completed · {failureRate}% failed
              </span>
            ) : null}
          </div>
        </article>
        <article className="kpi-card">
          <p>API requests</p>
          <div className="kpi-row">
            {loading ? <Skeleton height={30} width={90} /> : <strong>{formatNumber(totalRequests)}</strong>}
            {!loading ? (
              <span className="delta neutral">
                {traffic.length > 0 ? `${traffic.length} route/status pairs tracked` : 'no traffic counters exposed yet'}
              </span>
            ) : null}
          </div>
        </article>
        <article className="kpi-card">
          <p>Queue backlog</p>
          <div className="kpi-row">
            {loading ? <Skeleton height={30} width={90} /> : <strong>{formatNumber(metrics?.queueLength ?? 0)}</strong>}
            {!loading ? <span className="delta neutral">{formatNumber(inFlight)} runs in flight</span> : null}
          </div>
        </article>
      </section>

      <section className="content-grid">
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Traffic</p>
              <h3>Requests by route</h3>
            </div>
          </div>

          {traffic.length === 0 ? (
            <EmptyState
              title="No request counters reported"
              hint="/metrics did not include http_requests_total entries. They appear once the metrics middleware is wired and traffic flows through it."
            />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Route</th>
                    <th>Method</th>
                    <th>Status</th>
                    <th>Count</th>
                  </tr>
                </thead>
                <tbody>
                  {traffic.slice(0, 10).map((row, index) => (
                    <tr key={`${row.route}-${row.method}-${row.status}-${index}`}>
                      <td>
                        <code className="config-cell">{row.route}</code>
                      </td>
                      <td>{row.method}</td>
                      <td>{row.status}</td>
                      <td>
                        <strong>{formatNumber(row.count)}</strong>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </article>

        <article className="panel">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Tokens &amp; cost</p>
              <h3>Consumption</h3>
            </div>
          </div>

          {tokensTotal === undefined && costCentsTotal === undefined ? (
            <EmptyState
              title="No token or cost aggregates yet"
              hint="The /metrics snapshot does not expose token or cost counters yet. Per-step token usage and cost are visible on each run's timeline, and per-eval totals on eval runs."
            />
          ) : (
            <div className="summary-grid" style={{ gridTemplateColumns: '1fr' }}>
              {tokensTotal !== undefined ? <SummaryStat label="Tokens (all counters)" value={formatNumber(tokensTotal)} accent="info" /> : null}
              {costCentsTotal !== undefined ? <SummaryStat label="Cost (all counters)" value={formatCents(costCentsTotal)} accent="warning" /> : null}
            </div>
          )}

          <div className="quality-list" style={{ marginTop: 18 }}>
            <div>
              <label>Latest observed request latency</label>
              <div className="meter">
                <span style={{ width: latestLatencySeconds > 0 ? '100%' : '0%' }} />
              </div>
              <strong>{latestLatencySeconds > 0 ? formatDurationMs(latestLatencySeconds * 1000) : '—'}</strong>
            </div>
          </div>
        </article>
      </section>
    </>
  )
}
