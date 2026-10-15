// Budget resource (wave-8 issue C: per-scope budget gates, cmd/api/budgets.go):
//
//   GET /budgets[?active=true|false]   (runs.execute — MEMBER+)
//
// Contract response shape (snake_case, exact):
//   {"budgets":[{"id":"…","scope":"organization|agent","ref_id":"…",
//     "period":"monthly","limit_runs":0,"limit_cost_cents":0,
//     "is_active":true,"created_at":"…","updated_at":"…"}]}
//
// limit_runs / limit_cost_cents of 0 mean "unlimited" for that dimension;
// consumption itself is NOT part of the response — the cost dashboard derives
// month-to-date spend from GET /usage/costs of the same period. This client is
// read-only on purpose: writes (POST/PUT/DELETE) are users.manage and are not
// needed by a read-only analytics surface.

import { apiFetch } from './client'
import { asBoolean, asNumber, asString, pickField } from './types'

export type Budget = {
  id: string
  scope: string
  refId: string
  period: string
  limitRuns: number
  limitCostCents: number
  isActive: boolean
  createdAt?: string
  updatedAt?: string
}

function normalizeBudget(raw: unknown): Budget {
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    scope: asString(pickField(raw, 'scope')) ?? 'organization',
    refId: asString(pickField(raw, 'refId', 'ref_id')) ?? '',
    period: asString(pickField(raw, 'period')) ?? 'monthly',
    limitRuns: asNumber(pickField(raw, 'limitRuns', 'limit_runs')) ?? 0,
    limitCostCents: asNumber(pickField(raw, 'limitCostCents', 'limit_cost_cents')) ?? 0,
    isActive: asBoolean(pickField(raw, 'isActive', 'is_active')) ?? false,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

export async function listBudgets(activeOnly = false): Promise<Budget[]> {
  const raw = await apiFetch<unknown>(`/budgets${activeOnly ? '?active=true' : ''}`)
  const wrapped = pickField(raw, 'budgets')
  const list = Array.isArray(wrapped) ? wrapped : Array.isArray(raw) ? raw : []
  return list.map(normalizeBudget)
}
