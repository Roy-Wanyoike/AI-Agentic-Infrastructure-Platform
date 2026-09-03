// Workflows resource: list / create / get / publish / execute + workflow runs.
//
// Endpoint contract (mirrors cmd/api/workflows.go handlers):
// - GET  /workflows                  → {"workflows": [{id,name,description,status,current_version,created_at,updated_at}]}
// - POST /workflows/create           → {"workflow": {...}}; 422 {"errors":[{code,message,node_id}]}
// - GET  /workflows/{id}             → {"workflow": {..., "versions":[{version,status,created_at,dsl_snapshot}]}}
// - POST /workflows/{id}/validate    → {"valid": true} | 422 {"errors":[...]}
// - POST /workflows/{id}/publish     → {"workflow", "version"}
// - POST /workflows/{id}/execute     → {"workflow_run_id","run_ids":[...],"status":"pending"}
// - GET  /workflow-runs/{id}         → {"id","workflow_id","status","node_runs":[{node_id,run_id,status,started_at,finished_at,error}]}

import { ApiError, apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type WorkflowDslNode = {
  id: string
  type: string
  name?: string
  config?: Record<string, unknown>
}

export type WorkflowDslEdge = {
  from: string
  to: string
  condition?: string
}

export type WorkflowDsl = {
  nodes: WorkflowDslNode[]
  edges: WorkflowDslEdge[]
}

export type Workflow = {
  id: string
  name: string
  description: string
  status: string
  currentVersion?: number
  dsl?: WorkflowDsl | null
  createdAt?: string
  updatedAt?: string
}

export type WorkflowVersion = {
  version: number
  status: string
  createdAt?: string
  dslSnapshot?: WorkflowDsl | null
}

export type WorkflowDetail = Workflow & {
  versions: WorkflowVersion[]
}

export type WorkflowValidationIssue = {
  code: string
  message: string
  nodeId?: string
}

export type WorkflowExecution = {
  workflowRunId: string
  runIds: string[]
  status: string
}

export type WorkflowNodeRun = {
  nodeId: string
  runId?: string
  status: string
  startedAt?: string
  finishedAt?: string
  error?: string
}

export type WorkflowRun = {
  id: string
  workflowId: string
  status: string
  nodeRuns: WorkflowNodeRun[]
}

export type CreateWorkflowInput = {
  name: string
  description: string
  dsl: WorkflowDsl
}

const WORKFLOW_TERMINAL_STATUSES = new Set(['completed', 'failed', 'cancelled', 'timeout'])

export function isTerminalWorkflowRunStatus(status?: string | null): boolean {
  return WORKFLOW_TERMINAL_STATUSES.has((status ?? '').toLowerCase())
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeDsl(raw: unknown): WorkflowDsl | null {
  if (typeof raw === 'string' && raw.trim()) {
    try {
      return normalizeDsl(JSON.parse(raw))
    } catch {
      return null
    }
  }
  if (typeof raw !== 'object' || raw === null) return null
  const record = raw as Record<string, unknown>
  const nodes = Array.isArray(record.nodes) ? record.nodes : []
  const edges = Array.isArray(record.edges) ? record.edges : []
  return {
    nodes: nodes
      .filter((node): node is Record<string, unknown> => typeof node === 'object' && node !== null)
      .map((node) => ({
        id: asString(pickField(node, 'id')) ?? '',
        type: (asString(pickField(node, 'type')) ?? 'unknown').toLowerCase(),
        name: asString(pickField(node, 'name')),
        config: typeof node.config === 'object' && node.config !== null ? (node.config as Record<string, unknown>) : undefined,
      })),
    edges: edges
      .filter((edge): edge is Record<string, unknown> => typeof edge === 'object' && edge !== null)
      .map((edge) => ({
        from: asString(pickField(edge, 'from', 'source')) ?? '',
        to: asString(pickField(edge, 'to', 'target')) ?? '',
        condition: asString(pickField(edge, 'condition')),
      })),
  }
}

function normalizeWorkflow(raw: unknown): Workflow {
  return {
    id: asString(pickField(raw, 'id', 'workflowId')) ?? '',
    name: asString(pickField(raw, 'name')) ?? 'Untitled workflow',
    description: asString(pickField(raw, 'description')) ?? '',
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    currentVersion: asNumber(pickField(raw, 'currentVersion', 'current_version', 'version')),
    dsl: normalizeDsl(pickField(raw, 'dsl', 'definition')),
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

function normalizeWorkflowVersion(raw: unknown): WorkflowVersion {
  return {
    version: asNumber(pickField(raw, 'version')) ?? 0,
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    dslSnapshot: normalizeDsl(pickField(raw, 'dslSnapshot', 'dsl_snapshot', 'dsl')),
  }
}

function normalizeWorkflowRun(raw: unknown): WorkflowRun {
  const nodeRunsRaw = pickField(raw, 'nodeRuns', 'node_runs', 'nodes')
  const nodeRuns = Array.isArray(nodeRunsRaw) ? nodeRunsRaw : []
  return {
    id: asString(pickField(raw, 'id', 'workflowRunId', 'workflow_run_id')) ?? '',
    workflowId: asString(pickField(raw, 'workflowId', 'workflow_id')) ?? '',
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    nodeRuns: nodeRuns.map((node) => {
      const record = typeof node === 'object' && node !== null ? (node as Record<string, unknown>) : {}
      return {
        nodeId: asString(pickField(record, 'nodeId', 'node_id')) ?? '',
        runId: asString(pickField(record, 'runId', 'run_id')),
        status: (asString(pickField(record, 'status')) ?? 'unknown').toLowerCase(),
        startedAt: asString(pickField(record, 'startedAt', 'started_at')),
        finishedAt: asString(pickField(record, 'finishedAt', 'finished_at')),
        error: asString(pickField(record, 'error', 'errorMessage')),
      }
    }),
  }
}

// ---------------------------------------------------------------------------
// 422 validation-error extraction
// ---------------------------------------------------------------------------

/** Pulls {"errors":[{code,message,node_id}]} out of a 422 ApiError body. */
export function extractValidationIssues(error: unknown): WorkflowValidationIssue[] {
  if (!(error instanceof ApiError)) return []
  const errors = pickField(error.body, 'errors', 'validation_errors')
  if (!Array.isArray(errors)) {
    const message = asString(pickField(error.body, 'message'))
    return message ? [{ code: 'invalid_dsl', message }] : []
  }
  return errors
    .filter((issue): issue is Record<string, unknown> => typeof issue === 'object' && issue !== null)
    .map((issue) => ({
      code: asString(pickField(issue, 'code')) ?? 'invalid',
      message: asString(pickField(issue, 'message', 'error')) ?? 'Invalid workflow definition',
      nodeId: asString(pickField(issue, 'nodeId', 'node_id', 'node')),
    }))
}

/** Client-side sanity check mirroring the backend rules (fast feedback only — backend stays the source of truth). */
export function clientValidateDsl(dsl: WorkflowDsl): WorkflowValidationIssue[] {
  const issues: WorkflowValidationIssue[] = []
  const ids = new Set<string>()
  dsl.nodes.forEach((node, index) => {
    const nodeId = node.id?.trim()
    if (!nodeId) {
      issues.push({ code: 'missing_node_id', message: `Node at position ${index + 1} is missing an "id".`, nodeId: node.id || `#${index + 1}` })
    } else if (ids.has(nodeId)) {
      issues.push({ code: 'duplicate_node_id', message: `Duplicate node id "${nodeId}".`, nodeId })
    } else {
      ids.add(nodeId)
    }
    if (!node.type?.trim()) {
      issues.push({ code: 'missing_node_type', message: `Node "${nodeId || `#${index + 1}`}" is missing a "type".`, nodeId: nodeId || undefined })
    }
  })
  for (const edge of dsl.edges) {
    if (!ids.has(edge.from)) {
      issues.push({ code: 'unknown_edge_source', message: `Edge references unknown source node "${edge.from}".`, nodeId: edge.from })
    }
    if (!ids.has(edge.to)) {
      issues.push({ code: 'unknown_edge_target', message: `Edge references unknown target node "${edge.to}".`, nodeId: edge.to })
    }
  }
  return issues
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

function extractWorkflowList(raw: unknown): Workflow[] {
  const wrapped = pickField(raw, 'workflows', 'items', 'data')
  const list = Array.isArray(raw) ? raw : Array.isArray(wrapped) ? wrapped : []
  return list.map(normalizeWorkflow)
}

export async function listWorkflows(): Promise<Workflow[]> {
  return extractWorkflowList(await apiFetch<unknown>('/workflows'))
}

export async function getWorkflow(id: string): Promise<WorkflowDetail> {
  const raw = await apiFetch<unknown>(`/workflows/${encodeURIComponent(id)}`)
  const workflowRaw = pickField(raw, 'workflow') ?? raw
  const versionsRaw = pickField(workflowRaw, 'versions')
  return {
    ...normalizeWorkflow(workflowRaw),
    versions: Array.isArray(versionsRaw) ? versionsRaw.map(normalizeWorkflowVersion) : [],
  }
}

export async function createWorkflow(input: CreateWorkflowInput): Promise<Workflow> {
  const raw = await apiFetch<unknown>('/workflows/create', {
    method: 'POST',
    body: JSON.stringify({ name: input.name, description: input.description, dsl: input.dsl }),
  })
  return normalizeWorkflow(pickField(raw, 'workflow') ?? raw)
}

export async function publishWorkflow(id: string): Promise<{ workflow: Workflow; version?: number }> {
  const raw = await apiFetch<unknown>(`/workflows/${encodeURIComponent(id)}/publish`, { method: 'POST' })
  const workflowRaw = pickField(raw, 'workflow') ?? raw
  return {
    workflow: normalizeWorkflow(workflowRaw),
    version: asNumber(pickField(raw, 'version')),
  }
}

export async function executeWorkflow(id: string, input: string): Promise<WorkflowExecution> {
  const raw = await apiFetch<unknown>(`/workflows/${encodeURIComponent(id)}/execute`, {
    method: 'POST',
    body: JSON.stringify({ input }),
  })
  const runIdsRaw = pickField(raw, 'runIds', 'run_ids')
  return {
    workflowRunId: asString(pickField(raw, 'workflowRunId', 'workflow_run_id')) ?? '',
    runIds: Array.isArray(runIdsRaw) ? runIdsRaw.map((value) => asString(value) ?? '').filter(Boolean) : [],
    status: (asString(pickField(raw, 'status')) ?? 'pending').toLowerCase(),
  }
}

export async function getWorkflowRun(id: string): Promise<WorkflowRun> {
  return normalizeWorkflowRun(await apiFetch<unknown>(`/workflow-runs/${encodeURIComponent(id)}`))
}
