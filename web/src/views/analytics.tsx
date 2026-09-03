// Analytics view (real): latency percentiles, platform counters, queue depth
// and cost aggregates.
//
// Wired endpoints (no mocks):
// - GET /metrics?format=json  — counts / latency / queue_length plus the
//   additive "histograms" section (wave-2 track 2-h): bucketed p50/p95/p99
//   summaries keyed like
//   agentos_request_duration_seconds{route="…",method="…",status="…"} (seconds).
// - GET /usage/costs?group_by=day|agent|model — {"total_cost_cents": n,
//   "series":[{"bucket"|"agent_id"|"model","cost_cents","runs"}]} (usage.read).
//
// Honesty rules: a counter the API does not emit (e.g. agentos_runs_total /
// agentos_tools_total before the orchestrator wires IncRuns/IncTools) renders
// an explicit "not exposed by the API" state — never a invented number.

import { useMemo, useState } from 'react'
import { useMetrics, useUsageCosts } from '../lib/hooks'
import type { HistogramSummary, MetricsSnapshot } from '../lib/api/types'
import type { UsageCostGroupBy } from '../lib/api/usage'
import { formatCents, formatDurationMs, formatNumber } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, SummaryStat } from './shared'

const DURATION_FAMILY = 'agentos_request_duration_seconds{'
const RUNS_COUNTER = 'agentos_runs_total'
const TOOLS_COUNTER = 'agentos_tools_total'

const GROUP_BY_OPTIONS: ReadonlyArray<{ value: UsageCostGroupBy; label: string }> = [
  { value: 'day', label: 'By day' },
  { value: 'agent', label: 'By agent' },
  { value: 'model', label: 'By model' },
]

type RouteHistogram = {
  key: string
  route: string
  method: string
  status: string
  summary: HistogramSummary
}

/** Parses a metric key's `{label="value",…}` labels into a record. */
function parseMetricLabels(key: string): Record<string, string> {
  const labels: Record<string, string> = {}
  const start = key.indexOf('{')
  const end = key.lastIndexOf('}')
  if (start === -1 || end === -1 || end <= start) return labels
  for (const piece of key.slice(start + 1, end).split(',')) {
    const eq = piece.indexOf('=')
    if (eq === -1) continue
    labels[piece.slice(0, eq).trim()] = piece.slice(eq + 1).trim().replace(/^"|"$/g, '')
  }
  return labels
}

function routeHistograms(metrics: MetricsSnapshot | undefined): RouteHistogram[] {
  if (!metrics) return []
  const rows: RouteHistogram[] = []
  for (const [key, summary] of Object.entries(metrics.histograms)) {
    if (!key.startsWith(DURATION_FAMILY)) continue
    const labels = parseMetricLabels(key)
    rows.push({
      key,
      route: labels.route ?? 'unknown',
      method: labels.method ?? '—',
      status: labels.status ?? '—',
      summary,
    })
  }
  return rows.sort((a, b) => b.summary.count - a.summary.count)
}

/**
 * Percentile across ALL route histograms: merges the cumulative buckets by
 * bound (the backend creates every histogram with the same default bounds) and
 * then applies the exact interpolation used by
 * internal/observability/histogram.go (Prometheus histogram_quantile style).
 */
function mergedQuantile(summaries: HistogramSummary[], q: number): number | null {
  const total = summaries.reduce((sum, summary) => sum + summary.count, 0)
  if (total <= 0) return null
  const merged = new Map<number, number>()
  for (const summary of summaries) {
    for (const [bound, cumulative] of Object.entries(summary.buckets ?? {})) {
      if (bound === '+Inf') continue
      const value = Number(bound)
      if (!Number.isFinite(value)) continue
      merged.set(value, (merged.get(value) ?? 0) + cumulative)
    }
  }
  if (merged.size === 0) return null
  const bounds = [...merged.keys()].sort((a, b) => a - b)
  const rank = q * total
  let prevBound = 0
  let prevCum = 0
  for (const bound of bounds) {
    const cumulative = merged.get(bound) ?? 0
    if (cumulative < rank) {
      prevBound = bound
      prevCum = cumulative
      continue
    }
    if (cumulative === prevCum) return prevBound
    const fraction = (rank - prevCum) / (cumulative - prevCum)
    return prevBound + fraction * (bound - prevBound)
  }
  return prevBound
}

function formatSeconds(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds) || seconds <= 0) return '—'
  return formatDurationMs(seconds * 1000)
}

/** Renders "—" instead of a number when a counter is genuinely not emitted. */
function counterValue(counts: Record<string, number>, name: string): number | null {
  return Object.prototype.hasOwnProperty.call(counts, name) ? counts[name] : null
}

function CostPanel() {
  const [groupBy, setGroupBy] = useState<UsageCostGroupBy>('day')
  const costsQuery = useUsageCosts({ groupBy })
  const report = costsQuery.data

  const series = useMemo(() => report?.series ?? [], [report])
  const maxCost = useMemo(() => Math.max(...series.map((row) => row.costCents), 0), [series])
  const totalRuns = useMemo(() => series.reduce((sum, row) => sum + row.runs, 0), [series])

  const labelFor = (row: { bucket?: string; agentId?: string; model?: string }) =>
    row.bucket ?? row.agentId ?? row.model ?? '—'

  return (
    <article className="panel wide">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Spend</p>
          <h3>Cost aggregates</h3>
        </div>
        <div className="field">
          <label htmlFor="analytics-groupby">Group by</label>
          <select
            id="analytics-groupby"
            value={groupBy}
            onChange={(event) => setGroupBy(event.target.value as UsageCostGroupBy)}
          >
            {GROUP_BY_OPTIONS.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
        </div>
      </div>
      <p className="form-note">GET /usage/costs — defaults to the trailing 30 days.</p>

      {costsQuery.isError ? <ErrorBanner error={costsQuery.error} onRetry={() => void costsQuery.refetch()} /> : null}

      {costsQuery.isPending ? (
        <div className="stack-gap">
          <Skeleton height={16} />
          <Skeleton height={120} />
        </div>
      ) : series.length === 0 ? (
        <EmptyState
          title="No cost recorded for the window"
          hint="The endpoint answered, but no run has a non-zero cost_cents in the trailing 30 days. Costs are recorded per run once the pricing hook prices model usage."
        />
      ) : (
        <>
          <div className="summary-grid">
            <SummaryStat label="Total cost" value={formatCents(report?.totalCostCents ?? 0)} accent="warning" />
            <SummaryStat label="Runs in series" value={formatNumber(totalRuns)} accent="info" />
            <SummaryStat label="Grouping" value={groupBy} />
          </div>
          {groupBy === 'day' ? (
            <div className="usage-chart" aria-label="Cost per day">
              {series.slice(-14).map((row) => (
                <div key={row.bucket ?? row.agentId ?? row.model} className="usage-bar-wrap" title={`${row.bucket}: ${formatCents(row.costCents)} over ${formatNumber(row.runs)} runs`}>
                  <div
                    className="usage-bar"
                    style={{ height: `${Math.max(Math.round((row.costCents / (maxCost || 1)) * 100), 2)}%` }}
                  />
                </div>
              ))}
            </div>
          ) : null}
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>{groupBy === 'day' ? 'Day' : groupBy === 'agent' ? 'Agent' : 'Model'}</th>
                  <th>Runs</th>
                  <th>Cost</th>
                </tr>
              </thead>
              <tbody>
                {series.map((row, index) => (
                  <tr key={`${labelFor(row)}-${index}`}>
                    <td>
                      <code className="config-cell">{labelFor(row)}</code>
                    </td>
                    <td>{formatNumber(row.runs)}</td>
                    <td>
                      <strong>{formatCents(row.costCents)}</strong>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </article>
  )
}

export function AnalyticsView() {
  const metricsQuery = useMetrics()
  const metrics = metricsQuery.data
  const routes = useMemo(() => routeHistograms(metrics), [metrics])
  const allSummaries = useMemo(() => routes.map((row) => row.summary), [routes])
  const totalRequests = useMemo(() => allSummaries.reduce((sum, summary) => sum + summary.count, 0), [allSummaries])
  const meanSeconds = useMemo(() => {
    const count = allSummaries.reduce((sum, summary) => sum + summary.count, 0)
    const sum = allSummaries.reduce((sum, summary) => sum + summary.sum, 0)
    return count > 0 ? sum / count : null
  }, [allSummaries])

  const p50 = useMemo(() => mergedQuantile(allSummaries, 0.5), [allSummaries])
  const p95 = useMemo(() => mergedQuantile(allSummaries, 0.95), [allSummaries])
  const p99 = useMemo(() => mergedQuantile(allSummaries, 0.99), [allSummaries])

  const runsCounter = metrics ? counterValue(metrics.counts, RUNS_COUNTER) : null
  const toolsCounter = metrics ? counterValue(metrics.counts, TOOLS_COUNTER) : null

  const latencySection = () => {
    if (metricsQuery.isPending) {
      return (
        <div className="stack-gap">
          <Skeleton height={16} />
          <Skeleton height={16} />
        </div>
      )
    }
    if (routes.length === 0) {
      return (
        <EmptyState
          title="No latency histograms reported"
          hint="/metrics?format=json carries no agentos_request_duration_seconds entries yet. They appear once traffic flows through the metrics middleware."
        />
      )
    }
    return (
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Route</th>
              <th>Method</th>
              <th>Status</th>
              <th>Count</th>
              <th>Mean</th>
              <th>p50</th>
              <th>p95</th>
              <th>p99</th>
            </tr>
          </thead>
          <tbody>
            {routes.map((row) => (
              <tr key={row.key}>
                <td>
                  <code className="config-cell">{row.route}</code>
                </td>
                <td>{row.method}</td>
                <td>{row.status}</td>
                <td>{formatNumber(row.summary.count)}</td>
                <td>{formatSeconds(row.summary.count > 0 ? row.summary.sum / row.summary.count : null)}</td>
                <td>{formatSeconds(row.summary.p50)}</td>
                <td>{formatSeconds(row.summary.p95)}</td>
                <td>{formatSeconds(row.summary.p99)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    )
  }

  return (
    <>
      <PageHeader
        eyebrow="Observability"
        title="Analytics"
        actions={<span className="form-note">GET /metrics?format=json · GET /usage/costs</span>}
      />

      {metricsQuery.isError ? <ErrorBanner error={metricsQuery.error} onRetry={() => void metricsQuery.refetch()} /> : null}

      <section className="kpi-grid" aria-label="Latency percentiles and queue depth">
        <article className="kpi-card">
          <p>Latency p50</p>
          <div className="kpi-row">
            {metricsQuery.isPending ? <Skeleton height={30} width={90} /> : <strong>{formatSeconds(p50)}</strong>}
            {!metricsQuery.isPending ? <span className="delta neutral">all routes merged</span> : null}
          </div>
        </article>
        <article className="kpi-card">
          <p>Latency p95</p>
          <div className="kpi-row">
            {metricsQuery.isPending ? <Skeleton height={30} width={90} /> : <strong>{formatSeconds(p95)}</strong>}
            {!metricsQuery.isPending ? <span className="delta neutral">mean {formatSeconds(meanSeconds)}</span> : null}
          </div>
        </article>
        <article className="kpi-card">
          <p>Latency p99</p>
          <div className="kpi-row">
            {metricsQuery.isPending ? <Skeleton height={30} width={90} /> : <strong>{formatSeconds(p99)}</strong>}
            {!metricsQuery.isPending ? <span className="delta neutral">{formatNumber(totalRequests)} requests observed</span> : null}
          </div>
        </article>
        <article className="kpi-card">
          <p>Queue depth</p>
          <div className="kpi-row">
            {metricsQuery.isPending ? <Skeleton height={30} width={90} /> : <strong>{formatNumber(metrics?.queueLength ?? 0)}</strong>}
            {!metricsQuery.isPending ? <span className="delta neutral">queue_length gauge</span> : null}
          </div>
        </article>
      </section>

      <section className="content-grid">
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Latency</p>
              <h3>Request duration by route</h3>
            </div>
            <span className="form-note">agentos_request_duration_seconds histograms</span>
          </div>
          {latencySection()}
        </article>

        <article className="panel">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Counters</p>
              <h3>Platform totals</h3>
            </div>
          </div>

          {metricsQuery.isPending ? (
            <div className="stack-gap">
              <Skeleton height={16} />
              <Skeleton height={16} />
            </div>
          ) : runsCounter === null && toolsCounter === null ? (
            <EmptyState
              title="Run / tool counters not exposed by the API"
              hint="agentos_runs_total and agentos_tools_total are defined by the observability package but the /metrics snapshot does not include them until they are wired into the run/tool paths."
            />
          ) : (
            <div className="summary-grid" style={{ gridTemplateColumns: '1fr' }}>
              <SummaryStat
                label="Runs recorded"
                value={runsCounter === null ? 'not exposed by the API' : formatNumber(runsCounter)}
                accent={runsCounter === null ? 'default' : 'info'}
              />
              <SummaryStat
                label="Tool executions"
                value={toolsCounter === null ? 'not exposed by the API' : formatNumber(toolsCounter)}
                accent={toolsCounter === null ? 'default' : 'success'}
              />
            </div>
          )}

          <p className="form-note" style={{ marginTop: 14 }}>
            Everything on this page comes from /metrics?format=json and /usage/costs — nothing is simulated. Per-run
            latency remains visible on each run's timeline.
          </p>
        </article>
      </section>

      <CostPanel />
    </>
  )
}
