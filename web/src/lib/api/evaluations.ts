// Evaluations resource (track 2-d contract):
// - GET  /eval-datasets            → {"datasets": [{id,name,description,case_count,created_at}]}
// - POST /eval-datasets/create     → {"dataset"} (cases embedded)
// - GET  /eval-datasets/{id}       → dataset incl. cases
// - POST /eval-datasets/{id}/run   → {"eval_run_id","status"} (synchronous, bounded)
// - GET  /eval-runs/{id}           → run with results + summary
// - POST /eval-runs/compare        → baseline/candidate summaries + regressions/improvements

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

export const EVAL_SCORERS = ['exact', 'contains', 'regex', 'latency_under_ms', 'cost_under_cents'] as const

export type EvalScorer = (typeof EVAL_SCORERS)[number]

export type EvalCase = {
  id: string
  input: string
  expected?: string
  scorer: string
  params?: Record<string, unknown>
}

export type EvalDataset = {
  id: string
  name: string
  description: string
  caseCount?: number
  cases?: EvalCase[]
  createdAt?: string
}

export type EvalResult = {
  caseId: string
  output?: string
  passed: boolean
  score?: number
  latencyMs?: number
  costCents?: number
  error?: string
}

export type EvalSummary = {
  passRate?: number
  avgLatencyMs?: number
  totalCostCents?: number
  byScorer: Record<string, { passed?: number; failed?: number }>
}

export type EvalRun = {
  id: string
  datasetId?: string
  agentId?: string
  status: string
  results: EvalResult[]
  summary?: EvalSummary
}

export type EvalCompareSide = EvalSummary

export type EvalComparison = {
  baseline: EvalCompareSide
  candidate: EvalCompareSide
  regressions: { caseId: string }[]
  improvements: { caseId: string }[]
}

export type CreateEvalDatasetInput = {
  name: string
  description: string
  cases: EvalCase[]
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeEvalCase(raw: unknown): EvalCase {
  const record = typeof raw === 'object' && raw !== null ? (raw as Record<string, unknown>) : {}
  const params = pickField(record, 'params', 'parameters')
  return {
    id: asString(pickField(record, 'id', 'caseId', 'case_id')) ?? '',
    input: asString(pickField(record, 'input')) ?? '',
    expected: asString(pickField(record, 'expected', 'expectedOutput', 'expected_output')),
    scorer: (asString(pickField(record, 'scorer')) ?? 'unknown').toLowerCase(),
    params: typeof params === 'object' && params !== null ? (params as Record<string, unknown>) : undefined,
  }
}

function normalizeDataset(raw: unknown): EvalDataset {
  const casesRaw = pickField(raw, 'cases')
  return {
    id: asString(pickField(raw, 'id', 'datasetId', 'dataset_id')) ?? '',
    name: asString(pickField(raw, 'name')) ?? 'Untitled dataset',
    description: asString(pickField(raw, 'description')) ?? '',
    caseCount: asNumber(pickField(raw, 'caseCount', 'case_count')) ?? (Array.isArray(casesRaw) ? casesRaw.length : undefined),
    cases: Array.isArray(casesRaw) ? casesRaw.map(normalizeEvalCase) : undefined,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
  }
}

function normalizeSummary(raw: unknown): EvalSummary {
  const byScorerRaw = pickField(raw, 'byScorer', 'by_scorer')
  const byScorer: Record<string, { passed?: number; failed?: number }> = {}
  if (typeof byScorerRaw === 'object' && byScorerRaw !== null) {
    for (const [key, value] of Object.entries(byScorerRaw as Record<string, unknown>)) {
      if (typeof value === 'object' && value !== null) {
        byScorer[key] = {
          passed: asNumber(pickField(value, 'passed', 'pass')),
          failed: asNumber(pickField(value, 'failed', 'fail')),
        }
      }
    }
  }
  return {
    passRate: asNumber(pickField(raw, 'passRate', 'pass_rate')),
    avgLatencyMs: asNumber(pickField(raw, 'avgLatencyMs', 'avg_latency_ms', 'averageLatencyMs')),
    totalCostCents: asNumber(pickField(raw, 'totalCostCents', 'total_cost_cents')),
    byScorer,
  }
}

function normalizeEvalRun(raw: unknown): EvalRun {
  const resultsRaw = pickField(raw, 'results')
  const results = Array.isArray(resultsRaw) ? resultsRaw : []
  return {
    id: asString(pickField(raw, 'id', 'evalRunId', 'eval_run_id')) ?? '',
    datasetId: asString(pickField(raw, 'datasetId', 'dataset_id')),
    agentId: asString(pickField(raw, 'agentId', 'agent_id')),
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    results: results.map((result) => {
      const record = typeof result === 'object' && result !== null ? (result as Record<string, unknown>) : {}
      const passedRaw = pickField(record, 'passed', 'pass')
      return {
        caseId: asString(pickField(record, 'caseId', 'case_id')) ?? '',
        output: asString(pickField(record, 'output', 'result')),
        passed: passedRaw === true || passedRaw === 'true' || passedRaw === 1,
        score: asNumber(pickField(record, 'score')),
        latencyMs: asNumber(pickField(record, 'latencyMs', 'latency_ms', 'latency')),
        costCents: asNumber(pickField(record, 'costCents', 'cost_cents', 'cost')),
        error: asString(pickField(record, 'error', 'errorMessage')),
      }
    }),
    summary: normalizeSummary(pickField(raw, 'summary') ?? {}),
  }
}

// ---------------------------------------------------------------------------
// Client-side case validation (backend stays the source of truth)
// ---------------------------------------------------------------------------

export type EvalCaseIssue = { index: number; message: string }

export function validateEvalCases(cases: EvalCase[]): EvalCaseIssue[] {
  const issues: EvalCaseIssue[] = []
  const ids = new Set<string>()
  cases.forEach((entry, index) => {
    if (!entry.id.trim()) issues.push({ index, message: 'missing "id"' })
    else if (ids.has(entry.id)) issues.push({ index, message: `duplicate id "${entry.id}"` })
    else ids.add(entry.id)
    if (!entry.input.trim()) issues.push({ index, message: 'missing "input"' })
    if (!entry.scorer.trim()) {
      issues.push({ index, message: 'missing "scorer"' })
    } else if (!EVAL_SCORERS.includes(entry.scorer as EvalScorer)) {
      issues.push({ index, message: `unknown scorer "${entry.scorer}" (expected one of: ${EVAL_SCORERS.join(', ')})` })
    }
  })
  return issues
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

function extractDatasetList(raw: unknown): EvalDataset[] {
  const wrapped = pickField(raw, 'datasets', 'items', 'data')
  const list = Array.isArray(raw) ? raw : Array.isArray(wrapped) ? wrapped : []
  return list.map(normalizeDataset)
}

export async function listEvalDatasets(): Promise<EvalDataset[]> {
  return extractDatasetList(await apiFetch<unknown>('/eval-datasets'))
}

export async function getEvalDataset(id: string): Promise<EvalDataset> {
  const raw = await apiFetch<unknown>(`/eval-datasets/${encodeURIComponent(id)}`)
  return normalizeDataset(pickField(raw, 'dataset') ?? raw)
}

export async function createEvalDataset(input: CreateEvalDatasetInput): Promise<EvalDataset> {
  const raw = await apiFetch<unknown>('/eval-datasets/create', {
    method: 'POST',
    body: JSON.stringify(input),
  })
  return normalizeDataset(pickField(raw, 'dataset') ?? raw)
}

export async function runEvalDataset(id: string, agentId: string): Promise<{ evalRunId: string; status: string }> {
  const raw = await apiFetch<unknown>(`/eval-datasets/${encodeURIComponent(id)}/run`, {
    method: 'POST',
    body: JSON.stringify({ agent_id: agentId }),
  })
  return {
    evalRunId: asString(pickField(raw, 'evalRunId', 'eval_run_id', 'id')) ?? '',
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
  }
}

export async function getEvalRun(id: string): Promise<EvalRun> {
  return normalizeEvalRun(await apiFetch<unknown>(`/eval-runs/${encodeURIComponent(id)}`))
}

export async function compareEvalRuns(baselineRunId: string, candidateRunId: string): Promise<EvalComparison> {
  const raw = await apiFetch<unknown>('/eval-runs/compare', {
    method: 'POST',
    body: JSON.stringify({ baseline_run_id: baselineRunId, candidate_run_id: candidateRunId }),
  })
  const regressionsRaw = pickField(raw, 'regressions')
  const improvementsRaw = pickField(raw, 'improvements')
  const toCaseList = (value: unknown): { caseId: string }[] =>
    Array.isArray(value)
      ? value.map((entry) => ({ caseId: asString(pickField(entry, 'caseId', 'case_id')) ?? '' }))
      : []
  return {
    baseline: normalizeSummary(pickField(raw, 'baseline') ?? {}),
    candidate: normalizeSummary(pickField(raw, 'candidate') ?? {}),
    regressions: toCaseList(regressionsRaw),
    improvements: toCaseList(improvementsRaw),
  }
}
