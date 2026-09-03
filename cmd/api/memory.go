package main

// Track 3-d (memory + knowledge/RAG) HTTP handlers — memory half.
//
// Endpoints (registered on apiMux by registerMemoryRoutes; served under BOTH
// /v1 and /api/v1):
//
//      GET /memory?agent_id= -> list visible (non-expired) snippets
//      PUT /memory           -> replace the snippet set of one (org, agent) scope
//
// The tenant is taken from the auth claims only; client-supplied organization
// ids are never trusted. agent_id is optional: empty manages the org-level
// shared memory scope.
//
// Permission fallback (documented deviation in docs/wiring/knowledge.md):
// internal/auth has no memory:*/knowledge:* (nor tools:*) constants, so the
// closest existing grants are reused — agents.read / agents.write — until the
// orchestrator adds dedicated permissions.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/memory"
)

// memorySnippetView is the wire shape of one snippet. The organization id and
// the embedding vector stay server-side (embeddings are an internal retrieval
// index, not part of the memory contract; clients may SUPPLY one on write).
type memorySnippetView struct {
	ID         string     `json:"id"`
	AgentID    string     `json:"agent_id"`
	Scope      string     `json:"scope"`
	Content    string     `json:"content"`
	Importance float64    `json:"importance"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func newMemorySnippetView(sn memory.Snippet) memorySnippetView {
	return memorySnippetView{
		ID:         sn.ID,
		AgentID:    sn.AgentID,
		Scope:      sn.Scope,
		Content:    sn.Content,
		Importance: sn.Importance,
		ExpiresAt:  sn.ExpiresAt,
		CreatedAt:  sn.CreatedAt,
		UpdatedAt:  sn.UpdatedAt,
	}
}

// writeMemoryError maps memory service errors onto the contract's error
// envelope; returns true when the error was handled.
func writeMemoryError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, memory.ErrInvalidScope) {
		writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
		return true
	}
	return false
}

// listMemorySnippetsHandler serves GET /memory?agent_id=: the caller's visible
// (non-expired) snippets, newest first. agent_id filters to exactly that
// agent's snippets; omitted lists the whole organization (org-level + agents).
func listMemorySnippetsHandler(svc *memory.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		// Tenant guard: the listing is scoped to the caller's organization.
		snippets, err := svc.ListSnippets(r.Context(), orgID, r.URL.Query().Get("agent_id"))
		if err != nil {
			if !writeMemoryError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		views := make([]memorySnippetView, 0, len(snippets))
		for _, sn := range snippets {
			views = append(views, newMemorySnippetView(sn))
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"snippets": views})
	}
}

// putMemoryHandler serves PUT /memory with body
// {"agent_id":"…","snippets":[{"scope","content","importance","expires_at",
// "embedding"}]}: atomically replaces the snippet set for the (organization,
// agent) scope and returns the stored snippets. expires_at is RFC3339; the
// embedding field is optional (client-computed vectors) — content is embedded
// with the service embedder otherwise. 422 VALIDATION_ERROR for empty
// content, unknown scope, or malformed expires_at.
func putMemoryHandler(svc *memory.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		var req struct {
			AgentID  string `json:"agent_id"`
			Snippets []struct {
				Scope      string    `json:"scope"`
				Content    string    `json:"content"`
				Importance float64   `json:"importance"`
				ExpiresAt  *string   `json:"expires_at"`
				Embedding  []float64 `json:"embedding"`
			} `json:"snippets"`
		}
		if !readJSONVD(w, r, &req) {
			return
		}
		inputs := make([]memory.SnippetInput, 0, len(req.Snippets))
		for i, in := range req.Snippets {
			if strings.TrimSpace(in.Content) == "" {
				writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
					"snippets["+strconv.Itoa(i)+"].content is required")
				return
			}
			input := memory.SnippetInput{
				Scope:      in.Scope,
				Content:    in.Content,
				Importance: in.Importance,
				Embedding:  in.Embedding,
			}
			if in.ExpiresAt != nil {
				parsed, err := time.Parse(time.RFC3339, *in.ExpiresAt)
				if err != nil {
					writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
						"snippets["+strconv.Itoa(i)+"].expires_at must be an RFC3339 timestamp")
					return
				}
				input.ExpiresAt = &parsed
			}
			inputs = append(inputs, input)
		}
		// Tenant guard: the snippet set is replaced within the caller's org.
		stored, err := svc.PutSnippets(r.Context(), orgID, memory.PutRequest{
			AgentID:  req.AgentID,
			Snippets: inputs,
		})
		if err != nil {
			if !writeMemoryError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		views := make([]memorySnippetView, 0, len(stored))
		for _, sn := range stored {
			views = append(views, newMemorySnippetView(sn))
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"snippets": views})
	}
}

// registerMemoryRoutes mounts the memory routes on apiMux. Permission
// fallback: `memory:read`/`memory:write` do not exist in internal/auth, so the
// closest existing grants agents.read/agents.write are reused (see
// docs/wiring/knowledge.md — orchestrator decision pending).
func registerMemoryRoutes(apiMux *http.ServeMux, svc *memory.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	wrap := func(perm auth.Permission, h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	apiMux.Handle("GET /memory", wrap(auth.PermissionAgentsRead, listMemorySnippetsHandler(svc)))
	apiMux.Handle("PUT /memory", wrap(auth.PermissionAgentsWrite, putMemoryHandler(svc)))
}
