// Policies resource (track 2-c contract): list + evaluate.
//
// - GET  /policies            -> {"policies":[{id,name,effect,resource_type,actions,conditions,priority,enabled,created_at,updated_at}]}
// - POST /policies/evaluate   -> {"decision":"allow|deny","matched_policy_id":"…","reason":"…"}
//
// Evaluate takes the full request document (subject/action/resource/context)
// so the form stays honest about what the API accepts; see the placeholder in
// the policies view for the exact shape pinned by internal/policies/policy.go.

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type Policy = {
  id: string
  name: string
  effect: string
  resourceType: string
  actions: string[]
  priority: number
  enabled: boolean
  createdAt?: string
  updatedAt?: string
}

export type PolicyDecision = {
  decision: string
  matchedPolicyId?: string
  reason?: string
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizePolicy(raw: unknown): Policy {
  const actionsRaw = pickField(raw, 'actions')
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    name: asString(pickField(raw, 'name')) ?? 'Unnamed policy',
    effect: (asString(pickField(raw, 'effect')) ?? 'unknown').toLowerCase(),
    resourceType: (asString(pickField(raw, 'resourceType', 'resource_type')) ?? '*').toLowerCase(),
    actions: Array.isArray(actionsRaw) ? actionsRaw.map((action) => asString(action) ?? '').filter(Boolean) : [],
    priority: asNumber(pickField(raw, 'priority')) ?? 0,
    enabled: pickField(raw, 'enabled') === true,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

export async function listPolicies(): Promise<Policy[]> {
  const raw = await apiFetch<unknown>('/policies')
  const list = pickField(raw, 'policies')
  return (Array.isArray(list) ? list : []).map(normalizePolicy)
}

export type CreatePolicyInput = {
  name: string
  effect: string
  resource_type: string
  actions: string[]
  conditions: {
    tool_allowlist: string[]
    environments: string[]
    max_cost_cents: number | null
  }
  priority: number
  enabled: boolean
}

export async function createPolicy(input: CreatePolicyInput): Promise<Policy> {
  const raw = await apiFetch<unknown>('/policies/create', {
    method: 'POST',
    body: JSON.stringify(input),
  })
  return normalizePolicy(pickField(raw, 'policy') ?? raw)
}

export async function evaluatePolicy(request: unknown): Promise<PolicyDecision> {
  const raw = await apiFetch<unknown>('/policies/evaluate', {
    method: 'POST',
    body: JSON.stringify(request),
  })
  return {
    decision: (asString(pickField(raw, 'decision')) ?? 'unknown').toLowerCase(),
    matchedPolicyId: asString(pickField(raw, 'matchedPolicyId', 'matched_policy_id')),
    reason: asString(pickField(raw, 'reason')),
  }
}
