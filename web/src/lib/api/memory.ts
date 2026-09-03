// Memory resource (wave-3 memory endpoints, cmd/api/memory.go):
//
// - GET /memory?agent_id= -> {"snippets":[…]} (visible, non-expired; newest
//   first). agent_id is optional: omitted/empty lists the whole organization
//   (org-level shared snippets + every agent's snippets).
// - PUT /memory           -> {"snippets":[…]} — ATOMICALLY REPLACES the snippet
//   set of one (organization, agent) scope. Body:
//   {"agent_id":"…","snippets":[{"scope","content","importance","expires_at"}]}
//
// Snippets have NO dedicated DELETE endpoint: removal is expressed through the
// PUT set-replacement semantics (send the scope's remaining snippets). The
// view layer composes add/remove from the loaded set for that reason and says
// so in its copy — nothing is invented here.
//
// Valid scopes mirror internal/memory: "short_term" | "long_term" (empty is
// normalized to long_term server-side; importance is clamped to [0,1]).
// expires_at must be RFC3339 when present; snippets past it stop being listed.
//
// Permission fallback on the backend: these routes reuse agents.read /
// agents.write, so the view is gated with the established canWrite capability.
// Parsers are defensive: wrapped ({snippets:[]}) or bare array payloads both
// normalize, and field names are matched case- and separator-insensitively via
// pickField (Go json tags are snake_case here).

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type MemoryScope = 'short_term' | 'long_term'

export type MemorySnippet = {
  id: string
  agentId: string
  scope: MemoryScope | string
  content: string
  importance: number
  expiresAt?: string
  createdAt?: string
  updatedAt?: string
}

export type MemorySnippetInput = {
  scope?: MemoryScope | string
  content: string
  importance?: number
  expiresAt?: string
}

export type PutMemoryInput = {
  agentId?: string
  snippets: MemorySnippetInput[]
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

export function normalizeMemorySnippet(raw: unknown): MemorySnippet {
  return {
    id: asString(pickField(raw, 'id', 'snippetId', 'snippet_id')) ?? '',
    agentId: asString(pickField(raw, 'agentId', 'agent_id')) ?? '',
    scope: asString(pickField(raw, 'scope')) ?? 'long_term',
    content: asString(pickField(raw, 'content', 'text', 'value')) ?? '',
    importance: asNumber(pickField(raw, 'importance')) ?? 0,
    expiresAt: asString(pickField(raw, 'expiresAt', 'expires_at')),
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

/** Defensive list extraction: accepts {snippets:[]} or a bare array. */
function extractSnippets(raw: unknown): unknown[] {
  const wrapped = pickField(raw, 'snippets', 'items')
  if (Array.isArray(wrapped)) return wrapped
  if (Array.isArray(raw)) return raw
  return []
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

export async function listMemorySnippets(agentId?: string): Promise<MemorySnippet[]> {
  const query = agentId ? `?agent_id=${encodeURIComponent(agentId)}` : ''
  const raw = await apiFetch<unknown>(`/memory${query}`)
  return extractSnippets(raw).map(normalizeMemorySnippet)
}

/**
 * PUT /memory — replaces the whole snippet set of one (organization, agent)
 * scope and returns the stored snippets. The view layer is responsible for
 * including the snippets that must survive (add/remove are set rewrites).
 */
export async function putMemorySnippets(input: PutMemoryInput): Promise<MemorySnippet[]> {
  const raw = await apiFetch<unknown>('/memory', {
    method: 'PUT',
    body: JSON.stringify({
      agent_id: input.agentId ?? '',
      snippets: input.snippets.map((snippet) => ({
        scope: snippet.scope || 'long_term',
        content: snippet.content,
        ...(snippet.importance !== undefined ? { importance: snippet.importance } : {}),
        ...(snippet.expiresAt ? { expires_at: snippet.expiresAt } : {}),
      })),
    }),
  })
  return extractSnippets(raw).map(normalizeMemorySnippet)
}

/**
 * Groups snippets by the agent scope they belong to ("org-level" for empty
 * agent ids) so the view can rewrite one scope's set without touching others.
 */
export function groupSnippetsByScope(snippets: MemorySnippet[]): Map<string, MemorySnippet[]> {
  const groups = new Map<string, MemorySnippet[]>()
  for (const snippet of snippets) {
    const key = snippet.agentId || ''
    const bucket = groups.get(key)
    if (bucket) bucket.push(snippet)
    else groups.set(key, [snippet])
  }
  return groups
}
