// Workflows views (Priority 2) — wired to the track 2-a contract:
// list, create (JSON DSL + backend 422 node-by-node errors), detail with
// versions, publish, execute, and the workflow-run status view (node_runs).

import { useMemo, useState, type FormEvent } from 'react'
import { useCreateWorkflow, useExecuteWorkflow, usePublishWorkflow, useWorkflow, useWorkflowRun, useWorkflows } from '../lib/hooks'
import {
  clientValidateDsl,
  extractValidationIssues,
  type Workflow,
  type WorkflowDsl,
  type WorkflowValidationIssue,
  type WorkflowRun,
} from '../lib/api/workflows'
import { formatDateTime, formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

const DSL_PLACEHOLDER = `{
  "nodes": [
    { "id": "n1", "type": "agent", "name": "Planner", "config": { "agent_id": "<agent-uuid>", "input": "{{input}}" } },
    { "id": "n2", "type": "tool", "name": "Lookup", "config": { "tool_id": "<tool-uuid>", "timeout_seconds": 30 } },
    { "id": "n3", "type": "approval", "name": "Review", "config": { "reason": "High-risk action" } }
  ],
  "edges": [
    { "from": "n1", "to": "n2", "condition": "on_success" },
    { "from": "n2", "to": "n3", "condition": "on_success" }
  ]
}`

const NODE_TYPES = ['agent', 'tool', 'condition', 'parallel', 'approval', 'delay', 'webhook']

function ValidationIssueList({ issues, title }: { issues: WorkflowValidationIssue[]; title: string }) {
  return (
    <div className="form-error validation-list" role="alert">
      <strong>{title}</strong>
      <ul>
        {issues.map((issue, index) => (
          <li key={`${issue.nodeId ?? 'issue'}-${index}`}>
            {issue.nodeId ? <code>{issue.nodeId}</code> : <code>{issue.code}</code>} — {issue.message}
          </li>
        ))}
      </ul>
    </div>
  )
}

function CreateWorkflowForm({
  onCreated,
  onCancel,
}: {
  onCreated: (workflow: Workflow) => void
  onCancel: () => void
}) {
  const createWorkflow = useCreateWorkflow()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [dslText, setDslText] = useState('')
  const [localIssues, setLocalIssues] = useState<WorkflowValidationIssue[]>([])
  const [parseError, setParseError] = useState<string | null>(null)

  const backendIssues = createWorkflow.isError ? extractValidationIssues(createWorkflow.error) : []

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (createWorkflow.isPending) return
    setParseError(null)
    setLocalIssues([])

    let parsed: unknown
    try {
      parsed = JSON.parse(dslText)
    } catch (error) {
      setParseError(error instanceof Error ? `Invalid JSON: ${error.message}` : 'Invalid JSON')
      return
    }

    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      setParseError('The DSL must be a JSON object with "nodes" and "edges" arrays.')
      return
    }

    const record = parsed as Record<string, unknown>
    const nodes = Array.isArray(record.nodes) ? record.nodes : []
    const edges = Array.isArray(record.edges) ? record.edges : []
    const dsl: WorkflowDsl = {
      nodes: nodes.map((node) => {
        const nodeRecord = typeof node === 'object' && node !== null ? (node as Record<string, unknown>) : {}
        return {
          id: typeof nodeRecord.id === 'string' ? nodeRecord.id : '',
          type: typeof nodeRecord.type === 'string' ? nodeRecord.type : '',
          name: typeof nodeRecord.name === 'string' ? nodeRecord.name : undefined,
          config: typeof nodeRecord.config === 'object' && nodeRecord.config !== null ? (nodeRecord.config as Record<string, unknown>) : undefined,
        }
      }),
      edges: edges.map((edge) => {
        const edgeRecord = typeof edge === 'object' && edge !== null ? (edge as Record<string, unknown>) : {}
        return {
          from: typeof edgeRecord.from === 'string' ? edgeRecord.from : '',
          to: typeof edgeRecord.to === 'string' ? edgeRecord.to : '',
          condition: typeof edgeRecord.condition === 'string' ? edgeRecord.condition : undefined,
        }
      }),
    }

    const clientIssues = clientValidateDsl(dsl)
    if (clientIssues.length > 0) {
      setLocalIssues(clientIssues)
      return
    }

    createWorkflow.mutate(
      { name: name.trim(), description: description.trim(), dsl },
      { onSuccess: onCreated },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">New</p>
          <h3>Create workflow</h3>
        </div>
        <button type="button" className="link-button" onClick={onCancel}>
          Cancel
        </button>
      </div>

      {parseError ? <div className="form-error">{parseError}</div> : null}
      {localIssues.length > 0 ? <ValidationIssueList issues={localIssues} title="Fix the DSL before submitting:" /> : null}
      {backendIssues.length > 0 ? <ValidationIssueList issues={backendIssues} title="The backend rejected this definition:" /> : null}
      {createWorkflow.isError && backendIssues.length === 0 ? <div className="form-error">{describeError(createWorkflow.error)}</div> : null}

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="workflow-name">Name</label>
            <input id="workflow-name" value={name} onChange={(event) => setName(event.target.value)} placeholder="Support triage pipeline" required />
          </div>
          <div className="field">
            <label htmlFor="workflow-description">Description</label>
            <input
              id="workflow-description"
              value={description}
              onChange={(event) => setDescription(event.target.value)}
              placeholder="What does this workflow automate?"
            />
          </div>
          <div className="field span-2">
            <label htmlFor="workflow-dsl">Workflow DSL (JSON)</label>
            <textarea
              id="workflow-dsl"
              className="code-input"
              rows={12}
              value={dslText}
              onChange={(event) => setDslText(event.target.value)}
              placeholder={DSL_PLACEHOLDER}
              spellCheck={false}
              required
            />
            <span className="form-note">
              Node types: {NODE_TYPES.join(' · ')}. Edges accept condition: on_success | on_failure | always. The backend validates references, types and cycles.
            </span>
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">POST /workflows/create</span>
          <button type="submit" className="primary-button" disabled={createWorkflow.isPending}>
            {createWorkflow.isPending ? 'Creating…' : 'Create workflow'}
          </button>
        </div>
      </form>
    </article>
  )
}

function WorkflowDetailView({
  workflowId,
  onBack,
  onOpenWorkflowRun,
  canWrite,
}: {
  workflowId: string
  onBack: () => void
  onOpenWorkflowRun: (workflowRunId: string) => void
  canWrite: boolean
}) {
  const workflowQuery = useWorkflow(workflowId)
  const publishWorkflow = usePublishWorkflow()
  const executeWorkflow = useExecuteWorkflow()
  const [runInput, setRunInput] = useState('')
  const [publishError, setPublishError] = useState<string | null>(null)
  const workflow = workflowQuery.data

  const publish = () => {
    if (publishWorkflow.isPending) return
    setPublishError(null)
    publishWorkflow.mutate(workflowId, {
      onError: (error) => setPublishError(describeError(error)),
    })
  }

  const execute = (event: FormEvent) => {
    event.preventDefault()
    const input = runInput.trim()
    if (!input || executeWorkflow.isPending) return
    executeWorkflow.mutate(
      { id: workflowId, input },
      {
        onSuccess: (execution) => {
          setRunInput('')
          if (execution.workflowRunId) onOpenWorkflowRun(execution.workflowRunId)
        },
      },
    )
  }

  const nodes = workflow?.dsl?.nodes ?? []
  const edges = workflow?.dsl?.edges ?? []

  return (
    <>
      <PageHeader
        eyebrow="Workflow"
        title={workflow?.name ?? 'Workflow'}
        badge={workflow ? <StatusPill status={workflow.status} /> : undefined}
        actions={
          <button type="button" className="ghost-button" onClick={onBack}>
            ← Back to workflows
          </button>
        }
      />

      {workflowQuery.isError ? <ErrorBanner error={workflowQuery.error} onRetry={() => void workflowQuery.refetch()} /> : null}

      {workflowQuery.isPending ? (
        <article className="panel">
          <Skeleton height={18} width="40%" />
          <Skeleton height={140} style={{ marginTop: 16 }} />
        </article>
      ) : workflow ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Status" value={workflow.status} />
            <SummaryStat label="Current version" value={workflow.currentVersion !== undefined ? `v${workflow.currentVersion}` : '—'} accent="info" />
            <SummaryStat label="Nodes" value={String(nodes.length)} accent="success" />
            <SummaryStat label="Edges" value={String(edges.length)} accent="warning" />
          </section>

          <section className="content-grid">
            <article className="panel wide">
              <div className="panel-header">
                <div>
                  <p className="eyebrow">Definition</p>
                  <h3>Graph</h3>
                </div>
                <div className="topbar-actions">
                  {canWrite ? (
                    <button type="button" className="ghost-button" onClick={publish} disabled={publishWorkflow.isPending}>
                      {publishWorkflow.isPending ? 'Publishing…' : 'Publish version'}
                    </button>
                  ) : null}
                </div>
              </div>

              {publishError ? <div className="form-error">{publishError}</div> : null}
              {publishWorkflow.isSuccess ? <div className="form-note">Published as v{publishWorkflow.data.version ?? workflow.currentVersion}.</div> : null}

              {workflow.description ? <p className="detail-copy">{workflow.description}</p> : null}

              {nodes.length === 0 ? (
                <EmptyState title="No DSL stored on this workflow" hint="The definition may not have been attached when the workflow was created." />
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>Node</th>
                        <th>Type</th>
                        <th>Config</th>
                      </tr>
                    </thead>
                    <tbody>
                      {nodes.map((node) => (
                        <tr key={node.id}>
                          <td>
                            <strong>{node.name || node.id}</strong>
                            {node.name ? <span className="form-note"> {node.id}</span> : null}
                          </td>
                          <td>
                            <span className={`step-type step-type-${node.type}`}>{node.type}</span>
                          </td>
                          <td>
                            <code className="config-cell">{node.config ? JSON.stringify(node.config) : '—'}</code>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}

              {edges.length > 0 ? (
                <div className="edge-list">
                  {edges.map((edge, index) => (
                    <span key={`${edge.from}-${edge.to}-${index}`} className="edge-chip">
                      {edge.from} → {edge.to}
                      {edge.condition ? <em> ({edge.condition})</em> : null}
                    </span>
                  ))}
                </div>
              ) : null}
            </article>

            <article className="panel">
              <div className="panel-header">
                <div>
                  <p className="eyebrow">Execute</p>
                  <h3>Run workflow</h3>
                </div>
              </div>

              {!canWrite ? (
                <p className="detail-copy muted">Viewer role — executing workflows needs MEMBER and above.</p>
              ) : null}
              {canWrite && executeWorkflow.isError ? <div className="form-error">{describeError(executeWorkflow.error)}</div> : null}

              {canWrite ? (
              <form onSubmit={execute}>
                <div className="field">
                  <label htmlFor="workflow-run-input">Input</label>
                  <textarea
                    id="workflow-run-input"
                    rows={5}
                    value={runInput}
                    onChange={(event) => setRunInput(event.target.value)}
                    placeholder='Input passed to every "{{input}}" reference in the DSL'
                    required
                  />
                </div>
                <div className="form-actions">
                  <span className="form-note">POST /workflows/{shortenId(workflow.id)}/execute</span>
                  <button type="submit" className="primary-button" disabled={executeWorkflow.isPending}>
                    {executeWorkflow.isPending ? 'Executing…' : 'Execute'}
                  </button>
                </div>
              </form>
              ) : null}
            </article>
          </section>

          <article className="panel">
            <div className="panel-header">
              <div>
                <p className="eyebrow">History</p>
                <h3>Versions</h3>
              </div>
              <span className="form-note">GET /workflows/{shortenId(workflow.id)}</span>
            </div>

            {workflow.versions.length === 0 ? (
              <EmptyState
                title="No versions published yet"
                hint="The workflow definition is editable while in draft. Publishing snapshots it as an immutable version."
                action={
                  canWrite ? (
                    <button type="button" className="primary-button" onClick={publish} disabled={publishWorkflow.isPending}>
                      Publish first version
                    </button>
                  ) : undefined
                }
              />
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Version</th>
                      <th>Status</th>
                      <th>Published</th>
                      <th>Nodes</th>
                    </tr>
                  </thead>
                  <tbody>
                    {workflow.versions.map((version) => (
                      <tr key={version.version}>
                        <td>v{version.version}</td>
                        <td>
                          <StatusPill status={version.status} />
                        </td>
                        <td>{formatDateTime(version.createdAt)}</td>
                        <td>{version.dslSnapshot ? version.dslSnapshot.nodes.length : '—'}</td>
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
          title="Workflow not found"
          hint="It may have been removed or belongs to a different organization."
          action={
            <button type="button" className="ghost-button" onClick={onBack}>
              Back to workflows
            </button>
          }
        />
      )}
    </>
  )
}

function WorkflowRunDetailView({
  workflowRunId,
  onBack,
  onOpenRun,
}: {
  workflowRunId: string
  onBack: () => void
  onOpenRun: (runId: string) => void
}) {
  const workflowRunQuery = useWorkflowRun(workflowRunId)
  const run: WorkflowRun | undefined = workflowRunQuery.data
  const finished = run?.nodeRuns.filter((node) => node.status === 'completed' || node.status === 'succeeded').length ?? 0
  const failed = run?.nodeRuns.filter((node) => node.status === 'failed').length ?? 0
  const live = run ? ['pending', 'running', 'queued', 'waiting_approval', 'paused'].includes(run.status) : false

  return (
    <>
      <PageHeader
        eyebrow="Workflow run"
        title={run ? `${shortenId(run.id)}` : 'Workflow run'}
        badge={run ? <StatusPill status={run.status} /> : undefined}
        actions={
          <button type="button" className="ghost-button" onClick={onBack}>
            ← Back
          </button>
        }
      />

      {workflowRunQuery.isError ? <ErrorBanner error={workflowRunQuery.error} onRetry={() => void workflowRunQuery.refetch()} /> : null}

      {workflowRunQuery.isPending ? (
        <article className="panel">
          <Skeleton height={18} width="40%" />
          <Skeleton height={120} style={{ marginTop: 16 }} />
        </article>
      ) : run ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Status" value={run.status} accent={live ? 'info' : failed > 0 ? 'warning' : 'success'} />
            <SummaryStat label="Workflow" value={shortenId(run.workflowId)} accent="default" />
            <SummaryStat label="Nodes finished" value={`${finished}/${run.nodeRuns.length}`} accent="success" />
            <SummaryStat label="Failed nodes" value={String(failed)} accent={failed > 0 ? 'warning' : 'default'} />
          </section>

          <article className="panel wide">
            <div className="panel-header">
              <div>
                <p className="eyebrow">Fan-out</p>
                <h3>Node runs</h3>
              </div>
              <span className="form-note">{live ? 'Polling every 4s until terminal' : 'Final'} · GET /workflow-runs/{shortenId(run.id)}</span>
            </div>

            {run.nodeRuns.length === 0 ? (
              <EmptyState
                title="No node runs recorded"
                hint="The execution may still be initializing — the DAG expansion creates one run per agent/tool node."
              />
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Node</th>
                      <th>Run</th>
                      <th>Status</th>
                      <th>Started</th>
                      <th>Finished</th>
                      <th>Error</th>
                    </tr>
                  </thead>
                  <tbody>
                    {run.nodeRuns.map((node, index) => (
                      <tr key={`${node.nodeId}-${index}`}>
                        <td>
                          <strong>{node.nodeId || `#${index + 1}`}</strong>
                        </td>
                        <td>
                          {node.runId ? (
                            <button type="button" className="link-button" onClick={() => onOpenRun(node.runId as string)} title="Open run detail">
                              {shortenId(node.runId)}
                            </button>
                          ) : (
                            '—'
                          )}
                        </td>
                        <td>
                          <StatusPill status={node.status} />
                        </td>
                        <td>{formatRelativeTime(node.startedAt)}</td>
                        <td>{formatRelativeTime(node.finishedAt)}</td>
                        <td className="table-error-cell">{node.error ?? '—'}</td>
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
          title="Workflow run not found"
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

export function WorkflowsView({ onOpenRun, canWrite }: { onOpenRun: (runId: string) => void; canWrite: boolean }) {
  const workflowsQuery = useWorkflows()
  const [selectedWorkflowId, setSelectedWorkflowId] = useState<string | null>(null)
  const [selectedWorkflowRunId, setSelectedWorkflowRunId] = useState<string | null>(null)
  const [showCreate, setShowCreate] = useState(false)
  const workflows = useMemo(() => workflowsQuery.data ?? [], [workflowsQuery.data])
  const drafts = workflows.filter((workflow) => workflow.status === 'draft').length

  if (selectedWorkflowRunId) {
    return (
      <WorkflowRunDetailView
        workflowRunId={selectedWorkflowRunId}
        onBack={() => setSelectedWorkflowRunId(null)}
        onOpenRun={onOpenRun}
      />
    )
  }

  if (selectedWorkflowId) {
    return (
      <WorkflowDetailView
        workflowId={selectedWorkflowId}
        onBack={() => setSelectedWorkflowId(null)}
        onOpenWorkflowRun={(workflowRunId) => setSelectedWorkflowRunId(workflowRunId)}
        canWrite={canWrite}
      />
    )
  }

  return (
    <>
      <PageHeader
        eyebrow="Automation"
        title="Workflows"
        actions={
          canWrite ? (
            <button type="button" className="primary-button" onClick={() => setShowCreate((open) => !open)}>
              New workflow
            </button>
          ) : (
            <span className="form-note">Viewer role — creating workflows needs MEMBER and above</span>
          )
        }
      />

      {showCreate ? (
        <CreateWorkflowForm
          onCreated={(workflow) => {
            setShowCreate(false)
            setSelectedWorkflowId(workflow.id)
          }}
          onCancel={() => setShowCreate(false)}
        />
      ) : null}

      {workflowsQuery.isError ? <ErrorBanner error={workflowsQuery.error} onRetry={() => void workflowsQuery.refetch()} /> : null}

      <section className="summary-grid">
        {workflowsQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Total workflows" value={String(workflows.length)} />
            <SummaryStat label="Drafts" value={String(drafts)} accent="info" />
            <SummaryStat label="Published" value={String(workflows.length - drafts)} accent="success" />
            <SummaryStat label="API" value="/workflows" accent="warning" />
          </>
        )}
      </section>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Catalog</p>
            <h3>All workflows</h3>
          </div>
        </div>

        {workflowsQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : workflows.length === 0 ? (
          <EmptyState
            title="No workflows yet"
            hint="Workflows chain agents and tools into a DAG. Create one with a JSON DSL to get started."
            action={
              canWrite ? (
                <button type="button" className="primary-button" onClick={() => setShowCreate(true)}>
                  Create workflow
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
                  <th>Status</th>
                  <th>Version</th>
                  <th>Created</th>
                  <th>Updated</th>
                </tr>
              </thead>
              <tbody>
                {workflows.map((workflow) => (
                  <tr key={workflow.id} className="row-link" onClick={() => setSelectedWorkflowId(workflow.id)}>
                    <td>
                      <strong>{workflow.name}</strong>
                      {workflow.description ? <div className="form-note">{workflow.description}</div> : null}
                    </td>
                    <td>
                      <StatusPill status={workflow.status} />
                    </td>
                    <td>{workflow.currentVersion !== undefined ? `v${workflow.currentVersion}` : '—'}</td>
                    <td>{formatRelativeTime(workflow.createdAt)}</td>
                    <td>{formatRelativeTime(workflow.updatedAt)}</td>
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
