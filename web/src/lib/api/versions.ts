// Agent config versions + deployments + version diff (track 2-b + 3-e).
//
// Contract endpoints (served under /api/v1):
// - GET    /agents/{id}/versions                    -> {"versions":[{version,snapshot,published_at,published_by,status}]}
// - POST   /agents/{id}/versions/create             -> {"version":3}
// - POST   /agents/{id}/versions/{version}/publish  -> {"version":3}
// - POST   /agents/{id}/rollback {"target_version"}  -> {"current_version":2}
// - GET    /agents/{id}/versions/diff?from=&to=     -> {"agent_id","from","to","fields":[{field,from,to,changed}]}
// - GET    /deployments?agent_id=                   -> {"deployments":[...]}
// - POST   /deployments/create                      -> {"deployment":{...}}
// - POST   /deployments/{id}/promote                -> {"deployment":{...}}
// - POST   /deployments/{id}/rollback               -> {"deployment":{...},"rolled_back_to_version":2}

import { apiFetch } from './client'
import { asNumber, asRecord, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type AgentConfigVersion = {
  version: number
  status: string
  snapshot: Record<string, unknown> | null
  publishedAt?: string | null
  publishedBy?: string
  createdAt?: string
}

export type VersionDiffField = {
  field: string
  from: unknown
  to: unknown
  changed: boolean
}

export type VersionDiff = {
  agentId: string
  from: number
  to: number
  fields: VersionDiffField[]
}

export type DeploymentHealth = {
  errorRate?: number
  lastCheckAt?: string
  supersededBy?: string
  error?: string
}

export type Deployment = {
  id: string
  agentId: string
  version: number
  environment: string
  status: string
  health?: DeploymentHealth | null
  createdAt?: string
  updatedAt?: string
}

export const DEPLOYMENT_ENVIRONMENTS = ['development', 'staging', 'production'] as const

const TERMINAL_DEPLOYMENT_STATUSES = new Set(['healthy', 'failed'])

/** A terminal deployment cannot be promoted further (the API 409s anyway). */
export function isTerminalDeploymentStatus(status?: string | null): boolean {
  return TERMINAL_DEPLOYMENT_STATUSES.has((status ?? '').toLowerCase())
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeVersion(raw: unknown): AgentConfigVersion {
  return {
    version: asNumber(pickField(raw, 'version')) ?? 0,
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    snapshot: asRecord(pickField(raw, 'snapshot')),
    publishedAt: asString(pickField(raw, 'publishedAt', 'published_at')) ?? null,
    publishedBy: asString(pickField(raw, 'publishedBy', 'published_by')),
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
  }
}

function normalizeDiffField(raw: unknown): VersionDiffField {
  const record = asRecord(raw)
  return {
    field: asString(pickField(record, 'field')) ?? '',
    from: pickField(record, 'from'),
    to: pickField(record, 'to'),
    changed: pickField(record, 'changed') === true,
  }
}

export function normalizeVersionDiff(raw: unknown): VersionDiff {
  const fieldsRaw = pickField(raw, 'fields')
  return {
    agentId: asString(pickField(raw, 'agentId', 'agent_id')) ?? '',
    from: asNumber(pickField(raw, 'from')) ?? 0,
    to: asNumber(pickField(raw, 'to')) ?? 0,
    fields: Array.isArray(fieldsRaw) ? fieldsRaw.map(normalizeDiffField) : [],
  }
}

function normalizeHealth(raw: unknown): DeploymentHealth | null {
  const record = asRecord(raw)
  if (!record) return null
  return {
    errorRate: asNumber(pickField(record, 'errorRate', 'error_rate')),
    lastCheckAt: asString(pickField(record, 'lastCheckAt', 'last_check_at')),
    supersededBy: asString(pickField(record, 'supersededBy', 'superseded_by')),
    error: asString(pickField(record, 'error')),
  }
}

function normalizeDeployment(raw: unknown): Deployment {
  return {
    id: asString(pickField(raw, 'id', 'deploymentId')) ?? '',
    agentId: asString(pickField(raw, 'agentId', 'agent_id')) ?? '',
    version: asNumber(pickField(raw, 'version')) ?? 0,
    environment: (asString(pickField(raw, 'environment')) ?? '').toLowerCase(),
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    health: normalizeHealth(pickField(raw, 'health')),
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

export async function listAgentVersions(agentId: string): Promise<AgentConfigVersion[]> {
  const raw = await apiFetch<unknown>(`/agents/${encodeURIComponent(agentId)}/versions`)
  const list = pickField(raw, 'versions')
  return (Array.isArray(list) ? list : []).map(normalizeVersion)
}

export async function createAgentVersion(agentId: string): Promise<number | undefined> {
  const raw = await apiFetch<unknown>(`/agents/${encodeURIComponent(agentId)}/versions/create`, { method: 'POST' })
  return asNumber(pickField(raw, 'version'))
}

export async function publishAgentVersion(agentId: string, version: number): Promise<number | undefined> {
  const raw = await apiFetch<unknown>(`/agents/${encodeURIComponent(agentId)}/versions/${version}/publish`, {
    method: 'POST',
  })
  return asNumber(pickField(raw, 'version'))
}

export async function rollbackAgent(agentId: string, targetVersion: number): Promise<number | undefined> {
  const raw = await apiFetch<unknown>(`/agents/${encodeURIComponent(agentId)}/rollback`, {
    method: 'POST',
    body: JSON.stringify({ target_version: targetVersion }),
  })
  return asNumber(pickField(raw, 'currentVersion', 'current_version'))
}

export async function diffAgentVersions(agentId: string, from: number, to: number): Promise<VersionDiff> {
  const raw = await apiFetch<unknown>(
    `/agents/${encodeURIComponent(agentId)}/versions/diff?from=${encodeURIComponent(String(from))}&to=${encodeURIComponent(String(to))}`,
  )
  return normalizeVersionDiff(raw)
}

function extractDeployments(raw: unknown): Deployment[] {
  const list = pickField(raw, 'deployments')
  return (Array.isArray(list) ? list : []).map(normalizeDeployment)
}

export async function listDeployments(agentId?: string | null): Promise<Deployment[]> {
  const query = agentId ? `?agent_id=${encodeURIComponent(agentId)}` : ''
  return extractDeployments(await apiFetch<unknown>(`/deployments${query}`))
}

export async function createDeployment(input: {
  agentId: string
  version: number
  environment: string
}): Promise<Deployment> {
  const raw = await apiFetch<unknown>('/deployments/create', {
    method: 'POST',
    body: JSON.stringify({ agent_id: input.agentId, version: input.version, environment: input.environment }),
  })
  return normalizeDeployment(pickField(raw, 'deployment') ?? raw)
}

export async function promoteDeployment(id: string): Promise<Deployment> {
  const raw = await apiFetch<unknown>(`/deployments/${encodeURIComponent(id)}/promote`, { method: 'POST' })
  return normalizeDeployment(pickField(raw, 'deployment') ?? raw)
}

export async function rollbackDeployment(id: string): Promise<{ deployment: Deployment; rolledBackToVersion?: number }> {
  const raw = await apiFetch<unknown>(`/deployments/${encodeURIComponent(id)}/rollback`, { method: 'POST' })
  return {
    deployment: normalizeDeployment(pickField(raw, 'deployment') ?? raw),
    rolledBackToVersion: asNumber(pickField(raw, 'rolledBackToVersion', 'rolled_back_to_version')),
  }
}
