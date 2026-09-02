// Approvals resource (track 2-a contract):
// - GET  /approvals?status=pending → {"approvals": [...]}
// - GET  /approvals/{id}           → single approval
// - POST /approvals/{id}/decide    → {"decision":"approved|rejected","reason":"..."}

import { apiFetch } from './client'
import { asString, pickField } from './types'

export type Approval = {
  id: string
  runId?: string
  workflowRunId?: string
  resource?: string
  action?: string
  reason?: string
  risk?: string
  status: string
  requester?: string
  approver?: string
  createdAt?: string
  decidedAt?: string
}

export type ApprovalDecision = 'approved' | 'rejected'

function normalizeApproval(raw: unknown): Approval {
  return {
    id: asString(pickField(raw, 'id', 'approvalId')) ?? '',
    runId: asString(pickField(raw, 'runId', 'run_id')),
    workflowRunId: asString(pickField(raw, 'workflowRunId', 'workflow_run_id')),
    resource: asString(pickField(raw, 'resource')),
    action: asString(pickField(raw, 'action')),
    reason: asString(pickField(raw, 'reason', 'message')),
    risk: (asString(pickField(raw, 'risk', 'riskLevel', 'risk_level')) ?? '').toLowerCase() || undefined,
    status: (asString(pickField(raw, 'status', 'state')) ?? 'unknown').toLowerCase(),
    requester: asString(pickField(raw, 'requester', 'requestedBy', 'requested_by')),
    approver: asString(pickField(raw, 'approver', 'decidedBy', 'decided_by')),
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    decidedAt: asString(pickField(raw, 'decidedAt', 'decided_at')),
  }
}

function extractApprovalList(raw: unknown): Approval[] {
  const wrapped = pickField(raw, 'approvals', 'items', 'data')
  const list = Array.isArray(raw) ? raw : Array.isArray(wrapped) ? wrapped : []
  return list.map(normalizeApproval)
}

export async function listApprovals(status?: string): Promise<Approval[]> {
  const query = status && status !== 'all' ? `?status=${encodeURIComponent(status)}` : ''
  return extractApprovalList(await apiFetch<unknown>(`/approvals${query}`))
}

export async function getApproval(id: string): Promise<Approval> {
  return normalizeApproval(await apiFetch<unknown>(`/approvals/${encodeURIComponent(id)}`))
}

export async function decideApproval(id: string, decision: ApprovalDecision, reason?: string): Promise<Approval> {
  const raw = await apiFetch<unknown>(`/approvals/${encodeURIComponent(id)}/decide`, {
    method: 'POST',
    body: JSON.stringify({ decision, reason: reason ?? '' }),
  })
  return normalizeApproval(pickField(raw, 'approval') ?? raw)
}
