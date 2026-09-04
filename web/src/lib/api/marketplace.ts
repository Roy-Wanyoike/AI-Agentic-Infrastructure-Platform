// Marketplace resource (issue #28, cmd/api/marketplace.go):
//
//   GET    /marketplace/listings             -> {"listings":[…],"next_cursor":…}  browse (agents.read — any role)
//   POST   /marketplace/listings             -> {"listing":{…}}                    publish (agents.write — OWNER/ADMIN)
//   GET    /marketplace/listings/{slug}      -> {"listing":{…}}                    get (agents.read — any role)
//   POST   /marketplace/listings/{slug}/install -> {"listing":{…},"agent":{…}}     install (agents.write — OWNER/ADMIN)
//   DELETE /marketplace/listings/{slug}      -> {"deleted":true}                   unlist (agents.write — OWNER/ADMIN)
//
// Browse is a GLOBAL read (the published catalog for every organization);
// publish/install are org-scoped writes derived from the auth claims. Browse
// is keyset-paginated: pass `cursor` from the previous response's next_cursor.

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'
import { normalizeAgent, type Agent } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type MarketplaceListing = {
  id: string
  publisherOrgId: string
  publisherUserId: string
  sourceAgentId: string
  name: string
  slug: string
  description: string
  tags: string[]
  status: string
  downloadCount: number
  /** Decoded version_snapshot (agent config only — never secrets). */
  versionSnapshot: Record<string, unknown> | null
  createdAt?: string
  updatedAt?: string
}

export type BrowseListingsInput = {
  query?: string
  tags?: string[]
  limit?: number
  cursor?: string
}

export type BrowseListingsResult = {
  listings: MarketplaceListing[]
  nextCursor: string
}

export type InstallListingResult = {
  listing: MarketplaceListing
  agent: Agent
}

export type PublishListingInput = {
  agent_id: string
  /** >0 publishes the immutable config version with that number; 0/omitted snapshots the live config. */
  version?: number
  name: string
  slug?: string
  description?: string
  tags?: string[]
  /** "published" (default) or "draft". */
  status?: string
}

/** Backend slug regex (internal/marketplace/service.go), for form pre-validation. */
export const MARKETPLACE_SLUG_PATTERN = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeListing(raw: unknown): MarketplaceListing {
  const snapshot = pickField(raw, 'versionSnapshot', 'version_snapshot')
  const tags = pickField(raw, 'tags')
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    publisherOrgId: asString(pickField(raw, 'publisherOrgId', 'publisher_org_id')) ?? '',
    publisherUserId: asString(pickField(raw, 'publisherUserId', 'publisher_user_id')) ?? '',
    sourceAgentId: asString(pickField(raw, 'sourceAgentId', 'source_agent_id')) ?? '',
    name: asString(pickField(raw, 'name')) ?? 'Unnamed listing',
    slug: asString(pickField(raw, 'slug')) ?? '',
    description: asString(pickField(raw, 'description')) ?? '',
    tags: Array.isArray(tags) ? tags.map((tag) => asString(tag) ?? '').filter((tag) => tag !== '') : [],
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    downloadCount: asNumber(pickField(raw, 'downloadCount', 'download_count')) ?? 0,
    versionSnapshot: snapshot && typeof snapshot === 'object' && !Array.isArray(snapshot)
      ? { ...(snapshot as Record<string, unknown>) }
      : null,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

export async function browseListings(input: BrowseListingsInput = {}): Promise<BrowseListingsResult> {
  const params = new URLSearchParams()
  if (input.query) params.set('q', input.query)
  if (input.tags && input.tags.length > 0) params.set('tags', input.tags.join(','))
  if (input.limit && input.limit > 0) params.set('limit', String(input.limit))
  if (input.cursor) params.set('cursor', input.cursor)
  const query = params.toString()
  const raw = await apiFetch<unknown>(`/marketplace/listings${query ? `?${query}` : ''}`)
  const list = pickField(raw, 'listings')
  return {
    listings: (Array.isArray(list) ? list : []).map(normalizeListing),
    nextCursor: asString(pickField(raw, 'nextCursor', 'next_cursor')) ?? '',
  }
}

export async function installListing(slug: string): Promise<InstallListingResult> {
  const raw = await apiFetch<unknown>(`/marketplace/listings/${encodeURIComponent(slug)}/install`, {
    method: 'POST',
  })
  return {
    listing: normalizeListing(pickField(raw, 'listing') ?? raw),
    agent: normalizeAgent(pickField(raw, 'agent')),
  }
}

export async function publishListing(input: PublishListingInput): Promise<MarketplaceListing> {
  const raw = await apiFetch<unknown>('/marketplace/listings', {
    method: 'POST',
    body: JSON.stringify({
      agent_id: input.agent_id,
      version: input.version ?? 0,
      name: input.name,
      slug: input.slug ?? '',
      description: input.description ?? '',
      tags: input.tags ?? [],
      status: input.status ?? 'published',
    }),
  })
  return normalizeListing(pickField(raw, 'listing') ?? raw)
}
