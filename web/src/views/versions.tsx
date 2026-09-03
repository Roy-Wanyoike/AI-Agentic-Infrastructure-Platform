// Versions & deployments view (track 2-b contract + track 3-e diff viewer).
//
// Wired to live endpoints only (no mocks):
// - GET/POST /agents/{id}/versions… — snapshot list, publish, agent rollback
// - GET /agents/{id}/versions/diff?from=&to= — field-level diff (3-e)
// - GET/POST /deployments… — create, promote, rollback (RBAC-gated buttons)
//
// RBAC props mirror the API permission grants:
// - canManageVersions (agents.write → OWNER/ADMIN): snapshot, publish, restore
// - canWrite (deployments.write → MEMBER+): request a deployment
// - canDeploy (deployments.deploy → OWNER/ADMIN): promote/rollback

import { useMemo, useState, type FormEvent } from 'react'
import {
  useAgentVersions,
  useAgents,
  useCreateAgentVersion,
  useCreateDeployment,
  useDeployments,
  usePromoteDeployment,
  usePublishAgentVersion,
  useRollbackAgent,
  useRollbackDeployment,
  useVersionDiff,
} from '../lib/hooks'
import {
  DEPLOYMENT_ENVIRONMENTS,
  isTerminalDeploymentStatus,
  type AgentConfigVersion,
  type VersionDiffField,
} from '../lib/api/versions'
import { formatDateTime, formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

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
        </>
      )}
    </>
  )
}
