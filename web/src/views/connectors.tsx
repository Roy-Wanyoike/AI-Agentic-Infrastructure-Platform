// Connectors view (issue #53; issue-#30 contract, cmd/api/connectors.go):
//
// - POST /connectors           create (connectors.write — OWNER/ADMIN)
// - GET  /connectors           list   (connectors.read — MEMBER+)
// - DELETE /connectors/{id}    delete (connectors.write — OWNER/ADMIN)
// - POST /connectors/{id}/test live health check (connectors.write — OWNER/ADMIN)
//
// The probe outcome is shown INLINE on the row (status pill + latency + HTTP
// code) and also persisted server-side as last_check_at/last_check_status.
//
// NOTE on "U" in CRUD: the connectors service has an Update method but the
// HTTP surface exposes no PUT/PATCH route, so editing an existing connector is
// not possible via the API and is not faked here. secret_ref is a NAME
// reference into the Secrets view — no secret value is ever sent or shown.

import { useMemo, useState, type FormEvent } from 'react'
import { useConnectors, useCreateConnector, useDeleteConnector, useTestConnector } from '../lib/hooks'
import {
  CONNECTOR_AUTH_STYLES,
  CONNECTOR_STATUSES,
  CONNECTOR_TYPES,
  type Connector,
  type ConnectorTestResult,
} from '../lib/api/connectors'
import { formatDateTime, formatRelativeTime } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

/** Parses the "Header-Name: value" textarea into the config's header templates. */
function parseHeaders(raw: string): { headers: Record<string, string>; error: string | null } {
  const headers: Record<string, string> = {}
  for (const line of raw.split('\n')) {
    const trimmed = line.trim()
    if (trimmed === '') continue
    const sep = trimmed.indexOf(':')
    if (sep <= 0) return { headers: {}, error: `Header line "${trimmed}" is not "Name: value".` }
    headers[trimmed.slice(0, sep).trim()] = trimmed.slice(sep + 1).trim()
  }
  return { headers, error: null }
}

function describeAuth(connector: Connector): string {
  const style = connector.config.authStyle || 'none'
  if (style === 'basic') return `basic (user ${connector.config.username || '—'})`
  if (style === 'api_key_header') return `api_key_header → ${connector.config.apiKeyHeader || 'X-API-Key'}`
  return style
}

function ConnectorRowActions({
  connector,
  canManage,
}: {
  connector: Connector
  canManage: boolean
}) {
  const test = useTestConnector()
  const deleteConnector = useDeleteConnector()
  const [testResult, setTestResult] = useState<ConnectorTestResult | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)

  if (!canManage) return <span className="form-note">test/delete needs OWNER/ADMIN</span>

  const runTest = () => {
    setActionError(null)
    test.mutate(connector.id, {
      onSuccess: (result) => setTestResult(result),
      onError: (error) => setActionError(describeError(error)),
    })
  }

  const confirmDelete = () => {
    if (!window.confirm(`Delete connector "${connector.name}"? Agents will no longer reach ${connector.baseUrl}.`)) return
    setActionError(null)
    deleteConnector.mutate(connector.id, {
      onError: (error) => setActionError(describeError(error)),
    })
  }

  return (
    <div className="table-actions">
      {actionError ? <span className="form-error inline">{actionError}</span> : null}
      {testResult ? (
        <span className={`connector-check ${testResult.status === 'ok' ? 'check-ok' : 'check-error'}`}>
          {testResult.status.toUpperCase()} · {testResult.statusCode || '—'} · {testResult.latencyMs}ms ·{' '}
          {formatRelativeTime(testResult.checkedAt)}
          {testResult.error ? ` · ${testResult.error}` : ''}
        </span>
      ) : null}
      <button type="button" className="ghost-button small" disabled={test.isPending} onClick={runTest}>
        {test.isPending ? 'Testing…' : 'Test'}
      </button>
      <button type="button" className="danger-button small" disabled={deleteConnector.isPending} onClick={confirmDelete}>
        {deleteConnector.isPending ? 'Deleting…' : 'Delete'}
      </button>
    </div>
  )
}

function CreateConnectorForm({ onCreated }: { onCreated: (connector: Connector) => void }) {
  const createConnector = useCreateConnector()
  const [name, setName] = useState('')
  const [type, setType] = useState<string>(CONNECTOR_TYPES[0])
  const [baseUrl, setBaseUrl] = useState('')
  const [authStyle, setAuthStyle] = useState<string>('none')
  const [headersRaw, setHeadersRaw] = useState('')
  const [apiKeyHeader, setApiKeyHeader] = useState('X-API-Key')
  const [apiKeyPrefix, setApiKeyPrefix] = useState('')
  const [username, setUsername] = useState('')
  const [secretRef, setSecretRef] = useState('')
  const [status, setStatus] = useState<string>('active')
  const [message, setMessage] = useState<string | null>(null)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (createConnector.isPending) return
    const { headers, error } = parseHeaders(headersRaw)
    if (error) {
      setMessage(error)
      return
    }
    setMessage(null)
    createConnector.mutate(
      {
        name: name.trim(),
        type,
        base_url: baseUrl.trim(),
        auth_style: authStyle,
        headers: Object.keys(headers).length > 0 ? headers : undefined,
        api_key_header: authStyle === 'api_key_header' ? apiKeyHeader.trim() : undefined,
        api_key_prefix: authStyle === 'api_key_header' && apiKeyPrefix.trim() ? apiKeyPrefix.trim() : undefined,
        username: authStyle === 'basic' ? username.trim() : undefined,
        secret_ref: secretRef.trim() ? secretRef.trim() : undefined,
        status,
      },
      {
        onSuccess: (connector) => {
          setName('')
          setBaseUrl('')
          setHeadersRaw('')
          setSecretRef('')
          setMessage(`Connector "${connector.name}" created. Run a Test to probe ${connector.baseUrl}.`)
          onCreated(connector)
        },
        onError: (error) => setMessage(describeError(error)),
      },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">New connector</p>
          <h3>Create connector</h3>
        </div>
        <span className="form-note">POST /connectors</span>
      </div>

      {message ? <div className={createConnector.isError ? 'form-error' : 'form-note'}>{message}</div> : null}

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="connector-name">Name</label>
            <input id="connector-name" value={name} onChange={(event) => setName(event.target.value)} placeholder="billing-api" required />
          </div>
          <div className="field">
            <label htmlFor="connector-type">Type</label>
            <select id="connector-type" value={type} onChange={(event) => setType(event.target.value)}>
              {CONNECTOR_TYPES.map((candidate) => (
                <option key={candidate} value={candidate}>
                  {candidate}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="connector-base-url">Base URL</label>
            <input
              id="connector-base-url"
              type="url"
              value={baseUrl}
              onChange={(event) => setBaseUrl(event.target.value)}
              placeholder="https://api.example.com"
              required
            />
          </div>
          <div className="field">
            <label htmlFor="connector-auth">Auth style</label>
            <select id="connector-auth" value={authStyle} onChange={(event) => setAuthStyle(event.target.value)}>
              {CONNECTOR_AUTH_STYLES.map((candidate) => (
                <option key={candidate} value={candidate}>
                  {candidate}
                </option>
              ))}
            </select>
          </div>
          {authStyle === 'basic' ? (
            <div className="field">
              <label htmlFor="connector-username">Username (password comes from the secret)</label>
              <input id="connector-username" value={username} onChange={(event) => setUsername(event.target.value)} placeholder="svc-agentos" />
            </div>
          ) : null}
          {authStyle === 'api_key_header' ? (
            <>
              <div className="field">
                <label htmlFor="connector-apikey-header">API key header</label>
                <input id="connector-apikey-header" value={apiKeyHeader} onChange={(event) => setApiKeyHeader(event.target.value)} />
              </div>
              <div className="field">
                <label htmlFor="connector-apikey-prefix">API key prefix (optional)</label>
                <input id="connector-apikey-prefix" value={apiKeyPrefix} onChange={(event) => setApiKeyPrefix(event.target.value)} placeholder="Bearer " />
              </div>
            </>
          ) : null}
          <div className="field">
            <label htmlFor="connector-secret-ref">Secret reference (name from the Secrets view)</label>
            <input
              id="connector-secret-ref"
              value={secretRef}
              onChange={(event) => setSecretRef(event.target.value)}
              placeholder="e.g. EXAMPLE_API_KEY — never the value itself"
              spellCheck={false}
            />
          </div>
          <div className="field">
            <label htmlFor="connector-status">Status</label>
            <select id="connector-status" value={status} onChange={(event) => setStatus(event.target.value)}>
              {CONNECTOR_STATUSES.map((candidate) => (
                <option key={candidate} value={candidate}>
                  {candidate}
                </option>
              ))}
            </select>
          </div>
          <div className="field span-2">
            <label htmlFor="connector-headers">Static headers (optional, one "Name: value" per line)</label>
            <textarea
              id="connector-headers"
              rows={2}
              value={headersRaw}
              onChange={(event) => setHeadersRaw(event.target.value)}
              placeholder={'Accept: application/json\nX-Tenant: acme'}
              spellCheck={false}
            />
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">The secret reference is a name only — values stay encrypted in the secrets store.</span>
          <button type="submit" className="primary-button" disabled={createConnector.isPending}>
            {createConnector.isPending ? 'Creating…' : 'Create connector'}
          </button>
        </div>
      </form>
    </article>
  )
}

export function ConnectorsView({ canManage }: { canManage: boolean }) {
  const connectorsQuery = useConnectors()
  const [showCreate, setShowCreate] = useState(false)
  const connectors = useMemo(() => connectorsQuery.data ?? [], [connectorsQuery.data])
  const healthy = connectors.filter((connector) => connector.lastCheckStatus === 'ok').length
  const failing = connectors.filter((connector) => connector.lastCheckStatus === 'error').length

  return (
    <>
      <PageHeader
        eyebrow="Integrations"
        title="Connectors"
        actions={
          canManage ? (
            <button type="button" className="primary-button" onClick={() => setShowCreate((open) => !open)}>
              New connector
            </button>
          ) : (
            <span className="form-note">Viewing needs MEMBER and above — creating needs OWNER or ADMIN</span>
          )
        }
      />

      {showCreate && canManage ? <CreateConnectorForm onCreated={() => setShowCreate(false)} /> : null}

      {connectorsQuery.isError ? (
        <ErrorBanner error={connectorsQuery.error} onRetry={() => void connectorsQuery.refetch()} />
      ) : null}

      <section className="summary-grid">
        {connectorsQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Total connectors" value={String(connectors.length)} />
            <SummaryStat label="Last check OK" value={String(healthy)} accent="success" />
            <SummaryStat label="Last check failed" value={String(failing)} accent={failing > 0 ? 'warning' : 'default'} />
            <SummaryStat label="API" value="/connectors" accent="info" />
          </>
        )}
      </section>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Registry</p>
            <h3>All connectors</h3>
          </div>
          <span className="form-note">GET /connectors</span>
        </div>

        {connectorsQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : connectors.length === 0 ? (
          <EmptyState
            title="No connectors yet — connect one"
            hint="Connectors register external HTTP endpoints (with auth templates backed by Secrets). Create one, then hit Test to run a live health check."
            action={
              canManage ? (
                <button type="button" className="primary-button" onClick={() => setShowCreate(true)}>
                  Create connector
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
                  <th>Type</th>
                  <th>Base URL</th>
                  <th>Auth</th>
                  <th>Secret ref</th>
                  <th>Status</th>
                  <th>Last check</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {connectors.map((connector) => (
                  <tr key={connector.id}>
                    <td>
                      <strong>{connector.name}</strong>
                      <div className="form-note">{connector.id}</div>
                    </td>
                    <td>
                      <span className={`step-type step-type-${connector.type}`}>{connector.type}</span>
                    </td>
                    <td>
                      <code>{connector.baseUrl}</code>
                    </td>
                    <td>{describeAuth(connector)}</td>
                    <td>{connector.secretRef ? <code>{connector.secretRef}</code> : '—'}</td>
                    <td>
                      <StatusPill status={connector.status} />
                    </td>
                    <td>
                      {connector.lastCheckStatus ? (
                        <span className={`connector-check ${connector.lastCheckStatus === 'ok' ? 'check-ok' : 'check-error'}`}>
                          {connector.lastCheckStatus.toUpperCase()} · {formatRelativeTime(connector.lastCheckAt)} ({formatDateTime(connector.lastCheckAt)})
                        </span>
                      ) : (
                        <span className="form-note">never checked</span>
                      )}
                    </td>
                    <td>
                      <ConnectorRowActions connector={connector} canManage={canManage} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </article>
    </>
  )
}
