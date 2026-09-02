// Run detail timeline (Priority 1).
//
// Renders the execution trace of a run from GET /runs/{id}/steps and stays
// live by piggybacking on the SSE stream of GET /runs/{id}/events (see
// useRunEvents in lib/hooks.ts — the parent view forwards events here).

import { useMemo } from 'react'
import { useRunSteps } from '../lib/hooks'
import { formatCost, formatDateTime, formatDurationMs, formatNumber } from '../lib/format'
import type { RunStep } from '../lib/api/types'
import { EmptyState, Skeleton } from './shared'

function metaString(meta: Record<string, unknown>, keys: string[]): string | null {
  for (const key of keys) {
    const value = meta[key]
    if (typeof value === 'string' && value.trim()) return value
    if (typeof value === 'number' && Number.isFinite(value)) return String(value)
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

function tokenSummary(step: RunStep): string {
  const { promptTokens, completionTokens, totalTokens } = step.tokenUsage
  const parts: string[] = []
  if (promptTokens !== undefined) parts.push(`${formatNumber(promptTokens)} in`)
  if (completionTokens !== undefined) parts.push(`${formatNumber(completionTokens)} out`)
  if (parts.length === 0) {
    if (totalTokens !== undefined) return `${formatNumber(totalTokens)} tokens`
    return '—'
  }
  if (totalTokens !== undefined && totalTokens !== (promptTokens ?? 0) + (completionTokens ?? 0)) {
    parts.push(`${formatNumber(totalTokens)} total`)
  }
  return parts.join(' · ')
}

function stepDuration(step: RunStep): string {
  const start = step.startedAt ? Date.parse(step.startedAt) : NaN
  const end = step.completedAt ? Date.parse(step.completedAt) : NaN
  if (!Number.isNaN(start) && !Number.isNaN(end) && end >= start) return formatDurationMs(end - start)
  return '—'
}

function StepCard({ step }: { step: RunStep }) {
  const inputText = metaString(step.inputMeta, ['input', 'prompt', 'message', 'query'])
  const outputText = metaString(step.outputMeta, ['output', 'result', 'content', 'text'])
  const provider = metaString(step.inputMeta, ['name', 'provider', 'model', 'tool'])
  const inputJSON = metaJSON(step.inputMeta)
  const outputJSON = metaJSON(step.outputMeta)
  const hasDetails = Boolean(inputJSON || outputJSON)

  return (
    <li className="timeline-item" data-status={step.status}>
      <span className="timeline-marker" aria-hidden="true" />
      <div className="timeline-body">
        <div className="timeline-head">
          <span className="timeline-index">#{step.index + 1}</span>
          <span className={`step-type step-type-${step.stepType}`}>{step.stepType}</span>
          <span className={`status-badge ${step.status}`}>{step.status}</span>
          {provider ? <span className="timeline-provider">{provider}</span> : null}
        </div>

        {step.error ? <div className="timeline-error">{step.error}</div> : null}

        <div className="timeline-meta">
          <span title="Tokens consumed by this step">Tokens: {tokenSummary(step)}</span>
          <span title="Cost attributed to this step">Cost: {formatCost(step.cost)}</span>
          <span title="Duration from start to completion">Duration: {stepDuration(step)}</span>
          <span title="When the step started">{formatDateTime(step.startedAt ?? step.createdAt)}</span>
        </div>

        {inputText ? (
          <>
            <p className="run-output-label">Input</p>
            <div className="run-output small">{inputText}</div>
          </>
        ) : null}
        {outputText ? (
          <>
            <p className="run-output-label">Output</p>
            <div className="run-output small">{outputText}</div>
          </>
        ) : null}

        {hasDetails ? (
          <details className="timeline-details">
            <summary>Raw step metadata</summary>
            {inputJSON ? <pre>{inputJSON}</pre> : null}
            {outputJSON ? <pre>{outputJSON}</pre> : null}
          </details>
        ) : null}
      </div>
    </li>
  )
}

export function RunTimeline({ runId, live }: { runId: string; live: boolean }) {
  const stepsQuery = useRunSteps(runId)
  const steps = useMemo(() => {
    const list = stepsQuery.data ?? []
    return [...list].sort((a, b) => a.index - b.index)
  }, [stepsQuery.data])

  const totals = useMemo(() => {
    let tokens = 0
    let cost = 0
    let sawTokens = false
    let sawCost = false
    for (const step of steps) {
      const total = step.tokenUsage.totalTokens
      if (total !== undefined) {
        tokens += total
        sawTokens = true
      }
      if (step.cost !== undefined) {
        cost += step.cost
        sawCost = true
      }
    }
    return { tokens: sawTokens ? tokens : undefined, cost: sawCost ? cost : undefined }
  }, [steps])

  return (
    <article className="panel wide" id="run-timeline">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Trace</p>
          <h3>Step timeline</h3>
        </div>
        <span className="form-note">
          {live ? 'Live · refreshed from /runs/{id}/events' : 'Final trace'} · GET /runs/{runId.slice(0, 8)}…/steps
        </span>
      </div>

      {stepsQuery.isPending ? (
        <div className="stack-gap">
          <Skeleton height={64} />
          <Skeleton height={64} />
        </div>
      ) : stepsQuery.isError ? (
        <div className="form-error" role="alert">
          Could not load the step timeline: {stepsQuery.error instanceof Error ? stepsQuery.error.message : 'unknown error'}
        </div>
      ) : steps.length === 0 ? (
        <EmptyState
          title={live ? 'No steps recorded yet' : 'No steps were recorded for this run'}
          hint={
            live
              ? 'Steps appear here as the worker executes model calls and tools. Runs queued behind others may take a moment to start.'
              : 'This run completed without a recorded step trace (it may predate step recording or have been cancelled before execution).'
          }
        />
      ) : (
        <>
          <div className="timeline-totals">
            <span>
              {steps.length} step{steps.length === 1 ? '' : 's'}
            </span>
            <span>Tokens: {totals.tokens !== undefined ? formatNumber(totals.tokens) : '—'}</span>
            <span>Cost: {totals.cost !== undefined ? formatCost(totals.cost) : '—'}</span>
          </div>
          <ol className="timeline">
            {steps.map((step) => (
              <StepCard key={step.id || step.index} step={step} />
            ))}
          </ol>
        </>
      )}
    </article>
  )
}
