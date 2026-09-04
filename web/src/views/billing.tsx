// Billing view (issue #53; wave-5 billing contract, cmd/api/billing.go):
//
// - GET /billing/subscription -> {"subscription":{…},"quota":{…}} (runs.execute — MEMBER+)
// - GET /billing/plans        -> {"plans":[…]}                    (runs.execute — MEMBER+)
// - GET /billing/invoices     -> {"invoices":[…]}                 (runs.execute — MEMBER+)
// - POST /billing/subscriptions -> {"subscription":{…}}           (organization.manage — OWNER only)
//
// A missing subscription is the API's documented 404 NO_SUBSCRIPTION state —
// rendered as a helpful empty state (plan catalog + OWNER subscribe action),
// never as an error and never with invented data.

import { useMemo } from 'react'
import { useBillingInvoices, useBillingPlans, useBillingSubscription, useSubscribeBilling } from '../lib/hooks'
import type { BillingPlan } from '../lib/api/billing'
import { formatCents, formatDateTime, formatNumber } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { apiErrorCode, describeError } from './uiHelpers'

function QuotaBar({ consumed, included, exceeded }: { consumed: number; included: number; exceeded: boolean }) {
  const pct = included > 0 ? Math.min(Math.round((consumed / included) * 100), 100) : 0
  return (
    <div className="quota-row">
      <div className="quota-caption">
        <span>
          {formatNumber(consumed)} of {formatNumber(included)} included runs consumed
        </span>
        <span>{formatNumber(Math.max(included - consumed, 0))} remaining</span>
      </div>
      <div className={`meter${exceeded ? ' exceeded' : ''}`}>
        <span style={{ width: `${pct}%` }} />
      </div>
      <strong>{pct}% of the monthly run budget</strong>
    </div>
  )
}

function PlanCatalog({ plans, canSubscribe }: { plans: BillingPlan[]; canSubscribe: boolean }) {
  const subscribe = useSubscribeBilling()
  if (plans.length === 0) return null
  return (
    <article className="panel wide">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Plan catalog</p>
          <h3>Available plans</h3>
        </div>
        <span className="form-note">GET /billing/plans</span>
      </div>
      {subscribe.isError ? <div className="form-error">{describeError(subscribe.error)}</div> : null}
      <div className="agent-grid">
        {plans.map((plan) => (
          <article key={plan.id} className="agent-card panel">
            <div className="agent-card-header">
              <div>
                <h3>{plan.name}</h3>
                <p className="form-note">
                  {formatCents(plan.priceCents)} / month · {plan.currency.toUpperCase()}
                </p>
              </div>
            </div>
            <p>{plan.includedQuota > 0 ? `${formatNumber(plan.includedQuota)} runs included per month` : 'Unlimited runs included'}</p>
            <div className="listing-meta">
              {canSubscribe ? (
                <button
                  type="button"
                  className="primary-button small"
                  disabled={subscribe.isPending}
                  onClick={() => subscribe.mutate(plan.id)}
                >
                  {subscribe.isPending ? 'Subscribing…' : 'Subscribe'}
                </button>
              ) : (
                <span className="form-note">Subscribing is reserved to the organization OWNER</span>
              )}
            </div>
          </article>
        ))}
      </div>
    </article>
  )
}

export function BillingView({ canSubscribe }: { canSubscribe: boolean }) {
  const subscriptionQuery = useBillingSubscription()
  const plansQuery = useBillingPlans()
  const invoicesQuery = useBillingInvoices()

  const snapshot = subscriptionQuery.data
  const plans = useMemo(() => plansQuery.data ?? [], [plansQuery.data])
  const invoices = useMemo(() => invoicesQuery.data ?? [], [invoicesQuery.data])

  // The API's documented empty state: the org has never subscribed.
  const noSubscription = subscriptionQuery.isError && apiErrorCode(subscriptionQuery.error) === 'NO_SUBSCRIPTION'

  const planName = (planId: string) => plans.find((plan) => plan.id === planId)?.name ?? (planId || '—')

  const loading = subscriptionQuery.isPending || plansQuery.isPending

  return (
    <>
      <PageHeader
        eyebrow="Account"
        title="Billing"
        actions={<span className="form-note">GET /billing/subscription · GET /billing/invoices</span>}
      />

      {subscriptionQuery.isError && !noSubscription ? (
        <ErrorBanner error={subscriptionQuery.error} onRetry={() => void subscriptionQuery.refetch()} />
      ) : null}
      {invoicesQuery.isError ? (
        <ErrorBanner error={invoicesQuery.error} onRetry={() => void invoicesQuery.refetch()} />
      ) : null}

      {noSubscription ? (
        <EmptyState
          title="No subscription yet"
          hint="This organization has no billing plan, so runs are metered but no quota is reserved. Pick a plan below — subscribing is an OWNER action against POST /billing/subscriptions."
        />
      ) : null}

      {noSubscription ? (
        plansQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : (
          <PlanCatalog plans={plans} canSubscribe={canSubscribe} />
        )
      ) : null}

      {snapshot && !noSubscription ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Plan" value={planName(snapshot.subscription.planId)} accent="info" />
            <SummaryStat label="Subscription status" value={snapshot.subscription.status} accent="success" />
            <SummaryStat
              label="Runs consumed"
              value={snapshot.quota.unlimited ? `${formatNumber(snapshot.quota.consumedRuns)} (unlimited)` : formatNumber(snapshot.quota.consumedRuns)}
              accent={snapshot.quota.exceeded ? 'warning' : 'default'}
            />
            <SummaryStat
              label="Period ends"
              value={formatDateTime(snapshot.subscription.periodEnd)}
              accent={snapshot.subscription.cancelAtPeriodEnd ? 'warning' : 'default'}
            />
          </section>

          <section className="content-grid">
            <article className="panel wide">
              <div className="panel-header">
                <div>
                  <p className="eyebrow">Quota</p>
                  <h3>Monthly run budget</h3>
                </div>
                <span className="form-note">GET /billing/subscription → quota</span>
              </div>

              {snapshot.quota.unlimited ? (
                <EmptyState
                  title="Unlimited plan"
                  hint={`This plan does not cap runs. ${formatNumber(snapshot.quota.consumedRuns)} runs were consumed in the current period (${formatDateTime(snapshot.quota.periodStart)} → ${formatDateTime(snapshot.quota.periodEnd)}).`}
                />
              ) : (
                <QuotaBar consumed={snapshot.quota.consumedRuns} included={snapshot.quota.includedRuns} exceeded={snapshot.quota.exceeded} />
              )}

              <div className="quality-list" style={{ marginTop: 18 }}>
                <div>
                  <label>Subscription</label>
                  <strong>
                    {planName(snapshot.subscription.planId)} · <StatusPill status={snapshot.subscription.status} />
                  </strong>
                </div>
                <div>
                  <label>Billing period</label>
                  <strong>
                    {formatDateTime(snapshot.quota.periodStart)} → {formatDateTime(snapshot.quota.periodEnd)}
                  </strong>
                </div>
                {snapshot.subscription.cancelAtPeriodEnd ? (
                  <div>
                    <label>Cancellation</label>
                    <strong>Scheduled at period end{snapshot.subscription.canceledAt ? ` (canceled ${formatDateTime(snapshot.subscription.canceledAt)})` : ''}</strong>
                  </div>
                ) : null}
                {snapshot.quota.exceeded ? (
                  <div>
                    <label>Quota</label>
                    <strong>Exceeded — new runs may be denied while AGENTOS_BILLING_ENFORCEMENT is on</strong>
                  </div>
                ) : null}
              </div>
            </article>
          </section>
        </>
      ) : null}

      {!noSubscription ? (
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Invoices</p>
              <h3>Billing history</h3>
            </div>
            <span className="form-note">GET /billing/invoices</span>
          </div>

          {invoicesQuery.isPending && !loading ? (
            <div className="stack-gap">
              <Skeleton height={16} />
              <Skeleton height={16} />
            </div>
          ) : invoices.length === 0 ? (
            <EmptyState
              title="No invoices yet"
              hint="Invoices are cut per billing period; the first one appears after the current period closes."
            />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Period</th>
                    <th>Amount</th>
                    <th>Status</th>
                    <th>Created</th>
                  </tr>
                </thead>
                <tbody>
                  {invoices.map((invoice) => (
                    <tr key={invoice.id}>
                      <td>
                        <strong>
                          {formatDateTime(invoice.periodStart)} → {formatDateTime(invoice.periodEnd)}
                        </strong>
                        <div className="form-note">{invoice.id}</div>
                      </td>
                      <td>{formatCents(invoice.subtotalCents)}</td>
                      <td>
                        <StatusPill status={invoice.status} />
                      </td>
                      <td>{formatDateTime(invoice.createdAt)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </article>
      ) : null}
    </>
  )
}
