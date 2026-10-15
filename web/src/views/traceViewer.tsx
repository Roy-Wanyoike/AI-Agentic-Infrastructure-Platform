// Real-time execution trace viewer (issue #126).
//
// Rendered by RunTimeline (views/runTimeline.tsx) inside the run detail flow —
// one panel that adds, on top of the step timeline:
//   * execution waterfall: pure CSS bars per step, scaled to duration on an
//     aligned relative-time axis, with a live "now" cursor while the run runs
//   * per-step latency breakdown (wait before the step + active run time)
//   * token + cost attribution per step and rolled up by step type
//   * error details incl. derived retry attempts (repeated executions of the
//     same logical index/type/name — the backend records one row per attempt)
//   * trace id display + copy via GET /runs/{id}/trace (issue #114)
//   * live SSE event log driven by the existing useRunEvents hook
//
// Liveness model (live-append, never clobbers history): step data lives in
// the shared react-query cache ('runs', id, 'steps') that the run detail's
// SSE subscription invalidates (plus a polling fallback in useRunSteps).
// GET /runs/{id}/steps returns the FULL append-only step history on every
// refetch and react-query structural sharing keeps unchanged rows stable, so
// live updates grow the trace in place instead of resetting it. This
// component additionally subscribes with useRunEvents to (a) keep its own
// append-only SSE event log and (b) invalidate steps (throttled) so it stays
// live even when mounted outside the run detail flow.

import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError } from '../lib/api/client'
import { getRunTrace } from '../lib/api/runs'
import { asNumber, asString, pickField, type RunEvent, type RunStep } from '../lib/api/types'
import { useRunEvents, useRunSteps } from '../lib/hooks'
import { formatCost, formatDurationMs, formatNumber, parseTimestamp } from '../lib/format'
import { EmptyState, Skeleton } from './shared'

// ---------------------------------------------------------------------------
// Step helpers
// ---------------------------------------------------------------------------

/** Logical step index: the runtime loop index recorded in input_meta.index (falls back to the list position). */
function logicalIndex(step: RunStep): number {
  return asNumber(pickField(step.inputMeta, 'index', 'stepIndex', 'sequence')) ?? step.index
}

/** Provider name (model steps) or tool name (tool steps) recorded in input_meta. */
function stepName(step: RunStep): string | null {
  return asString(pickField(step.inputMeta, 'name', 'provider', 'model', 'tool')) ?? null
}

function metaText(meta: Record<string, unknown>, keys: string[]): string | null {
  for (const key of keys) {
    const value = asString(pickField(meta, key))
    if (value && value.trim()) return value
  }
  return null
}

function metaJSON(meta: Record<string, unknown>): string | null {
  const entries = Object.entries(meta)
  if (entries.length === 0) return null
  try {
    return JSON.stringify(meta, null, 2)
  } catch {
    return null
  }
}

function truncate(text: string, max: number): string {
  return text.length <= max ? text : `${text.slice(0, max)}…`
}

/** Stable identity for merge/dedup — prefer the persisted row id. */
function stepKey(step: RunStep): string {
  if (step.id) return `id:${step.id}`
  return `syn:${step.runId}:${logicalIndex(step)}:${step.stepType}:${stepName(step) ?? ''}:${step.startedAt ?? step.createdAt ?? ''}`
}

function stepStart(step: RunStep): number | null {
  return parseTimestamp(step.startedAt ?? step.createdAt)
}

function stepEnd(step: RunStep, now: number): number | null {
  const completed = parseTimestamp(step.completedAt)
  if (completed !== null) return completed
  const start = stepStart(step)
  if (start !== null && (step.status === 'pending' || step.status === 'running')) return now
  return null
}

function sortSteps(steps: RunStep[]): RunStep[] {
  return [...steps].sort((a, b) => {
    const byIndex = logicalIndex(a) - logicalIndex(b)
    if (byIndex !== 0) return byIndex
    const aStart = stepStart(a) ?? Number.POSITIVE_INFINITY
    const bStart = stepStart(b) ?? Number.POSITIVE_INFINITY
    if (aStart !== bStart) return aStart - bStart
    return (a.id || '').localeCompare(b.id || '')
  })
}

// ---------------------------------------------------------------------------
// Retry attempts (derived)
// ---------------------------------------------------------------------------

type AttemptInfo = { attempt: number; attempts: number; retryAfterFailure: boolean }

/**
 * The backend records one row per execution and does not carry a retry
 * counter, so retries are derived honestly: a step executed more than once
 * repeats with the same logical index + type + name. Those repeats form an
 * attempt group and are labeled "attempt n/m". A model step followed by a
 * tool step in the same loop iteration (same index) never shares the group
 * key, so normal fan-out is not misreported as a retry.
 */
function attemptInfo(steps: RunStep[]): Map<string, AttemptInfo> {
  const groups = new Map<string, RunStep[]>()
  for (const step of steps) {
    const key = `${logicalIndex(step)}|${step.stepType}|${stepName(step) ?? ''}`
    const group = groups.get(key)
    if (group) group.push(step)
    else groups.set(key, [step])
  }
  const info = new Map<string, AttemptInfo>()
  for (const group of groups.values()) {
    if (group.length < 2) continue
    group.forEach((step, position) => {
      info.set(stepKey(step), {
        attempt: position + 1,
        attempts: group.length,
        retryAfterFailure: position > 0 && group[position - 1].status === 'failed',
      })
    })
  }
  return info
}

// ---------------------------------------------------------------------------
// Waterfall geometry
// ---------------------------------------------------------------------------

type WaterfallRow = {
  step: RunStep
  startPct: number
  widthPct: number
  /** Idle time between the previous step's end and this step's start (ms). */
  waitMs: number | null
  /** Step execution time start → end (ms); null while still running. */
  activeMs: number | null
}

type Waterfall = {
  span: number
  rows: WaterfallRow[]
  nowPct: number | null
}

function buildWaterfall(steps: RunStep[], live: boolean, now: number): Waterfall | null {
  const bounds: number[] = []
  for (const step of steps) {
    const start = stepStart(step)
    if (start !== null) bounds.push(start)
    const end = stepEnd(step, now)
    if (end !== null) bounds.push(end)
  }
  if (bounds.length === 0) return null
  const t0 = Math.min(...bounds)
  let t1 = Math.max(...bounds)
  if (live) t1 = Math.max(t1, now)
  const span = Math.max(t1 - t0, 1)
  const pct = (ms: number) => Math.min(100, Math.max(0, ((ms - t0) / span) * 100))

  let prevEnd: number | null = null
  const rows = steps.map((step) => {
    const start = stepStart(step)
    const end = stepEnd(step, now)
    const startPct = start === null ? (prevEnd === null ? 0 : pct(prevEnd)) : pct(start)
    const endPct = end === null ? startPct : pct(end)
    const waitMs = start !== null && prevEnd !== null && start > prevEnd ? start - prevEnd : null
    const activeMs = start !== null && end !== null && end >= start ? end - start : null
    if (end !== null) prevEnd = end
    else if (start !== null) prevEnd = start
    return { step, startPct, widthPct: Math.max(endPct - startPct, 0), waitMs, activeMs }
  })
  return { span, rows, nowPct: live ? pct(now) : null }
}

// ---------------------------------------------------------------------------
// Attribution rollup
// ---------------------------------------------------------------------------

type AttributionRow = { stepType: string; steps: number; tokens?: number; cost?: number }

function attributionRows(steps: RunStep[]): AttributionRow[] {
  const byType = new Map<string, AttributionRow>()
  for (const step of steps) {
    const key = step.stepType || 'unknown'
    const row = byType.get(key) ?? { stepType: key, steps: 0 }
    row.steps += 1
    const total = step.tokenUsage.totalTokens
    if (total !== undefined) row.tokens = (row.tokens ?? 0) + total
    if (step.cost !== undefined) row.cost = (row.cost ?? 0) + step.cost
    byType.set(key, row)
  }
  return [...byType.values()].sort((a, b) => b.steps - a.steps)
}

// ---------------------------------------------------------------------------
// Clipboard
// ---------------------------------------------------------------------------

async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // fall through to the legacy path (clipboard API needs a secure context)
  }
  try {
    const area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', '')
    area.style.position = 'fixed'
    area.style.opacity = '0'
    document.body.appendChild(area)
    area.select()
    const copied = document.execCommand('copy')
    document.body.removeChild(area)
    return copied
  } catch {
    return false
  }
}

function useCopyToClipboard(resetMs = 2000): [boolean, (text: string) => void] {
  const [copied, setCopied] = useState(false)
  const timer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timer.current), [])
  const copy = (text: string) => {
    void copyText(text).then((ok) => {
      if (!ok) return
      setCopied(true)
      window.clearTimeout(timer.current)
      timer.current = window.setTimeout(() => setCopied(false), resetMs)
    })
  }
  return [copied, copy]
}

// ---------------------------------------------------------------------------
// Live SSE event log (existing useRunEvents hook — no new deps)
// ---------------------------------------------------------------------------

const EVENT_BUFFER = 40

function eventClock(event: RunEvent): string {
  const ts = parseTimestamp(event.createdAt)
  return ts === null ? '—' : new Date(ts).toLocaleTimeString()
}

function eventSummary(event: RunEvent): string {
  const parts: string[] = []
  const status = asString(pickField(event.payload, 'status'))
  if (status) parts.push(`→ ${status}`)
  const output = asString(pickField(event.payload, 'output', 'result'))
  if (output) parts.push(truncate(output, 120))
  return parts.join(' · ') || '(no payload summary)'
}

function eventPayloadJSON(event: RunEvent): string {
  try {
    return JSON.stringify(event.payload, null, 2)
  } catch {
    return '{}'
  }
}

/**
 * Subscribes to the run's SSE stream via the existing useRunEvents hook and
 * keeps an append-only ring buffer of the latest events (live-append — the
 * log never rewrites earlier entries, it only trims to the buffer cap).
 */
function useLiveEventLog(runId: string): RunEvent[] {
  const queryClient = useQueryClient()
  const [events, setEvents] = useState<RunEvent[]>([])
  const lastInvalidateRef = useRef(0)

  useRunEvents(runId, (event: RunEvent) => {
    setEvents((previous) => {
      const next = [...previous, event]
      return next.length > EVENT_BUFFER ? next.slice(next.length - EVENT_BUFFER) : next
    })
    // Keep the steps fresh even when this viewer is mounted outside the run
    // detail flow (standalone use). Throttled — when mounted under
    // RunTimeline the parent's SSE handler already invalidates every event.
    const now = Date.now()
    if (now - lastInvalidateRef.current > 2000) {
      lastInvalidateRef.current = now
      void queryClient.invalidateQueries({ queryKey: ['runs', runId, 'steps'] })
    }
  })

  return events
}

function LiveEventLog({ events }: { events: RunEvent[] }) {
  return (
    <details className="trace-event-log">
      <summary>Live SSE event log{events.length > 0 ? ` (${events.length})` : ''} — {'GET /runs/{id}/events'}</summary>
      {events.length === 0 ? (
        <p className="form-note">Waiting for the first event on the run's SSE stream…</p>
      ) : (
        <ul>
          {events.map((event, index) => (
            <li key={`${event.createdAt ?? 'no-ts'}-${index}`} title={eventPayloadJSON(event)}>
              <span className="trace-event-time">{eventClock(event)}</span>
              <span className="trace-event-name">{event.name || event.type || 'event'}</span>
              <span className="trace-event-detail">{eventSummary(event)}</span>
            </li>
          ))}
        </ul>
      )}
    </details>
  )
}

// ---------------------------------------------------------------------------
// Trace id row (GET /runs/{id}/trace, issue #114)
// ---------------------------------------------------------------------------

function TraceIdRow({ runId, live }: { runId: string; live: boolean }) {
  // 404 (trace_not_available) is an expected "no trace recorded" answer, so
  // no retry storm: a single attempt, refreshed lightly while the run is live
  // (the trace id registers at run start when tracing is enabled).
  const traceQuery = useQuery({
    queryKey: ['runs', runId, 'trace'],
    queryFn: () => getRunTrace(runId),
    retry: false,
    staleTime: 30_000,
    refetchInterval: live ? 20_000 : false,
  })
  const [copied, copy] = useCopyToClipboard()
  const traceId = traceQuery.data?.traceId
  const endpoint = `GET /runs/${runId.slice(0, 8)}…/trace`

  if (traceId) {
    return (
      <div className="trace-id-row">
        <span className="trace-id-label">Trace ID</span>
        <code className="trace-id-value" title={traceId}>
          {traceId}
        </code>
        <button type="button" className="ghost-button small" onClick={() => copy(traceId)}>
          {copied ? 'Copied ✓' : 'Copy'}
        </button>
        <span className="trace-id-source">{endpoint}</span>
      </div>
    )
  }
  if (traceQuery.isPending) {
    return (
      <div className="trace-id-row muted">
        <span>Resolving trace id… ({endpoint})</span>
      </div>
    )
  }
  if (traceQuery.isError) {
    const status = traceQuery.error instanceof ApiError ? traceQuery.error.status : undefined
    const note =
      status === 404
        ? 'No trace id recorded for this run (tracing disabled, or the in-process trace registry evicted/restarted).'
        : `Trace id unavailable: ${traceQuery.error instanceof Error ? traceQuery.error.message : 'unknown error'}`
    return (
      <div className="trace-id-row muted">
        <span>{note}</span>
      </div>
    )
  }
  return null
}

// ---------------------------------------------------------------------------
// Waterfall block
// ---------------------------------------------------------------------------

function WaterfallBlock({ waterfall, attempts }: { waterfall: Waterfall; attempts: Map<string, AttemptInfo> }) {
  const ticks = [0, 0.25, 0.5, 0.75, 1]
  // The "now" cursor overlays the track column only; the calc keeps it aligned
  // with the shared axis regardless of the label/duration column widths.
  const cursorLeft =
    waterfall.nowPct === null
      ? undefined
      : `calc(var(--trace-label-col) + var(--trace-col-gap) + (100% - var(--trace-label-col) - var(--trace-dur-col) - var(--trace-col-gap) * 2) * ${waterfall.nowPct / 100})`

  return (
    <section className="waterfall" aria-label="Execution waterfall">
      <div className="waterfall-axis" aria-hidden="true">
        <span />
        <span className="waterfall-axis-track">
          {ticks.map((tick) => (
            <span
              key={tick}
              className={`waterfall-tick${tick === 0 ? ' first' : ''}${tick === 1 ? ' last' : ''}`}
              style={{ left: `${tick * 100}%` }}
            >
              {formatDurationMs(tick * waterfall.span)}
            </span>
          ))}
        </span>
        <span />
      </div>
      <div className="waterfall-rows">
        {waterfall.rows.map(({ step, startPct, widthPct, activeMs }) => {
          const attempt = attempts.get(stepKey(step))
          const name = stepName(step)
          const label = `#${logicalIndex(step)} ${step.stepType}${attempt && attempt.attempt > 1 ? ` · retry ${attempt.attempt}` : ''}`
          const title = [
            `#${logicalIndex(step)} ${step.stepType}${name ? ` (${name})` : ''}`,
            `status: ${step.status}`,
            activeMs !== null ? `duration: ${formatDurationMs(activeMs)}` : 'duration: running…',
          ].join(' · ')
          return (
            <div className="waterfall-row" key={stepKey(step)} data-status={step.status}>
              <span className="waterfall-label" title={title}>
                {label}
              </span>
              <span className="waterfall-track">
                <span
                  className="waterfall-bar"
                  data-status={step.status}
                  style={{ left: `${startPct.toFixed(2)}%`, width: `${widthPct.toFixed(2)}%` }}
                  title={title}
                />
              </span>
              <span className="waterfall-duration">{activeMs !== null ? formatDurationMs(activeMs) : 'running…'}</span>
            </div>
          )
        })}
        {cursorLeft ? <span className="waterfall-cursor" style={{ left: cursorLeft }} title="now" /> : null}
      </div>
    </section>
  )
}

// ---------------------------------------------------------------------------
// Attribution block
// ---------------------------------------------------------------------------

function AttributionBlock({ rows }: { rows: AttributionRow[] }) {
  return (
    <section className="trace-attribution" aria-label="Token and cost attribution by step type">
      <p className="run-output-label">Attribution by step type</p>
      <table>
        <thead>
          <tr>
            <th scope="col">Step type</th>
            <th scope="col">Steps</th>
            <th scope="col">Tokens</th>
            <th scope="col">Attributed cost</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.stepType}>
              <td>
                <span className={`step-type step-type-${row.stepType}`}>{row.stepType}</span>
              </td>
              <td>{row.steps}</td>
              <td>{row.tokens !== undefined ? formatNumber(row.tokens) : '—'}</td>
              <td>{row.cost !== undefined ? formatCost(row.cost) : '—'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  )
}

// ---------------------------------------------------------------------------
// Step card
// ---------------------------------------------------------------------------

function TraceStepCard({
  step,
  attempt,
  latency,
}: {
  step: RunStep
  attempt?: AttemptInfo
  latency?: { waitMs: number | null; activeMs: number | null }
}) {
  const inputText = metaText(step.inputMeta, ['input', 'prompt', 'message', 'query'])
  const outputText = metaText(step.outputMeta, ['output', 'result', 'content', 'text'])
  const inputJSON = metaJSON(step.inputMeta)
  const outputJSON = metaJSON(step.outputMeta)
  const name = stepName(step)
  const { promptTokens, completionTokens, totalTokens } = step.tokenUsage
  const errorCode = /^([a-z0-9_]+):/i.exec(step.error ?? '')?.[1]
  const attemptTitle = attempt
    ? `Execution ${attempt.attempt} of ${attempt.attempts} recorded for this step${attempt.retryAfterFailure ? ' (retried after a failed attempt)' : ''}`
    : undefined

  return (
    <li className="trace-step" data-status={step.status}>
      <div className="trace-step-head">
        <span className="timeline-index">#{logicalIndex(step)}</span>
        <span className={`step-type step-type-${step.stepType}`}>{step.stepType}</span>
        <span className={`status-badge ${step.status}`}>{step.status}</span>
        {name ? <span className="timeline-provider">{name}</span> : null}
        {attempt ? (
          <span className={`trace-attempt${attempt.retryAfterFailure ? ' retry' : ''}`} title={attemptTitle}>
            attempt {attempt.attempt}/{attempt.attempts}
            {attempt.retryAfterFailure ? ' · retried after failure' : ''}
          </span>
        ) : null}
      </div>

      {step.error ? (
        <div className="timeline-error">
          {errorCode ? <span className="trace-error-code">{errorCode}</span> : null}
          {step.error}
        </div>
      ) : null}

      <div className="timeline-meta">
        {promptTokens !== undefined ? <span title="Prompt tokens">Tokens in: {formatNumber(promptTokens)}</span> : null}
        {completionTokens !== undefined ? <span title="Completion tokens">Tokens out: {formatNumber(completionTokens)}</span> : null}
        {totalTokens !== undefined ? <span title="Total tokens for this step">Total: {formatNumber(totalTokens)}</span> : null}
        <span title="Cost attributed to this step">Cost: {formatCost(step.cost)}</span>
      </div>

      {latency ? (
        <div className="trace-latency">
          {latency.waitMs !== null && latency.waitMs > 0 ? (
            <span title="Idle time between the previous step and this one">wait {formatDurationMs(latency.waitMs)}</span>
          ) : null}
          {latency.activeMs !== null ? (
            <span title="Step execution time (started → completed)">run {formatDurationMs(latency.activeMs)}</span>
          ) : (
            <span title="The step has started but not completed yet">running…</span>
          )}
        </div>
      ) : null}

      {inputText ? (
        <div className="trace-io">
          <p className="run-output-label">Input</p>
          <div className="run-output small">{truncate(inputText, 400)}</div>
        </div>
      ) : null}
      {outputText ? (
        <div className="trace-io">
          <p className="run-output-label">Output</p>
          <div className="run-output small">{truncate(outputText, 400)}</div>
        </div>
      ) : null}

      {inputJSON || outputJSON ? (
        <details className="timeline-details">
          <summary>Raw step metadata</summary>
          {inputJSON ? <pre>{inputJSON}</pre> : null}
          {outputJSON ? <pre>{outputJSON}</pre> : null}
        </details>
      ) : null}
    </li>
  )
}

// ---------------------------------------------------------------------------
// TraceViewer
// ---------------------------------------------------------------------------

export function TraceViewer({ runId, live }: { runId: string; live: boolean }) {
  const stepsQuery = useRunSteps(runId)
  const liveEvents = useLiveEventLog(runId)

  // Full history from the server, chronologically sorted (stable identities
  // across refetches via react-query structural sharing — history grows,
  // nothing is clobbered).
  const steps = useMemo(() => sortSteps(stepsQuery.data ?? []), [stepsQuery.data])

  // 1s re-render ticker while live so the waterfall "now" cursor and running
  // bars move between SSE/invalidation-driven refetches. "now" is derived
  // from the last successful fetch time, advanced by the interval callback —
  // render stays pure.
  const [tickNow, setTickNow] = useState<number | null>(null)
  useEffect(() => {
    if (!live) return
    const handle = window.setInterval(() => setTickNow(Date.now()), 1000)
    return () => window.clearInterval(handle)
  }, [live])
  const now = Math.max(stepsQuery.dataUpdatedAt, tickNow ?? 0)
  const attempts = useMemo(() => attemptInfo(steps), [steps])
  const waterfall = useMemo(
    () => (steps.length > 0 ? buildWaterfall(steps, live, now) : null),
    [steps, live, now],
  )
  const attribution = useMemo(() => attributionRows(steps), [steps])
  const totals = useMemo(() => {
    let tokens: number | undefined
    let cost: number | undefined
    let retries = 0
    for (const step of steps) {
      const total = step.tokenUsage.totalTokens
      if (total !== undefined) tokens = (tokens ?? 0) + total
      if (step.cost !== undefined) cost = (cost ?? 0) + step.cost
    }
    for (const info of attempts.values()) {
      if (info.attempt > 1) retries += 1
    }
    return { tokens, cost, retries, spanMs: waterfall?.span }
  }, [steps, attempts, waterfall])

  const rows = waterfall
    ? waterfall.rows.map(({ step, waitMs, activeMs }) => ({
        step,
        latency: { waitMs, activeMs } as { waitMs: number | null; activeMs: number | null },
      }))
    : steps.map((step) => ({ step, latency: undefined }))

  return (
    <article className="panel wide trace-viewer" id="trace-viewer">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Trace viewer</p>
          <h3>Execution waterfall &amp; attribution</h3>
        </div>
        <span className={`form-note${live ? ' live-note' : ''}`}>
          {live ? '● Live — SSE /runs/{id}/events' : 'Final trace'} · GET /runs/{runId.slice(0, 8)}…/steps
        </span>
      </div>

      <TraceIdRow runId={runId} live={live} />

      {stepsQuery.isError && steps.length > 0 ? (
        <div className="form-note trace-refresh-note" role="status">
          Last refresh failed — showing the trace as last known.{' '}
          <button type="button" className="ghost-button small" onClick={() => void stepsQuery.refetch()}>
            Retry
          </button>
        </div>
      ) : null}

      {stepsQuery.isPending && steps.length === 0 ? (
        <div className="stack-gap">
          <Skeleton height={56} />
          <Skeleton height={56} />
        </div>
      ) : stepsQuery.isError && steps.length === 0 ? (
        <div className="form-error" role="alert">
          Could not load the execution trace: {stepsQuery.error instanceof Error ? stepsQuery.error.message : 'unknown error'}
        </div>
      ) : steps.length === 0 ? (
        <EmptyState
          title={live ? 'No steps recorded yet' : 'No steps were recorded for this run'}
          hint={
            live
              ? 'The waterfall and attribution build up here in real time as the worker records model and tool steps.'
              : 'This run completed without a recorded step trace (it may predate step recording or have been cancelled before execution).'
          }
        />
      ) : (
        <>
          <div className="trace-summary">
            <span>
              {steps.length} step{steps.length === 1 ? '' : 's'}
            </span>
            <span title="Wall-clock span from the first step start to the last completion">
              Span: {totals.spanMs !== undefined ? formatDurationMs(totals.spanMs) : '—'}
            </span>
            <span>Tokens: {totals.tokens !== undefined ? formatNumber(totals.tokens) : '—'}</span>
            <span>Cost: {totals.cost !== undefined ? formatCost(totals.cost) : '—'}</span>
            <span title="Steps executed more than once (same index, type and name)">
              Retries observed: {totals.retries}
            </span>
          </div>

          {waterfall ? (
            <WaterfallBlock waterfall={waterfall} attempts={attempts} />
          ) : (
            <p className="form-note">No step timestamps recorded — the waterfall is unavailable for this run.</p>
          )}

          {attribution.length > 0 ? <AttributionBlock rows={attribution} /> : null}

          <ol className="trace-steps">
            {rows.map(({ step, latency }) => (
              <TraceStepCard
                key={stepKey(step)}
                step={step}
                attempt={attempts.get(stepKey(step))}
                latency={latency}
              />
            ))}
          </ol>

          <LiveEventLog events={liveEvents} />
        </>
      )}
    </article>
  )
}
