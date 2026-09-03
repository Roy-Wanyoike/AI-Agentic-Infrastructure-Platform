// Policies view (track 2-c contract): list + evaluate form + OWNER/ADMIN-gated
// policy creation.
//
// Wired endpoints (no mocks):
// - GET  /policies          -> {"policies":[…]}          (policies.read — all roles)
// - POST /policies/evaluate -> {"decision","matched_policy_id","reason"} (policies.read)
// - POST /policies/create   -> {"policy":{…}}            (policies.write — OWNER/ADMIN only)
//
// The evaluate form posts the exact request document the API pins
// (subject/action/resource/context — internal/policies/policy.go), so the UI
// never pretends a simpler contract exists.

import { useState, type FormEvent } from 'react'
import { useCreatePolicy, useEvaluatePolicy, usePolicies } from '../lib/hooks'
import type { Policy } from '../lib/api/policies'
import { formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

const EVALUATE_PLACEHOLDER = `{
  "subject": { "user_id": "user-1", "role": "MEMBER" },
  "action": "execute",
  "resource": { "type": "tool", "id": "tool-1", "tenant_id": "" },
  "context": { "estimated_cost_cents": 0, "environment": "development" }
}`

const POLICY_RESOURCE_TYPES = ['tool', 'agent', 'workflow', 'deployment', '*'] as const

function CreatePolicyForm({ onDone }: { onDone: () => void }) {
  const createPolicy = useCreatePolicy()
  const [name, setName] = useState('')
  const [effect, setEffect] = useState('deny')
  const [resourceType, setResourceType] = useState<string>(POLICY_RESOURCE_TYPES[0])
  const [actions, setActions] = useState('')
  const [priority, setPriority] = useState('100')
  const [toolAllowlist, setToolAllowlist] = useState('')
  const [environments, setEnvironments] = useState('')
  const [maxCostCents, setMaxCostCents] = useState('')

  const toList = (raw: string): string[] =>
    raw
      .split(',')
      .map((entry) => entry.trim())
      .filter(Boolean)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (createPolicy.isPending) return
    const parsedCost = maxCostCents.trim() === '' ? null : Number(maxCostCents)
    createPolicy.mutate(
      {
        name: name.trim(),
        effect,
        resource_type: resourceType,
        actions: toList(actions),
        conditions: {
          tool_allowlist: toList(toolAllowlist),
          environments: toList(environments),
          max_cost_cents: parsedCost !== null && Number.isFinite(parsedCost) ? parsedCost : null,
        },
        priority: Number(priority) || 0,
        enabled: true,
      },
      { onSuccess: onDone },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">New policy</p>
          <h3>Create policy</h3>
        </div>
        <button type="button" className="link-button" onClick={onDone}>
          Cancel
        </button>
      </div>

      {createPolicy.isError ? <div className="form-error">{describeError(createPolicy.error)}</div> : null}

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="policy-name">Name</label>
            <input id="policy-name" value={name} onChange={(event) => setName(event.target.value)} placeholder="Block expensive tools in prod" required />
          </div>
          <div className="field">
            <label htmlFor="policy-effect">Effect</label>
            <select id="policy-effect" value={effect} onChange={(event) => setEffect(event.target.value)}>
              <option value="deny">deny</option>
              <option value="allow">allow</option>
            </select>
          </div>
          <div className="field">
            <label htmlFor="policy-resource">Resource type</label>
            <select id="policy-resource" value={resourceType} onChange={(event) => setResourceType(event.target.value)}>
              {POLICY_RESOURCE_TYPES.map((candidate) => (
                <option key={candidate} value={candidate}>
                  {candidate}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="policy-actions">Actions (comma-separated, empty = all)</label>
            <input id="policy-actions" value={actions} onChange={(event) => setActions(event.target.value)} placeholder="execute, deploy" />
          </div>
          <div className="field">
            <label htmlFor="policy-priority">Priority (higher wins)</label>
            <input id="policy-priority" type="number" value={priority} onChange={(event) => setPriority(event.target.value)} />
          </div>
          <div className="field">
            <label htmlFor="policy-tools">Tool allowlist (comma-separated, optional)</label>
            <input id="policy-tools" value={toolAllowlist} onChange={(event) => setToolAllowlist(event.target.value)} placeholder="search, calculator" />
          </div>
          <div className="field">
            <label htmlFor="policy-envs">Environments (comma-separated, optional)</label>
            <input id="policy-envs" value={environments} onChange={(event) => setEnvironments(event.target.value)} placeholder="production" />
          </div>
          <div className="field">
            <label htmlFor="policy-cost">Max cost (cents, optional)</label>
            <input id="policy-cost" type="number" min="0" value={maxCostCents} onChange={(event) => setMaxCostCents(event.target.value)} placeholder="50" />
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">POST /policies/create (policies.write)</span>
          <button type="submit" className="primary-button" disabled={createPolicy.isPending}>
            {createPolicy.isPending ? 'Creating…' : 'Create policy'}
          </button>
        </div>
      </form>
    </article>
  )
}

function PolicyRow({ policy }: { policy: Policy }) {
  return (
    <tr>
      <td>
        <strong>{policy.name}</strong>
        <div className="form-note">{shortenId(policy.id)}</div>
      </td>
      <td>
        <StatusPill status={policy.effect} />
      </td>
      <td>
        <code>{policy.resourceType}</code>
      </td>
      <td>{policy.actions.length > 0 ? policy.actions.join(', ') : 'all'}</td>
      <td>{policy.priority}</td>
      <td>
        <StatusPill status={policy.enabled ? 'enabled' : 'disabled'} />
      </td>
      <td>{formatRelativeTime(policy.updatedAt ?? policy.createdAt)}</td>
    </tr>
  )
}

export function PoliciesView({ canManage }: { canManage: boolean }) {
  const policiesQuery = usePolicies()
  const [showCreate, setShowCreate] = useState(false)
  const [requestText, setRequestText] = useState('')
  const [parseError, setParseError] = useState<string | null>(null)
  const evaluate = useEvaluatePolicy()

  const policies = policiesQuery.data ?? []
  const denyPolicies = policies.filter((policy) => policy.effect === 'deny').length
  const decision = evaluate.data

  const submitEvaluate = (event: FormEvent) => {
    event.preventDefault()
    if (evaluate.isPending) return
    setParseError(null)
    let parsed: unknown
    try {
      parsed = JSON.parse(requestText)
    } catch (error) {
      setParseError(error instanceof Error ? `Invalid JSON: ${error.message}` : 'Invalid JSON')
      return
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      setParseError('The evaluate request must be a JSON object (subject / action / resource / context).')
      return
    }
    evaluate.mutate(parsed)
  }

  return (
    <>
      <PageHeader
        eyebrow="Govern"
        title="Policies"
        actions={
          canManage ? (
            <button type="button" className="primary-button" onClick={() => setShowCreate((open) => !open)}>
              New policy
            </button>
          ) : (
            <span className="form-note">Creating policies needs OWNER or ADMIN (policies.write)</span>
          )
        }
      />

      {showCreate && canManage ? (
        <CreatePolicyForm onDone={() => setShowCreate(false)} />
      ) : null}

      {policiesQuery.isError ? <ErrorBanner error={policiesQuery.error} onRetry={() => void policiesQuery.refetch()} /> : null}

      <section className="summary-grid">
        {policiesQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Total policies" value={String(policies.length)} />
            <SummaryStat label="Enabled" value={String(policies.filter((policy) => policy.enabled).length)} accent="success" />
            <SummaryStat label="Deny rules" value={String(denyPolicies)} accent="warning" />
            <SummaryStat label="API" value="/policies" accent="info" />
          </>
        )}
      </section>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Catalog</p>
            <h3>All policies</h3>
          </div>
          <span className="form-note">GET /policies</span>
        </div>

        {policiesQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : policies.length === 0 ? (
          <EmptyState
            title="No policies defined"
            hint="Policies gate agent/tool/workflow actions with allow/deny decisions. Create one to get started."
            action={
              canManage ? (
                <button type="button" className="primary-button" onClick={() => setShowCreate(true)}>
                  Create policy
                </button>
              ) : undefined
            }
          />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Effect</th>
                  <th>Resource</th>
                  <th>Actions</th>
                  <th>Priority</th>
                  <th>Enabled</th>
                  <th>Updated</th>
                </tr>
              </thead>
              <tbody>
                {policies.map((policy) => (
                  <PolicyRow key={policy.id} policy={policy} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </article>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Simulate</p>
            <h3>Evaluate a request</h3>
          </div>
          <span className="form-note">POST /policies/evaluate</span>
        </div>

        <form onSubmit={submitEvaluate}>
          <div className="field">
            <label htmlFor="policy-evaluate">Request document (JSON)</label>
            <textarea
              id="policy-evaluate"
              className="code-input"
              rows={8}
              value={requestText}
              onChange={(event) => setRequestText(event.target.value)}
              placeholder={EVALUATE_PLACEHOLDER}
              spellCheck={false}
              required
            />
            <span className="form-note">
              subject: who · action: what they want to do · resource: type/id · context: environment, estimated cost, tool override.
            </span>
          </div>
          {parseError ? <div className="form-error">{parseError}</div> : null}
          {evaluate.isError && !parseError ? <div className="form-error">{describeError(evaluate.error)}</div> : null}
          <div className="form-actions">
            <span className="form-note">Evaluated against your organization&apos;s enabled policies.</span>
            <button type="submit" className="primary-button" disabled={evaluate.isPending}>
              {evaluate.isPending ? 'Evaluating…' : 'Evaluate'}
            </button>
          </div>
        </form>

        {decision ? (
          <div className={`decision-card decision-${decision.decision === 'allow' ? 'allow' : 'deny'}`} role="status">
            <div className="decision-head">
              <StatusPill status={decision.decision.toUpperCase()} />
              <strong>{decision.decision === 'allow' ? 'Request allowed' : 'Request denied'}</strong>
            </div>
            <dl className="decision-detail">
              <div>
                <dt>Matched policy</dt>
                <dd>{decision.matchedPolicyId ? <code>{decision.matchedPolicyId}</code> : 'none (default decision)'}</dd>
              </div>
              <div>
                <dt>Reason</dt>
                <dd>{decision.reason || '—'}</dd>
              </div>
            </dl>
          </div>
        ) : null}
      </article>
    </>
  )
}
