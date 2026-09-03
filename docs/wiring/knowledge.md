# Wiring — Track 3-d (Memory persistence + Knowledge/RAG foundation)

Everything in this track lives in NEW files; `cmd/api/main.go` is NOT edited by
track 3-d. The orchestrator applies the lines below.

## 1. Files added by this track

| File | Purpose |
|------|---------|
| `migrations/014_memory_knowledge.sql` | `memory_snippets` (org-scoped, nullable `agent_id`, `scope` short_term\|long_term, `importance`, `expires_at`, `embedding` JSONB) + `knowledge_documents` + `knowledge_chunks` (`document_id` FK, `ordinal`, `embedding` JSONB, tenant-scoped indexes). Idempotent (`IF NOT EXISTS`, 006-011 style). Auto-discovered — no registry edit needed. |
| `internal/memory/service.go` | Dual-mode memory `Service` (in-memory maps / Postgres store), `PutSnippets` (atomic replace per org+agent scope), `ListSnippets` (expiry-filtered), `Retrieve` (cosine + lexical fallback, agent scope includes org-level shared memory). The pre-existing in-memory `Store`/`Entry` API in `memory.go` is untouched. |
| `internal/memory/store.go` | `pgStore` (`SnippetStore`): transactional delete+insert per scope, expiry guard inside the SQL, every statement tenant-scoped. |
| `internal/memory/embed.go` | Deterministic offline `HashEmbedder` (256-dim signed bucket bag-of-words, L2-normalized) + `cosineSimilarity`/`lexicalScore` helpers. |
| `internal/knowledge/service.go` | NEW package: dual-mode knowledge `Service` — `IngestDocument` (create -> chunk -> embed -> store), `ListDocuments`, `GetDocument`, `Search` (top-k chunks with score + citation). |
| `internal/knowledge/chunker.go` | `ChunkText`: ~800-rune chunks, 15% overlap, cut on paragraph/sentence/word boundaries; deterministic, hard-cut progress guarantee. |
| `internal/knowledge/embedder.go` | `Embedder` interface, `NewEmbedderFromEnv` (offline vs OpenAI-compatible), minimal `OpenAIEmbedder` HTTP client (owned by this package — `internal/models` untouched), offline `HashEmbedder`. |
| `internal/knowledge/scoring.go` | `cosineSimilarity` + `lexicalScore` (Jaccard token overlap) shared by retrieval. |
| `internal/knowledge/store.go` | `pgStore` (`Store`): transactional document+chunk insert, org-scoped candidate join query for retrieval. |
| `cmd/api/knowledge.go` | `registerKnowledgeRoutes` + ingest/list/search handlers (local `*Knb` helpers). |
| `cmd/api/memory.go` | `registerMemoryRoutes` + list/put handlers (local `*Mem` helpers; reuses `*VD` JSON helpers from `versions.go` via thin `*Knb` aliases). |
| `api/fragments/knowledge.yaml` | OpenAPI 3.1 fragment (3 knowledge paths + `/memory`, `Knb*`/`Mem*` schemas, `x-required-permission` on every operation). |
| Tests | `internal/memory/service_test.go`, `internal/memory/store_test.go` (sqlmock), `internal/knowledge/service_test.go`, `internal/knowledge/store_test.go` (sqlmock), `internal/knowledge/embedder_test.go` (httptest), `cmd/api/knowledge_test.go`, `cmd/api/memory_test.go` (httptest over the real middleware chain). |

## 2. Exact constructor lines for `cmd/api/main.go`

### 2a. `app` struct — add two fields

```go
type app struct {
        // ...existing fields...
        knowledgeSvc *knowledge.Service
        memorySvc    *memory.Service
}
```

### 2b. `newApp` — construct the services (both modes)

Postgres mode (inside `if db != nil`):

```go
a.knowledgeSvc = knowledge.NewServiceWithStore(db)
a.memorySvc = memory.NewServiceWithStore(db)
```

In-memory mode (inside `else`):

```go
a.knowledgeSvc = knowledge.NewService()
a.memorySvc = memory.NewService()
```

Imports to add: `agentos/internal/knowledge`, `agentos/internal/memory`.

### 2c. `routes()` — register the routes

```go
registerKnowledgeRoutes(apiMux, a.knowledgeSvc, a.authSvc, a.apiKeysSvc)
registerMemoryRoutes(apiMux, a.memorySvc, a.authSvc, a.apiKeysSvc)
```

Both mount on `apiMux` → served under BOTH `/v1` and `/api/v1`. Order does
not matter (no catch-all pattern overlaps: `/knowledge/...` and `/memory` are
distinct from `/agents/` and `/runs/`).

## 3. Embedding env knobs (both services)

| Variable | Meaning | Default behavior |
|---|---|---|
| `AGENTOS_EMBEDDING_BASE_URL` | Root of an OpenAI-compatible API (e.g. `https://api.openai.com/v1`); the client POSTs `{base}/embeddings`. | Unset = offline mode |
| `AGENTOS_EMBEDDING_MODEL` | Embeddings model (e.g. `text-embedding-3-small`). | Unset = offline mode; set without base URL → OpenAI default |
| `AGENTOS_EMBEDDING_API_KEY` | Bearer token for the embeddings API. | Optional (some local backends need none) |

Selection happens in `knowledge.NewEmbedderFromEnv()`: **any** of base URL /
model set → remote embedder; none set → offline hash embedder.

## 4. Endpoints + RBAC

| Method & path | Enforced permission (`x-required-permission`) | Contract permission | Response |
|---|---|---|---|
| `POST /knowledge/documents` body `{"title","content","source","metadata"}` | `agents.write` | `knowledge:write` | 201 `{"document":{id,title,source,metadata,chunk_count,created_at,updated_at},"warning"?}` |
| `GET /knowledge/documents` | `agents.read` | `knowledge:read` | `{"documents":[…]}` (newest first) |
| `POST /knowledge/search` body `{"query","k"?}` | `agents.read` | `knowledge:read` | `{"results":[{document_id,chunk_ordinal,content,score,citation}]}` (+additive `document_title`,`chunk_id`) |
| `GET /memory?agent_id=` | `agents.read` | `memory:read` | `{"snippets":[{id,agent_id,scope,content,importance,expires_at,created_at,updated_at}]}` |
| `PUT /memory` body `{"agent_id","snippets":[{scope,content,importance,expires_at,embedding?}]}` | `agents.write` | `memory:write` | `{"snippets":[…]}` (stored set, post-normalization) |

Error envelope is the shared `{"error":{"code","message"}}`:
`DOCUMENT_NOT_FOUND` (reserved for the single-document route), `VALIDATION_ERROR`
(422), `INVALID_REQUEST` (400), `INTERNAL_ERROR` (500). Embeddings-backend
failures on ingest are NON-FATAL: 201 + additive `warning` field, chunks stored
unembedded.

## 5. Permission decisions (deviation — orchestrator decision pending)

- The contract's `knowledge:read`/`knowledge:write` and `memory:read`/
  `memory:write` permissions do not exist in `internal/auth`.
- The contract's suggested fallback `tools:read`/`tools:write` does not exist
  in `internal/auth` either (the tools package registers no permission
  constants).
- Per the contract ("use the closest existing permissions and document"),
  this track enforces **`agents.read`** (OWNER/ADMIN/MEMBER/VIEWER) for reads
  and **`agents.write`** (OWNER/ADMIN/MEMBER) for writes — the closest
  semantic match (tenant-scoped agent-adjacent resource data, VIEWER
  read-only). `internal/auth` was NOT modified (contract forbids it without
  an orchestrator decision).
- The fragment records both values: `x-required-permission` (enforced today)
  and `x-contract-permission` (the value to switch to once dedicated
  constants land). Wiring after the orchestrator adds `knowledge.*/memory.*`
  constants is a two-line change per route inside `registerKnowledgeRoutes` /
  `registerMemoryRoutes`.

## 6. Deviations & notes

1. **No pgvector** — embeddings live in `JSONB` columns as JSON float arrays
   (e.g. `[0.0187, -0.0821, …]`); similarity is computed in Go over an
   org-scoped candidate window (`LIMIT 2000`, oldest-first, deterministic).
   A pgvector/ANN upgrade can migrate the JSONB columns forward without a
   table-shape rewrite.
2. **Offline hashing embedder** — without `AGENTOS_EMBEDDING_*` config the
   deterministic hash embedder (256-dim, signed two-bucket bag-of-words,
   L2-normalized) provides pseudo-embeddings so ingest + search work in
   tests/dev with zero infrastructure. This is NOT semantically compatible
   with model embeddings: scores are soft token-overlap and bucket collisions
   give low-level noise (~0.1-0.2 cosine for disjoint vocabularies); ranking
   still favors genuine overlap. Mixed spaces are handled safely: cosine is
   only used when the query and candidate vectors have the same dimension,
   otherwise retrieval falls back to lexical (Jaccard) scoring.
3. **Embedder failure is non-fatal on ingest** — chunks store `NULL`
   embeddings (201 + `warning`), retrieval keeps working lexically.
4. **`internal/memory` compatibility** — the legacy in-memory `Store`/`Entry`
   API (`memory.go`, used by orchestration) is untouched; the new
   `Service`/`Snippet` layer lives beside it.
5. **Embeddings are never echoed** by the memory API (write-accepted via the
   optional `embedding` field, never returned) — they are an internal
   retrieval index. Knowledge search returns chunk text + citation, not
   vectors.
6. **org-level shared memory** — `agent_id` NULL in SQL / empty string on the
   wire; `PUT /memory` with no `agent_id` replaces that scope atomically, and
   agent-scoped retrieval (`memory.Retrieve` with `AgentID`) includes it.
