// Secrets view (issue #53; wave-5 secrets contract, cmd/api/secrets.go):
//
// - GET    /secrets               metadata list (runs.execute — MEMBER+)
// - POST   /secrets               create        (agents.write — OWNER/ADMIN)
// - DELETE /secrets/{name}        soft delete   (agents.write — OWNER/ADMIN)
// - POST   /secrets/{name}/reveal one-time value reveal (organization.manage — OWNER)
//
// SECURITY: the table renders METADATA only (name, key_version, created_at) —
// the API's list projection has no value field and none is invented here. The
// reveal flow is explicit-confirm → value shown once in a dialog → blanked
// from component state the moment the dialog closes (the API never returns
// the same value twice).

import { useMemo, useState, type FormEvent } from 'react'
import { useCreateSecret, useDeleteSecret, useRevealSecret, useSecrets } from '../lib/hooks'
import { SECRET_NAME_PATTERN, type SecretMetadata } from '../lib/api/secrets'
import { formatDateTime, formatRelativeTime } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

function CreateSecretForm({ onCreated }: { onCreated: (secret: SecretMetadata) => void }) {
  const createSecret = useCreateSecret()
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [message, setMessage] = useState<string | null>(null)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (createSecret.isPending) return
    setMessage(null)
    if (!SECRET_NAME_PATTERN.test(name.trim())) {
      setMessage('Secret name must start with a letter or digit and may contain letters, digits, dots, underscores and dashes (max 255 chars).')
      return
    }
    createSecret.mutate(
      { name: name.trim(), value },
      {
        onSuccess: (secret) => {
          setName('')
          setValue('')
          setMessage(`Secret "${secret.name}" created (encrypted with key version ${secret.keyVersion}).`)
          onCreated(secret)
        },
        onError: (error) => setMessage(describeError(error)),
      },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">New secret</p>
          <h3>Create secret</h3>
        </div>
        <span className="form-note">POST /secrets</span>
      </div>

      {message ? <div className={createSecret.isError ? 'form-error' : 'form-note'}>{message}</div> : null}

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="secret-name">Name</label>
            <input
              id="secret-name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              placeholder="OPENAI_API_KEY"
              autoComplete="off"
              spellCheck={false}
              required
            />
          </div>
          <div className="field">
            <label htmlFor="secret-value">Value</label>
            <input
              id="secret-value"
              type="password"
              value={value}
              onChange={(event) => setValue(event.target.value)}
              placeholder="Pasted value is sealed with AES-256-GCM"
              autoComplete="new-password"
              required
            />
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">The value is encrypted server-side and never echoed back in list responses.</span>
          <button type="submit" className="primary-button" disabled={createSecret.isPending}>
            {createSecret.isPending ? 'Creating…' : 'Create secret'}
          </button>
        </div>
      </form>
    </article>
  )
}

/**
 * One-time reveal dialog. The value lives ONLY in this component's props —
 * the parent drops its state on close, so closing blanks the value from the
 * DOM and from memory.
 */
function SecretRevealDialog({ name, value, onClose }: { name: string; value: string; onClose: () => void }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
    } catch {
      // Clipboard unavailable (permissions/insecure context) — the value
      // stays selectable in the dialog, nothing is faked.
      setCopied(false)
    }
  }
  return (
    <div
      className="reveal-overlay"
      role="dialog"
      aria-modal="true"
      aria-label={`Revealed secret ${name}`}
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose()
      }}
    >
      <div className="reveal-panel">
        <div className="panel-header">
          <div>
            <p className="eyebrow">One-time reveal</p>
            <h3>{name}</h3>
          </div>
        </div>
        <p className="form-note">
          This is the only time the API returns this value — it is not stored in the dashboard and cannot be revealed
          again. Copy it now if you need it elsewhere.
        </p>
        <code className="secret-value">{value}</code>
        <div className="form-actions" style={{ marginTop: 14 }}>
          <button type="button" className="ghost-button small" onClick={() => void copy()}>
            {copied ? 'Copied ✓' : 'Copy value'}
          </button>
          <button type="button" className="primary-button" onClick={onClose}>
            {copied ? 'Done — hide value' : 'Hide value'}
          </button>
        </div>
      </div>
    </div>
  )
}

function SecretRowActions({
  secret,
  canManage,
  canReveal,
  onRevealed,
}: {
  secret: SecretMetadata
  canManage: boolean
  canReveal: boolean
  onRevealed: (name: string, value: string) => void
}) {
  const reveal = useRevealSecret()
  const deleteSecret = useDeleteSecret()
  const [actionError, setActionError] = useState<string | null>(null)

  const confirmReveal = () => {
    if (!window.confirm(`Reveal secret "${secret.name}"? The value is shown exactly once and never again.`)) return
    setActionError(null)
    reveal.mutate(secret.name, {
      onSuccess: (revealed) => onRevealed(revealed.name, revealed.value),
      onError: (error) => setActionError(describeError(error)),
    })
  }

  const confirmDelete = () => {
    if (!window.confirm(`Delete secret "${secret.name}"? Connectors and runs that reference it will start failing.`)) return
    setActionError(null)
    deleteSecret.mutate(secret.name, {
      onError: (error) => setActionError(describeError(error)),
    })
  }

  return (
    <div className="table-actions">
      {actionError ? <span className="form-error inline">{actionError}</span> : null}
      {canReveal ? (
        <button type="button" className="ghost-button small" disabled={reveal.isPending} onClick={confirmReveal}>
          {reveal.isPending ? 'Revealing…' : 'Reveal'}
        </button>
      ) : null}
      {canManage ? (
        <button type="button" className="danger-button small" disabled={deleteSecret.isPending} onClick={confirmDelete}>
          {deleteSecret.isPending ? 'Deleting…' : 'Delete'}
        </button>
      ) : (
        <span className="form-note">delete needs OWNER/ADMIN</span>
      )}
    </div>
  )
}

export function SecretsView({ canManage, canReveal }: { canManage: boolean; canReveal: boolean }) {
  const secretsQuery = useSecrets()
  const [showCreate, setShowCreate] = useState(false)
  // The ONLY place a revealed plaintext is kept — dropped on close.
  const [revealed, setRevealed] = useState<{ name: string; value: string } | null>(null)
  const secrets = useMemo(() => secretsQuery.data ?? [], [secretsQuery.data])

  return (
    <>
      <PageHeader
        eyebrow="Security"
        title="Secrets"
        actions={
          canManage ? (
            <button type="button" className="primary-button" onClick={() => setShowCreate((open) => !open)}>
              New secret
            </button>
          ) : (
            <span className="form-note">Viewer/Member role — creating secrets needs OWNER or ADMIN</span>
          )
        }
      />

      {revealed ? (
        <SecretRevealDialog name={revealed.name} value={revealed.value} onClose={() => setRevealed(null)} />
      ) : null}

      {showCreate && canManage ? <CreateSecretForm onCreated={() => setShowCreate(false)} /> : null}

      {secretsQuery.isError ? <ErrorBanner error={secretsQuery.error} onRetry={() => void secretsQuery.refetch()} /> : null}

      <section className="summary-grid">
        {secretsQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Total secrets" value={String(secrets.length)} />
            <SummaryStat label="Encryption" value="AES-256-GCM" accent="success" />
            <SummaryStat label="Value exposure" value="one-time reveal" accent="info" />
            <SummaryStat label="API" value="/secrets" accent="warning" />
          </>
        )}
      </section>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Credential vault</p>
            <h3>All secrets</h3>
          </div>
          <span className="form-note">GET /secrets — metadata only, values are never listed</span>
        </div>

        {secretsQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : secrets.length === 0 ? (
          <EmptyState
            title="No secrets yet — create one"
            hint="Secrets store API keys and passwords encrypted at rest. Connectors reference them by name (secret_ref) and the value is only ever revealed once."
            action={
              canManage ? (
                <button type="button" className="primary-button" onClick={() => setShowCreate(true)}>
                  Create secret
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
                  <th>Key version</th>
                  <th>Created by</th>
                  <th>Created</th>
                  <th>Updated</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {secrets.map((secret) => (
                  <tr key={secret.name}>
                    <td>
                      <code>{secret.name}</code>
                    </td>
                    <td>v{secret.keyVersion}</td>
                    <td>{secret.createdBy || '—'}</td>
                    <td>{formatDateTime(secret.createdAt)}</td>
                    <td>{formatRelativeTime(secret.updatedAt)}</td>
                    <td>
                      <SecretRowActions
                        secret={secret}
                        canManage={canManage}
                        canReveal={canReveal}
                        onRevealed={(name, value) => setRevealed({ name, value })}
                      />
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
