// Shared domain types + normalizers for the AgentOS API layer.
//
// The Go backend currently marshals structs without JSON tags, so field names
// arrive in PascalCase (ID, Name, CreatedAt, OrganizationID, ...), while some
// handlers return snake_case map keys (run_id, queue_length, organization_id).
// Every normalizer below is case-insensitive so the UI can rely on the stable
// camelCase types exported here, no matter which casing the backend sends.

export type Agent = {
  id: string
  name: string
  description: string
  instructions: string
  model: string
  status: string
  organizationId?: string
  currentVersionId?: string
  version?: number
  createdAt?: string
  updatedAt?: string
}

export type AgentVersion = {
  id: string
  agentId: string
  version: number
  instructions: string
  model: string
  createdAt?: string
}

export type Run = {
  id: string
  agentId?: string
  organizationId?: string
  input?: string
  output?: string
  status: string
  createdAt?: string
  updatedAt?: string
}

export type RunEvent = {
  runId?: string
  type?: string
  name?: string
  payload: Record<string, unknown>
  createdAt?: string
}

export type RunStepTokens = {
  promptTokens?: number
  completionTokens?: number
  totalTokens?: number
}

/**
 * One recorded execution step of a run's timeline (backend `runs.Step`).
 * The Go struct currently marshals PascalCase (ID, StepType, TokenUsage, …);
 * the normalizer is case-insensitive so snake_case from future json tags
 * works too. Status values: succeeded | failed | pending.
 */
export type RunStep = {
  id: string
  runId: string
  index: number
  stepType: string
  status: string
  inputMeta: Record<string, unknown>
  outputMeta: Record<string, unknown>
  error?: string
  tokenUsage: RunStepTokens
  cost?: number
  startedAt?: string
  completedAt?: string
  createdAt?: string
}

export type MetricsSnapshot = {
  counts: Record<string, number>
  latency: Record<string, number>
  queueLength: number
}

export type AuthUser = {
  id: string
  email: string
  organization?: string
  organizationId?: string
  organizationName?: string
  role?: string
  createdAt?: string
}

/** Case-insensitive field lookup that survives PascalCase / camelCase / snake_case. */
export function pickField(source: unknown, ...names: string[]): unknown {
  if (typeof source !== 'object' || source === null) return undefined
  const index = new Map<string, unknown>()
  for (const [key, value] of Object.entries(source as Record<string, unknown>)) {
    index.set(key.toLowerCase().replace(/[^a-z0-9]/g, ''), value)
  }
  for (const name of names) {
    const normalized = name.toLowerCase().replace(/[^a-z0-9]/g, '')
    if (index.has(normalized)) {
      const value = index.get(normalized)
      if (value !== undefined) return value
    }
  }
  return undefined
}

export function asString(value: unknown): string | undefined {
  if (typeof value === 'string') return value
  if (typeof value === 'number' && Number.isFinite(value)) return String(value)
  if (typeof value === 'boolean') return String(value)
  return undefined
}

export function asNumber(value: unknown): number | undefined {
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value === 'string' && value.trim() !== '') {
    const parsed = Number(value)
    if (Number.isFinite(parsed)) return parsed
  }
  return undefined
}

export function asRecord(value: unknown): Record<string, unknown> | null {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null
}

export function normalizeAgent(raw: unknown): Agent {
  const currentVersionId = asString(pickField(raw, 'currentVersionId', 'versionId'))
  const version = asNumber(pickField(raw, 'version'))
  return {
    id: asString(pickField(raw, 'id', 'agentId')) ?? '',
    name: asString(pickField(raw, 'name')) ?? 'Unnamed agent',
    description: asString(pickField(raw, 'description')) ?? '',
    instructions: asString(pickField(raw, 'instructions')) ?? '',
    model: asString(pickField(raw, 'model')) ?? '',
    status: asString(pickField(raw, 'status')) ?? 'UNKNOWN',
    organizationId: asString(pickField(raw, 'organizationId', 'orgId')),
    currentVersionId,
    version,
    createdAt: asString(pickField(raw, 'createdAt')),
    updatedAt: asString(pickField(raw, 'updatedAt')),
  }
}

export function normalizeAgentVersion(raw: unknown): AgentVersion {
  return {
    id: asString(pickField(raw, 'id', 'versionId')) ?? '',
    agentId: asString(pickField(raw, 'agentId')) ?? '',
    version: asNumber(pickField(raw, 'version')) ?? 0,
    instructions: asString(pickField(raw, 'instructions')) ?? '',
    model: asString(pickField(raw, 'model')) ?? '',
    createdAt: asString(pickField(raw, 'createdAt')),
  }
}

export function normalizeRun(raw: unknown): Run {
  return {
    id: asString(pickField(raw, 'id', 'runId')) ?? '',
    agentId: asString(pickField(raw, 'agentId')),
    organizationId: asString(pickField(raw, 'organizationId', 'orgId')),
    input: asString(pickField(raw, 'input')),
    output: asString(pickField(raw, 'output', 'result')),
    status: (asString(pickField(raw, 'status', 'state')) ?? 'UNKNOWN').toUpperCase(),
    createdAt: asString(pickField(raw, 'createdAt')),
    updatedAt: asString(pickField(raw, 'updatedAt')),
  }
}

export function normalizeRunEvent(raw: unknown): RunEvent {
  const payloadRecord = asRecord(pickField(raw, 'payload', 'data'))
  const payload: Record<string, unknown> = payloadRecord ? { ...payloadRecord } : {}
  const topLevelStatus = asString(pickField(raw, 'status'))
  if (topLevelStatus && payload.status === undefined) payload.status = topLevelStatus
  const topLevelOutput = asString(pickField(raw, 'output'))
  if (topLevelOutput && payload.output === undefined) payload.output = topLevelOutput
  return {
    runId: asString(pickField(raw, 'runId')),
    type: asString(pickField(raw, 'type')),
    name: asString(pickField(raw, 'name')),
    payload,
    createdAt: asString(pickField(raw, 'createdAt', 'ts', 'timestamp')),
  }
}

/** Status carried by a run event (payload.status), uppercased. */
export function eventStatus(event: RunEvent): string | null {
  const status = asString(pickField(event.payload, 'status'))
  return status ? status.toUpperCase() : null
}

/** Output carried by a run event (payload.output). */
export function eventOutput(event: RunEvent): string | null {
  return asString(pickField(event.payload, 'output', 'result')) ?? null
}

export function normalizeMetrics(raw: unknown): MetricsSnapshot {
  const counts: Record<string, number> = {}
  const countsRecord = asRecord(pickField(raw, 'counts'))
  if (countsRecord) {
    for (const [key, value] of Object.entries(countsRecord)) {
      const parsed = asNumber(value)
      if (parsed !== undefined) counts[key] = parsed
    }
  }
  const latency: Record<string, number> = {}
  const latencyRecord = asRecord(pickField(raw, 'latency', 'latencies'))
  if (latencyRecord) {
    for (const [key, value] of Object.entries(latencyRecord)) {
      const parsed = asNumber(value)
      if (parsed !== undefined) latency[key] = parsed
    }
  }
  return {
    counts,
    latency,
    queueLength: asNumber(pickField(raw, 'queueLength', 'queueDepth')) ?? 0,
  }
}

const CREDENTIAL_PROPS = new Set(['password', 'passwordhash', 'pwd'])

/**
 * Normalizes a backend user object and strips any credential material that
 * the backend currently leaks in registration responses (PasswordHash).
 */
export function normalizeAuthUser(raw: unknown): AuthUser {
  const record = asRecord(raw)
  if (!record) return { id: '', email: '' }
  const safeSource: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(record)) {
    if (CREDENTIAL_PROPS.has(key.toLowerCase().replace(/[^a-z0-9]/g, ''))) continue
    safeSource[key] = value
  }
  return {
    id: asString(pickField(safeSource, 'id', 'userId')) ?? '',
    email: asString(pickField(safeSource, 'email')) ?? '',
    organization: asString(pickField(safeSource, 'organization', 'orgId')),
    organizationId: asString(pickField(safeSource, 'organizationId', 'organization', 'orgId')),
    organizationName: asString(pickField(safeSource, 'organizationName', 'orgName')),
    role: asString(pickField(safeSource, 'role')),
    createdAt: asString(pickField(safeSource, 'createdAt')),
  }
}

export function isTerminalRunStatus(status?: string | null): boolean {
  return status === 'COMPLETED' || status === 'FAILED'
}

export function normalizeRunStep(raw: unknown, fallbackIndex = 0): RunStep {
  const usageRecord = asRecord(pickField(raw, 'tokenUsage', 'tokens', 'usage'))
  const inputRecord = asRecord(pickField(raw, 'inputMeta', 'input'))
  const outputRecord = asRecord(pickField(raw, 'outputMeta', 'output'))
  return {
    id: asString(pickField(raw, 'id', 'stepId')) ?? '',
    runId: asString(pickField(raw, 'runId')) ?? '',
    index: asNumber(pickField(raw, 'index', 'stepIndex', 'sequence')) ?? fallbackIndex,
    stepType: (asString(pickField(raw, 'stepType', 'type')) ?? 'unknown').toLowerCase(),
    status: (asString(pickField(raw, 'status', 'state')) ?? 'unknown').toLowerCase(),
    inputMeta: inputRecord ? { ...inputRecord } : {},
    outputMeta: outputRecord ? { ...outputRecord } : {},
    error: asString(pickField(raw, 'error', 'errorMessage')),
    tokenUsage: {
      promptTokens: asNumber(pickField(usageRecord, 'promptTokens', 'prompt_tokens', 'prompt')),
      completionTokens: asNumber(pickField(usageRecord, 'completionTokens', 'completion_tokens', 'completion')),
      totalTokens: asNumber(pickField(usageRecord, 'totalTokens', 'total_tokens', 'total')),
    },
    cost: asNumber(pickField(raw, 'cost', 'costCents', 'cost_usd')),
    startedAt: asString(pickField(raw, 'startedAt', 'startTime')),
    completedAt: asString(pickField(raw, 'completedAt', 'endTime', 'finishedAt')),
    createdAt: asString(pickField(raw, 'createdAt', 'created')),
  }
}
