package main

// Track 3-d (memory + knowledge/RAG) HTTP handlers — knowledge half.
//
// Endpoints (registered on apiMux by registerKnowledgeRoutes; served under
// BOTH /v1 and /api/v1):
//
//      POST /knowledge/documents -> ingest (create -> chunk -> embed -> store)
//      GET  /knowledge/documents -> list
//      POST /knowledge/search    -> top-k chunk retrieval with citations
//
// The tenant is taken from the auth claims only; client-supplied organization
// ids are never trusted. Error bodies use the shared
// {"error":{"code","message"}} envelope via the local writeErrorKnb helper.
//
// Permission fallback (documented deviation in docs/wiring/knowledge.md):
// internal/auth has no knowledge:*/memory:* (nor tools:*) constants, so the
// closest existing grants are reused — agents.read for reads, agents.write
// for writes — until the orchestrator adds dedicated permissions.

import (
	"errors"
	"net/http"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/knowledge"
)

// writeJSONKnb renders a JSON response (local helper, distinct name avoids
// collisions with other tracks' helpers in package main).
func writeJSONKnb(w http.ResponseWriter, status int, payload any) {
	writeJSONVD(w, status, payload)
}

// writeErrorKnb renders the shared structured error envelope.
func writeErrorKnb(w http.ResponseWriter, status int, code, message string) {
	writeErrorVD(w, status, code, message)
}

// readJSONKnb decodes a JSON request body, writing 400 on malformed input.
func readJSONKnb(w http.ResponseWriter, r *http.Request, dst any) bool {
	return readJSONVD(w, r, dst)
}

// claimsOrgIDKnb resolves the caller's tenant from the auth context.
func claimsOrgIDKnb(w http.ResponseWriter, r *http.Request) (string, bool) {
	return claimsOrgIDVD(w, r)
}

// knowledgeDocumentView is the wire shape of one ingested document. The
// organization id stays server-side (the tenant is implied by the caller's
// claims), matching the other track views.
type knowledgeDocumentView struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Source     string         `json:"source,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	ChunkCount int            `json:"chunk_count"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
}

func newKnowledgeDocumentView(doc *knowledge.Document) knowledgeDocumentView {
	return knowledgeDocumentView{
		ID:         doc.ID,
		Title:      doc.Title,
		Source:     doc.Source,
		Metadata:   doc.Metadata,
		ChunkCount: doc.ChunkCount,
		CreatedAt:  doc.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:  doc.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// writeKnowledgeError maps knowledge service errors onto the contract's error
// envelope; returns true when the error was handled.
func writeKnowledgeError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, knowledge.ErrDocumentNotFound) {
		writeErrorKnb(w, http.StatusNotFound, "DOCUMENT_NOT_FOUND", "knowledge document not found")
		return true
	}
	return false
}

// createKnowledgeDocumentHandler serves POST /knowledge/documents with body
// {"title","content","source","metadata"}: runs the ingestion pipeline
// (create -> chunk ~800 chars/15% overlap -> embed -> store) and returns 201.
// An embeddings-backend failure is NON-FATAL (201 + "warning": the chunks are
// stored unembedded and search falls back to lexical scoring for them).
func createKnowledgeDocumentHandler(svc *knowledge.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDKnb(w, r)
		if !ok {
			return
		}
		var req struct {
			Title    string         `json:"title"`
			Content  string         `json:"content"`
			Source   string         `json:"source"`
			Metadata map[string]any `json:"metadata"`
		}
		if !readJSONKnb(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Content) == "" {
			writeErrorKnb(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "title and content are required")
			return
		}
		// Tenant guard: the document is created within the caller's org.
		// IngestDocument returns the embedder failure as a NON-FATAL error
		// alongside the stored document (chunks stay unembedded); a fatal
		// failure (store down, invalid input) comes back with doc == nil.
		doc, _, err := svc.IngestDocument(r.Context(), orgID, knowledge.IngestRequest{
			Title:    req.Title,
			Content:  req.Content,
			Source:   req.Source,
			Metadata: req.Metadata,
		})
		if err != nil && doc == nil {
			if !writeKnowledgeError(w, err) {
				writeErrorKnb(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		payload := map[string]any{"document": newKnowledgeDocumentView(doc)}
		if err != nil {
			payload["warning"] = err.Error()
		}
		writeJSONKnb(w, http.StatusCreated, payload)
	}
}

// listKnowledgeDocumentsHandler serves GET /knowledge/documents: the caller's
// documents, newest first.
func listKnowledgeDocumentsHandler(svc *knowledge.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDKnb(w, r)
		if !ok {
			return
		}
		docs, err := svc.ListDocuments(r.Context(), orgID)
		if err != nil {
			if !writeKnowledgeError(w, err) {
				writeErrorKnb(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		views := make([]knowledgeDocumentView, 0, len(docs))
		for i := range docs {
			views = append(views, newKnowledgeDocumentView(&docs[i]))
		}
		writeJSONKnb(w, http.StatusOK, map[string]any{"documents": views})
	}
}

// searchKnowledgeHandler serves POST /knowledge/search with body
// {"query","k"}: top-k chunks scored by cosine similarity over the org-scoped
// candidate set (lexical fallback for unembedded chunks). Response shape is
// pinned by the wave-3 contract:
// {"results":[{"document_id","chunk_ordinal","content","score","citation"}]}
// plus additive document_title/chunk_id fields.
func searchKnowledgeHandler(svc *knowledge.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDKnb(w, r)
		if !ok {
			return
		}
		var req struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}
		if !readJSONKnb(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Query) == "" {
			writeErrorKnb(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "query is required")
			return
		}
		// Tenant guard: candidates are scoped to the caller's organization.
		results, err := svc.Search(r.Context(), orgID, req.Query, req.K)
		if err != nil {
			if !writeKnowledgeError(w, err) {
				writeErrorKnb(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONKnb(w, http.StatusOK, map[string]any{"results": results})
	}
}

// registerKnowledgeRoutes mounts the knowledge/RAG routes on apiMux.
// Permission fallback: `knowledge:read`/`knowledge:write` do not exist in
// internal/auth (and the contract's suggested tools:read/tools:write do not
// either), so the closest existing grants agents.read/agents.write are reused
// (see docs/wiring/knowledge.md — orchestrator decision pending).
func registerKnowledgeRoutes(apiMux *http.ServeMux, svc *knowledge.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	wrap := func(perm auth.Permission, h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	apiMux.Handle("POST /knowledge/documents", wrap(auth.PermissionAgentsWrite, createKnowledgeDocumentHandler(svc)))
	apiMux.Handle("GET /knowledge/documents", wrap(auth.PermissionAgentsRead, listKnowledgeDocumentsHandler(svc)))
	apiMux.Handle("POST /knowledge/search", wrap(auth.PermissionAgentsRead, searchKnowledgeHandler(svc)))
}
