// Billing resource (wave-5 track 5-c contract, cmd/api/billing.go):
//
//   GET /billing/subscription  -> {"subscription":{…},"quota":{…}}   (runs.execute — MEMBER+)
//   GET /billing/plans         -> {"plans":[…]}                      (runs.execute — MEMBER+)
//   GET /billing/invoices      -> {"invoices":[…]}                   (runs.execute — MEMBER+)
//
// Wire shapes are snake_case map views built by billSubscriptionView /
// billQuotaView / billPlanView / billInvoiceView. There is deliberately no
// organization_id anywhere: the caller IS the tenant.
//
// GET /billing/subscription answers 404 NO_SUBSCRIPTION when the org has no
// subscription row yet — callers treat that as an empty state, not a failure.

import { apiFetch } from './client'
import { asBoolean, asNumber, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type BillingPlan = {
  id: string
  name: string
  priceCents: number
  currency: string
  includedQuota: number
  createdAt?: string
  updatedAt?: string
}

export type BillingSubscription = {
  id: string
  planId: string
  status: string
  periodStart: string
  periodEnd: string
  cancelAtPeriodEnd: boolean
  canceledAt?: string | null
  createdAt?: string
  updatedAt?: string
}

export type BillingQuota = {
  subscriptionId: string
  status: string
  includedRuns: number
  unlimited: boolean
  consumedRuns: number
  remainingRuns: number
  exceeded: boolean
  periodStart: string
  periodEnd: string
}

export type BillingSubscriptionSnapshot = {
  subscription: BillingSubscription
  quota: BillingQuota
}

export type BillingInvoice = {
  id: string
  subscriptionId: string
  periodStart: string
  periodEnd: string
  subtotalCents: number
  currency: string
  status: string
  createdAt?: string
  updatedAt?: string
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizePlan(raw: unknown): BillingPlan {
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    name: asString(pickField(raw, 'name')) ?? 'Unnamed plan',
    priceCents: asNumber(pickField(raw, 'priceCents', 'price_cents')) ?? 0,
    currency: asString(pickField(raw, 'currency')) ?? 'usd',
    includedQuota: asNumber(pickField(raw, 'includedQuota', 'included_quota')) ?? 0,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

function normalizeSubscription(raw: unknown): BillingSubscription {
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    planId: asString(pickField(raw, 'planId', 'plan_id')) ?? '',
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    periodStart: asString(pickField(raw, 'periodStart', 'period_start')) ?? '',
    periodEnd: asString(pickField(raw, 'periodEnd', 'period_end')) ?? '',
    cancelAtPeriodEnd: asBoolean(pickField(raw, 'cancelAtPeriodEnd', 'cancel_at_period_end')) ?? false,
    canceledAt: asString(pickField(raw, 'canceledAt', 'canceled_at')) ?? null,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

function normalizeQuota(raw: unknown): BillingQuota {
  return {
    subscriptionId: asString(pickField(raw, 'subscriptionId', 'subscription_id')) ?? '',
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    includedRuns: asNumber(pickField(raw, 'includedRuns', 'included_runs')) ?? 0,
    unlimited: asBoolean(pickField(raw, 'unlimited')) ?? false,
    consumedRuns: asNumber(pickField(raw, 'consumedRuns', 'consumed_runs')) ?? 0,
    remainingRuns: asNumber(pickField(raw, 'remainingRuns', 'remaining_runs')) ?? 0,
    exceeded: asBoolean(pickField(raw, 'exceeded')) ?? false,
    periodStart: asString(pickField(raw, 'periodStart', 'period_start')) ?? '',
    periodEnd: asString(pickField(raw, 'periodEnd', 'period_end')) ?? '',
  }
}

function normalizeInvoice(raw: unknown): BillingInvoice {
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    subscriptionId: asString(pickField(raw, 'subscriptionId', 'subscription_id')) ?? '',
    periodStart: asString(pickField(raw, 'periodStart', 'period_start')) ?? '',
    periodEnd: asString(pickField(raw, 'periodEnd', 'period_end')) ?? '',
    subtotalCents: asNumber(pickField(raw, 'subtotalCents', 'subtotal_cents')) ?? 0,
    currency: asString(pickField(raw, 'currency')) ?? 'usd',
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

export async function getBillingSubscription(): Promise<BillingSubscriptionSnapshot> {
  const raw = await apiFetch<unknown>('/billing/subscription')
  return {
    subscription: normalizeSubscription(pickField(raw, 'subscription') ?? raw),
    quota: normalizeQuota(pickField(raw, 'quota')),
  }
}

export async function listBillingPlans(): Promise<BillingPlan[]> {
  const raw = await apiFetch<unknown>('/billing/plans')
  const list = pickField(raw, 'plans')
  return (Array.isArray(list) ? list : []).map(normalizePlan)
}

export async function listBillingInvoices(): Promise<BillingInvoice[]> {
  const raw = await apiFetch<unknown>('/billing/invoices')
  const list = pickField(raw, 'invoices')
  return (Array.isArray(list) ? list : []).map(normalizeInvoice)
}

/** POST /billing/subscriptions (organization.manage — OWNER only). */
export async function subscribeBilling(planId: string): Promise<BillingSubscription> {
  const raw = await apiFetch<unknown>('/billing/subscriptions', {
    method: 'POST',
    body: JSON.stringify({ plan_id: planId }),
  })
  return normalizeSubscription(pickField(raw, 'subscription') ?? raw)
}
