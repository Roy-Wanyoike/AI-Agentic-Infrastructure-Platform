// Versions & deployments view (track 2-b contract + track 3-e diff viewer).
//
// Wired to live endpoints only (no mocks):
// - GET/POST /agents/{id}/versions… — snapshot list, publish, agent rollback
// - GET /agents/{id}/versions/diff?from=&to= — field-level diff (3-e)
// - GET/POST /deployments… — create, promote, rollback (RBAC-gated buttons)
// - POST /deployments/{id}/canary(/promote|/abort) + GET /agents/{agentId}/canary/status
//   — progressive canary rollout controls (issues #13/#51/#81)
//
// RBAC props mirror the API permission grants:
// - canManageVersions (agents.write → OWNER/ADMIN): snapshot, publish, restore
// - canWrite (deployments.write → MEMBER+): request a deployment
// - canDeploy (deployments.deploy → OWNER/ADMIN): promote/rollback + canary ops

import { useMemo, useState, type FormEvent } from 'react'
import {
  useAbortCanary,
  useAgentVersions,
  useAgents,
  useCanaryStatus,
  useCreateAgentVersion,
  useCreateDeployment,
  useDeployments,
  usePromoteCanary,
  usePromoteDeployment,
  usePublishAgentVersion,
  useRollbackAgent,
  useRollbackDeployment,
  useSetDeploymentCanary,
  useVersionDiff,
} from '../lib/hooks'
import {
  DEPLOYMENT_ENVIRONMENTS,
  isTerminalDeploymentStatus,
  type AgentConfigVersion,
  type VersionDiffField,
} from '../lib/api/versions'
import type { CanaryStatus } from '../lib/api/canary'
import { ApiError } from '../lib/api/client'
import { formatDateTime, formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { apiErrorCode, describeError } from './uiHelpers'

/** Renders a raw JSON diff value defensively (never "undefined" / "[object Object]"). */
function formatDiffValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'string') return value === '' ? '""' : value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  try {
    return JSON.stringify(value)
  } catch {
    return String(value)
  }
}

function SnapshotButton({ agentId, canManage }: { agentId: string; canManage: boolean }) {
  const createVersion = useCreateAgentVersion(agentId)
  if (!canManage) {
    return <span className="form-note">Viewer/Member role — publishing versions needs OWNER or ADMIN</span>
  }
  return (
    <div className="topbar-actions">
      {createVersion.isError ? <span className="form-error inline">{describeError(createVersion.error)}</span> : null}
      <button
        type="button"
        className="ghost-button"
        onClick={() => createVersion.mutate()}
        disabled={createVersion.isPending}
      >
        {createVersion.isPending ? 'Snapshotting…' : 'Snapshot current config'}
      </button>
    </div>
  )
}

function VersionRowActions({
  agentId,
  version,
  canManage,
}: {
  agentId: string
  version: AgentConfigVersion
  canManage: boolean
}) {
  const publish = usePublishAgentVersion(agentId)
  const rollback = useRollbackAgent(agentId)
  if (!canManage) return <span className="form-note">—</span>
  const busy = publish.isPending || rollback.isPending
  const error = publish.isError ? describeError(publish.error) : rollback.isError ? describeError(rollback.error) : null
  return (
    <div className="table-actions">
      {error ? <span className="form-error inline">{error}</span> : null}
      {version.status === 'draft' ? (
        <button type="button" className="ghost-button small" disabled={busy} onClick={() => publish.mutate(version.version)}>
          Publish
        </button>
      ) : null}
      {version.status !== 'draft' ? (
        <button
          type="button"
          className="ghost-button small"
          disabled={busy}
          title="Re-point the agent to this version and restore its snapshot config"
          onClick={() => rollback.mutate(version.version)}
        >
          Restore
        </button>
      ) : null}
    </div>
  )
}

/** Side-by-side field diff driven by GET /agents/{id}/versions/diff (3-e). */
function DiffViewer({ agentId, versions }: { agentId: string; versions: AgentConfigVersion[] }) {
  const [fromRaw, setFromRaw] = useState('')
  const [toRaw, setToRaw] = useState('')

  // Derived defaults (never setState-in-effect): the two most recent versions.
  const sortedDesc = useMemo(() => [...versions].sort((a, b) => b.version - a.version), [versions])
  const defaultTo = sortedDesc[0]?.version ?? null
  const defaultFrom = sortedDesc[1]?.version ?? defaultTo
  const from = fromRaw !== '' ? Number(fromRaw) : defaultFrom
  const to = toRaw !== '' ? Number(toRaw) : defaultTo

  const diffQuery = useVersionDiff(agentId, from, to)
  const diff = diffQuery.data
  const changedCount = diff?.fields.filter((field) => field.changed).length ?? 0

  const options = sortedDesc.map((version) => (
    <option key={version.version} value={version.version}>
      v{version.version} ({version.status})
    </option>
  ))

  return (
    <article className="panel wide">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Compare</p>
          <h3>Version diff</h3>
        </div>
        <div className="topbar-actions">
          <label className="inline-label" htmlFor="diff-from">
            From
            <select id="diff-from" value={fromRaw !== '' ? fromRaw : String(defaultFrom ?? '')} onChange={(event) => setFromRaw(event.target.value)}>
              {options}
            </select>
          </label>
          <label className="inline-label" htmlFor="diff-to">
            To
            <select id="diff-to" value={toRaw !== '' ? toRaw : String(defaultTo ?? '')} onChange={(event) => setToRaw(event.target.value)}>
              {options}
            </select>
          </label>
        </div>
      </div>

      <p className="form-note">
        GET /agents/{shortenId(agentId)}/versions/diff?from={from ?? '—'}&amp;to={to ?? '—'} · changed fields highlighted
      </p>

      {diffQuery.isError ? <ErrorBanner error={diffQuery.error} onRetry={() => void diffQuery.refetch()} /> : null}

      {diffQuery.isPending ? (
        <div className="stack-gap">
          <Skeleton height={16} />
          <Skeleton height={16} />
          <Skeleton height={16} />
        </div>
      ) : diff ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Fields compared" value={String(diff.fields.length)} />
            <SummaryStat label="Changed" value={String(changedCount)} accent={changedCount > 0 ? 'warning' : 'success'} />
            <SummaryStat label="From" value={`v${diff.from}`} accent="info" />
            <SummaryStat label="To" value={`v${diff.to}`} accent="default" />
          </section>
          <div className="table-wrap diff-table-wrap">
            <table className="diff-table">
              <thead>
                <tr>
                  <th>Field</th>
                  <th>From v{diff.from}</th>
                  <th>To v{diff.to}</th>
                </tr>
              </thead>
              <tbody>
                {diff.fields.map((field: VersionDiffField) => (
                  <tr key={field.field} className={field.changed ? 'diff-row-changed' : undefined}>
                    <td>
                      <code>{field.field}</code>
                    </td>
                    <td className={field.changed ? 'diff-cell-from' : undefined}>
                      {formatDiffValue(field.from)}
                    </td>
                    <td className={field.changed ? 'diff-cell-to' : undefined}>
                      {formatDiffValue(field.to)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      ) : null}
    </article>
  )
}

function CreateDeploymentForm({ agentId, publishedVersions }: { agentId: string; publishedVersions: AgentConfigVersion[] }) {
  const createDeployment = useCreateDeployment()
  const [version, setVersion] = useState('')
  const [environment, setEnvironment] = useState<string>(DEPLOYMENT_ENVIRONMENTS[0])
  const [message, setMessage] = useState<string | null>(null)

  const effectiveVersion = version !== '' ? Number(version) : publishedVersions[0]?.version
  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (!effectiveVersion) return
    setMessage(null)
    createDeployment.mutate(
      { agentId, version: effectiveVersion, environment },
      {
        onSuccess: (deployment) =>
          setMessage(`Deployment ${shortenId(deployment.id)} requested for ${deployment.environment}.`),
        onError: (error) => setMessage(describeError(error)),
      },
    )
  }

  return (
    <form onSubmit={submit} className="stack-gap">
      <div className="form-grid">
        <div className="field">
          <label htmlFor="deployment-version">Version (must be published)</label>
          <select id="deployment-version" value={effectiveVersion !== undefined ? String(effectiveVersion) : ''} onChange={(event) => setVersion(event.target.value)} required>
            {publishedVersions.length === 0 ? <option value="">No published versions</option> : null}
            {publishedVersions.map((candidate) => (
              <option key={candidate.version} value={candidate.version}>
                v{candidate.version}
              </option>
            ))}
          </select>
        </div>
        <div className="field">
          <label htmlFor="deployment-environment">Environment</label>
          <select id="deployment-environment" value={environment} onChange={(event) => setEnvironment(event.target.value)}>
            {DEPLOYMENT_ENVIRONMENTS.map((candidate) => (
              <option key={candidate} value={candidate}>
                {candidate}
              </option>
            ))}
          </select>
        </div>
      </div>
      {message ? <div className={createDeployment.isError ? 'form-error' : 'form-note'}>{message}</div> : null}
      <div className="form-actions">
        <span className="form-note">POST /deployments/create</span>
        <button type="submit" className="primary-button" disabled={createDeployment.isPending || publishedVersions.length === 0}>
          {createDeployment.isPending ? 'Requesting…' : 'Request deployment'}
        </button>
      </div>
    </form>
  )
}

function DeploymentActions({ deploymentId, status, canDeploy }: { deploymentId: string; status: string; canDeploy: boolean }) {
  const promote = usePromoteDeployment()
  const rollback = useRollbackDeployment()
  if (!canDeploy) {
    return <span className="form-note">deploy needs OWNER/ADMIN</span>
  }
  const busy = promote.isPending || rollback.isPending
  const error = promote.isError ? describeError(promote.error) : rollback.isError ? describeError(rollback.error) : null
  return (
    <div className="table-actions">
      {error ? <span className="form-error inline">{error}</span> : null}
      <button
        type="button"
        className="ghost-button small"
        disabled={busy || isTerminalDeploymentStatus(status)}
        title={isTerminalDeploymentStatus(status) ? 'Terminal deployments cannot advance' : 'Advance the lifecycle one step'}
        onClick={() => promote.mutate(deploymentId)}
      >
        Promote
      </button>
      <button
        type="button"
        className="ghost-button small"
        disabled={busy}
        title="Re-point the environment to the previous healthy version"
        onClick={() => rollback.mutate(deploymentId)}
      >
        Roll back
      </button>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Canary rollout (issue #81): traffic split controls over the deployment
// lifecycle. The status endpoint (GET /agents/{agentId}/canary/status) is the
// single source of truth — the split %, the eval-gated decision (when the
// auto-promotion engine recorded one) and fresh eval sample stats are polled
// live; writes go through the same healthy deployment row the status names.
// ---------------------------------------------------------------------------

/**
 * Live split slider (1-100). The surrounding container is keyed by the live
 * server weight so this re-initializes only when the split actually changes
 * server-side — dragging never fights the poller.
 */
function CanaryWeightSlider({ initialWeight, value, onChange }: { initialWeight: number; value?: number; onChange?: (weight: number) => void }) {
  const [weight, setWeight] = useState(initialWeight)
  const current = value ?? weight
  const update = (next: number) => {
    setWeight(next)
    onChange?.(next)
  }
  return (
    <div className="field">
      <label htmlFor="canary-weight">
        Canary traffic — <strong>{current}%</strong> canary / {100 - current}% stable
      </label>
      <input
        id="canary-weight"
        type="range"
        className="weight-slider"
        min={1}
        max={100}
        step={1}
        value={current}
        onChange={(event) => update(Number(event.target.value))}
      />
    </div>
  )
}

function CanaryControls({ status, versions, canDeploy }: { status: CanaryStatus; versions: AgentConfigVersion[]; canDeploy: boolean }) {
  const setCanary = useSetDeploymentCanary(status.agentId, status.environment)
  const promote = usePromoteCanary(status.agentId, status.environment)
  const abort = useAbortCanary(status.agentId, status.environment)
  const [message, setMessage] = useState<string | null>(null)
  const [messageError, setMessageError] = useState(false)
  // Local slider position; it re-snaps to the polled server truth whenever
  // the split changes out-of-band (another operator, the auto-decision), so
  // dragging never fights the live status poller.
  const [weight, setWeight] = useState(Math.max(status.canaryWeight, 1))
  const [seenServerWeight, setSeenServerWeight] = useState(status.canaryWeight)
  if (status.canaryWeight !== seenServerWeight) {
    setSeenServerWeight(status.canaryWeight)
    setWeight(Math.max(status.canaryWeight, 1))
  }

  const report = (ok: string | null, error: unknown) => {
    setMessageError(Boolean(error))
    setMessage(error ? describeError(error) : ok)
  }

  const busy = setCanary.isPending || promote.isPending || abort.isPending

  const candidateVersions = useMemo(
    () =>
      versions
        .filter((version) => version.version !== status.stableVersion)
        .sort((a, b) => b.version - a.version),
    [versions, status.stableVersion],
  )

  const startCanary = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const raw = new FormData(event.currentTarget).get('canary-version')
    const version = Number(raw)
    if (!version) return
    setCanary.mutate(
      { deploymentId: status.deploymentId, canaryVersion: version },
      { onSuccess: (deployment) => report(`Canary v${version} attached to ${deployment.environment} at ${deployment.canaryWeight}% traffic.`, null), onError: (error) => report(null, error) },
    )
  }

  const confirmPromote = () => {
    if (!window.confirm(`Promote canary v${status.canaryVersion} to stable? It will serve 100% of ${status.environment} traffic.`)) return
    promote.mutate(status.deploymentId, {
      onSuccess: (deployment) => report(`Canary promoted — v${deployment.version} is now the stable ${deployment.environment} version.`, null),
      onError: (error) => report(null, error),
    })
  }

  const confirmAbort = () => {
    if (!window.confirm(`Abort canary v${status.canaryVersion}? The stable v${status.stableVersion} returns to 100% of traffic.`)) return
    abort.mutate(status.deploymentId, {
      onSuccess: () => report(`Canary aborted — stable v${status.stableVersion} serves 100% again.`, null),
      onError: (error) => report(null, error),
    })
  }

  return (
    <div className="stack-gap">
      {!canDeploy ? <p className="detail-copy muted">Canary operations change what serves traffic — they need OWNER or ADMIN (deployments.deploy).</p> : null}
      {canDeploy && message ? <div className={messageError ? 'form-error' : 'form-note'}>{message}</div> : null}

      {canDeploy && !status.canaryActive ? (
        <form onSubmit={startCanary} className="stack-gap">
          <div className="form-grid">
            <div className="field">
              <label htmlFor="canary-version">Canary version (must differ from stable v{status.stableVersion})</label>
              <select id="canary-version" name="canary-version" required defaultValue="">
                <option value="" disabled>
                  {candidateVersions.length === 0 ? 'No other versions exist' : 'Pick a version…'}
                </option>
                {candidateVersions.map((version) => (
                  <option key={version.version} value={version.version}>
                    v{version.version} ({version.status})
                  </option>
                ))}
              </select>
            </div>
          </div>
          <div className="form-actions">
            <span className="form-note">POST /deployments/{shortenId(status.deploymentId)}/canary — ramp starts at 0%</span>
            <button type="submit" className="primary-button" disabled={busy || candidateVersions.length === 0}>
              {setCanary.isPending ? 'Starting…' : 'Start canary'}
            </button>
          </div>
        </form>
      ) : null}

      {canDeploy && status.canaryActive ? (
        <>
          <div className="canary-controls" key={`${status.deploymentId}:${status.canaryWeight}`}>
            <CanaryWeightSlider initialWeight={Math.max(status.canaryWeight, 1)} value={weight} onChange={setWeight} />
            <button
              type="button"
              className="ghost-button small"
              disabled={busy || weight === status.canaryWeight}
              onClick={() =>
                setCanary.mutate(
                  { deploymentId: status.deploymentId, canaryWeight: weight },
                  { onSuccess: (deployment) => report(`Split moved — canary now serves ${deployment.canaryWeight}%.`, null), onError: (error) => report(null, error) },
                )
              }
            >
              {setCanary.isPending ? 'Applying…' : 'Apply split'}
            </button>
          </div>
          <div className="card-actions">
            <button type="button" className="primary-button small" disabled={busy} onClick={confirmPromote}>
              {promote.isPending ? 'Promoting…' : `Promote canary v${status.canaryVersion}`}
            </button>
            <button type="button" className="danger-button small" disabled={busy} onClick={confirmAbort}>
              {abort.isPending ? 'Aborting…' : 'Abort canary'}
            </button>
          </div>
        </>
      ) : null}
    </div>
  )
}

function CanaryDecisionPanel({ status }: { status: CanaryStatus }) {
  const decision = status.decision
  if (!decision) {
    return status.policy ? (
      <p className="form-note">No automatic decision yet — the eval-gated engine records one per canary window once {status.policy.minCanaryRuns}+ eval run(s) complete.</p>
    ) : null
  }
  return (
    <div className="canary-decision">
      <div className="canary-decision-head">
        <StatusPill status={decision.action === 'promote' ? 'promote' : 'rollback'} />
        <span>{formatDateTime(decision.decidedAt)}</span>
        <span>
          {decision.runsCounted} run(s) · pass rate {(decision.passRate * 100).toFixed(1)}% · p95 {decision.p95LatencyMs}ms · avg cost {decision.avgCostCents.toFixed(2)}¢
        </span>
      </div>
      {decision.reason ? <p className="canary-reason">{decision.reason}</p> : null}
    </div>
  )
}

function CanaryPanel({ agentId, versions, canDeploy }: { agentId: string; versions: AgentConfigVersion[]; canDeploy: boolean }) {
  const [environment, setEnvironment] = useState<string>(DEPLOYMENT_ENVIRONMENTS[2])
  const statusQuery = useCanaryStatus(agentId, environment)
  const status = statusQuery.data

  // 404 NOT_FOUND = "no healthy deployment serves this agent+environment" —
  // a documented empty state (request a deployment first), not a failure.
  const noServingDeployment =
    statusQuery.isError && ((statusQuery.error instanceof ApiError && statusQuery.error.status === 404) || apiErrorCode(statusQuery.error) === 'NOT_FOUND')

  return (
    <article className="panel wide">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Progressive rollout</p>
          <h3>Canary</h3>
        </div>
        <div className="topbar-actions">
          {status?.canaryActive ? <StatusPill status="live" /> : null}
          <label className="inline-label" htmlFor="canary-environment">
            Environment
            <select id="canary-environment" value={environment} onChange={(event) => setEnvironment(event.target.value)}>
              {DEPLOYMENT_ENVIRONMENTS.map((candidate) => (
                <option key={candidate} value={candidate}>
                  {candidate}
                </option>
              ))}
            </select>
          </label>
        </div>
      </div>

      <p className="form-note">GET /agents/{shortenId(agentId)}/canary/status?environment={environment} · polled every 5s</p>

      {statusQuery.isPending ? (
        <div className="stack-gap">
          <Skeleton height={16} />
          <Skeleton height={16} />
          <Skeleton height={16} />
        </div>
      ) : statusQuery.isError ? (
        noServingDeployment ? (
          <EmptyState
            title={`No healthy deployment serves ${environment}`}
            hint="A canary rides on the environment's healthy deployment row — request a deployment of a published version above, then start the canary here."
          />
        ) : (
          <ErrorBanner error={statusQuery.error} onRetry={() => void statusQuery.refetch()} />
        )
      ) : status ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Deployment" value={shortenId(status.deploymentId)} accent="info" />
            <SummaryStat label="Stable" value={`v${status.stableVersion}`} accent="success" />
            <SummaryStat label="Canary" value={status.canaryActive ? `v${status.canaryVersion}` : '—'} accent={status.canaryActive ? 'warning' : 'default'} />
            <SummaryStat label="Split" value={status.canaryActive ? `${status.canaryWeight}% canary` : 'no canary'} accent={status.canaryActive ? 'info' : 'default'} />
          </section>

          {status.canaryActive ? (
            <div className="quality-list" aria-label="Live traffic split">
              <div>
                <label>Traffic split</label>
                <div className="meter">
                  <span style={{ width: `${status.canaryWeight}%` }} />
                </div>
                <strong>
                  {status.canaryWeight}% canary v{status.canaryVersion} · {100 - status.canaryWeight}% stable v{status.stableVersion}
                </strong>
              </div>
            </div>
          ) : null}

          <CanaryDecisionPanel status={status} />

          {status.stats ? (
            <section className="summary-grid">
              <SummaryStat label="Eval runs in window" value={String(status.stats.runsCounted)} />
              <SummaryStat
                label="Case pass rate"
                value={status.stats.casesCounted > 0 ? `${((status.stats.passedCases / status.stats.casesCounted) * 100).toFixed(1)}%` : '—'}
                accent="success"
              />
              <SummaryStat label="p95 latency" value={`${status.stats.p95LatencyMs} ms`} accent="info" />
              <SummaryStat label="Avg cost / run" value={`${status.stats.avgCostCents.toFixed(2)}¢`} accent="warning" />
            </section>
          ) : null}

          {status.policy ? (
            <p className="form-note">
              Policy — min pass rate {(status.policy.minPassRate * 100).toFixed(0)}% · min {status.policy.minCanaryRuns} eval run(s)
              {status.policy.maxP95LatencyMs > 0 ? ` · p95 ≤ ${status.policy.maxP95LatencyMs}ms` : ''}
              {status.policy.maxCostPerRunCents > 0 ? ` · avg cost ≤ ${status.policy.maxCostPerRunCents}¢/run` : ''}
            </p>
          ) : null}

          <CanaryControls status={status} versions={versions} canDeploy={canDeploy} />
        </>
      ) : null}
    </article>
  )
}

export function VersionsView({
  canManageVersions,
  canWrite,
  canDeploy,
}: {
  canManageVersions: boolean
  canWrite: boolean
  canDeploy: boolean
}) {
  const agentsQuery = useAgents()
  const agents = useMemo(() => agentsQuery.data ?? [], [agentsQuery.data])
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null)
  const agentId = selectedAgentId ?? agents[0]?.id ?? null

  const versionsQuery = useAgentVersions(agentId)
  const deploymentsQuery = useDeployments(agentId)
  const versions = useMemo(() => versionsQuery.data ?? [], [versionsQuery.data])
  const deployments = useMemo(
    () =>
      [...(deploymentsQuery.data ?? [])].sort((a, b) => a.environment.localeCompare(b.environment)),
    [deploymentsQuery.data],
  )
  const publishedVersions = useMemo(
    () => versions.filter((version) => version.status === 'published').sort((a, b) => b.version - a.version),
    [versions],
  )
  const selectedAgent = agents.find((agent) => agent.id === agentId)

  return (
    <>
      <PageHeader
        eyebrow="Build"
        title="Versions & deployments"
        actions={
          agents.length > 0 ? (
            <label className="inline-label" htmlFor="versions-agent">
              Agent
              <select id="versions-agent" value={agentId ?? ''} onChange={(event) => setSelectedAgentId(event.target.value)}>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name}
                  </option>
                ))}
              </select>
            </label>
          ) : undefined
        }
      />

      {agentsQuery.isError ? <ErrorBanner error={agentsQuery.error} onRetry={() => void agentsQuery.refetch()} /> : null}
      {versionsQuery.isError ? <ErrorBanner error={versionsQuery.error} onRetry={() => void versionsQuery.refetch()} /> : null}

      {agents.length === 0 ? (
        <EmptyState
          title="No agents yet"
          hint="Config versions are snapshots of an agent's configuration. Create an agent first."
        />
      ) : versionsQuery.isPending || !agentId ? (
        <article className="panel">
          <Skeleton height={18} width="40%" />
          <Skeleton height={140} style={{ marginTop: 16 }} />
        </article>
      ) : (
        <>
          <section className="summary-grid">
            <SummaryStat label="Agent" value={selectedAgent?.name ?? shortenId(agentId)} accent="info" />
            <SummaryStat label="Versions" value={String(versions.length)} />
            <SummaryStat label="Published" value={String(publishedVersions.length)} accent="success" />
            <SummaryStat label="Deployments" value={String(deployments.length)} accent="warning" />
          </section>

          <article className="panel wide">
            <div className="panel-header">
              <div>
                <p className="eyebrow">History</p>
                <h3>Config versions</h3>
              </div>
              <SnapshotButton agentId={agentId} canManage={canManageVersions} />
            </div>

            {versions.length === 0 ? (
              <EmptyState
                title="No config versions yet"
                hint="A snapshot freezes the agent's name, description, instructions and model as an immutable version."
              />
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Version</th>
                      <th>Status</th>
                      <th>Model</th>
                      <th>Published</th>
                      <th>By</th>
                      <th>Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {[...versions]
                      .sort((a, b) => b.version - a.version)
                      .map((version) => (
                        <tr key={version.version}>
                          <td>
                            <strong>v{version.version}</strong>
                          </td>
                          <td>
                            <StatusPill status={version.status} />
                          </td>
                          <td>{typeof version.snapshot?.model === 'string' ? version.snapshot.model : '—'}</td>
                          <td>{version.publishedAt ? formatDateTime(version.publishedAt) : '—'}</td>
                          <td>{shortenId(version.publishedBy)}</td>
                          <td>
                            <VersionRowActions agentId={agentId} version={version} canManage={canManageVersions} />
                          </td>
                        </tr>
                      ))}
                  </tbody>
                </table>
              </div>
            )}
          </article>

          <DiffViewer agentId={agentId} versions={versions} />

          <article className="panel wide">
            <div className="panel-header">
              <div>
                <p className="eyebrow">Rollout</p>
                <h3>Deployments by environment</h3>
              </div>
              <span className="form-note">GET /deployments?agent_id={shortenId(agentId)}</span>
            </div>

            {deploymentsQuery.isError ? (
              <ErrorBanner error={deploymentsQuery.error} onRetry={() => void deploymentsQuery.refetch()} />
            ) : null}

            {canWrite ? (
              <CreateDeploymentForm agentId={agentId} publishedVersions={publishedVersions} />
            ) : (
              <p className="detail-copy muted">Viewer role — requesting deployments needs MEMBER and above.</p>
            )}

            {deployments.length === 0 ? (
              <EmptyState
                title="No deployments recorded for this agent"
                hint="Request a deployment of a published version above — environments: development, staging, production."
              />
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Environment</th>
                      <th>Version</th>
                      <th>Status</th>
                      <th>Health</th>
                      <th>Updated</th>
                      <th>Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {deployments.map((deployment) => (
                      <tr key={deployment.id}>
                        <td>
                          <strong>{deployment.environment}</strong>
                        </td>
                        <td>v{deployment.version}</td>
                        <td>
                          <StatusPill status={deployment.status} />
                        </td>
                        <td>
                          {deployment.health?.error
                            ? <span className="table-error-cell">{deployment.health.error}</span>
                            : deployment.health?.errorRate !== undefined
                              ? `error rate ${(deployment.health.errorRate * 100).toFixed(1)}%`
                              : '—'}
                        </td>
                        <td>{formatRelativeTime(deployment.updatedAt)}</td>
                        <td>
                          <DeploymentActions deploymentId={deployment.id} status={deployment.status} canDeploy={canDeploy} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </article>

          {/* issue #81: canary split/promote/abort + eval-gated decision + live stats */}
          <CanaryPanel agentId={agentId} versions={versions} canDeploy={canDeploy} />
        </>
      )}
    </>
  )
}
