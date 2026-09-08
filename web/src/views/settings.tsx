// Settings view (issue #81): the workspace-level admin surface, starting with
// the Identity section — SSO (OIDC) status + SCIM token mint/revoke.
//
// Backend contracts (cmd/api/sso.go, cmd/api/scim.go):
//   GET    /auth/sso/{org_slug}/login  unauthenticated browser flow — probed
//                                      (redirect: manual, never followed):
//                                      302 = configured, 404 envelope code
//                                      ORG_NOT_FOUND / SSO_NOT_CONFIGURED.
//   POST   /scim/tokens                organization.manage (EXACTLY OWNER);
//                                      201 {"token":{…},"secret":"scim_…"}
//                                      with the plaintext shown ONCE.
//   DELETE /scim/tokens/{id}           204 revoked | 404 unknown/foreign id
//                                      (parallel backend build, pinned 204/404).
//
// HONEST LIMITATION (mirrored from the API): the backend exposes SCIM token
// mint + revoke only — there is no list endpoint for issued tokens. The list
// below is therefore THIS SESSION's mint history (real mint-response
// metadata, never invented) plus revoke-by-ID for tokens minted elsewhere.

import { useMemo, useState, type FormEvent } from 'react'
import { useMintScimToken, useRevokeScimToken, useSsoLoginProbe } from '../lib/hooks'
import { slugifyOrgName, type MintedScimToken, type ScimTokenMetadata, type SsoProbeOutcome } from '../lib/api/identity'
import { formatDateTime, shortenId } from '../lib/format'
import { EmptyState, PageHeader, StatusPill } from './shared'
import { describeError } from './uiHelpers'

// ---------------------------------------------------------------------------
// One-time reveal (same pattern + CSS as the secrets view: the value lives in
// component state only and is dropped the moment the dialog closes).
// ---------------------------------------------------------------------------

function TokenRevealDialog({ token, onClose }: { token: MintedScimToken; onClose: () => void }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(token.secret)
      setCopied(true)
    } catch {
      setCopied(false)
    }
  }
  return (
    <div
      className="reveal-overlay"
      role="dialog"
      aria-modal="true"
      aria-label="Revealed SCIM token"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose()
      }}
    >
      <div className="reveal-panel">
        <div className="panel-header">
          <div>
            <p className="eyebrow">One-time reveal</p>
            <h3>SCIM credential</h3>
          </div>
        </div>
        <p className="form-note">
          Only the SHA-256 hash is stored server-side — this is the only time the scim_ secret is ever shown. Copy it
          into your directory automation now; it cannot be revealed again.
        </p>
        <code className="secret-value">{token.secret}</code>
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

// ---------------------------------------------------------------------------
// SSO (OIDC) status
// ---------------------------------------------------------------------------

function SsoStatusCard({ organizationName }: { organizationName: string }) {
  const probe = useSsoLoginProbe()
  const [slug, setSlug] = useState(() => slugifyOrgName(organizationName))
  const outcome: SsoProbeOutcome | null = probe.data ?? null

  const probeNow = () => {
    if (slug.trim()) probe.mutate(slug)
  }

  const pill = (outcome: SsoProbeOutcome): { status: string; note: string } => {
    switch (outcome.state) {
      case 'configured':
        return { status: 'configured', note: 'SSO is configured — the login endpoint redirects to your identity provider (302).' }
      case 'not-configured':
        return { status: 'not-configured', note: 'The API answered SSO_NOT_CONFIGURED — no OIDC configuration exists for this slug yet.' }
      case 'unknown-org':
        return { status: 'not-configured', note: 'The API answered ORG_NOT_FOUND — no organization matches this login slug.' }
      case 'unreachable':
        return { status: 'unknown', note: 'The API could not be reached to check the SSO endpoint.' }
      case 'unexpected':
        return { status: 'unknown', note: `Unexpected answer (HTTP ${outcome.status}) — check the API logs.` }
    }
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Single sign-on</p>
          <h3>SSO / OIDC status</h3>
        </div>
        {outcome ? <StatusPill status={pill(outcome).status} /> : null}
      </div>

      <p className="detail-copy">
        AgentOS tenants log in through org-scoped OIDC authorization-code flows. Configuration (issuer, client
        credentials, scopes) lives server-side in the sso_config table; this check probes the public login endpoint —
        it redirects (302) when the org is configured and answers a JSON 404 otherwise.
      </p>

      <form
        className="stack-gap"
        onSubmit={(event) => {
          event.preventDefault()
          probeNow()
        }}
      >
        <div className="form-grid">
          <div className="field">
            <label htmlFor="sso-slug">Organization login slug</label>
            <input id="sso-slug" value={slug} onChange={(event) => setSlug(event.target.value)} placeholder="acme-ai" spellCheck={false} />
          </div>
        </div>
        {probe.isError ? <div className="form-error">{describeError(probe.error)}</div> : null}
        {outcome && outcome.state !== 'unreachable' && 'loginUrl' in outcome ? (
          <p className="form-note">Login URL: {outcome.loginUrl}</p>
        ) : null}
        {outcome ? <p className="form-note">{pill(outcome).note}</p> : null}
        <div className="form-actions">
          <span className="form-note">GET /auth/sso/{'{org_slug}'}/login · redirect never followed</span>
          <button type="submit" className="ghost-button" disabled={probe.isPending || !slug.trim()}>
            {probe.isPending ? 'Checking…' : 'Check SSO status'}
          </button>
        </div>
      </form>
    </article>
  )
}

// ---------------------------------------------------------------------------
// SCIM tokens
// ---------------------------------------------------------------------------

function ScimTokenRow({ token, mintedThisSession, onRevoked }: { token: ScimTokenMetadata; mintedThisSession: boolean; onRevoked: (id: string) => void }) {
  const revoke = useRevokeScimToken()
  const [error, setError] = useState<string | null>(null)

  const confirmRevoke = () => {
    if (!window.confirm(`Revoke SCIM token ${shortenId(token.id)}? Directory automation using it will stop working immediately.`)) return
    setError(null)
    revoke.mutate(token.id, {
      onSuccess: () => onRevoked(token.id),
      onError: (cause) => setError(describeError(cause)),
    })
  }

  return (
    <tr>
      <td title={token.id}>{shortenId(token.id)}</td>
      <td>{token.createdBy || '—'}</td>
      <td>{formatDateTime(token.createdAt)}</td>
      <td>{mintedThisSession ? 'this session' : 'by id'}</td>
      <td>
        <div className="table-actions">
          {error ? <span className="form-error inline">{error}</span> : null}
          <button type="button" className="danger-button small" disabled={revoke.isPending} onClick={confirmRevoke}>
            {revoke.isPending ? 'Revoking…' : 'Revoke'}
          </button>
        </div>
      </td>
    </tr>
  )
}

function ScimAdminCard({ canManage }: { canManage: boolean }) {
  const mint = useMintScimToken()
  const [revealed, setRevealed] = useState<MintedScimToken | null>(null)
  // Session-local mint history (the API has no list endpoint — see header).
  const [sessionTokens, setSessionTokens] = useState<ScimTokenMetadata[]>([])
  const [revokeId, setRevokeId] = useState('')
  const revokeById = useRevokeScimToken()
  const [revokeMessage, setRevokeMessage] = useState<string | null>(null)
  const [revokeError, setRevokeError] = useState(false)

  const confirmMint = () => {
    if (!window.confirm('Mint a new SCIM bearer credential? The scim_ secret is shown exactly once.')) return
    mint.mutate(undefined, {
      onSuccess: (minted) => {
        setRevealed(minted)
        setSessionTokens((prev) => [minted.token, ...prev])
      },
    })
  }

  const submitRevokeById = (event: FormEvent) => {
    event.preventDefault()
    const id = revokeId.trim()
    if (!id) return
    setRevokeMessage(null)
    revokeById.mutate(id, {
      onSuccess: () => {
        setRevokeMessage(`Token ${shortenId(id)} revoked (204).`)
        setRevokeError(false)
        setRevokeId('')
      },
      onError: (cause) => {
        setRevokeMessage(`${describeError(cause)} — a 404 means the id is unknown, foreign, or already revoked.`)
        setRevokeError(true)
      },
    })
  }

  return (
    <article className="panel">
      {revealed ? <TokenRevealDialog token={revealed} onClose={() => setRevealed(null)} /> : null}
      <div className="panel-header">
        <div>
          <p className="eyebrow">Directory provisioning</p>
          <h3>SCIM credentials</h3>
        </div>
        <span className="form-note">POST /scim/tokens · DELETE /scim/tokens/{'{id}'}</span>
      </div>

      <p className="detail-copy">
        SCIM 2.0 provisioning endpoints accept ONLY dedicated scim_ bearer credentials (never sessions or API keys),
        hashed at rest. Minting and revoking are owner-level actions.
      </p>

      {!canManage ? (
        <p className="detail-copy muted">Viewer/Member/Admin role — minting and revoking SCIM credentials needs the organization owner.</p>
      ) : (
        <div className="stack-gap">
          {mint.isError ? <div className="form-error">{describeError(mint.error)}</div> : null}
          <div className="form-actions">
            <span className="form-note">The plaintext secret appears exactly once after minting.</span>
            <button type="button" className="primary-button" disabled={mint.isPending} onClick={confirmMint}>
              {mint.isPending ? 'Minting…' : 'Mint SCIM token'}
            </button>
          </div>

          {revokeMessage ? <div className={revokeError ? 'form-error' : 'form-note'}>{revokeMessage}</div> : null}

          {sessionTokens.length === 0 ? (
            <EmptyState
              title="No tokens minted in this session"
              hint="The API exposes mint + revoke only — there is no list endpoint for issued tokens. Tokens minted in other sessions are revoked by id below."
            />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Token</th>
                    <th>Minted by</th>
                    <th>Created</th>
                    <th>Source</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {sessionTokens.map((token) => (
                    <ScimTokenRow
                      key={token.id}
                      token={token}
                      mintedThisSession
                      onRevoked={(id) => setSessionTokens((prev) => prev.filter((entry) => entry.id !== id))}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          )}

          <form className="filter-row" onSubmit={submitRevokeById}>
            <div className="field">
              <label htmlFor="scim-revoke-id">Revoke by token id</label>
              <input id="scim-revoke-id" value={revokeId} onChange={(event) => setRevokeId(event.target.value)} placeholder="scim token id (uuid)" spellCheck={false} />
            </div>
            <div className="filter-actions">
              <button type="submit" className="danger-button small" disabled={revokeById.isPending || !revokeId.trim()}>
                {revokeById.isPending ? 'Revoking…' : 'Revoke'}
              </button>
            </div>
          </form>
        </div>
      )}
    </article>
  )
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

export function SettingsView({ canManageIdentity, organizationName }: { canManageIdentity: boolean; organizationName: string }) {
  const orgLabel = useMemo(() => organizationName || 'your organization', [organizationName])
  return (
    <>
      <PageHeader eyebrow="Workspace" title="Settings" actions={<span className="form-note">signed in to {orgLabel}</span>} />

      <p className="form-note">Workspace-level administration. The API is the source of truth — every action here goes straight to it.</p>

      <section className="content-grid">
        <SsoStatusCard organizationName={organizationName} />
        <ScimAdminCard canManage={canManageIdentity} />
      </section>
    </>
  )
}
