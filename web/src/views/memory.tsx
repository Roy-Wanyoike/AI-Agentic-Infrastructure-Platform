// Memory view (real): org/agent memory snippets.
//
// Wired endpoints (no mocks — cmd/api/memory.go):
// - GET /memory?agent_id= -> {"snippets":[…]}  (agents.read fallback)
// - PUT /memory           -> {"snippets":[…]}  (agents.write fallback)
//
// Backend reality (checked in the handler):
// - Snippets have no "key": they carry scope (short_term|long_term), content,
//   importance [0..1] and an optional RFC3339 expires_at.
// - There is NO single-snippet DELETE endpoint. PUT /memory atomically
//   REPLACES the snippet set of one (organization, agent) scope, so both
//   "add" and "remove" are set rewrites over the loaded list for that scope.
//   The UI says so instead of pretending a per-row delete API exists.
// - GET never lists expired snippets, so expired rows simply disappear.

import { useMemo, useState, type FormEvent } from 'react'
import { useAgents, useMemorySnippets, usePutMemory } from '../lib/hooks'
import { groupSnippetsByScope, type MemoryScope, type MemorySnippet } from '../lib/api/memory'
import { formatNumber, formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

const MEMORY_SCOPES: ReadonlyArray<{ value: MemoryScope; label: string }> = [
  { value: 'long_term', label: 'long_term (default)' },
  { value: 'short_term', label: 'short_term' },
]

function preview(content: string, max = 140): string {
  const collapsed = content.replace(/\s+/g, ' ').trim()
  return collapsed.length > max ? `${collapsed.slice(0, max - 1)}…` : collapsed
}

function clampImportance(value: number): number {
  if (!Number.isFinite(value)) return 0
  return Math.min(Math.max(value, 0), 1)
}

/**
 * Picks the snippets that must survive a PUT for one agent scope (everything
 * for that agent, minus `without` when removing). Callers append the new
 * snippet for adds, or send only the survivors for removals — either way the
 * PUT rewrites the scope's whole set.
 */
function composeSet(
  snippets: MemorySnippet[],
  agentId: string,
  options: { without?: MemorySnippet } = {},
): MemorySnippet[] {
  const survivors = snippets.filter(
    (snippet) => (snippet.agentId || '') === agentId && snippet.id !== options.without?.id,
  )
  return survivors
}

function UpsertSnippetForm() {
  const putMemory = usePutMemory()
  const agentsQuery = useAgents()
  const agents = agentsQuery.data ?? []
  const [agentId, setAgentId] = useState('')
  const [scope, setScope] = useState<MemoryScope>('long_term')
  const [content, setContent] = useState('')
  const [importance, setImportance] = useState('0.5')
  const [expiresAt, setExpiresAt] = useState('')
  const [message, setMessage] = useState<string | null>(null)

  // The form always composes from an ORG-WIDE list (independent of the
  // view's agent filter) so a PUT can re-send everything that must survive
  // for the selected scope — otherwise snippets outside the active filter
  // could be silently dropped by the set replacement.
  const allSnippetsQuery = useMemorySnippets(null)
  const allSnippets = useMemo(() => allSnippetsQuery.data ?? [], [allSnippetsQuery.data])

  const scopeLabel = agentId === '' ? 'org-level shared memory' : shortenId(agentId)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (putMemory.isPending || !content.trim()) return
    setMessage(null)
    const parsedImportance = clampImportance(Number(importance))
    const expires = expiresAt ? new Date(expiresAt) : null
    putMemory.mutate(
      {
        agentId,
        snippets: [
          // Set replacement: everything that must survive for this scope…
          ...composeSet(allSnippets, agentId).map((snippet) => ({
            scope: snippet.scope,
            content: snippet.content,
            importance: snippet.importance,
            expiresAt: snippet.expiresAt,
          })),
          // …plus the new snippet.
          {
            scope,
            content: content.trim(),
            importance: parsedImportance,
            ...(expires && !Number.isNaN(expires.getTime()) ? { expiresAt: expires.toISOString() } : {}),
          },
        ],
      },
      {
        onSuccess: (stored) => {
          setMessage(`Stored ${formatNumber(stored.length)} snippet(s) for ${scopeLabel}.`)
          setContent('')
          setImportance('0.5')
          setExpiresAt('')
        },
        onError: (error) => setMessage(describeError(error)),
      },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Write</p>
          <h3>Put snippets</h3>
        </div>
        <span className="form-note">PUT /memory</span>
      </div>

      {message ? <div className={putMemory.isError ? 'form-error' : 'form-note'}>{message}</div> : null}

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="memory-agent">Scope owner</label>
            <select id="memory-agent" value={agentId} onChange={(event) => setAgentId(event.target.value)}>
              <option value="">Org-level shared memory</option>
              {agents.map((agent) => (
                <option key={agent.id} value={agent.id}>
                  Agent: {agent.name}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="memory-scope">Scope</label>
            <select
              id="memory-scope"
              value={scope}
              onChange={(event) => setScope(event.target.value as MemoryScope)}
            >
              {MEMORY_SCOPES.map((candidate) => (
                <option key={candidate.value} value={candidate.value}>
                  {candidate.label}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="memory-importance">Importance (0–1)</label>
            <input
              id="memory-importance"
              type="number"
              min="0"
              max="1"
              step="0.05"
              value={importance}
              onChange={(event) => setImportance(event.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="memory-expires">Expires at (optional)</label>
            <input
              id="memory-expires"
              type="datetime-local"
              value={expiresAt}
              onChange={(event) => setExpiresAt(event.target.value)}
            />
          </div>
          <div className="field span-2">
            <label htmlFor="memory-content">Content</label>
            <textarea
              id="memory-content"
              rows={3}
              value={content}
              onChange={(event) => setContent(event.target.value)}
              placeholder="User prefers concise answers with citations…"
              required
            />
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">
            PUT replaces the whole snippet set of the selected (org, agent) scope — existing snippets for that scope are
            re-sent so nothing is silently dropped.
          </span>
          <button type="submit" className="primary-button" disabled={putMemory.isPending}>
            {putMemory.isPending ? 'Storing…' : 'Store snippet'}
          </button>
        </div>
      </form>
    </article>
  )
}

function RemoveSnippetButton({ snippet, snippets }: { snippet: MemorySnippet; snippets: MemorySnippet[] }) {
  const putMemory = usePutMemory()
  const [confirming, setConfirming] = useState(false)

  if (putMemory.isPending && confirming) {
    return <span className="form-note">Removing…</span>
  }

  if (!confirming) {
    return (
      <button type="button" className="ghost-button small" onClick={() => setConfirming(true)}>
        Remove
      </button>
    )
  }

  const remove = () => {
    // No DELETE endpoint: removal = PUT the scope's set without this snippet.
    putMemory.mutate(
      {
        agentId: snippet.agentId || '',
        snippets: composeSet(snippets, snippet.agentId || '', { without: snippet }).map((keep) => ({
          scope: keep.scope,
          content: keep.content,
          importance: keep.importance,
          expiresAt: keep.expiresAt,
        })),
      },
      {
        onSettled: () => setConfirming(false),
      },
    )
  }

  return (
    <div className="table-actions">
      {putMemory.isError ? <span className="form-error inline">{describeError(putMemory.error)}</span> : null}
      <button type="button" className="ghost-button small" onClick={() => setConfirming(false)}>
        Keep
      </button>
      <button type="button" className="primary-button small" onClick={remove}>
        Confirm removal
      </button>
    </div>
  )
}

export function MemoryView({ canWrite }: { canWrite: boolean }) {
  const [agentFilter, setAgentFilter] = useState<string>('')
  const snippetsQuery = useMemorySnippets(agentFilter || null)
  const agentsQuery = useAgents()
  const agents = agentsQuery.data ?? []
  const snippets = useMemo(() => snippetsQuery.data ?? [], [snippetsQuery.data])

  const longTerm = snippets.filter((snippet) => snippet.scope === 'long_term').length
  const shortTerm = snippets.filter((snippet) => snippet.scope === 'short_term').length
  const scopedAgents = useMemo(() => groupSnippetsByScope(snippets).size, [snippets])

  const agentName = (id: string) => agents.find((agent) => agent.id === id)?.name ?? shortenId(id)

  return (
    <>
      <PageHeader
        eyebrow="Agent memory"
        title="Memory"
        actions={
          canWrite ? undefined : (
            <span className="form-note">Viewer role — writing memory needs MEMBER and above</span>
          )
        }
      />

      {snippetsQuery.isError ? <ErrorBanner error={snippetsQuery.error} onRetry={() => void snippetsQuery.refetch()} /> : null}

      <section className="summary-grid">
        {snippetsQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Visible snippets" value={formatNumber(snippets.length)} />
            <SummaryStat label="Long term" value={formatNumber(longTerm)} accent="info" />
            <SummaryStat label="Short term" value={formatNumber(shortTerm)} accent="warning" />
            <SummaryStat label="Scopes in use" value={formatNumber(scopedAgents)} accent="success" />
          </>
        )}
      </section>

      {canWrite ? <UpsertSnippetForm /> : null}

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Stored memory</p>
            <h3>Snippets</h3>
          </div>
          <div className="field">
            <label htmlFor="memory-filter">Agent filter</label>
            <select
              id="memory-filter"
              value={agentFilter}
              onChange={(event) => setAgentFilter(event.target.value)}
            >
              <option value="">All (org + agents)</option>
              {agents.map((agent) => (
                <option key={agent.id} value={agent.id}>
                  Agent: {agent.name}
                </option>
              ))}
            </select>
          </div>
        </div>

        {!canWrite ? (
          <p className="form-note">
            There is no single-snippet DELETE endpoint — removal rewrites the scope's snippet set via PUT /memory, so it
            needs write access (MEMBER and above).
          </p>
        ) : (
          <p className="form-note">GET /memory — expired snippets are hidden by the backend; removal rewrites the scope's set via PUT.</p>
        )}

        {snippetsQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : snippets.length === 0 ? (
          <EmptyState
            title="No memory snippets stored"
            hint={
              agentFilter
                ? `No visible snippets for ${agentName(agentFilter)}. Agents or operators can put snippets with PUT /memory.`
                : 'Nothing stored yet for this organization. Put a snippet below, or let agents persist facts during runs.'
            }
          />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Scope owner</th>
                  <th>Scope</th>
                  <th>Content</th>
                  <th>Importance</th>
                  <th>Expires</th>
                  <th>Updated</th>
                  {canWrite ? <th>Actions</th> : null}
                </tr>
              </thead>
              <tbody>
                {snippets.map((snippet) => (
                  <tr key={snippet.id || `${snippet.agentId}-${snippet.updatedAt}-${preview(snippet.content, 24)}`}>
                    <td>
                      <strong>{snippet.agentId ? agentName(snippet.agentId) : 'Org-level'}</strong>
                    </td>
                    <td>
                      <code>{snippet.scope}</code>
                    </td>
                    <td>
                      <span title={snippet.content}>{preview(snippet.content)}</span>
                    </td>
                    <td>{snippet.importance.toFixed(2)}</td>
                    <td>{snippet.expiresAt ? formatRelativeTime(snippet.expiresAt) : 'never'}</td>
                    <td>{snippet.updatedAt ? formatRelativeTime(snippet.updatedAt) : '—'}</td>
                    {canWrite ? (
                      <td>
                        <RemoveSnippetButton snippet={snippet} snippets={snippets} />
                      </td>
                    ) : null}
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
