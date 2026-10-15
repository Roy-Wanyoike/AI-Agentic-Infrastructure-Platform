// Cost Analytics view (issue #127) — spend trends, agent efficiency, model
// cost comparison, budget forecasting and CSV export.
//
// Wired endpoints (no mocks — every number is a real API response or a
// client-side aggregation of one):
// - GET /usage/costs?from=&to=&group_by=day|agent|model — spend series. Daily
//   buckets are aggregated client-side into weekly (ISO Monday) / monthly
//   views; the same report powers the model comparison table.
// - GET /showback?group_by=agent — per-agent metered tokens/tool-calls/cost
//   (the runs cost ledger), the richest source for efficiency metrics.
// - GET /runs + GET /agents — window-filtered run outcomes per agent and the
//   agent → configured-model mapping used for per-1k-token attribution.
// - GET /budgets — active budget gates; utilization is derived from
//   month-to-date /usage/costs spend (the budgets response itself carries
//   limits only, no consumption).
//
// Honesty rules: a dimension the API does not expose per bucket (e.g. tokens
// per model — showback is agent-scoped) renders with its attribution method
// labeled or an explicit "—", never an invented number. The month-end
// projection is a plain linear extrapolation of month-to-date spend and says
// so.

import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { listBudgets, type Budget } from '../lib/api/budgets'
import { getShowbackReport } from '../lib/api/showback'
import { useAgents, useRuns, useUsageCosts } from '../lib/hooks'
import type { UsageCostBucket } from '../lib/api/usage'
import { formatCents, formatNumber, parseTimestamp } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

// ---------------------------------------------------------------------------
// Date helpers (UTC — the backend buckets by UTC days)
// ---------------------------------------------------------------------------

function isoDate(date: Date): string {
  return date.toISOString().slice(0, 10)
}

function utcDayOffset(offset: number): string {
  const date = new Date()
  date.setUTCHours(0, 0, 0, 0)
  date.setUTCDate(date.getUTCDate() + offset)
  return isoDate(date)
}

function monthStartIso(): string {
  const now = new Date()
  return `${now.getUTCFullYear()}-${String(now.getUTCMonth() + 1).padStart(2, '0')}-01`
}

function utcDayOfMonth(): number {
  return new Date().getUTCDate()
}

function utcDaysInMonth(): number {
  const now = new Date()
  return new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 0)).getUTCDate()
}

/** Monday of the ISO week containing the given YYYY-MM-DD UTC day. */
function isoWeekStart(day: string): string {
  const date = new Date(`${day}T00:00:00Z`)
  const daysSinceMonday = (date.getUTCDay() + 6) % 7
  date.setUTCDate(date.getUTCDate() - daysSinceMonday)
  return isoDate(date)
}

// ---------------------------------------------------------------------------
// Trend aggregation: daily /usage/costs buckets → day / week / month
// ---------------------------------------------------------------------------

type TrendGranularity = 'day' | 'week' | 'month'

type TrendBucket = { label: string; costCents: number; runs: number }

function aggregateTrend(series: UsageCostBucket[], granularity: TrendGranularity): TrendBucket[] {
  const buckets = new Map<string, TrendBucket>()
  for (const row of series) {
    if (!row.bucket) continue
    const label =
      granularity === 'day' ? row.bucket : granularity === 'week' ? isoWeekStart(row.bucket) : row.bucket.slice(0, 7)
    const entry = buckets.get(label) ?? { label, costCents: 0, runs: 0 }
    entry.costCents += row.costCents
    entry.runs += row.runs
    buckets.set(label, entry)
  }
  return [...buckets.values()].sort((a, b) => a.label.localeCompare(b.label))
}

// ---------------------------------------------------------------------------
// Hand-rolled SVG charts (no chart libraries)
// ---------------------------------------------------------------------------

function SpendBarChart({ buckets }: { buckets: TrendBucket[] }) {
  const W = 760
  const H = 240
  const padL = 64
  const padR = 12
  const padT = 16
  const padB = 28
  const innerW = W - padL - padR
  const innerH = H - padT - padB
  const scaleMax = Math.max(...buckets.map((bucket) => bucket.costCents), 0) || 1
  const step = innerW / Math.max(buckets.length, 1)
  const barWidth = Math.min(Math.max(step - 2, 1.5), 34)
  const labelIndexes = buckets.length > 2 ? [0, Math.floor((buckets.length - 1) / 2), buckets.length - 1] : buckets.map((_, index) => index)

  return (
    <svg className="cost-svg" viewBox={`0 0 ${W} ${H}`} role="img" aria-label="Spend per bucket">
      {[0, 0.25, 0.5, 0.75, 1].map((fraction) => {
        const y = padT + innerH * (1 - fraction)
        return (
          <g key={fraction}>
            <line x1={padL} x2={W - padR} y1={y} y2={y} className="cost-grid" />
            <text x={padL - 8} y={y + 3} textAnchor="end" className="cost-axis">
              {formatCents(scaleMax * fraction)}
            </text>
          </g>
        )
      })}
      {buckets.map((bucket, index) => {
        const height = (bucket.costCents / scaleMax) * innerH
        const x = padL + index * step + (step - barWidth) / 2
        return (
          <rect
            key={bucket.label}
            className="cost-bar"
            x={x}
            y={padT + innerH - height}
            width={barWidth}
            height={bucket.costCents > 0 ? Math.max(height, 1.5) : 0}
            rx={3}
          >
            <title>{`${bucket.label}: ${formatCents(bucket.costCents)} across ${formatNumber(bucket.runs)} runs`}</title>
          </rect>
        )
      })}
      {labelIndexes.map((index) => (
        <text key={index} x={padL + index * step + step / 2} y={H - 8} textAnchor="middle" className="cost-axis">
          {buckets[index]?.label}
        </text>
      ))}
    </svg>
  )
}

function ForecastChart({
  cumulative,
  daysElapsed,
  daysInMonth,
  projectedTotal,
}: {
  cumulative: { day: number; costCents: number }[]
  daysElapsed: number
  daysInMonth: number
  projectedTotal: number
}) {
  const W = 760
  const H = 220
  const padL = 64
  const padR = 16
  const padT = 16
  const padB = 28
  const innerW = W - padL - padR
  const innerH = H - padT - padB
  const maxY = Math.max(projectedTotal, ...cumulative.map((point) => point.costCents), 1)
  const xFor = (day: number) => padL + (innerW * (day - 1)) / Math.max(daysInMonth - 1, 1)
  const yFor = (cents: number) => padT + innerH * (1 - cents / maxY)
  const actualPoints = cumulative.map((point) => `${xFor(point.day)},${yFor(point.costCents)}`).join(' ')
  const last = cumulative[cumulative.length - 1]
  const projectionPoints = last
    ? [`${xFor(last.day)},${yFor(last.costCents)}`, `${xFor(daysInMonth)},${yFor(projectedTotal)}`].join(' ')
    : ''
  const gridFractions = [0, 0.25, 0.5, 0.75, 1]

  return (
    <svg className="cost-svg" viewBox={`0 0 ${W} ${H}`} role="img" aria-label="Month-to-date spend with linear month-end projection">
      {gridFractions.map((fraction) => {
        const y = padT + innerH * (1 - fraction)
        return (
          <g key={fraction}>
            <line x1={padL} x2={W - padR} y1={y} y2={y} className="cost-grid" />
            <text x={padL - 8} y={y + 3} textAnchor="end" className="cost-axis">
              {formatCents(maxY * fraction)}
            </text>
          </g>
        )
      })}
      {actualPoints ? <polyline className="cost-line" points={actualPoints} /> : null}
      {projectionPoints ? <polyline className="cost-line-dashed" points={projectionPoints} /> : null}
      {last ? <circle className="cost-dot" cx={xFor(last.day)} cy={yFor(last.costCents)} r={4} /> : null}
      <text x={xFor(1)} y={H - 8} textAnchor="start" className="cost-axis">
        day 1
      </text>
      <text x={xFor(daysElapsed)} y={H - 8} textAnchor="middle" className="cost-axis">
        today (day {daysElapsed})
      </text>
      <text x={xFor(daysInMonth)} y={H - 8} textAnchor="end" className="cost-axis">
        month end
      </text>
    </svg>
  )
}

// ---------------------------------------------------------------------------
// CSV export (client-side blob download)
// ---------------------------------------------------------------------------

function csvCell(value: string | number): string {
  const text = String(value)
  return /[",\n]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text
}

function downloadCsv(filename: string, rows: (string | number)[][]): void {
  const csv = rows.map((row) => row.map(csvCell).join(',')).join('\n')
  const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  URL.revokeObjectURL(url)
}

// ---------------------------------------------------------------------------
// Derived row types
// ---------------------------------------------------------------------------

type EfficiencyRow = {
  agentId: string
  name: string
  runs: number
  completedRuns: number
  toolCalls: number
  tokens: number
  costCents: number
  costPerSuccessfulRun: number | null
  tokensPerRun: number | null
  costPer1kTokens: number | null
}

type ModelRow = {
  model: string
  runs: number
  costCents: number
  avgCostPerRun: number | null
  attributedTokens: number | null
  costPer1kTokens: number | null
}

type BudgetRow = {
  budget: Budget
  scopeLabel: string
  spentCostCents: number
  spentRuns: number
  costUtilPct: number | null
  runUtilPct: number | null
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

const GRANULARITY_OPTIONS: ReadonlyArray<{ value: TrendGranularity; label: string }> = [
  { value: 'day', label: 'Daily' },
  { value: 'week', label: 'Weekly (ISO weeks)' },
  { value: 'month', label: 'Monthly' },
]

export function CostAnalyticsView() {
  const [windowDays, setWindowDays] = useState<30 | 90>(30)
  const [granularity, setGranularity] = useState<TrendGranularity>('day')

  // Half-open [from, to) windows in UTC; `to` is tomorrow so today is included.
  const trendFrom = utcDayOffset(-(windowDays - 1))
  const trendTo = utcDayOffset(1)
  const mtdFrom = monthStartIso()

  const trendDayQuery = useUsageCosts({ groupBy: 'day', from: trendFrom, to: trendTo })
  const trendModelQuery = useUsageCosts({ groupBy: 'model', from: trendFrom, to: trendTo })
  const mtdDayQuery = useUsageCosts({ groupBy: 'day', from: mtdFrom, to: trendTo })
  const mtdAgentQuery = useUsageCosts({ groupBy: 'agent', from: mtdFrom, to: trendTo })
  const showbackQuery = useQuery({
    queryKey: ['showback', trendFrom, trendTo, 'agent'],
    queryFn: () => getShowbackReport({ from: trendFrom, to: trendTo, groupBy: 'agent' }),
    retry: false,
  })
  const budgetsQuery = useQuery({ queryKey: ['budgets'], queryFn: () => listBudgets(false), retry: false })
  const runsQuery = useRuns()
  const agentsQuery = useAgents()

  const trendBuckets = useMemo(
    () => aggregateTrend(trendDayQuery.data?.series ?? [], granularity),
    [trendDayQuery.data, granularity],
  )
  const windowSpendCents = trendDayQuery.data?.totalCostCents ?? 0
  const windowRuns = useMemo(
    () => (trendDayQuery.data?.series ?? []).reduce((sum, row) => sum + row.runs, 0),
    [trendDayQuery.data],
  )

  const daysElapsed = utcDayOfMonth()
  const daysInMonth = utcDaysInMonth()
  const mtdSpendCents = mtdDayQuery.data?.totalCostCents ?? 0
  const mtdRuns = useMemo(
    () => (mtdDayQuery.data?.series ?? []).reduce((sum, row) => sum + row.runs, 0),
    [mtdDayQuery.data],
  )
  const projectedSpendCents = daysElapsed > 0 ? (mtdSpendCents / daysElapsed) * daysInMonth : null
  const projectedRuns = daysElapsed > 0 ? (mtdRuns / daysElapsed) * daysInMonth : null

  const cumulativeMtd = useMemo(() => {
    const byDay = new Map<string, number>()
    for (const row of mtdDayQuery.data?.series ?? []) {
      if (row.bucket) byDay.set(row.bucket, (byDay.get(row.bucket) ?? 0) + row.costCents)
    }
    const now = new Date()
    const out: { day: number; costCents: number }[] = []
    let running = 0
    for (let day = 1; day <= utcDayOfMonth(); day++) {
      running += byDay.get(isoDate(new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), day)))) ?? 0
      out.push({ day, costCents: running })
    }
    return out
  }, [mtdDayQuery.data])

  // Agent metadata: id → name / configured model (used for attribution).
  const agentMeta = useMemo(() => {
    const map = new Map<string, { name: string; model: string }>()
    for (const agent of agentsQuery.data ?? []) {
      map.set(agent.id, { name: agent.name, model: agent.model })
    }
    return map
  }, [agentsQuery.data])

  // Window-filtered successful runs per agent (GET /runs, COMPLETED only).
  const completedByAgent = useMemo(() => {
    const map = new Map<string, number>()
    for (const run of runsQuery.data ?? []) {
      if (!run.agentId) continue
      if (run.status?.toUpperCase() !== 'COMPLETED') continue
      const created = parseTimestamp(run.createdAt)
      if (created === null) continue
      if (created < Date.parse(`${trendFrom}T00:00:00Z`) || created >= Date.parse(`${trendTo}T00:00:00Z`)) continue
      map.set(run.agentId, (map.get(run.agentId) ?? 0) + 1)
    }
    return map
  }, [runsQuery.data, trendFrom, trendTo])

  const efficiencyRows = useMemo<EfficiencyRow[]>(() => {
    const rows = (showbackQuery.data?.rows ?? [])
      .filter((row) => row.scopeRef)
      .map((row) => {
        const completed = completedByAgent.get(row.scopeRef) ?? 0
        const tokens = row.tokensIn + row.tokensOut
        return {
          agentId: row.scopeRef,
          name: agentMeta.get(row.scopeRef)?.name ?? row.scopeRef,
          runs: row.runs,
          completedRuns: completed,
          toolCalls: row.toolCalls,
          tokens,
          costCents: row.costCents,
          costPerSuccessfulRun: completed > 0 ? row.costCents / completed : null,
          tokensPerRun: row.runs > 0 ? tokens / row.runs : null,
          costPer1kTokens: tokens > 0 ? (row.costCents / tokens) * 1000 : null,
        }
      })
    return rows.sort((a, b) => b.costCents - a.costCents)
  }, [showbackQuery.data, agentMeta, completedByAgent])

  const modelRows = useMemo<ModelRow[]>(() => {
    // Token attribution: /showback meters tokens per AGENT; join each agent to
    // its configured model (GET /agents) and roll up per model. Labeled in the
    // table — the API does not expose tokens per serving model directly.
    const attributed = new Map<string, { tokens: number; costCents: number }>()
    for (const row of showbackQuery.data?.rows ?? []) {
      const model = agentMeta.get(row.scopeRef)?.model
      if (!model) continue
      const entry = attributed.get(model) ?? { tokens: 0, costCents: 0 }
      entry.tokens += row.tokensIn + row.tokensOut
      entry.costCents += row.costCents
      attributed.set(model, entry)
    }
    return (trendModelQuery.data?.series ?? [])
      .map((row) => {
        const model = row.model || 'unknown'
        const match = attributed.get(model)
        return {
          model,
          runs: row.runs,
          costCents: row.costCents,
          avgCostPerRun: row.runs > 0 ? row.costCents / row.runs : null,
          attributedTokens: match?.tokens ?? null,
          costPer1kTokens: match && match.tokens > 0 ? (match.costCents / match.tokens) * 1000 : null,
        }
      })
      .sort((a, b) => b.costCents - a.costCents)
  }, [trendModelQuery.data, showbackQuery.data, agentMeta])

  const budgetRows = useMemo<BudgetRow[]>(() => {
    if (!budgetsQuery.data) return []
    const mtdByAgent = new Map<string, { costCents: number; runs: number }>()
    for (const row of mtdAgentQuery.data?.series ?? []) {
      if (!row.agentId) continue
      const entry = mtdByAgent.get(row.agentId) ?? { costCents: 0, runs: 0 }
      entry.costCents += row.costCents
      entry.runs += row.runs
      mtdByAgent.set(row.agentId, entry)
    }
    return budgetsQuery.data
      .filter((budget) => budget.isActive)
      .map((budget) => {
        const scoped =
          budget.scope === 'agent'
            ? mtdByAgent.get(budget.refId)
            : { costCents: mtdSpendCents, runs: mtdRuns }
        const spentCostCents = scoped?.costCents ?? 0
        const spentRuns = scoped?.runs ?? 0
        return {
          budget,
          scopeLabel:
            budget.scope === 'agent'
              ? `agent · ${agentMeta.get(budget.refId)?.name ?? (budget.refId || '—')}`
              : 'organization',
          spentCostCents,
          spentRuns,
          costUtilPct: budget.limitCostCents > 0 ? (spentCostCents / budget.limitCostCents) * 100 : null,
          runUtilPct: budget.limitRuns > 0 ? (spentRuns / budget.limitRuns) * 100 : null,
        }
      })
      .sort((a, b) => (b.costUtilPct ?? -1) - (a.costUtilPct ?? -1))
  }, [budgetsQuery.data, mtdAgentQuery.data, mtdSpendCents, mtdRuns, agentMeta])

  const exportCsv = () => {
    downloadCsv(`agentos-spend-${granularity}-${trendFrom}_to_${trendTo}.csv`, [
      ['bucket', 'runs', 'cost_cents', 'cost_usd'],
      ...trendBuckets.map((bucket) => [
        bucket.label,
        bucket.runs,
        bucket.costCents.toFixed(2),
        (bucket.costCents / 100).toFixed(4),
      ]),
    ])
  }

  const retryTrend = () => {
    void trendDayQuery.refetch()
    void mtdDayQuery.refetch()
  }

  const trendPending = trendDayQuery.isPending || mtdDayQuery.isPending
  const trendError = trendDayQuery.error ?? mtdDayQuery.error
  const windowAvgPerRun = windowRuns > 0 ? windowSpendCents / windowRuns : null

  return (
    <>
      <PageHeader
        eyebrow="FinOps"
        title="Cost Analytics"
        actions={<span className="form-note">GET /usage/costs · GET /showback · GET /budgets</span>}
      />

      {trendError ? <ErrorBanner error={trendError} onRetry={retryTrend} /> : null}

      <section className="kpi-grid" aria-label="Cost headline numbers">
        <article className="kpi-card">
          <p>Spend ({windowDays}d)</p>
          <div className="kpi-row">
            {trendPending ? <Skeleton height={30} width={90} /> : <strong>{formatCents(windowSpendCents)}</strong>}
            {!trendPending ? <span className="delta neutral">{formatNumber(windowRuns)} runs in window</span> : null}
          </div>
        </article>
        <article className="kpi-card">
          <p>Month to date</p>
          <div className="kpi-row">
            {mtdDayQuery.isPending ? <Skeleton height={30} width={90} /> : <strong>{formatCents(mtdSpendCents)}</strong>}
            {!mtdDayQuery.isPending ? <span className="delta neutral">since {mtdFrom}</span> : null}
          </div>
        </article>
        <article className="kpi-card">
          <p>Projected month end</p>
          <div className="kpi-row">
            {mtdDayQuery.isPending ? (
              <Skeleton height={30} width={90} />
            ) : (
              <strong>{projectedSpendCents === null ? '—' : formatCents(projectedSpendCents)}</strong>
            )}
            {!mtdDayQuery.isPending ? <span className="delta neutral">linear from MTD pace</span> : null}
          </div>
        </article>
        <article className="kpi-card">
          <p>Avg cost / run</p>
          <div className="kpi-row">
            {trendPending ? (
              <Skeleton height={30} width={90} />
            ) : (
              <strong>{windowAvgPerRun === null ? '—' : formatCents(windowAvgPerRun)}</strong>
            )}
            {!trendPending ? <span className="delta neutral">{windowDays}-day window</span> : null}
          </div>
        </article>
      </section>

      {/* ------------------------------------------------ Spend trend ------ */}
      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Spend trend</p>
            <h3>Cost over time</h3>
          </div>
          <div className="cost-controls">
            <div className="field">
              <label htmlFor="cost-window">Window</label>
              <select
                id="cost-window"
                value={windowDays}
                onChange={(event) => setWindowDays(event.target.value === '90' ? 90 : 30)}
              >
                <option value={30}>Last 30 days</option>
                <option value={90}>Last 90 days</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="cost-granularity">Granularity</label>
              <select
                id="cost-granularity"
                value={granularity}
                onChange={(event) => setGranularity(event.target.value as TrendGranularity)}
              >
                {GRANULARITY_OPTIONS.map((option) => (
                  <option key={option.value} value={option.value}>
                    {option.label}
                  </option>
                ))}
              </select>
            </div>
            <button type="button" className="ghost-button small" disabled={trendBuckets.length === 0} onClick={exportCsv}>
              Export CSV
            </button>
          </div>
        </div>
        <p className="form-note">
          GET /usage/costs?group_by=day — weekly/monthly buckets are aggregated client-side from the daily series
          ({trendFrom} → {trendTo}, half-open window).
        </p>

        {trendBuckets.length === 0 && !trendPending ? (
          <EmptyState
            title="No cost recorded for this window"
            hint="The endpoint answered, but no run has a non-zero cost_cents in the selected window. Costs are recorded per run once the pricing hook prices model usage."
          />
        ) : (
          <>
            <SpendBarChart buckets={trendBuckets} />
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>{granularity === 'day' ? 'Day' : granularity === 'week' ? 'Week starting' : 'Month'}</th>
                    <th>Runs</th>
                    <th>Cost</th>
                  </tr>
                </thead>
                <tbody>
                  {[...trendBuckets].reverse().map((bucket) => (
                    <tr key={bucket.label}>
                      <td>
                        <code className="config-cell">{bucket.label}</code>
                      </td>
                      <td>{formatNumber(bucket.runs)}</td>
                      <td>
                        <strong>{formatCents(bucket.costCents)}</strong>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        )}
      </article>

      {/* ------------------------------------------------ Efficiency ------- */}
      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Agent efficiency</p>
            <h3>Cost per run, per successful run and per token</h3>
          </div>
          <span className="form-note">GET /showback?group_by=agent · GET /runs · GET /agents</span>
        </div>

        {showbackQuery.isError ? (
          <ErrorBanner error={showbackQuery.error} onRetry={() => void showbackQuery.refetch()} />
        ) : null}

        {showbackQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={120} />
          </div>
        ) : efficiencyRows.length === 0 ? (
          <EmptyState
            title="No metered agent usage in this window"
            hint="/showback returned no agent rows: either no runs carried cost/tokens in the window or the showback source has nothing metered yet."
          />
        ) : (
          <>
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Agent</th>
                    <th>Runs</th>
                    <th>Successful</th>
                    <th>Tool calls</th>
                    <th>Tokens</th>
                    <th>Cost</th>
                    <th>Cost / successful run</th>
                    <th>Tokens / run</th>
                    <th>Cost / 1k tokens</th>
                  </tr>
                </thead>
                <tbody>
                  {efficiencyRows.map((row) => (
                    <tr key={row.agentId}>
                      <td>
                        <strong>{row.name}</strong>
                        <div className="form-note">{row.agentId}</div>
                      </td>
                      <td>{formatNumber(row.runs)}</td>
                      <td>{formatNumber(row.completedRuns)}</td>
                      <td>{formatNumber(row.toolCalls)}</td>
                      <td>{formatNumber(row.tokens)}</td>
                      <td>
                        <strong>{formatCents(row.costCents)}</strong>
                      </td>
                      <td>{row.costPerSuccessfulRun === null ? '—' : formatCents(row.costPerSuccessfulRun)}</td>
                      <td>{row.tokensPerRun === null ? '—' : formatNumber(Math.round(row.tokensPerRun))}</td>
                      <td>{row.costPer1kTokens === null ? '—' : formatCents(row.costPer1kTokens)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className="form-note">
              "Successful" counts COMPLETED runs created inside the same window (GET /runs); showback run counts are the
              metered ledger totals, so the two need not match 1:1.
            </p>
            {showbackQuery.data && showbackQuery.data.notes.length > 0 ? (
              <ul className="cost-notes">
                {showbackQuery.data.notes.map((note) => (
                  <li key={note}>{note}</li>
                ))}
              </ul>
            ) : null}
          </>
        )}
      </article>

      {/* ------------------------------------------------ Model comparison - */}
      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Model cost comparison</p>
            <h3>Spend by serving model</h3>
          </div>
          <span className="form-note">GET /usage/costs?group_by=model</span>
        </div>

        {trendModelQuery.isError ? (
          <ErrorBanner error={trendModelQuery.error} onRetry={() => void trendModelQuery.refetch()} />
        ) : null}

        {trendModelQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={120} />
          </div>
        ) : modelRows.length === 0 ? (
          <EmptyState
            title="No model spend recorded for this window"
            hint="No run in the window was attributed to a model in the cost ledger."
          />
        ) : (
          <>
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Model</th>
                    <th>Runs</th>
                    <th>Cost</th>
                    <th>Avg cost / run</th>
                    <th>Tokens (via agent attribution)</th>
                    <th>Cost / 1k tokens (attributed)</th>
                  </tr>
                </thead>
                <tbody>
                  {modelRows.map((row) => (
                    <tr key={row.model}>
                      <td>
                        <code className="config-cell">{row.model}</code>
                      </td>
                      <td>{formatNumber(row.runs)}</td>
                      <td>
                        <strong>{formatCents(row.costCents)}</strong>
                      </td>
                      <td>{row.avgCostPerRun === null ? '—' : formatCents(row.avgCostPerRun)}</td>
                      <td>{row.attributedTokens === null ? '—' : formatNumber(row.attributedTokens)}</td>
                      <td>{row.costPer1kTokens === null ? '—' : formatCents(row.costPer1kTokens)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className="form-note">
              Runs, cost and avg cost/run come straight from the cost ledger (group_by=model). The API does not expose
              tokens per serving model, so the per-1k-token column rolls /showback tokens up through each agent's
              configured model (GET /agents) — an attribution, not a per-request measurement; "—" when the model has no
              attributed token volume.
            </p>
          </>
        )}
      </article>

      {/* ------------------------------------------------ Forecasting ------ */}
      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Budget forecasting</p>
            <h3>Month-end projection and budget gates</h3>
          </div>
          <span className="form-note">GET /usage/costs (MTD) · GET /budgets</span>
        </div>

        {budgetsQuery.isError ? (
          <div className="error-banner" role="alert">
            <div>
              <strong>Budgets unavailable</strong>
              <span>
                {describeError(budgetsQuery.error)} — utilization checks need GET /budgets (runs.execute, MEMBER+); the
                projection below still works from month-to-date spend.
              </span>
            </div>
            <button type="button" className="ghost-button small" onClick={() => void budgetsQuery.refetch()}>
              Retry
            </button>
          </div>
        ) : null}

        <div className="summary-grid">
          <SummaryStat label="Month-to-date spend" value={mtdDayQuery.isPending ? '…' : formatCents(mtdSpendCents)} accent="warning" />
          <SummaryStat
            label="Average / day (MTD)"
            value={mtdDayQuery.isPending || daysElapsed === 0 ? '…' : formatCents(mtdSpendCents / daysElapsed)}
            accent="info"
          />
          <SummaryStat
            label="Projected month-end spend"
            value={mtdDayQuery.isPending || projectedSpendCents === null ? '…' : formatCents(projectedSpendCents)}
            accent="warning"
          />
          <SummaryStat
            label="Projected month-end runs"
            value={mtdDayQuery.isPending || projectedRuns === null ? '…' : formatNumber(Math.round(projectedRuns))}
            accent="info"
          />
        </div>

        {mtdDayQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={160} />
          </div>
        ) : cumulativeMtd.length === 0 ? (
          <EmptyState
            title="No month-to-date spend yet"
            hint="The projection needs at least one metered day in the current month (GET /usage/costs?group_by=day)."
          />
        ) : (
          <ForecastChart
            cumulative={cumulativeMtd}
            daysElapsed={daysElapsed}
            daysInMonth={daysInMonth}
            projectedTotal={projectedSpendCents ?? 0}
          />
        )}
        <p className="form-note">
          Linear projection: month-to-date spend ÷ days elapsed × days in month ({daysElapsed}/{daysInMonth} days). The
          dashed segment is the projection, not observed data.
        </p>

        <div className="cost-budget-section">
          <h4>Active budgets ({formatNumber(budgetRows.length)})</h4>
          {budgetsQuery.isPending ? (
            <div className="stack-gap">
              <Skeleton height={16} />
              <Skeleton height={16} />
            </div>
          ) : !budgetsQuery.isError && budgetRows.length === 0 ? (
            <EmptyState
              title="No active budgets"
              hint="Nothing is gated: create budgets with POST /budgets (OWNER/ADMIN) — org or agent scope, monthly run/cost limits."
            />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Budget</th>
                    <th>Scope</th>
                    <th>MTD spend</th>
                    <th>Cost limit</th>
                    <th>Cost utilization</th>
                    <th>Run limit</th>
                    <th>Run utilization</th>
                    <th>Status</th>
                  </tr>
                </thead>
                <tbody>
                  {budgetRows.map((row) => {
                    const alert = (row.costUtilPct ?? 0) >= 80 || (row.runUtilPct ?? 0) >= 80
                    return (
                      <tr key={row.budget.id} className={alert ? 'cost-budget-alert' : undefined}>
                        <td>
                          <code className="config-cell">{row.budget.id || '—'}</code>
                        </td>
                        <td>{row.scopeLabel}</td>
                        <td>
                          {formatCents(row.spentCostCents)}
                          <div className="form-note">{formatNumber(row.spentRuns)} runs</div>
                        </td>
                        <td>{row.budget.limitCostCents > 0 ? formatCents(row.budget.limitCostCents) : 'unlimited'}</td>
                        <td>
                          {row.costUtilPct === null ? (
                            '—'
                          ) : (
                            <div className="cost-util">
                              <div className="meter">
                                <span style={{ width: `${Math.min(Math.round(row.costUtilPct), 100)}%` }} />
                              </div>
                              <span>{Math.round(row.costUtilPct)}%</span>
                            </div>
                          )}
                        </td>
                        <td>{row.budget.limitRuns > 0 ? formatNumber(row.budget.limitRuns) : 'unlimited'}</td>
                        <td>
                          {row.runUtilPct === null ? (
                            '—'
                          ) : (
                            <div className="cost-util">
                              <div className="meter">
                                <span style={{ width: `${Math.min(Math.round(row.runUtilPct), 100)}%` }} />
                              </div>
                              <span>{Math.round(row.runUtilPct)}%</span>
                            </div>
                          )}
                        </td>
                        <td>
                          {alert ? (
                            <strong className="cost-alert-text">
                              {(row.costUtilPct ?? 0) >= 100 || (row.runUtilPct ?? 0) >= 100 ? 'over limit' : '≥80% of budget'}
                            </strong>
                          ) : (
                            'ok'
                          )}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
          <p className="form-note">
            Utilization is derived from month-to-date spend of the SAME cost ledger the budget gate reads; a budget
            response carries limits only. Agent-scoped budgets match the agent's month-to-date totals.
          </p>
        </div>
      </article>
    </>
  )
}
