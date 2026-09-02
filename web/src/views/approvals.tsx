// Approvals view (Priority 3) — pending queue from GET /approvals?status=pending
// with approve/reject actions (POST /approvals/{id}/decide) behind a confirm.

import { useMemo, useState } from 'react'
import { useApprovals, useDecideApproval } from '../lib/hooks'
import type { Approval, ApprovalDecision } from '../lib/api/approvals'
import { formatDateTime, formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

const FILTERS = ['pending', 'approved', 'rejected', 'all'] as const
type Filter = (typeof FILTERS)[number]

function DecisionConfirm({
  decision,
  onConfirm,
  onCancel,
  pending,
}: {
  decision: ApprovalDecision
  onConfirm: (reason: string) => void
  onCancel: () => void
  pending: boolean
}) {
  const [reason, setReason] = useState('')
  return (
    <div className="decision-confirm" role="alertdialog" aria-label={`Confirm ${decision}`}>
      <strong>{decision === 'approved' ? 'Approve this request?' : 'Reject this request?'}</strong>
      <div className="field">
        <input
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          placeholder={decision === 'approved' ? 'Reason (optional)' : 'Why is this rejected?'}
          aria-label="Decision reason"
        />
      </div>
      <div className="card-actions">
        <button type="button" className="ghost-button small" onClick={onCancel} disabled={pending}>
          Cancel
        </button>
        <button
          type="button"
          className={decision === 'approved' ? 'primary-button small' : 'danger-button small'}
          onClick={() => onConfirm(reason)}
          disabled={pending}
        >
          {pending ? 'Submitting…' : decision === 'approved' ? 'Confirm approve' : 'Confirm reject'}
        </button>
      </div>
    </div>
  )
}

function ApprovalCard({ approval, canDecide }: { approval: Approval; canDecide: boolean }) {
  const decideApproval = useDecideApproval()
  const [confirming, setConfirming] = useState<ApprovalDecision | null>(null)

  const confirm = (decision: ApprovalDecision, reason: string) => {
    decideApproval.mutate(
      { id: approval.id, decision, reason },
      {
        onSuccess: () => setConfirming(null),
        onError: () => {
          // keep the confirm open so the error below is contextual
        },
      },
    )
  }

  const isPending = approval.status === 'pending'

  return (
    <article className="approval-item">
      <div className="approval-item-head">
        <div>
          <strong>{approval.action || 'Approval request'}</strong>
          {approval.resource ? <small>{approval.resource}</small> : null}
        </div>
        <div className="approval-item-badges">
          {approval.risk ? <span className={`risk-badge ${approval.risk}`}>{approval.risk}</span> : null}
          <StatusPill status={approval.status} />
        </div>
      </div>

      {approval.reason ? <p className="approval-reason">{approval.reason}</p> : null}

      <div className="approval-meta">
        {approval.id ? (
          <div>
            <label>Approval ID</label>
            <strong>{shortenId(approval.id)}</strong>
          </div>
        ) : null}
        {approval.runId ? (
          <div>
            <label>Run</label>
            <strong>{shortenId(approval.runId)}</strong>
          </div>
        ) : null}
        {approval.workflowRunId ? (
          <div>
            <label>Workflow run</label>
            <strong>{shortenId(approval.workflowRunId)}</strong>
          </div>
        ) : null}
        <div>
          <label>Requester</label>
          <strong>{approval.requester || '—'}</strong>
        </div>
        <div>
          <label>Requested</label>
          <strong title={formatDateTime(approval.createdAt)}>{formatRelativeTime(approval.createdAt)}</strong>
        </div>
        {approval.approver ? (
          <div>
            <label>{approval.status === 'approved' ? 'Approver' : 'Decided by'}</label>
            <strong>{approval.approver}</strong>
          </div>
        ) : null}
        {approval.decidedAt ? (
          <div>
            <label>Decided</label>
            <strong>{formatRelativeTime(approval.decidedAt)}</strong>
          </div>
        ) : null}
      </div>

      {decideApproval.isError ? <div className="form-error">{describeError(decideApproval.error)}</div> : null}

      {confirming !== null ? (
        <DecisionConfirm decision={confirming} onConfirm={(reason) => confirm(confirming, reason)} onCancel={() => setConfirming(null)} pending={decideApproval.isPending} />
      ) : isPending && canDecide ? (
        <div className="card-actions">
          <button type="button" className="ghost-button small" onClick={() => setConfirming('rejected')}>
            Reject
          </button>
          <button type="button" className="primary-button small" onClick={() => setConfirming('approved')}>
            Approve
          </button>
        </div>
      ) : null}
    </article>
  )
}

export function ApprovalsView({ canDecide }: { canDecide: boolean }) {
  const [filter, setFilter] = useState<Filter>('pending')
  const approvalsQuery = useApprovals(filter === 'all' ? undefined : filter)
  const approvals = useMemo(() => approvalsQuery.data ?? [], [approvalsQuery.data])

  return (
    <>
      <PageHeader
        eyebrow="Governance"
        title="Approvals"
        actions={
          <div className="filter-tabs" role="tablist" aria-label="Approval status filter">
            {FILTERS.map((option) => (
              <button
                key={option}
                type="button"
                className={filter === option ? 'ghost-button small active' : 'ghost-button small'}
                onClick={() => setFilter(option)}
              >
                {option === 'all' ? 'All' : option.charAt(0).toUpperCase() + option.slice(1)}
              </button>
            ))}
          </div>
        }
      />

      {approvalsQuery.isError ? <ErrorBanner error={approvalsQuery.error} onRetry={() => void approvalsQuery.refetch()} /> : null}

      <section className="summary-grid">
        {approvalsQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Pending" value={String(filter === 'pending' ? approvals.length : approvals.filter((a) => a.status === 'pending').length)} accent="warning" />
            <SummaryStat label="Approved" value={String(approvals.filter((a) => a.status === 'approved').length)} accent="success" />
            <SummaryStat label="Rejected" value={String(approvals.filter((a) => a.status === 'rejected').length)} />
            <SummaryStat label="Filter" value={filter === 'all' ? 'All statuses' : filter} accent="info" />
          </>
        )}
      </section>

      <section className="approval-stack">
        {approvalsQuery.isPending ? (
          [0, 1, 2].map((index) => (
            <article key={index} className="approval-item">
              <Skeleton height={16} width="40%" />
              <Skeleton height={14} style={{ marginTop: 12 }} />
              <Skeleton height={14} width="70%" style={{ marginTop: 8 }} />
            </article>
          ))
        ) : approvals.length === 0 ? (
          <EmptyState
            title={filter === 'pending' ? 'No approvals to review' : 'No approval records match this filter'}
            hint={
              filter === 'pending'
                ? 'When a run or workflow requests human sign-off (approval node, HITL policy), it appears here for a decision.'
                : 'Decisions and older requests will appear here once approvals flow through the platform.'
            }
          />
        ) : (
          approvals.map((approval) => <ApprovalCard key={approval.id} approval={approval} canDecide={canDecide} />)
        )}
      </section>
    </>
  )
}
