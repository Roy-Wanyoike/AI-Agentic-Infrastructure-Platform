// Knowledge / RAG resource (wave-3 knowledge endpoints, cmd/api/knowledge.go):
//
// - POST /knowledge/documents -> 201 {"document":{…},"warning":"…"?}
//   (a warning means chunks were stored unembedded; search falls back to
//   lexical scoring for them — surfaced honestly in the UI)
// - GET  /knowledge/documents -> {"documents":[…]} (newest first)
// - POST /knowledge/search    -> {"results":[{document_id,document_title,
//   chunk_id,chunk_ordinal,content,score,citation}]}
//
// Permission fallback on the backend: these routes reuse agents.read /
// agents.write, so the view is gated with the established canWrite capability.
// Parsers are defensive: wrapped ({documents:[]} / {document:{}} / {results:[]})
// or bare payloads both normalize, and field names are matched case- and
// separator-insensitively via pickField (Go json tags are snake_case here).

import { apiFetch } from './client'
import { asNumber, asRecord, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type KnowledgeDocument = {
  id: string
  title: string
  source?: string
  metadata?: Record<string, unknown>
  chunkCount: number
  createdAt?: string
  updatedAt?: string
}

export type KnowledgeSearchResult = {
  documentId: string
  documentTitle?: string
  chunkId?: string
  chunkOrdinal?: number
  content: string
  score: number
  citation?: string
}

export type CreateKnowledgeDocumentInput = {
  title: string
  content: string
  source?: string
  metadata?: Record<string, unknown>
}

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

export function normalizeKnowledgeDocument(raw: unknown): KnowledgeDocument {
  const metadata = asRecord(pickField(raw, 'metadata'))
  return {
    id: asString(pickField(raw, 'id', 'documentId', 'document_id')) ?? '',
    title: asString(pickField(raw, 'title', 'name')) ?? 'Untitled document',
    source: asString(pickField(raw, 'source')),
    metadata: metadata ? { ...metadata } : undefined,
    chunkCount: asNumber(pickField(raw, 'chunkCount', 'chunk_count', 'chunks')) ?? 0,
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

export function normalizeKnowledgeSearchResult(raw: unknown): KnowledgeSearchResult {
  return {
    documentId: asString(pickField(raw, 'documentId', 'document_id')) ?? '',
    documentTitle: asString(pickField(raw, 'documentTitle', 'document_title')),
    chunkId: asString(pickField(raw, 'chunkId', 'chunk_id')),
    chunkOrdinal: asNumber(pickField(raw, 'chunkOrdinal', 'chunk_ordinal', 'ordinal')),
    content: asString(pickField(raw, 'content', 'text', 'snippet')) ?? '',
    score: asNumber(pickField(raw, 'score', 'similarity')) ?? 0,
    citation: asString(pickField(raw, 'citation', 'source')),
  }
}

/** Defensive list extraction: accepts {documents:[]} or a bare array. */
function extractList(raw: unknown, wrapper: string): unknown[] {
  const wrapped = pickField(raw, wrapper)
  if (Array.isArray(wrapped)) return wrapped
  if (Array.isArray(raw)) return raw
  return []
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

export async function listKnowledgeDocuments(): Promise<KnowledgeDocument[]> {
  const raw = await apiFetch<unknown>('/knowledge/documents')
  return extractList(raw, 'documents').map(normalizeKnowledgeDocument)
}

export async function createKnowledgeDocument(
  input: CreateKnowledgeDocumentInput,
): Promise<{ document: KnowledgeDocument; warning?: string }> {
  const raw = await apiFetch<unknown>('/knowledge/documents', {
    method: 'POST',
    body: JSON.stringify(input),
  })
  const document = normalizeKnowledgeDocument(pickField(raw, 'document') ?? raw)
  const warning = asString(pickField(raw, 'warning'))
  return warning ? { document, warning } : { document }
}

export async function searchKnowledge(query: string, k?: number): Promise<KnowledgeSearchResult[]> {
  const raw = await apiFetch<unknown>('/knowledge/search', {
    method: 'POST',
    body: JSON.stringify({ query, ...(k ? { k } : {}) }),
  })
  return extractList(raw, 'results')
    .map(normalizeKnowledgeSearchResult)
    .sort((a, b) => b.score - a.score)
}
