// Evaluations views (Priority 4) — datasets, eval runs, and run comparison,
// wired to the track 2-d contract. The API exposes no eval-run list endpoint,
// so run detail/compare entry points come from running a dataset or pasting
// run IDs into the compare form (honest about that in the UI).

import { useMemo, useState, type FormEvent } from 'react'
import {
  useAgents,
  useCompareEvalRuns,
  useCreateEvalDataset,
  useEvalDataset,
  useEvalDatasets,
  useEvalRun,
  useRunEvalDataset,
} from '../lib/hooks'
import {
  EVAL_SCORERS,
  validateEvalCases,
  type EvalCase,
  type EvalCaseIssue,
  type EvalComparison,
  type EvalDataset,
  type EvalRun,
} from '../lib/api/evaluations'
import { formatCents, formatDateTime, formatDurationMs, formatNumber, formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

function formatPercent(rate?: number): string {
  if (rate === undefined || rate === null || !Number.isFinite(rate)) return '—'
  const pct = rate <= 1 ? rate * 100 : rate
  return `${Math.round(pct * 10) / 10}%`
}

// ---------------------------------------------------------------------------
// Create dataset
// ---------------------------------------------------------------------------

const CASES_PLACEHOLDER = `[
  { "id": "c1", "input": "What is 2+2?", "expected": "4", "scorer": "exact" },
  { "id": "c2", "input": "Say hello", "expected": "hello", "scorer": "contains" },
  { "id": "c3", "input": "Summarize the doc", "scorer": "latency_under_ms", "params": { "threshold_ms": 1500 } }
]`

function CreateDatasetForm({
  onCreated,
  onCancel,
}: {
  onCreated: (dataset: EvalDataset) => void
  onCancel: () => void
}) {
  const createDataset = useCreateEvalDataset()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [casesText, setCasesText] = useState('')
  const [parseError, setParseError] = useState<string | null>(null)
  const [caseIssues, setCaseIssues] = useState<EvalCaseIssue[]>([])

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (createDataset.isPending) return
    setParseError(null)
    setCaseIssues([])

    let parsed: unknown
    try {
      parsed = JSON.parse(casesText)
    } catch (error) {
      setParseError(error instanceof Error ? `Invalid JSON: ${error.message}` : 'Invalid JSON')
      return
    }

    if (!Array.isArray(parsed)) {
      setParseError('Cases must be a JSON array of case objects.')
      return
    }

    const cases: EvalCase[] = parsed.map((entry) => {
      const record = typeof entry === 'object' && entry !== null ? (entry as Record<string, unknown>) : {}
      const params = pickFieldSafe(record, 'params')
      return {
        id: typeof record.id === 'string' ? record.id : '',
        input: typeof record.input === 'string' ? record.input : '',
        expected: typeof record.expected === 'string' ? record.expected : undefined,
        scorer: typeof record.scorer === 'string' ? record.scorer : '',
        params,
      }
    })

    const issues = validateEvalCases(cases)
    if (issues.length > 0) {
      setCaseIssues(issues)
      return
    }

    createDataset.mutate(
      { name: name.trim(), description: description.trim(), cases },
      { onSuccess: onCreated },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">New</p>
          <h3>Create eval dataset</h3>
        </div>
        <button type="button" className="link-button" onClick={onCancel}>
          Cancel
        </button>
      </div>

      {parseError ? <div className="form-error">{parseError}</div> : null}
      {caseIssues.length > 0 ? (
        <div className="form-error validation-list" role="alert">
          <strong>Fix the cases before submitting:</strong>
          <ul>
            {caseIssues.map((issue, index) => (
              <li key={index}>
                <code>cases[{issue.index}]</code> — {issue.message}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
      {createDataset.isError ? <div className="form-error">{describeError(createDataset.error)}</div> : null}

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="dataset-name">Name</label>
            <input id="dataset-name" value={name} onChange={(event) => setName(event.target.value)} placeholder="Support answers regression set" required />
          </div>
          <div className="field">
            <label htmlFor="dataset-description">Description</label>
            <input
              id="dataset-description"
              value={description}
              onChange={(event) => setDescription(event.target.value)}
              placeholder="What does this dataset measure?"
            />
          </div>
          <div className="field span-2">
            <label htmlFor="dataset-cases">Cases (JSON array)</label>
            <textarea
              id="dataset-cases"
              className="code-input"
              rows={10}
              value={casesText}
              onChange={(event) => setCasesText(event.target.value)}
              placeholder={CASES_PLACEHOLDER}
              spellCheck={false}
              required
            />
            <span className="form-note">Scorers: {EVAL_SCORERS.join(' · ')}. The backend enforces per-scorer params (pattern, threshold_ms, threshold_cents).</span>
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">POST /eval-datasets/create</span>
          <button type="submit" className="primary-button" disabled={createDataset.isPending}>
            {createDataset.isPending ? 'Creating…' : 'Create dataset'}
          </button>
        </div>
      </form>
    </article>
  )
}

function pickFieldSafe(record: Record<string, unknown>, key: string): Record<string, unknown> | undefined {
  const value = record[key]
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined
}

// ---------------------------------------------------------------------------
// Eval run detail
// ---------------------------------------------------------------------------

function SummaryChips({ summary }: { summary?: EvalRun['summary'] }) {
  const scorers = Object.entries(summary?.byScorer ?? {})
  if (scorers.length === 0) return null
  return (
    <div className="edge-list">
      {scorers.map(([scorer, counts]) => (
        <span key={scorer} className="edge-chip">
          {scorer}: <strong>{counts.passed ?? 0} passed</strong> / {counts.failed ?? 0} failed
        </span>
      ))}
    </div>
  )
}

function EvalRunDetailView({
  evalRunId,
  onBack,
}: {
  evalRunId: string
  onBack: () => void
}) {
  const evalRunQuery = useEvalRun(evalRunId)
  const run = evalRunQuery.data

  return (
    <>
      <PageHeader
        eyebrow="Eval run"
        title={`Run ${shortenId(evalRunId)}`}
        badge={run ? <StatusPill status={run.status} /> : undefined}
        actions={
          <button type="button" className="ghost-button" onClick={onBack}>
            ← Back
          </button>
        }
      />

      {evalRunQuery.isError ? <ErrorBanner error={evalRunQuery.error} onRetry={() => void evalRunQuery.refetch()} /> : null}

      {evalRunQuery.isPending ? (
        <article className="panel">
          <Skeleton height={18} width="40%" />
          <Skeleton height={140} style={{ marginTop: 16 }} />
        </article>
      ) : run ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Pass rate" value={formatPercent(run.summary?.passRate)} accent="success" />
            <SummaryStat label="Avg latency" value={formatDurationMs(run.summary?.avgLatencyMs)} accent="info" />
            <SummaryStat label="Total cost" value={run.summary?.totalCostCents !== undefined ? formatCents(run.summary.totalCostCents) : '—'} accent="warning" />
            <SummaryStat label="Cases" value={String(run.results.length)} />
          </section>

          <article className="panel wide">
            <div className="panel-header">
              <div>
                <p className="eyebrow">Results</p>
                <h3>Case outcomes</h3>
              </div>
              <span className="form-note">
                dataset {shortenId(run.datasetId)} · agent {shortenId(run.agentId)} · GET /eval-runs/{shortenId(run.id)}
              </span>
            </div>

            <SummaryChips summary={run.summary} />

            {run.results.length === 0 ? (
              <EmptyState title="No case results recorded" hint="The eval run may have failed before executing any case." />
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Case</th>
                      <th>Passed</th>
                      <th>Score</th>
                      <th>Latency</th>
                      <th>Cost</th>
                      <th>Output</th>
                      <th>Error</th>
                    </tr>
                  </thead>
                  <tbody>
                    {run.results.map((result, index) => (
                      <tr key={result.caseId || index}>
                        <td>
                          <strong>{result.caseId || `#${index + 1}`}</strong>
                        </td>
                        <td>
                          <StatusPill status={result.passed ? 'passed' : 'failed'} />
                        </td>
                        <td>{result.score !== undefined ? result.score : '—'}</td>
                        <td>{formatDurationMs(result.latencyMs)}</td>
                        <td>{result.costCents !== undefined ? formatCents(result.costCents) : '—'}</td>
                        <td className="output-cell" title={result.output}>
                          {result.output ? (result.output.length > 80 ? `${result.output.slice(0, 80)}…` : result.output) : '—'}
                        </td>
                        <td className="table-error-cell">{result.error ?? '—'}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </article>
        </>
      ) : (
        <EmptyState
          title="Eval run not found"
          hint="It may have been removed or belongs to a different organization."
          action={
            <button type="button" className="ghost-button" onClick={onBack}>
              Back
            </button>
          }
        />
      )}
    </>
  )
}

// ---------------------------------------------------------------------------
// Dataset detail (+ run eval)
// ---------------------------------------------------------------------------

function DatasetDetailView({
  datasetId,
  onBack,
  onEvalStarted,
  canWrite,
}: {
  datasetId: string
  onBack: () => void
  onEvalStarted: (evalRunId: string) => void
  canWrite: boolean
}) {
  const datasetQuery = useEvalDataset(datasetId)
  const agentsQuery = useAgents()
  const runEval = useRunEvalDataset()
  const dataset = datasetQuery.data
  const agents = useMemo(() => agentsQuery.data ?? [], [agentsQuery.data])
  const [agentId, setAgentId] = useState('')

  const startRun = (event: FormEvent) => {
    event.preventDefault()
    if (!agentId || runEval.isPending) return
    runEval.mutate(
      { datasetId, agentId },
      {
        onSuccess: (result) => {
          if (result.evalRunId) onEvalStarted(result.evalRunId)
        },
      },
    )
  }

  const cases = dataset?.cases ?? []

  return (
    <>
      <PageHeader
        eyebrow="Dataset"
        title={dataset?.name ?? 'Dataset'}
        badge={dataset ? <StatusPill status="active" /> : undefined}
        actions={
          <button type="button" className="ghost-button" onClick={onBack}>
            ← Back to datasets
          </button>
        }
      />

      {datasetQuery.isError ? <ErrorBanner error={datasetQuery.error} onRetry={() => void datasetQuery.refetch()} /> : null}

      {datasetQuery.isPending ? (
        <article className="panel">
          <Skeleton height={18} width="40%" />
          <Skeleton height={140} style={{ marginTop: 16 }} />
        </article>
      ) : dataset ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Cases" value={dataset.caseCount !== undefined ? String(dataset.caseCount) : String(cases.length)} />
            <SummaryStat label="Created" value={formatRelativeTime(dataset.createdAt)} accent="info" />
            <SummaryStat label="Dataset ID" value={shortenId(dataset.id)} accent="default" />
            <SummaryStat label="Endpoint" value="/eval-datasets" accent="success" />
          </section>

          <article className="panel wide">
            <div className="panel-header">
              <div>
                <p className="eyebrow">Definition</p>
                <h3>Cases</h3>
              </div>
              <span className="form-note">GET /eval-datasets/{shortenId(dataset.id)}</span>
            </div>

            {dataset.description ? <p className="detail-copy">{dataset.description}</p> : null}

            {cases.length === 0 ? (
              <EmptyState title="No cases returned" hint="The dataset detail endpoint did not include case definitions." />
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Case</th>
                      <th>Scorer</th>
                      <th>Input</th>
                      <th>Expected</th>
                      <th>Params</th>
                    </tr>
                  </thead>
                  <tbody>
                    {cases.map((entry, index) => (
                      <tr key={entry.id || index}>
                        <td>
                          <strong>{entry.id || `#${index + 1}`}</strong>
                        </td>
                        <td>
                          <span className="step-type step-type-tool">{entry.scorer}</span>
                        </td>
                        <td className="output-cell" title={entry.input}>
                          {entry.input.length > 60 ? `${entry.input.slice(0, 60)}…` : entry.input}
                        </td>
                        <td className="output-cell" title={entry.expected}>
                          {entry.expected ?? '—'}
                        </td>
                        <td>
                          <code className="config-cell">{entry.params ? JSON.stringify(entry.params) : '—'}</code>
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
                <p className="eyebrow">Execute</p>
                <h3>Run eval</h3>
              </div>
            </div>

            {runEval.isError ? <div className="form-error">{describeError(runEval.error)}</div> : null}

            {!canWrite ? (
              <EmptyState title="Read-only role" hint="Running an evaluation executes cases against an agent and requires write permissions." />
            ) : agents.length === 0 ? (
              <EmptyState title="No agents available" hint="Evals execute each case through an agent. Create an agent first." />
            ) : (
              <form onSubmit={startRun}>
                <div className="field">
                  <label htmlFor="eval-agent">Agent under test</label>
                  <select id="eval-agent" value={agentId} onChange={(event) => setAgentId(event.target.value)} required>
                    <option value="" disabled>
                      Select an agent…
                    </option>
                    {agents.map((agent) => (
                      <option key={agent.id} value={agent.id}>
                        {agent.name} ({agent.model || 'unknown model'})
                      </option>
                    ))}
                  </select>
                </div>
                <div className="form-actions">
                  <span className="form-note">POST /eval-datasets/{shortenId(dataset.id)}/run · synchronous, max 50 cases</span>
                  <button type="submit" className="primary-button" disabled={runEval.isPending}>
                    {runEval.isPending ? 'Running…' : 'Run eval'}
                  </button>
                </div>
              </form>
            )}
          </article>
        </>
      ) : (
        <EmptyState
          title="Dataset not found"
          hint="It may have been removed or belongs to a different organization."
          action={
            <button type="button" className="ghost-button" onClick={onBack}>
              Back
            </button>
          }
        />
      )}
    </>
  )
}

// ---------------------------------------------------------------------------
// Compare
// ---------------------------------------------------------------------------

function CompareView({ onBack }: { onBack: () => void }) {
  const compare = useCompareEvalRuns()
  const [baselineRunId, setBaselineRunId] = useState('')
  const [candidateRunId, setCandidateRunId] = useState('')
  const [result, setResult] = useState<EvalComparison | null>(null)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (compare.isPending) return
    compare.mutate(
      { baselineRunId: baselineRunId.trim(), candidateRunId: candidateRunId.trim() },
      { onSuccess: setResult },
    )
  }

  const side = (label: string, summary: EvalComparison['baseline']) => (
    <div className="compare-side">
      <label>{label}</label>
      <div>Pass rate: <strong>{formatPercent(summary.passRate)}</strong></div>
      <div>Avg latency: <strong>{formatDurationMs(summary.avgLatencyMs)}</strong></div>
      <div>Total cost: <strong>{summary.totalCostCents !== undefined ? formatCents(summary.totalCostCents) : '—'}</strong></div>
    </div>
  )

  return (
    <>
      <PageHeader
        eyebrow="Quality"
        title="Compare eval runs"
        actions={
          <button type="button" className="ghost-button" onClick={onBack}>
            ← Back
          </button>
        }
      />

      <article className="panel">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Input</p>
            <h3>Pick two runs</h3>
          </div>
          <span className="form-note">POST /eval-runs/compare</span>
        </div>

        {compare.isError ? <div className="form-error">{describeError(compare.error)}</div> : null}

        <form onSubmit={submit}>
          <div className="form-grid">
            <div className="field">
              <label htmlFor="compare-baseline">Baseline eval run ID</label>
              <input
                id="compare-baseline"
                className="code-input"
                value={baselineRunId}
                onChange={(event) => setBaselineRunId(event.target.value)}
                placeholder="e.g. the run you ran last week"
                required
              />
            </div>
            <div className="field">
              <label htmlFor="compare-candidate">Candidate eval run ID</label>
              <input
                id="compare-candidate"
                className="code-input"
                value={candidateRunId}
                onChange={(event) => setCandidateRunId(event.target.value)}
                placeholder="e.g. the run you just finished"
                required
              />
            </div>
          </div>
          <div className="form-actions">
            <span className="form-note">Run IDs come from running a dataset (this API has no eval-run list endpoint).</span>
            <button type="submit" className="primary-button" disabled={compare.isPending}>
              {compare.isPending ? 'Comparing…' : 'Compare'}
            </button>
          </div>
        </form>
      </article>

      {result ? (
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Delta</p>
              <h3>Baseline vs candidate</h3>
            </div>
          </div>

          <div className="compare-grid">
            {side('Baseline', result.baseline)}
            {side('Candidate', result.candidate)}
          </div>

          <div className="compare-lists">
            <div>
              <h4 className="compare-heading regressions">Regressions ({result.regressions.length})</h4>
              {result.regressions.length === 0 ? (
                <p className="form-note">No cases regressed — the candidate is at least as good everywhere.</p>
              ) : (
                <ul>
                  {result.regressions.map((entry) => (
                    <li key={entry.caseId}>
                      <code>{entry.caseId}</code>
                    </li>
                  ))}
                </ul>
              )}
            </div>
            <div>
              <h4 className="compare-heading improvements">Improvements ({result.improvements.length})</h4>
              {result.improvements.length === 0 ? (
                <p className="form-note">No new cases passed — the candidate only matched the baseline.</p>
              ) : (
                <ul>
                  {result.improvements.map((entry) => (
                    <li key={entry.caseId}>
                      <code>{entry.caseId}</code>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>
        </article>
      ) : null}
    </>
  )
}

// ---------------------------------------------------------------------------
// Top-level view
// ---------------------------------------------------------------------------

export function EvaluationsView({ canWrite }: { canWrite: boolean }) {
  const datasetsQuery = useEvalDatasets()
  const [selectedDatasetId, setSelectedDatasetId] = useState<string | null>(null)
  const [selectedEvalRunId, setSelectedEvalRunId] = useState<string | null>(null)
  const [showCreate, setShowCreate] = useState(false)
  const [showCompare, setShowCompare] = useState(false)
  const datasets = useMemo(() => datasetsQuery.data ?? [], [datasetsQuery.data])
  const totalCases = datasets.reduce((sum, dataset) => sum + (dataset.caseCount ?? 0), 0)

  if (showCompare) {
    return <CompareView onBack={() => setShowCompare(false)} />
  }

  if (selectedEvalRunId) {
    return <EvalRunDetailView evalRunId={selectedEvalRunId} onBack={() => setSelectedEvalRunId(null)} />
  }

  if (selectedDatasetId) {
    return (
      <DatasetDetailView
        datasetId={selectedDatasetId}
        onBack={() => setSelectedDatasetId(null)}
        onEvalStarted={(evalRunId) => setSelectedEvalRunId(evalRunId)}
        canWrite={canWrite}
      />
    )
  }

  return (
    <>
      <PageHeader
        eyebrow="Quality"
        title="Evaluations"
        actions={
          canWrite ? (
            <button type="button" className="primary-button" onClick={() => setShowCreate((open) => !open)}>
              New dataset
            </button>
          ) : null
        }
      />

      {showCreate ? (
        <CreateDatasetForm
          onCreated={(dataset) => {
            setShowCreate(false)
            setSelectedDatasetId(dataset.id)
          }}
          onCancel={() => setShowCreate(false)}
        />
      ) : null}

      {datasetsQuery.isError ? <ErrorBanner error={datasetsQuery.error} onRetry={() => void datasetsQuery.refetch()} /> : null}

      <section className="summary-grid">
        {datasetsQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Datasets" value={String(datasets.length)} />
            <SummaryStat label="Total cases" value={formatNumber(totalCases)} accent="info" />
            <SummaryStat label="Endpoint" value="/eval-datasets" accent="success" />
            <SummaryStat label="Compare" value="/eval-runs/compare" accent="warning" />
          </>
        )}
      </section>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Datasets</p>
            <h3>All eval datasets</h3>
          </div>
          <div className="topbar-actions">
            <button type="button" className="ghost-button" onClick={() => setShowCompare(true)}>
              Compare runs
            </button>
          </div>
        </div>

        {datasetsQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : datasets.length === 0 ? (
          <EmptyState
            title="No eval datasets yet"
            hint="Datasets hold scored cases (exact, contains, regex, latency, cost) that run against an agent to catch regressions."
            action={
              canWrite ? (
                <button type="button" className="primary-button" onClick={() => setShowCreate(true)}>
                  Create dataset
                </button>
              ) : undefined
            }
          />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Cases</th>
                  <th>Created</th>
                </tr>
              </thead>
              <tbody>
                {datasets.map((dataset) => (
                  <tr key={dataset.id} className="row-link" onClick={() => setSelectedDatasetId(dataset.id)}>
                    <td>
                      <strong>{dataset.name}</strong>
                      {dataset.description ? <div className="form-note">{dataset.description}</div> : null}
                    </td>
                    <td>{dataset.caseCount !== undefined ? formatNumber(dataset.caseCount) : '—'}</td>
                    <td>{formatDateTime(dataset.createdAt)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </article>
    </>
  )
}
