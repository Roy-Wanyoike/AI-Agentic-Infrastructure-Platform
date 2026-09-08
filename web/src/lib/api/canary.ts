// Canary deployment controls (issues #13 + #51; cmd/api/canary.go,
// internal/deployments/{canary,policy}.go).
//
//   POST /deployments/{id}/canary           {canary_version?, canary_weight?}
//         -> {"deployment":{…}}             (deployments.deploy — OWNER/ADMIN)
//   POST /deployments/{id}/canary/promote    -> {"deployment":{…}} (OWNER/ADMIN)
//   POST /deployments/{id}/canary/abort      -> {"deployment":{…}} (OWNER/ADMIN)
//   GET  /agents/{agentId}/canary/status?environment=
//         -> {"canary_status":{…}}          (runs.read — all roles)
//
// The status read model (deployments.CanaryStatus) carries the live split %,
// the eval-gated promotion policy in effect, the single recorded automatic
// decision (+ its human-readable reason) and — for an active canary with a
// sample source wired — fresh sample stats. Fields policy/decision/stats/
// window_start are omitempty: absent means exactly "not configured / not yet
// decided / no sample source", never an error.
//
// The deployment views echoed by the write endpoints are the same
// {"deployment":{...,"canary_version","canary_weight"}} shape the versions
// module already normalizes (normalizeDeployment).

import { apiFetch } from './client'
import { asBoolean, asNumber, asRecord, asString, pickField } from './types'
import { normalizeDeployment, type Deployment } from './versions'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Eval-gated promotion policy (deployments.AgentPromotionPolicy). */
export type CanaryPolicy = {
  minPassRate: number
  minCanaryRuns: number
  /** 0 disables the latency gate. */
  maxP95LatencyMs: number
  /** 0 disables the cost gate. */
  maxCostPerRunCents: number
}

/** The immutable record of one automatic promote/rollback decision. */
export type CanaryDecision = {
  action: string
  /** Backend format: "<metric> <observed> <op> <threshold> → <action>". */
  reason: string
  decidedAt?: string
  runsCounted: number
  passRate: number
  p95LatencyMs: number
  avgCostCents: number
}

/** Aggregate of the in-window eval sample (deployments.CanarySampleStats). */
export type CanarySampleStats = {
  runsCounted: number
  casesCounted: number
  passedCases: number
  passRate: number
  p95LatencyMs: number
  avgCostCents: number
}

/** GET /agents/{agentId}/canary/status read model (deployments.CanaryStatus). */
export type CanaryStatus = {
  agentId: string
  environment: string
  deploymentId: string
  stableVersion: number
  canaryVersion: number
  /** Current traffic split percentage (0-100). */
  canaryWeight: number
  canaryActive: boolean
  windowStart?: string
  policy?: CanaryPolicy | null
  decision?: CanaryDecision | null
  stats?: CanarySampleStats | null
}

export type SetCanaryInput = {
  /** Attach/replace the canary version (any existing version of the same agent ≠ stable). */
  canaryVersion?: number
  /** Move the 0-100 split point (requires an attached canary). */
  canaryWeight?: number
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizePolicy(raw: unknown): CanaryPolicy | null {
  const record = asRecord(raw)
  if (!record) return null
  return {
    minPassRate: asNumber(pickField(record, 'min_pass_rate', 'minPassRate')) ?? 0,
    minCanaryRuns: asNumber(pickField(record, 'min_canary_runs', 'minCanaryRuns')) ?? 0,
    maxP95LatencyMs: asNumber(pickField(record, 'max_p95_latency_ms', 'maxP95LatencyMs')) ?? 0,
    maxCostPerRunCents: asNumber(pickField(record, 'max_cost_per_run_cents', 'maxCostPerRunCents')) ?? 0,
  }
}

function normalizeDecision(raw: unknown): CanaryDecision | null {
  const record = asRecord(raw)
  if (!record) return null
  return {
    action: asString(pickField(record, 'action')) ?? 'unknown',
    reason: asString(pickField(record, 'reason')) ?? '',
    decidedAt: asString(pickField(record, 'decided_at', 'decidedAt')),
    runsCounted: asNumber(pickField(record, 'runs_counted', 'runsCounted')) ?? 0,
    passRate: asNumber(pickField(record, 'pass_rate', 'passRate')) ?? 0,
    p95LatencyMs: asNumber(pickField(record, 'p95_latency_ms', 'p95LatencyMs')) ?? 0,
    avgCostCents: asNumber(pickField(record, 'avg_cost_cents', 'avgCostCents')) ?? 0,
  }
}

function normalizeStats(raw: unknown): CanarySampleStats | null {
  const record = asRecord(raw)
  if (!record) return null
  return {
    runsCounted: asNumber(pickField(record, 'runs_counted', 'runsCounted')) ?? 0,
    casesCounted: asNumber(pickField(record, 'cases_counted', 'casesCounted')) ?? 0,
    passedCases: asNumber(pickField(record, 'passed_cases', 'passedCases')) ?? 0,
    passRate: asNumber(pickField(record, 'pass_rate', 'passRate')) ?? 0,
    p95LatencyMs: asNumber(pickField(record, 'p95_latency_ms', 'p95LatencyMs')) ?? 0,
    avgCostCents: asNumber(pickField(record, 'avg_cost_cents', 'avgCostCents')) ?? 0,
  }
}

export function normalizeCanaryStatus(raw: unknown): CanaryStatus {
  const record = asRecord(raw) ?? {}
  return {
    agentId: asString(pickField(record, 'agent_id', 'agentId')) ?? '',
    environment: (asString(pickField(record, 'environment')) ?? '').toLowerCase(),
    deploymentId: asString(pickField(record, 'deployment_id', 'deploymentId')) ?? '',
    stableVersion: asNumber(pickField(record, 'stable_version', 'stableVersion')) ?? 0,
    canaryVersion: asNumber(pickField(record, 'canary_version', 'canaryVersion')) ?? 0,
    canaryWeight: asNumber(pickField(record, 'canary_weight', 'canaryWeight')) ?? 0,
    canaryActive: asBoolean(pickField(record, 'canary_active', 'canaryActive')) ?? false,
    windowStart: asString(pickField(record, 'window_start', 'windowStart')),
    policy: normalizePolicy(pickField(record, 'policy')),
    decision: normalizeDecision(pickField(record, 'decision')),
    stats: normalizeStats(pickField(record, 'stats')),
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

/** Live canary read model. 404 NOT_FOUND = no healthy deployment serves the pair. */
export async function getCanaryStatus(agentId: string, environment: string): Promise<CanaryStatus> {
  const query = environment ? `?environment=${encodeURIComponent(environment)}` : ''
  const raw = await apiFetch<unknown>(`/agents/${encodeURIComponent(agentId)}/canary/status${query}`)
  return normalizeCanaryStatus(pickField(raw, 'canary_status', 'status') ?? raw)
}

/** Attach/replace the canary version and/or move the split (omitted = unchanged). */
export async function setDeploymentCanary(deploymentId: string, input: SetCanaryInput): Promise<Deployment> {
  const body: Record<string, number> = {}
  if (input.canaryVersion !== undefined) body.canary_version = input.canaryVersion
  if (input.canaryWeight !== undefined) body.canary_weight = input.canaryWeight
  const raw = await apiFetch<unknown>(`/deployments/${encodeURIComponent(deploymentId)}/canary`, {
    method: 'POST',
    body: JSON.stringify(body),
  })
  return normalizeDeployment(pickField(raw, 'deployment') ?? raw)
}

/** The canary becomes the stable version (version swap, canary cleared). */
export async function promoteCanary(deploymentId: string): Promise<Deployment> {
  const raw = await apiFetch<unknown>(`/deployments/${encodeURIComponent(deploymentId)}/canary/promote`, {
    method: 'POST',
  })
  return normalizeDeployment(pickField(raw, 'deployment') ?? raw)
}

/** Clear the canary config; the stable version keeps serving 100%. */
export async function abortCanary(deploymentId: string): Promise<Deployment> {
  const raw = await apiFetch<unknown>(`/deployments/${encodeURIComponent(deploymentId)}/canary/abort`, {
    method: 'POST',
  })
  return normalizeDeployment(pickField(raw, 'deployment') ?? raw)
}
