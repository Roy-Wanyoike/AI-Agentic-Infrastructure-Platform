import { useMemo, useState, useSyncExternalStore, useRef, type FormEvent } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import './App.css'
import {
  getAuthSnapshot,
  login as performLogin,
  loginWithApiKey,
  logout as performLogout,
  register as performRegister,
  subscribeAuth,
} from './lib/api/auth'
import { API_BASE, APP_NAME } from './lib/api/client'
import { isTerminalRunStatus, type Agent, type Run } from './lib/api/types'
import { isDemoView } from './lib/demo'
import { formatDateTime, formatNumber, formatRelativeTime, shortenId } from './lib/format'
import {
  useAgent,
  useAgents,
  useCreateAgent,
  useHealth,
  useMetrics,
  useRun,
  useRunAgent,
  useRuns,
  useRunEvents,
} from './lib/hooks'
import {
  DemoBadge,
  DemoStrip,
  EmptyState,
  ErrorBanner,
  PageHeader,
  Skeleton,
  StatusPill,
  SummaryStat,
} from './views/shared'
import {
  countByStatus,
  describeError,
  navItems,
  sortRunsDesc,
  statusAccent,
  type ViewName,
} from './views/uiHelpers'
import { RunTimeline } from './views/runTimeline'
import { WorkflowsView } from './views/workflows'
import { ApprovalsView } from './views/approvals'
import { EvaluationsView } from './views/evaluations'
import { UsageView } from './views/usage'
import { canDecide, canWrite } from './lib/rbac'

const COMMON_MODELS = ['gpt-4o-mini', 'gpt-4o', 'claude-3-7-sonnet', 'llama-3.1-70b']

// ---------------------------------------------------------------------------
// Shared building blocks live in ./views/shared.tsx
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Auth gate
// ---------------------------------------------------------------------------

function AuthGate() {
  const [mode, setMode] = useState<'login' | 'register' | 'apikey'>('login')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [organization, setOrganization] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const switchMode = (next: 'login' | 'register' | 'apikey') => {
    setMode(next)
    setError(null)
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (pending) return
    setError(null)
    setPending(true)
    try {
      if (mode === 'apikey') {
        loginWithApiKey(apiKey)
      } else if (mode === 'login') {
        await performLogin({ email: email.trim(), password })
      } else {
        await performRegister({ organization, email: email.trim(), password })
      }
    } catch (cause) {
      setError(describeError(cause))
    } finally {
      setPending(false)
    }
  }

  const ctaLabel = mode === 'apikey' ? 'Use API key' : mode === 'login' ? 'Sign in' : 'Create account'

  return (
    <div className="auth-shell">
      <div className="auth-card panel">
        <div className="auth-brand">
          <div className="brand-mark">A</div>
          <div>
            <p className="eyebrow">Platform</p>
            <h1>{APP_NAME}</h1>
          </div>
        </div>
        <p className="auth-subtitle">
          Sign in to operate the control plane at <code>{API_BASE}</code>.
        </p>

        <div className="auth-tabs">
          <button type="button" className={mode === 'login' ? 'ghost-button auth-tab active' : 'ghost-button auth-tab'} onClick={() => switchMode('login')}>
            Sign in
          </button>
          <button type="button" className={mode === 'register' ? 'ghost-button auth-tab active' : 'ghost-button auth-tab'} onClick={() => switchMode('register')}>
            Register
          </button>
          <button type="button" className={mode === 'apikey' ? 'ghost-button auth-tab active' : 'ghost-button auth-tab'} onClick={() => switchMode('apikey')}>
            API key
          </button>
        </div>

        {error ? (
          <div className="form-error" role="alert">
            {error}
          </div>
        ) : null}

        <form onSubmit={submit}>
          {mode === 'apikey' ? (
            <div className="auth-fields">
              <div className="field">
                <label htmlFor="auth-api-key">API key</label>
                <input
                  id="auth-api-key"
                  value={apiKey}
                  onChange={(event) => setApiKey(event.target.value)}
                  placeholder="Paste an X-API-Key value"
                  autoComplete="off"
                  required
                />
              </div>
            </div>
          ) : (
            <div className="auth-fields">
              {mode === 'register' ? (
                <div className="field">
                  <label htmlFor="auth-organization">Organization</label>
                  <input
                    id="auth-organization"
                    value={organization}
                    onChange={(event) => setOrganization(event.target.value)}
                    placeholder="Acme AI (defaults to your email name)"
                    autoComplete="organization"
                  />
                </div>
              ) : null}
              <div className="field">
                <label htmlFor="auth-email">Email</label>
                <input
                  id="auth-email"
                  type="email"
                  value={email}
                  onChange={(event) => setEmail(event.target.value)}
                  placeholder="you@company.com"
                  autoComplete="email"
                  required
                />
              </div>
              <div className="field">
                <label htmlFor="auth-password">Password</label>
                <input
                  id="auth-password"
                  type="password"
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                  placeholder="••••••••"
                  autoComplete={mode === 'login' ? 'current-password' : 'new-password'}
                  required
                />
              </div>
            </div>
          )}

          <div className="form-actions auth-actions">
            <span className="form-note">
              {mode === 'apikey' ? 'Stored locally for dev convenience' : 'Token kept in localStorage'}
            </span>
            <button type="submit" className="primary-button" disabled={pending}>
              {pending ? 'Working…' : ctaLabel}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Overview (live: /metrics + /agents + /runs + /healthz + /readyz)
// ---------------------------------------------------------------------------

function OverviewView({ onNavigate, onOpenRun }: { onNavigate: (view: ViewName) => void; onOpenRun: (runId: string) => void }) {
  const agentsQuery = useAgents()
  const runsQuery = useRuns()
  const metricsQuery = useMetrics()
  const health = useHealth()

  const agents = useMemo(() => agentsQuery.data ?? [], [agentsQuery.data])
  const runs = useMemo(() => sortRunsDesc(runsQuery.data ?? []), [runsQuery.data])
  const metrics = metricsQuery.data
  const statusCounts = useMemo(() => countByStatus(runs), [runs])
  const agentName = (id?: string) => agents.find((agent) => agent.id === id)?.name ?? shortenId(id)

  const draftCount = agents.filter((agent) => agent.status.toUpperCase() === 'DRAFT').length
  const loading = agentsQuery.isPending || runsQuery.isPending || metricsQuery.isPending
  const loadError = agentsQuery.error ?? runsQuery.error ?? metricsQuery.error

  const retryAll = () => {
    void agentsQuery.refetch()
    void runsQuery.refetch()
    void metricsQuery.refetch()
  }

  const kpis = [
    { label: 'Agents', value: formatNumber(agents.length), note: `${formatNumber(draftCount)} in draft`, tone: 'default' as const },
    { label: 'Runs', value: formatNumber(runs.length), note: `${formatNumber(statusCounts.RUNNING)} running`, tone: 'info' as const },
    { label: 'Queue backlog', value: formatNumber(metrics ? metrics.queueLength : undefined), note: 'queued tasks', tone: 'warning' as const },
    { label: 'Completed runs', value: formatNumber(statusCounts.COMPLETED), note: `${formatNumber(statusCounts.FAILED)} failed`, tone: 'success' as const },
  ]

  const chartBars = useMemo(() => {
    const max = Math.max(statusCounts.QUEUED, statusCounts.RUNNING, statusCounts.COMPLETED, statusCounts.FAILED, 1)
    return (['QUEUED', 'RUNNING', 'COMPLETED', 'FAILED'] as const).map((status) => ({
      status,
      count: statusCounts[status],
      height: Math.max(Math.round((statusCounts[status] / max) * 100), 2),
    }))
  }, [statusCounts])

  const total = runs.length
  const completedPct = total ? Math.round((statusCounts.COMPLETED / total) * 100) : null
  const failedPct = total ? Math.round((statusCounts.FAILED / total) * 100) : null
  const activePct = total ? Math.round(((statusCounts.RUNNING + statusCounts.QUEUED) / total) * 100) : null

  const healthz = health.data?.healthz
  const readyz = health.data?.readyz
  const healthRows = [
    {
      name: 'API /healthz',
      value: healthz ?? (health.isPending ? 'checking…' : 'unreachable'),
      online: (healthz ?? '').trim().toLowerCase() === 'ok',
    },
    {
      name: 'Readiness /readyz',
      value: readyz ?? (health.isPending ? 'checking…' : 'unreachable'),
      online: Boolean(readyz && readyz !== 'unreachable'),
    },
    {
      name: 'Queue backlog',
      value: `${formatNumber(metrics ? metrics.queueLength : 0)} pending`,
      online: (metrics ? metrics.queueLength : 0) < 100,
    },
  ]

  return (
    <>
      <div className="hero-panel">
        <div>
          <p className="eyebrow">Mission control</p>
          <h2>Good morning.</h2>
          <p className="hero-copy">
            Live view of your agent fleet and execution telemetry — agents from /agents, runs from /runs, queue depth from /metrics.
          </p>
        </div>
        <div className="hero-actions">
          <button type="button" className="ghost-button" onClick={() => onNavigate('Runs')}>
            Run agent
          </button>
          <button type="button" className="primary-button" onClick={() => onNavigate('Agents')}>
            Create agent
          </button>
        </div>
      </div>

      {loadError && !loading ? <ErrorBanner error={loadError} onRetry={retryAll} /> : null}

      <section className="kpi-grid" aria-label="Key metrics">
        {kpis.map((kpi) => (
          <article key={kpi.label} className="kpi-card">
            <p>{kpi.label}</p>
            <div className="kpi-row">
              {loading ? <Skeleton height={30} width={90} /> : <strong>{kpi.value}</strong>}
              {!loading ? <span className={`delta ${kpi.tone === 'warning' ? 'neutral' : 'positive'}`}>{kpi.note}</span> : null}
            </div>
          </article>
        ))}
      </section>

      <section className="content-grid">
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Execution</p>
              <h3>Run status mix</h3>
            </div>
            <button type="button" className="link-button" onClick={() => onNavigate('Runs')}>
              View runs
            </button>
          </div>

          {runsQuery.isPending ? (
            <Skeleton height={200} />
          ) : total === 0 ? (
            <EmptyState
              title="No runs yet"
              hint="Trigger your first agent run to see live execution telemetry here."
              action={
                <button type="button" className="primary-button" onClick={() => onNavigate('Runs')}>
                  Go to runs
                </button>
              }
            />
          ) : (
            <div className="mini-chart" aria-label="Runs by status">
              {chartBars.map((bar) => (
                <span
                  key={bar.status}
                  title={`${bar.status}: ${bar.count}`}
                  style={{ height: `${bar.height}%`, gridColumn: 'span 3' }}
                />
              ))}
            </div>
          )}
        </article>

        <article className="panel">
          <div className="panel-header">
            <div>
              <p className="eyebrow">System</p>
              <h3>System health</h3>
            </div>
          </div>

          <div className="health-stack">
            {healthRows.map((service) => (
              <div key={service.name} className="health-stack-row">
                <span>{service.name}</span>
                <span className={service.online ? 'status-dot online' : 'status-dot offline'} />
                <strong>{service.value}</strong>
              </div>
            ))}
          </div>
        </article>
      </section>

      <section className="bottom-grid">
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Activity</p>
              <h3>Recent runs</h3>
            </div>
          </div>

          {runsQuery.isPending ? (
            <div className="stack-gap">
              <Skeleton height={16} />
              <Skeleton height={16} />
              <Skeleton height={16} />
              <Skeleton height={16} />
            </div>
          ) : runs.length === 0 ? (
            <EmptyState
              title="No runs yet"
              hint="Run an agent from the Agents view to populate the execution log."
              action={
                <button type="button" className="primary-button" onClick={() => onNavigate('Agents')}>
                  Browse agents
                </button>
              }
            />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Run</th>
                    <th>Agent</th>
                    <th>Status</th>
                    <th>Created</th>
                  </tr>
                </thead>
                <tbody>
                  {runs.slice(0, 6).map((run) => (
                    <tr key={run.id} onClick={() => onOpenRun(run.id)} className="row-link">
                      <td>{shortenId(run.id)}</td>
                      <td>{agentName(run.agentId)}</td>
                      <td>
                        <StatusPill status={run.status} />
                      </td>
                      <td>{formatRelativeTime(run.createdAt)}</td>
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
              <p className="eyebrow">Quality</p>
              <h3>Run quality</h3>
            </div>
          </div>

          {runsQuery.isPending ? (
            <div className="stack-gap">
              <Skeleton height={16} />
              <Skeleton height={16} />
              <Skeleton height={16} />
            </div>
          ) : (
            <div className="quality-list">
              <div>
                <label>Completion rate</label>
                <div className="meter">
                  <span style={{ width: `${completedPct ?? 0}%` }} />
                </div>
                <strong>{completedPct === null ? 'No runs yet' : `${completedPct}% of ${formatNumber(total)}`}</strong>
              </div>
              <div>
                <label>Failure rate</label>
                <div className="meter">
                  <span style={{ width: `${failedPct ?? 0}%` }} />
                </div>
                <strong>{failedPct === null ? 'No runs yet' : `${failedPct}% of runs`}</strong>
              </div>
              <div>
                <label>Active share</label>
                <div className="meter">
                  <span style={{ width: `${activePct ?? 0}%` }} />
                </div>
                <strong>{activePct === null ? 'No runs yet' : `${activePct}% queued or running`}</strong>
              </div>
            </div>
          )}
        </article>
      </section>
    </>
  )
}

// ---------------------------------------------------------------------------
// Agents (live: /agents, /agents/create, /agents/{id}; runs via /runs)
// ---------------------------------------------------------------------------

function AgentsView({
  selectedAgentId,
  onSelectAgent,
  onOpenRun,
}: {
  selectedAgentId: string | null
  onSelectAgent: (id: string | null) => void
  onOpenRun: (runId: string) => void
}) {
  const agentsQuery = useAgents()
  const [showCreate, setShowCreate] = useState(false)
  const agents = useMemo(() => agentsQuery.data ?? [], [agentsQuery.data])
  const draftCount = agents.filter((agent) => agent.status.toUpperCase() === 'DRAFT').length
  const modelCount = new Set(agents.map((agent) => agent.model).filter(Boolean)).size

  if (selectedAgentId) {
    return <AgentDetailView agentId={selectedAgentId} onBack={() => onSelectAgent(null)} onRunStarted={onOpenRun} />
  }

  return (
    <>
      <PageHeader
        eyebrow="Catalog"
        title="Agents"
        actions={
          <>
            <button type="button" className="ghost-button">
              Filters
            </button>
            <button type="button" className="primary-button" onClick={() => setShowCreate((open) => !open)}>
              New agent
            </button>
          </>
        }
      />

      {showCreate ? (
        <CreateAgentForm
          onCreated={(agent) => {
            setShowCreate(false)
            onSelectAgent(agent.id)
          }}
          onCancel={() => setShowCreate(false)}
        />
      ) : null}

      {agentsQuery.isError ? <ErrorBanner error={agentsQuery.error} onRetry={() => void agentsQuery.refetch()} /> : null}

      <section className="summary-grid">
        {agentsQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Total agents" value={formatNumber(agents.length)} />
            <SummaryStat label="Drafts" value={formatNumber(draftCount)} accent="info" />
            <SummaryStat label="Models in use" value={formatNumber(modelCount)} accent="success" />
            <SummaryStat label="Live (non-draft)" value={formatNumber(agents.length - draftCount)} accent="warning" />
          </>
        )}
      </section>

      <section className="agent-grid">
        {agentsQuery.isPending ? (
          [0, 1, 2].map((index) => (
            <article key={index} className="agent-card">
              <Skeleton height={18} width="60%" />
              <Skeleton height={12} style={{ marginTop: 10 }} />
              <Skeleton height={12} width="80%" style={{ marginTop: 8 }} />
              <Skeleton height={54} style={{ marginTop: 16 }} />
            </article>
          ))
        ) : agents.length === 0 ? (
          <div className="agent-grid-empty">
            <EmptyState
              title="No agents yet — create your first agent"
              hint="Agents bundle instructions, a model, and tool access. Register one and it appears here immediately."
              action={
                <button type="button" className="primary-button" onClick={() => setShowCreate(true)}>
                  Create agent
                </button>
              }
            />
          </div>
        ) : (
          agents.map((agent) => (
            <article key={agent.id} className="agent-card">
              <div className="agent-card-header">
                <div>
                  <h3>{agent.name}</h3>
                  <p>{agent.description || 'No description provided.'}</p>
                </div>
                <StatusPill status={agent.status} />
              </div>

              <div className="detail-grid">
                <div>
                  <label>Model</label>
                  <strong>{agent.model || '—'}</strong>
                </div>
                <div>
                  <label>Version</label>
                  <strong>{agent.currentVersionId ? shortenId(agent.currentVersionId) : '—'}</strong>
                </div>
                <div>
                  <label>Created</label>
                  <strong>{formatRelativeTime(agent.createdAt)}</strong>
                </div>
              </div>

              <div className="card-actions">
                <button type="button" className="ghost-button" onClick={() => onSelectAgent(agent.id)}>
                  Inspect
                </button>
                <button type="button" className="primary-button small" onClick={() => onSelectAgent(agent.id)}>
                  Run
                </button>
              </div>
            </article>
          ))
        )}
      </section>
    </>
  )
}

function CreateAgentForm({ onCreated, onCancel }: { onCreated: (agent: Agent) => void; onCancel: () => void }) {
  const createAgent = useCreateAgent()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [instructions, setInstructions] = useState('')
  const [model, setModel] = useState('gpt-4o-mini')

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (createAgent.isPending) return
    createAgent.mutate(
      {
        name: name.trim(),
        description: description.trim(),
        instructions: instructions.trim(),
        model: model.trim(),
      },
      { onSuccess: onCreated },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">New</p>
          <h3>Create agent</h3>
        </div>
        <button type="button" className="link-button" onClick={onCancel}>
          Cancel
        </button>
      </div>

      {createAgent.isError ? <div className="form-error">{describeError(createAgent.error)}</div> : null}

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="agent-name">Name</label>
            <input id="agent-name" value={name} onChange={(event) => setName(event.target.value)} placeholder="Support triage" required />
          </div>
          <div className="field">
            <label htmlFor="agent-model">Model</label>
            <input id="agent-model" list="common-models" value={model} onChange={(event) => setModel(event.target.value)} required />
            <datalist id="common-models">
              {COMMON_MODELS.map((entry) => (
                <option key={entry} value={entry} />
              ))}
            </datalist>
          </div>
          <div className="field span-2">
            <label htmlFor="agent-description">Description</label>
            <input
              id="agent-description"
              value={description}
              onChange={(event) => setDescription(event.target.value)}
              placeholder="What does this agent do?"
            />
          </div>
          <div className="field span-2">
            <label htmlFor="agent-instructions">Instructions</label>
            <textarea
              id="agent-instructions"
              rows={4}
              value={instructions}
              onChange={(event) => setInstructions(event.target.value)}
              placeholder="System instructions sent to the model on every run"
              required
            />
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">POST /agents/create</span>
          <button type="submit" className="primary-button" disabled={createAgent.isPending}>
            {createAgent.isPending ? 'Creating…' : 'Create agent'}
          </button>
        </div>
      </form>
    </article>
  )
}

function AgentDetailView({
  agentId,
  onBack,
  onRunStarted,
}: {
  agentId: string
  onBack: () => void
  onRunStarted: (runId: string) => void
}) {
  const agentQuery = useAgent(agentId)
  const runAgent = useRunAgent()
  const [runInput, setRunInput] = useState('')
  const agent = agentQuery.data

  const startRun = (event: FormEvent) => {
    event.preventDefault()
    const input = runInput.trim()
    if (!input || runAgent.isPending) return
    runAgent.mutate({ agentId, input }, { onSuccess: (run) => { setRunInput(''); onRunStarted(run.id) } })
  }

  return (
    <>
      <PageHeader
        eyebrow="Agent"
        title={agent?.name ?? 'Agent'}
        actions={
          <button type="button" className="ghost-button" onClick={onBack}>
            ← Back to agents
          </button>
        }
      />

      {agentQuery.isError ? <ErrorBanner error={agentQuery.error} onRetry={() => void agentQuery.refetch()} /> : null}

      {agentQuery.isPending ? (
        <article className="panel">
          <Skeleton height={18} width="40%" />
          <Skeleton height={120} style={{ marginTop: 16 }} />
        </article>
      ) : agent ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Status" value={agent.status} accent={statusAccent(agent.status)} />
            <SummaryStat label="Model" value={agent.model || '—'} accent="info" />
            <SummaryStat
              label="Version"
              value={agent.currentVersionId ? shortenId(agent.currentVersionId) : agent.version !== undefined ? `v${agent.version}` : '—'}
              accent="success"
            />
            <SummaryStat label="Created" value={formatRelativeTime(agent.createdAt)} accent="warning" />
          </section>

          <section className="content-grid">
            <article className="panel wide">
              <div className="panel-header">
                <div>
                  <p className="eyebrow">Configuration</p>
                  <h3>Instructions</h3>
                </div>
              </div>
              <p className="detail-copy">{agent.instructions || 'No instructions set.'}</p>
              {agent.description ? <p className="detail-copy muted">{agent.description}</p> : null}
              <div className="detail-grid">
                <div>
                  <label>Agent ID</label>
                  <strong>{agent.id || '—'}</strong>
                </div>
                <div>
                  <label>Organization</label>
                  <strong>{agent.organizationId ? shortenId(agent.organizationId) : '—'}</strong>
                </div>
                <div>
                  <label>Updated</label>
                  <strong>{formatDateTime(agent.updatedAt)}</strong>
                </div>
              </div>
            </article>

            <article className="panel">
              <div className="panel-header">
                <div>
                  <p className="eyebrow">Execute</p>
                  <h3>Run agent</h3>
                </div>
              </div>

              {runAgent.isError ? <div className="form-error">{describeError(runAgent.error)}</div> : null}

              <form onSubmit={startRun}>
                <div className="field">
                  <label htmlFor="run-input">Input</label>
                  <textarea
                    id="run-input"
                    rows={5}
                    value={runInput}
                    onChange={(event) => setRunInput(event.target.value)}
                    placeholder="Give this agent something to do…"
                    required
                  />
                </div>
                <div className="form-actions">
                  <span className="form-note">Queues a run via POST /runs</span>
                  <button type="submit" className="primary-button" disabled={runAgent.isPending}>
                    {runAgent.isPending ? 'Queuing…' : 'Start run'}
                  </button>
                </div>
              </form>
            </article>
          </section>
        </>
      ) : (
        <EmptyState
          title="Agent not found"
          hint="It may have been removed or belongs to a different organization."
          action={
            <button type="button" className="ghost-button" onClick={onBack}>
              Back to agents
            </button>
          }
        />
      )}
    </>
  )
}

// ---------------------------------------------------------------------------
// Runs (live: /runs, /runs/{id}, /runs/{id}/events via SSE)
// ---------------------------------------------------------------------------

function RunsView({
  selectedRunId,
  onSelectRun,
  onNavigate,
}: {
  selectedRunId: string | null
  onSelectRun: (id: string | null) => void
  onNavigate: (view: ViewName) => void
}) {
  const runsQuery = useRuns()
  const agentsQuery = useAgents()
  const [showTrigger, setShowTrigger] = useState(false)
  const runs = useMemo(() => sortRunsDesc(runsQuery.data ?? []), [runsQuery.data])
  const agents = useMemo(() => agentsQuery.data ?? [], [agentsQuery.data])
  const statusCounts = useMemo(() => countByStatus(runs), [runs])
  const agentName = (id?: string) => agents.find((agent) => agent.id === id)?.name ?? shortenId(id)

  if (selectedRunId) {
    return <RunDetailView runId={selectedRunId} agentName={agentName} onClose={() => onSelectRun(null)} />
  }

  return (
    <>
      <PageHeader
        eyebrow="Execution"
        title="Runs"
        actions={
          <>
            <button type="button" className="ghost-button">
              Filters
            </button>
            <button type="button" className="primary-button" onClick={() => setShowTrigger((open) => !open)}>
              Trigger run
            </button>
          </>
        }
      />

      {showTrigger ? (
        <TriggerRunForm
          agents={agents}
          onStarted={(run) => {
            setShowTrigger(false)
            onSelectRun(run.id)
          }}
          onCancel={() => setShowTrigger(false)}
          onNavigate={onNavigate}
        />
      ) : null}

      {runsQuery.isError ? <ErrorBanner error={runsQuery.error} onRetry={() => void runsQuery.refetch()} /> : null}

      <section className="summary-grid">
        {runsQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Queued" value={formatNumber(statusCounts.QUEUED)} />
            <SummaryStat label="Running" value={formatNumber(statusCounts.RUNNING)} accent="info" />
            <SummaryStat label="Completed" value={formatNumber(statusCounts.COMPLETED)} accent="success" />
            <SummaryStat label="Failed" value={formatNumber(statusCounts.FAILED)} accent="warning" />
          </>
        )}
      </section>

      <section className="content-grid">
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Live</p>
              <h3>Recent executions</h3>
            </div>
          </div>

          {runsQuery.isPending ? (
            <div className="stack-gap">
              <Skeleton height={16} />
              <Skeleton height={16} />
              <Skeleton height={16} />
              <Skeleton height={16} />
            </div>
          ) : runs.length === 0 ? (
            <EmptyState
              title="No runs yet"
              hint="Trigger a run against one of your agents to see live status and output here."
              action={
                agents.length > 0 ? (
                  <button type="button" className="primary-button" onClick={() => setShowTrigger(true)}>
                    Trigger run
                  </button>
                ) : (
                  <button type="button" className="primary-button" onClick={() => onNavigate('Agents')}>
                    Create an agent first
                  </button>
                )
              }
            />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Run</th>
                    <th>Agent</th>
                    <th>Status</th>
                    <th>Created</th>
                    <th>Updated</th>
                  </tr>
                </thead>
                <tbody>
                  {runs.slice(0, 12).map((run) => (
                    <tr key={run.id} onClick={() => onSelectRun(run.id)} className="row-link">
                      <td>{shortenId(run.id)}</td>
                      <td>{agentName(run.agentId)}</td>
                      <td>
                        <StatusPill status={run.status} />
                      </td>
                      <td>{formatRelativeTime(run.createdAt)}</td>
                      <td>{formatRelativeTime(run.updatedAt)}</td>
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
              <p className="eyebrow">Status</p>
              <h3>Run health</h3>
            </div>
          </div>

          {runsQuery.isPending ? (
            <div className="stack-gap">
              <Skeleton height={16} />
              <Skeleton height={16} />
              <Skeleton height={16} />
            </div>
          ) : (
            <div className="quality-list">
              <div>
                <label>Completion rate</label>
                <div className="meter">
                  <span style={{ width: `${runs.length ? Math.round((statusCounts.COMPLETED / runs.length) * 100) : 0}%` }} />
                </div>
                <strong>{runs.length ? `${Math.round((statusCounts.COMPLETED / runs.length) * 100)}% of ${formatNumber(runs.length)}` : 'No runs yet'}</strong>
              </div>
              <div>
                <label>Failure rate</label>
                <div className="meter">
                  <span style={{ width: `${runs.length ? Math.round((statusCounts.FAILED / runs.length) * 100) : 0}%` }} />
                </div>
                <strong>{runs.length ? `${Math.round((statusCounts.FAILED / runs.length) * 100)}% of runs` : 'No runs yet'}</strong>
              </div>
              <div>
                <label>In flight</label>
                <div className="meter">
                  <span style={{ width: `${runs.length ? Math.round(((statusCounts.QUEUED + statusCounts.RUNNING) / runs.length) * 100) : 0}%` }} />
                </div>
                <strong>{formatNumber(statusCounts.QUEUED + statusCounts.RUNNING)} queued or running</strong>
              </div>
            </div>
          )}
        </article>
      </section>
    </>
  )
}

function TriggerRunForm({
  agents,
  onStarted,
  onCancel,
  onNavigate,
}: {
  agents: Agent[]
  onStarted: (run: Run) => void
  onCancel: () => void
  onNavigate: (view: ViewName) => void
}) {
  const runAgent = useRunAgent()
  const [agentId, setAgentId] = useState(agents[0]?.id ?? '')
  const [input, setInput] = useState('')

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (runAgent.isPending || !agentId) return
    runAgent.mutate({ agentId, input: input.trim() }, { onSuccess: onStarted })
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Execute</p>
          <h3>Trigger run</h3>
        </div>
        <button type="button" className="link-button" onClick={onCancel}>
          Cancel
        </button>
      </div>

      {runAgent.isError ? <div className="form-error">{describeError(runAgent.error)}</div> : null}

      {agents.length === 0 ? (
        <EmptyState
          title="No agents available"
          hint="You need at least one agent before you can trigger a run."
          action={
            <button type="button" className="ghost-button" onClick={() => onNavigate('Agents')}>
              Go to agents
            </button>
          }
        />
      ) : (
        <form onSubmit={submit}>
          <div className="form-grid">
            <div className="field">
              <label htmlFor="trigger-agent">Agent</label>
              <select id="trigger-agent" value={agentId} onChange={(event) => setAgentId(event.target.value)} required>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name} ({agent.model || 'unknown model'})
                  </option>
                ))}
              </select>
            </div>
            <div className="field span-2">
              <label htmlFor="trigger-input">Input</label>
              <textarea id="trigger-input" rows={4} value={input} onChange={(event) => setInput(event.target.value)} required placeholder="Input sent to the agent" />
            </div>
          </div>
          <div className="form-actions">
            <span className="form-note">POST /runs</span>
            <button type="submit" className="primary-button" disabled={runAgent.isPending}>
              {runAgent.isPending ? 'Queuing…' : 'Start run'}
            </button>
          </div>
        </form>
      )}
    </article>
  )
}

function RunDetailView({
  runId,
  agentName,
  onClose,
}: {
  runId: string
  agentName: (id?: string) => string
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const runQuery = useRun(runId)
  const lastStepRefreshRef = useRef(0)
  // The SSE stream doubles as the timeline refresh signal: step events on
  // /runs/{id}/events trigger a refetch of /runs/{id}/steps (throttled).
  useRunEvents(runId, () => {
    const now = Date.now()
    if (now - lastStepRefreshRef.current < 1200) return
    lastStepRefreshRef.current = now
    void queryClient.invalidateQueries({ queryKey: ['runs', runId, 'steps'] })
  })
  const run = runQuery.data
  const live = run ? !isTerminalRunStatus(run.status) : true

  return (
    <>
      <PageHeader
        eyebrow="Execution"
        title={`Run ${shortenId(runId)}`}
        actions={
          <button type="button" className="ghost-button" onClick={onClose}>
            ← Back to runs
          </button>
        }
      />

      {runQuery.isError ? <ErrorBanner error={runQuery.error} onRetry={() => void runQuery.refetch()} /> : null}

      {runQuery.isPending ? (
        <article className="panel">
          <Skeleton height={18} width="40%" />
          <Skeleton height={120} style={{ marginTop: 16 }} />
        </article>
      ) : run ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Status" value={run.status} accent={statusAccent(run.status)} />
            <SummaryStat label="Agent" value={agentName(run.agentId)} accent="info" />
            <SummaryStat label="Queued" value={formatRelativeTime(run.createdAt)} accent="success" />
            <SummaryStat label="Updated" value={formatRelativeTime(run.updatedAt)} accent="warning" />
          </section>

          <RunTimeline runId={runId} live={live} />

          <section className="content-grid">
            <article className="panel wide">
              <div className="panel-header">
                <div>
                  <p className="eyebrow">Live feed</p>
                  <h3>Run output</h3>
                </div>
                <span className="form-note">{live ? 'Streaming via SSE · polling fallback every 4s' : 'Final'}</span>
              </div>
              <StatusPill status={run.status} />
              <p className="run-output-label" style={{ marginTop: 14 }}>
                Input
              </p>
              <div className="run-output">{run.input || '—'}</div>
              <p className="run-output-label">Output</p>
              <div className="run-output">{run.output || (live ? 'Waiting for output…' : 'No output recorded')}</div>
            </article>

            <article className="panel">
              <div className="panel-header">
                <div>
                  <p className="eyebrow">Metadata</p>
                  <h3>Details</h3>
                </div>
              </div>
              <div className="quality-list">
                <div>
                  <label>Run ID</label>
                  <strong>{run.id || '—'}</strong>
                </div>
                <div>
                  <label>Agent ID</label>
                  <strong>{run.agentId || '—'}</strong>
                </div>
                <div>
                  <label>Organization</label>
                  <strong>{run.organizationId ? shortenId(run.organizationId) : '—'}</strong>
                </div>
                <div>
                  <label>Created</label>
                  <strong>{formatDateTime(run.createdAt)}</strong>
                </div>
                <div>
                  <label>Updated</label>
                  <strong>{formatDateTime(run.updatedAt)}</strong>
                </div>
              </div>
            </article>
          </section>
        </>
      ) : (
        <EmptyState
          title="Run not found"
          hint="The run may have been pruned or belongs to a different organization."
          action={
            <button type="button" className="ghost-button" onClick={onClose}>
              Back to runs
            </button>
          }
        />
      )}
    </>
  )
}

// ---------------------------------------------------------------------------
// Demo views (no live endpoints yet) — content kept, honestly badged
// ---------------------------------------------------------------------------

const toolCards = [
  { name: 'Slack notifier', category: 'Communication', status: 'Healthy', latency: '180ms', permissions: 'write:messages' },
  { name: 'Postgres query', category: 'Data access', status: 'Running', latency: '590ms', permissions: 'read:analytics' },
  { name: 'Kubernetes deploy', category: 'Infrastructure', status: 'Review', latency: '1.2s', permissions: 'deploy:staging' },
  { name: 'CRM sync', category: 'Sales ops', status: 'Healthy', latency: '240ms', permissions: 'write:records' },
]

const securityRows = [
  { name: 'MFA enforcement', status: 'Healthy', coverage: '96%' },
  { name: 'Secret rotation', status: 'Review', coverage: '71%' },
  { name: 'IAM drift', status: 'Running', coverage: '84%' },
  { name: 'Audit trail', status: 'Healthy', coverage: '99%' },
]

const infrastructureRows = [
  { name: 'Core API', region: 'us-east-1', replicas: '8', status: 'Healthy' },
  { name: 'Workers', region: 'eu-west-1', replicas: '6', status: 'Running' },
  { name: 'Queue broker', region: 'us-west-2', replicas: '3', status: 'Review' },
  { name: 'Storage', region: 'global', replicas: '4', status: 'Healthy' },
]

function ToolsView() {
  return (
    <>
      <PageHeader
        eyebrow="Tooling"
        title="Tools"
        badge={<DemoBadge />}
        actions={
          <>
            <button type="button" className="ghost-button">Marketplace</button>
            <button type="button" className="primary-button">Add tool</button>
          </>
        }
      />

      <section className="summary-grid">
        <SummaryStat label="Total tools" value="36" accent="default" />
        <SummaryStat label="Secure" value="31" accent="info" />
        <SummaryStat label="Needs review" value="3" accent="warning" />
        <SummaryStat label="Calls / hr" value="24.8k" accent="success" />
      </section>

      <section className="tool-grid">
        {toolCards.map((tool) => (
          <article key={tool.name} className="tool-card">
            <div className="tool-card-header">
              <div>
                <p className="eyebrow small">{tool.category}</p>
                <h3>{tool.name}</h3>
              </div>
              <StatusPill status={tool.status} />
            </div>

            <div className="tool-meta">
              <div>
                <label>Latency</label>
                <strong>{tool.latency}</strong>
              </div>
              <div>
                <label>Permission</label>
                <strong>{tool.permissions}</strong>
              </div>
            </div>

            <div className="card-actions">
              <button type="button" className="ghost-button">Permissions</button>
              <button type="button" className="primary-button small">Enable</button>
            </div>
          </article>
        ))}
      </section>
    </>
  )
}

function SecurityView() {
  return (
    <>
      <PageHeader
        eyebrow="Governance"
        title="Security"
        badge={<DemoBadge />}
        actions={
          <>
            <button type="button" className="ghost-button">Policies</button>
            <button type="button" className="primary-button">Run audit</button>
          </>
        }
      />

      <section className="summary-grid">
        <article className="mini-stat">
          <span>Controls</span>
          <strong>41</strong>
        </article>
        <article className="mini-stat">
          <span>Protected</span>
          <strong>98%</strong>
        </article>
        <article className="mini-stat">
          <span>Incidents</span>
          <strong>2</strong>
        </article>
        <article className="mini-stat">
          <span>MTTR</span>
          <strong>14m</strong>
        </article>
      </section>

      <section className="security-grid">
        {securityRows.map((control) => (
          <article key={control.name} className="security-card">
            <div className="security-card-header">
              <h3>{control.name}</h3>
              <StatusPill status={control.status} />
            </div>
            <div className="security-row">
              <label>Coverage</label>
              <strong>{control.coverage}</strong>
            </div>
            <div className="meter"><span style={{ width: control.coverage }} /></div>
          </article>
        ))}
      </section>
    </>
  )
}

function InfrastructureView() {
  return (
    <>
      <PageHeader
        eyebrow="Operations"
        title="Infrastructure"
        badge={<DemoBadge />}
        actions={
          <>
            <button type="button" className="ghost-button">Scale plan</button>
            <button type="button" className="primary-button">Deploy update</button>
          </>
        }
      />

      <section className="summary-grid">
        <article className="mini-stat">
          <span>Clusters</span>
          <strong>7</strong>
        </article>
        <article className="mini-stat">
          <span>Replicas</span>
          <strong>26</strong>
        </article>
        <article className="mini-stat">
          <span>Uptime</span>
          <strong>99.97%</strong>
        </article>
        <article className="mini-stat">
          <span>Alerts</span>
          <strong>3</strong>
        </article>
      </section>

      <section className="infra-grid">
        {infrastructureRows.map((service) => (
          <article key={service.name} className="infra-card">
            <div className="infra-card-header">
              <div>
                <p className="eyebrow small">{service.region}</p>
                <h3>{service.name}</h3>
              </div>
              <StatusPill status={service.status} />
            </div>

            <div className="tool-meta">
              <div>
                <label>Replicas</label>
                <strong>{service.replicas}</strong>
              </div>
              <div>
                <label>Region</label>
                <strong>{service.region}</strong>
              </div>
            </div>
          </article>
        ))}
      </section>
    </>
  )
}

// ---------------------------------------------------------------------------
// App shell
// ---------------------------------------------------------------------------

export default function App() {
  const auth = useSyncExternalStore(subscribeAuth, getAuthSnapshot)
  const health = useHealth()
  const authed = auth.token !== null || auth.apiKey !== null

  const [activeView, setActiveView] = useState<ViewName>('Overview')
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null)
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null)

  if (!authed) return <AuthGate />

  const openRun = (runId: string) => {
    setSelectedRunId(runId)
    setActiveView('Runs')
  }

  const healthz = health.data?.healthz
  const healthOnline = (healthz ?? '').trim().toLowerCase() === 'ok'

  const renderView = () => {
    switch (activeView) {
      case 'Overview':
        return <OverviewView onNavigate={setActiveView} onOpenRun={openRun} />
      case 'Agents':
        return <AgentsView selectedAgentId={selectedAgentId} onSelectAgent={setSelectedAgentId} onOpenRun={openRun} />
      case 'Runs':
        return <RunsView selectedRunId={selectedRunId} onSelectRun={setSelectedRunId} onNavigate={setActiveView} />
      case 'Workflows':
        return <WorkflowsView onOpenRun={openRun} />
      case 'Approvals':
        return <ApprovalsView canDecide={canDecide(auth.user?.role)} />
      case 'Evaluations':
        return <EvaluationsView canWrite={canWrite(auth.user?.role)} />
      case 'Tools':
        return <ToolsView />
      case 'Knowledge':
        return (
          <>
            <DemoStrip note="No Knowledge API endpoint yet — the live platform feed is shown below." />
            <OverviewView onNavigate={setActiveView} onOpenRun={openRun} />
          </>
        )
      case 'Analytics':
        return (
          <>
            <DemoStrip note="No Analytics API endpoint yet — the live platform feed is shown below." />
            <OverviewView onNavigate={setActiveView} onOpenRun={openRun} />
          </>
        )
      case 'Usage':
        return <UsageView />
      case 'Security':
        return <SecurityView />
      case 'Infrastructure':
        return <InfrastructureView />
      default:
        return <OverviewView onNavigate={setActiveView} onOpenRun={openRun} />
    }
  }

  return (
    <div className="dashboard-shell">
      <aside className="sidebar">
        <div className="brand-block">
          <div className="brand-mark">A</div>
          <div>
            <p className="eyebrow">Platform</p>
            <h1>{APP_NAME}</h1>
          </div>
        </div>

        <nav className="nav" aria-label="Main menu">
          {navItems.map((item) => (
            <button
              key={item}
              className={item === activeView ? 'nav-item active' : 'nav-item'}
              type="button"
              onClick={() => setActiveView(item)}
            >
              {item}
              {isDemoView(item) ? <span className="nav-demo-dot" title="Demo data" /> : null}
            </button>
          ))}
        </nav>

        <div className="sidebar-card">
          <p className="eyebrow">System health</p>
          <div className="health-row">
            <span className={healthOnline ? 'dot green' : 'dot amber'} />
            <span>API {healthOnline ? 'healthy' : health.isPending ? 'checking…' : 'unreachable'}</span>
          </div>
          <div className="health-row">
            <span className={health.data?.readyz && health.data.readyz !== 'unreachable' ? 'dot green' : 'dot amber'} />
            <span>Ready: {health.data?.readyz ?? 'checking…'}</span>
          </div>
        </div>
      </aside>

      <main className="main-panel">
        <header className="global-header">
          <div className="searchbox">
            <span className="search-icon">⌕</span>
            <span>Search AgentOS...</span>
          </div>
          <div className="global-header-actions">
            <div className="status-chip">
              <span className={healthOnline ? 'status-dot online' : 'status-dot offline'} />
              {healthOnline ? 'API operational' : 'API unreachable'}
            </div>
            <span className="auth-note">{auth.user?.email ?? 'API key session'}</span>
            <button type="button" className="ghost-button" onClick={() => performLogout()}>
              Sign out
            </button>
          </div>
        </header>

        {renderView()}
      </main>
    </div>
  )
}
