// Knowledge view (real): document list + ingestion + semantic search.
//
// Wired endpoints (no mocks — cmd/api/knowledge.go):
// - GET  /knowledge/documents -> {"documents":[…]}          (agents.read fallback)
// - POST /knowledge/documents -> {"document":{…},"warning"?} (agents.write fallback)
// - POST /knowledge/search    -> {"results":[…]}             (agents.read fallback)
//
// A 201 with "warning" means the embedder failed and chunks were stored
// unembedded — search falls back to lexical scoring for them. That warning is
// surfaced verbatim instead of being hidden.

import { useMemo, useState, type FormEvent } from 'react'
import {
  useCreateKnowledgeDocument,
  useKnowledgeDocuments,
  useSearchKnowledge,
} from '../lib/hooks'
import type { KnowledgeSearchResult } from '../lib/api/knowledge'
import { formatNumber, formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

function CreateDocumentForm({ onCreated }: { onCreated: () => void }) {
  const createDocument = useCreateKnowledgeDocument()
  const [title, setTitle] = useState('')
  const [content, setContent] = useState('')
  const [source, setSource] = useState('')
  const [message, setMessage] = useState<string | null>(null)
  const [warning, setWarning] = useState<string | null>(null)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (createDocument.isPending) return
    setMessage(null)
    setWarning(null)
    createDocument.mutate(
      { title: title.trim(), content, source: source.trim() || undefined },
      {
        onSuccess: ({ document, warning: embedWarning }) => {
          setMessage(`Ingested "${document.title}" (${formatNumber(document.chunkCount)} chunks).`)
          // Non-fatal embedder failure: chunks stored unembedded, search uses
          // lexical scoring for them. Shown honestly, never swallowed.
          setWarning(embedWarning ?? null)
          setTitle('')
          setContent('')
          setSource('')
          onCreated()
        },
        onError: (error) => setMessage(describeError(error)),
      },
    )
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Ingest</p>
          <h3>Add document</h3>
        </div>
        <span className="form-note">POST /knowledge/documents</span>
      </div>

      {message ? <div className={createDocument.isError ? 'form-error' : 'form-note'}>{message}</div> : null}
      {warning ? (
        <div className="form-note" role="note">
          Embedding warning: {warning}
        </div>
      ) : null}

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="knowledge-title">Title</label>
            <input
              id="knowledge-title"
              value={title}
              onChange={(event) => setTitle(event.target.value)}
              placeholder="Runbook: queue backlog triage"
              required
            />
          </div>
          <div className="field">
            <label htmlFor="knowledge-source">Source (optional)</label>
            <input
              id="knowledge-source"
              value={source}
              onChange={(event) => setSource(event.target.value)}
              placeholder="https://… or handbook path"
            />
          </div>
          <div className="field span-2">
            <label htmlFor="knowledge-content">Content</label>
            <textarea
              id="knowledge-content"
              rows={5}
              value={content}
              onChange={(event) => setContent(event.target.value)}
              placeholder="Paste the text to chunk (~800 chars/chunk, 15% overlap), embed and store…"
              required
            />
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">Chunks are embedded and stored per organization.</span>
          <button type="submit" className="primary-button" disabled={createDocument.isPending}>
            {createDocument.isPending ? 'Ingesting…' : 'Ingest document'}
          </button>
        </div>
      </form>
    </article>
  )
}

function SearchPanel() {
  const search = useSearchKnowledge()
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<KnowledgeSearchResult[] | null>(null)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (search.isPending || !query.trim()) return
    search.mutate(
      { query: query.trim() },
      {
        onSuccess: (found) => setResults(found),
        onError: () => setResults(null),
      },
    )
  }

  return (
    <article className="panel wide">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Retrieval</p>
          <h3>Search knowledge</h3>
        </div>
        <span className="form-note">POST /knowledge/search</span>
      </div>

      <form onSubmit={submit}>
        <div className="form-grid">
          <div className="field span-2">
            <label htmlFor="knowledge-query">Query</label>
            <input
              id="knowledge-query"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="How do I clear a stuck queue backlog?"
            />
          </div>
        </div>
        <div className="form-actions">
          <span className="form-note">Top-k chunks scored by cosine similarity (lexical fallback for unembedded chunks).</span>
          <button type="submit" className="primary-button" disabled={search.isPending || !query.trim()}>
            {search.isPending ? 'Searching…' : 'Search'}
          </button>
        </div>
      </form>

      {search.isError ? <ErrorBanner error={search.error} onRetry={() => search.reset()} /> : null}

      {results !== null ? (
        results.length === 0 ? (
          <EmptyState
            title="No matching chunks"
            hint="Nothing in the organization's knowledge base scored against that query. Ingest more documents or rephrase."
          />
        ) : (
          <div className="stack-gap">
            {results.map((result, index) => (
              <article key={`${result.chunkId || result.documentId}-${index}`} className="knowledge-hit">
                <div className="knowledge-hit-header">
                  <div>
                    <strong>{result.documentTitle || shortenId(result.documentId)}</strong>
                    {result.citation ? <span className="form-note"> · {result.citation}</span> : null}
                  </div>
                  <span className="knowledge-score" title="Retrieval score">
                    score {result.score.toFixed(4)}
                  </span>
                </div>
                <p className="knowledge-snippet">{result.content}</p>
                <p className="form-note">
                  chunk {result.chunkOrdinal ?? '—'}
                  {result.chunkId ? ` · ${shortenId(result.chunkId)}` : ''}
                </p>
              </article>
            ))}
          </div>
        )
      ) : null}
    </article>
  )
}

export function KnowledgeView({ canWrite }: { canWrite: boolean }) {
  const documentsQuery = useKnowledgeDocuments()
  const [showCreate, setShowCreate] = useState(false)
  const documents = useMemo(() => documentsQuery.data ?? [], [documentsQuery.data])
  const totalChunks = useMemo(() => documents.reduce((sum, document) => sum + document.chunkCount, 0), [documents])

  return (
    <>
      <PageHeader
        eyebrow="Retrieval-augmented generation"
        title="Knowledge"
        actions={
          canWrite ? (
            <button type="button" className="primary-button" onClick={() => setShowCreate((open) => !open)}>
              New document
            </button>
          ) : (
            <span className="form-note">Viewer role — ingesting documents needs MEMBER and above</span>
          )
        }
      />

      {showCreate && canWrite ? <CreateDocumentForm onCreated={() => setShowCreate(false)} /> : null}

      {documentsQuery.isError ? <ErrorBanner error={documentsQuery.error} onRetry={() => void documentsQuery.refetch()} /> : null}

      <SearchPanel />

      <section className="summary-grid">
        {documentsQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Documents" value={formatNumber(documents.length)} />
            <SummaryStat label="Chunks stored" value={formatNumber(totalChunks)} accent="info" />
            <SummaryStat label="API" value="/knowledge/documents" accent="default" />
            <SummaryStat label="Search" value="POST /knowledge/search" accent="success" />
          </>
        )}
      </section>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Corpus</p>
            <h3>Ingested documents</h3>
          </div>
          <span className="form-note">GET /knowledge/documents</span>
        </div>

        {documentsQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : documents.length === 0 ? (
          <EmptyState
            title="No documents ingested yet"
            hint="Knowledge is per-organization: ingest a document and its chunks become searchable with citations."
            action={
              canWrite ? (
                <button type="button" className="primary-button" onClick={() => setShowCreate(true)}>
                  Ingest document
                </button>
              ) : undefined
            }
          />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Title</th>
                  <th>Source</th>
                  <th>Chunks</th>
                  <th>Created</th>
                  <th>Updated</th>
                </tr>
              </thead>
              <tbody>
                {documents.map((document) => (
                  <tr key={document.id || document.title}>
                    <td>
                      <strong>{document.title}</strong>
                    </td>
                    <td>
                      {document.source ? <code className="config-cell">{document.source}</code> : '—'}
                    </td>
                    <td>{formatNumber(document.chunkCount)}</td>
                    <td>{document.createdAt ? formatRelativeTime(document.createdAt) : '—'}</td>
                    <td>{document.updatedAt ? formatRelativeTime(document.updatedAt) : '—'}</td>
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
