// Marketplace view (issue #53; issue-#28 contract, cmd/api/marketplace.go):
//
// - GET  /marketplace/listings             browse the GLOBAL catalog (agents.read — any role)
// - POST /marketplace/listings/{slug}/install  create an agent in YOUR org from the snapshot
//                                              (agents.write — OWNER/ADMIN)
// - POST /marketplace/listings             publish one of YOUR agents (agents.write — OWNER/ADMIN)
//
// Browse supports the backend's real query surface: q (name/description text
// match), tags (ANY overlap) and keyset pagination via next_cursor. Install
// success surfaces the freshly created agent with a link to the Agents view.
// No listing data is ever invented — an empty catalog means the API returned
// zero published listings.

import { useMemo, useState, type FormEvent } from 'react'
import { useAgents, useInstallListing, useMarketplaceListings, usePublishListing } from '../lib/hooks'
import { MARKETPLACE_SLUG_PATTERN, type MarketplaceListing } from '../lib/api/marketplace'
import { formatNumber, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill } from './shared'
import { describeError } from './uiHelpers'

function InstallButton({ listing, canInstall, onInstalled }: { listing: MarketplaceListing; canInstall: boolean; onInstalled: (agentName: string) => void }) {
  const install = useInstallListing()
  const [error, setError] = useState<string | null>(null)

  if (!canInstall) return <span className="form-note">install needs OWNER/ADMIN</span>

  return (
    <div className="table-actions">
      {error ? <span className="form-error inline">{error}</span> : null}
      <button
        type="button"
        className="primary-button small"
        disabled={install.isPending}
        onClick={() => {
          setError(null)
          install.mutate(listing.slug, {
            onSuccess: (result) => onInstalled(result.agent.name || listing.name),
            onError: (cause) => setError(describeError(cause)),
          })
        }}
      >
        {install.isPending ? 'Installing…' : 'Install'}
      </button>
    </div>
  )
}

function PublishListingForm({ onPublished }: { onPublished: (slug: string) => void }) {
  const publish = usePublishListing()
  const agentsQuery = useAgents()
  const agents = agentsQuery.data ?? []
  const [agentId, setAgentId] = useState('')
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [description, setDescription] = useState('')
  const [tags, setTags] = useState('')
  const [version, setVersion] = useState('')
  const [status, setStatus] = useState('published')
  const [message, setMessage] = useState<string | null>(null)

  const effectiveAgent = agentId || agents[0]?.id || ''

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (publish.isPending || !effectiveAgent) return
    setMessage(null)
    const trimmedSlug = slug.trim()
    if (trimmedSlug && !MARKETPLACE_SLUG_PATTERN.test(trimmedSlug)) {
      setMessage('Slug must be lowercase letters, digits and dashes (^[a-z0-9]([a-z0-9-]*[a-z0-9])?$). Leave it empty to derive it from the name.')
      return
    }
    const parsedVersion = version.trim() === '' ? 0 : Number(version)
    if (!Number.isInteger(parsedVersion) || parsedVersion < 0) {
      setMessage('Version must be a non-negative integer (leave empty to snapshot the agent’s live configuration).')
      return
    }
    publish.mutate(
      {
        agent_id: effectiveAgent,
        version: parsedVersion,
        name: name.trim(),
        slug: trimmedSlug,
        description: description.trim(),
        tags: tags
          .split(',')
          .map((tag) => tag.trim())
          .filter((tag) => tag !== ''),
        status,
      },
      {
        onSuccess: (listing) => {
          setName('')
          setSlug('')
          setDescription('')
          setTags('')
          setVersion('')
          setMessage(`Listing published: ${listing.name} (${listing.slug}).`)
          onPublished(listing.slug)
        },
        onError: (error) => setMessage(describeError(error)),
      },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Share an agent</p>
          <h3>Publish to the marketplace</h3>
        </div>
        <span className="form-note">POST /marketplace/listings</span>
      </div>

      {message ? <div className={publish.isError ? 'form-error' : 'form-note'}>{message}</div> : null}

      {agents.length === 0 ? (
        <EmptyState title="No agents to publish" hint="Publishing snapshots one of your organization's agents — create an agent first." />
      ) : (
        <form onSubmit={submit}>
          <div className="form-grid">
            <div className="field">
              <label htmlFor="listing-agent">Agent</label>
              <select id="listing-agent" value={effectiveAgent} onChange={(event) => setAgentId(event.target.value)} required>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name}
                  </option>
                ))}
              </select>
            </div>
            <div className="field">
              <label htmlFor="listing-name">Listing name</label>
              <input id="listing-name" value={name} onChange={(event) => setName(event.target.value)} placeholder="Support triage copilot" required />
            </div>
            <div className="field">
              <label htmlFor="listing-slug">Slug (optional)</label>
              <input id="listing-slug" value={slug} onChange={(event) => setSlug(event.target.value)} placeholder="derived from the name when empty" spellCheck={false} />
            </div>
            <div className="field">
              <label htmlFor="listing-version">Version (optional)</label>
              <input
                id="listing-version"
                type="number"
                min="0"
                value={version}
                onChange={(event) => setVersion(event.target.value)}
                placeholder="0 = live configuration"
              />
            </div>
            <div className="field">
              <label htmlFor="listing-status">Visibility</label>
              <select id="listing-status" value={status} onChange={(event) => setStatus(event.target.value)}>
                <option value="published">published</option>
                <option value="draft">draft</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="listing-tags">Tags (comma separated, max 10)</label>
              <input id="listing-tags" value={tags} onChange={(event) => setTags(event.target.value)} placeholder="support, triage" />
            </div>
            <div className="field span-2">
              <label htmlFor="listing-description">Description</label>
              <textarea
                id="listing-description"
                rows={3}
                value={description}
                onChange={(event) => setDescription(event.target.value)}
                placeholder="What does this agent do for the org that installs it?"
              />
            </div>
          </div>
          <div className="form-actions">
            <span className="form-note">The snapshot carries configuration only (name/description/instructions/model/status) — never secrets.</span>
            <button type="submit" className="primary-button" disabled={publish.isPending}>
              {publish.isPending ? 'Publishing…' : 'Publish listing'}
            </button>
          </div>
        </form>
      )}
    </article>
  )
}

export function MarketplaceView({ canPublish, onNavigate }: { canPublish: boolean; onNavigate: (view: 'Agents') => void }) {
  const [showPublish, setShowPublish] = useState(false)
  const [queryInput, setQueryInput] = useState('')
  const [tagsInput, setTagsInput] = useState('')
  const [appliedQuery, setAppliedQuery] = useState('')
  const [appliedTags, setAppliedTags] = useState<string[]>([])
  const [cursor, setCursor] = useState('')
  const [installedAgent, setInstalledAgent] = useState<string | null>(null)

  const listingsQuery = useMarketplaceListings({ query: appliedQuery, tags: appliedTags, cursor: cursor || undefined })
  const listings = useMemo(() => listingsQuery.data?.listings ?? [], [listingsQuery.data])
  const nextCursor = listingsQuery.data?.nextCursor ?? ''

  const search = (event: FormEvent) => {
    event.preventDefault()
    setAppliedQuery(queryInput.trim())
    setAppliedTags(
      tagsInput
        .split(',')
        .map((tag) => tag.trim())
        .filter((tag) => tag !== ''),
    )
    setCursor('')
    setInstalledAgent(null)
  }

  const filtering = appliedQuery !== '' || appliedTags.length > 0

  return (
    <>
      <PageHeader
        eyebrow="Catalog"
        title="Marketplace"
        actions={
          canPublish ? (
            <button type="button" className="primary-button" onClick={() => setShowPublish((open) => !open)}>
              Publish agent
            </button>
          ) : (
            <span className="form-note">Browsing is open to every role — publishing/installing needs OWNER or ADMIN</span>
          )
        }
      />

      {installedAgent ? (
        <div className="secret-banner" role="status">
          <div>
            <strong>Agent installed</strong>
            <span>
              “{installedAgent}” was created in your organization from the listing snapshot (POST /marketplace/listings/
              {'{slug}'}/install). It is ready to run.
            </span>
          </div>
          <div className="topbar-actions">
            <button type="button" className="primary-button small" onClick={() => onNavigate('Agents')}>
              Go to Agents
            </button>
            <button type="button" className="ghost-button small" onClick={() => setInstalledAgent(null)}>
              Dismiss
            </button>
          </div>
        </div>
      ) : null}

      {showPublish && canPublish ? <PublishListingForm onPublished={() => setShowPublish(false)} /> : null}

      {listingsQuery.isError ? <ErrorBanner error={listingsQuery.error} onRetry={() => void listingsQuery.refetch()} /> : null}

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Browse</p>
            <h3>Published listings</h3>
          </div>
          <span className="form-note">GET /marketplace/listings?q=…&amp;tags=…&amp;cursor=…</span>
        </div>

        <form onSubmit={search} className="listing-search">
          <div className="field">
            <label htmlFor="listing-search-q">Search</label>
            <input
              id="listing-search-q"
              value={queryInput}
              onChange={(event) => setQueryInput(event.target.value)}
              placeholder="Name or description text…"
            />
          </div>
          <div className="field">
            <label htmlFor="listing-search-tags">Tags</label>
            <input id="listing-search-tags" value={tagsInput} onChange={(event) => setTagsInput(event.target.value)} placeholder="support, rag" />
          </div>
          <button type="submit" className="ghost-button">
            Search
          </button>
        </form>

        {listingsQuery.isPending ? (
          <div className="agent-grid">
            {[0, 1, 2].map((index) => (
              <article key={index} className="agent-card panel">
                <Skeleton height={18} width="60%" />
                <Skeleton height={40} style={{ marginTop: 12 }} />
              </article>
            ))}
          </div>
        ) : listings.length === 0 ? (
          <EmptyState
            title={filtering ? 'No listings match your search' : 'No listings yet — publish one of your agents'}
            hint={
              filtering
                ? 'Try a different query or clear the tag filter — the catalog only contains published listings.'
                : 'The catalog is global: anything an organization publishes shows up here, and installing creates a ready-to-run agent in your org.'
            }
            action={
              canPublish && !filtering ? (
                <button type="button" className="primary-button" onClick={() => setShowPublish(true)}>
                  Publish an agent
                </button>
              ) : undefined
            }
          />
        ) : (
          <>
            <div className="agent-grid">
              {listings.map((listing) => (
                <article key={listing.id} className="agent-card panel">
                  <div className="agent-card-header">
                    <div>
                      <h3>{listing.name}</h3>
                      <p className="form-note">
                        {listing.slug} · by org {shortenId(listing.publisherOrgId)}
                      </p>
                    </div>
                    <StatusPill status={listing.status} />
                  </div>
                  <p>{listing.description || 'No description provided.'}</p>
                  {listing.tags.length > 0 ? (
                    <div className="listing-meta">
                      {listing.tags.map((tag) => (
                        <code key={tag} className="event-chip">
                          {tag}
                        </code>
                      ))}
                    </div>
                  ) : null}
                  <div className="listing-meta">
                    <span className="listing-installs">
                      {formatNumber(listing.downloadCount)} {listing.downloadCount === 1 ? 'install' : 'installs'}
                    </span>
                    <InstallButton
                      listing={listing}
                      canInstall={canPublish}
                      onInstalled={(agentName) => setInstalledAgent(agentName)}
                    />
                  </div>
                </article>
              ))}
            </div>
            {nextCursor ? (
              <div className="form-actions">
                <span className="form-note">More listings are available (keyset pagination).</span>
                <button type="button" className="ghost-button" onClick={() => setCursor(nextCursor)}>
                  Load next page
                </button>
              </div>
            ) : null}
          </>
        )}
      </article>
    </>
  )
}
