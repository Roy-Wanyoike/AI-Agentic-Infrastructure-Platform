package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/runs"
)

func requireOrganizationAccess(w http.ResponseWriter, r *http.Request, orgID string) bool {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return false
	}
	if strings.TrimSpace(orgID) != "" && orgID != claims.OrganizationID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// claimsOrganizationID resolves the caller's tenant, honouring an explicit
// organization_id only after the tenant-access check passes.
func claimsOrganizationID(w http.ResponseWriter, r *http.Request, requested string) (string, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return "", false
	}
	if strings.TrimSpace(requested) != "" {
		if !requireOrganizationAccess(w, r, requested) {
			return "", false
		}
		return requested, true
	}
	return claims.OrganizationID, true
}

func listAgentsHandler(service *agents.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		orgID, ok := claimsOrganizationID(w, r, r.URL.Query().Get("organization_id"))
		if !ok {
			return
		}
		// Tenant guard: the service query filters on organization_id.
		list, err := service.ListAgentsCtx(r.Context(), orgID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}
}

func createAgentHandler(service *agents.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			OrganizationID string `json:"organization_id"`
			Name           string `json:"name"`
			Description    string `json:"description"`
			Instructions   string `json:"instructions"`
			Model          string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.OrganizationID == "" {
			claims, err := auth.ExtractClaims(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			req.OrganizationID = claims.OrganizationID
		}
		if !requireOrganizationAccess(w, r, req.OrganizationID) {
			return
		}
		// Tenant guard: the agent is created with the caller's organization_id.
		agent, err := service.CreateAgentCtx(r.Context(), req.OrganizationID, req.Name, req.Description, req.Instructions, req.Model)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			claims, claimsErr := auth.ExtractClaims(r.Context())
			if claimsErr == nil {
				_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "agent.created", req.OrganizationID, "agents/"+agent.ID, nil)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(agent)
	}
}

func agentDetailHandler(service *agents.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		agentID := trimRoutePrefix(r.URL.Path, "/agents/")
		if agentID == "" || strings.Contains(agentID, "/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: the lookup requires the agent's organization_id to
		// match the caller's tenant; foreign agents surface as 404.
		agent, err := service.GetAgentCtx(r.Context(), orgID, agentID)
		if err != nil {
			if errors.Is(err, agents.ErrAgentNotFound) {
				http.Error(w, "agent not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agent)
	}
}

// --- Issue #49 (wave 6-g): finish CRUD on the flagship resource ---
//
// Endpoints (registered on apiMux by registerAgentsLifecycleRoutes; served
// under BOTH /v1 and /api/v1):
//
//	PUT    /agents/{id}  (agents.write — OWNER/ADMIN)  update draft fields
//	DELETE /agents/{id}  (agents.write — OWNER/ADMIN)  delete (guarded)
//
// Update semantics (following agents.Service.UpdateAgentCtx exactly): the
// live agent row IS the working draft — name, description, instructions
// (the system prompt; the version contract's canonical "system_prompt" key
// is accepted as an alias) and model are merged onto the current row.
// Provided values must be non-blank strings (name/instructions/model).
//
// Immutable rejection: any other key in the request body (e.g. tools,
// config, temperature, params, status, version, ids, timestamps) belongs to
// the immutable published-version snapshot domain (internal/agents/versions.go:
// "published versions are immutable") or to identity/bookkeeping and is
// REJECTED with 400 IMMUTABLE_FIELDS enumerating the offending keys — fields
// are never silently dropped. Publishing/rollback (not PUT) owns those
// values through the version lifecycle.
//
// Delete semantics (following agents.Service.DeleteAgentCtx exactly): a HARD
// delete — the agents row is removed, there is no soft-delete column. The
// durable schema cascades agent_versions (migration 003), deployments +
// canary rows (007/015), schedules (011) and memory snippets (014) via ON
// DELETE CASCADE; runs.agent_id (migration 001, fk_runs_agent) has NO ON
// DELETE action, so Postgres itself refuses to delete an agent that runs
// still reference. The handler mirrors that durable constraint: it counts
// the tenant's runs referencing the agent first and answers a structured
// 409 AGENT_HAS_RUNS instead of letting the store surface a raw FK 500.
// Evaluations and marketplace provenance keep agent_id strings without FKs
// by design and are not blocked.
//
// RBAC: both routes are guarded by agents.write, whose existing grant
// matrix is exactly OWNER/ADMIN. The tenant comes exclusively from the auth
// claims; foreign/unknown agents surface as 404 with no existence leak.
// Both operations write best-effort audit entries (agent.updated /
// agent.deleted) scoped to the caller's organization.

// registerAgentsLifecycleRoutes mounts the issue #49 agent update/delete
// routes. The method-prefixed patterns are more specific than the legacy
// "/agents/" catch-all registered by main.go, so GET behavior is unchanged
// (wiring is one additive line in cmd/api/main.go routes()).
func registerAgentsLifecycleRoutes(apiMux *http.ServeMux, agentsSvc *agents.Service, runsSvc *runs.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	// auth wrap pattern from cmd/api/main.go: RequireAuthOrAPIKey outer,
	// RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	apiMux.Handle("PUT /agents/{id}", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(updateAgentHandler(agentsSvc, auditSvc))))
	apiMux.Handle("DELETE /agents/{id}", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(deleteAgentHandler(agentsSvc, runsSvc, auditSvc))))
}

// writeJSONAL serializes v with the given status (distinct name avoids
// clashing with the other handler files' local helpers in package main).
func writeJSONAL(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErrorAL emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeErrorAL(w http.ResponseWriter, status int, code, message string) {
	writeJSONAL(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// agentDraftUpdateFields maps accepted request keys onto the canonical draft
// field the agents service persists. system_prompt is an accepted alias of
// instructions (the canonical snapshot key in the version contract).
var agentDraftUpdateFields = map[string]string{
	"name":          "name",
	"description":   "description",
	"instructions":  "instructions",
	"system_prompt": "instructions",
	"model":         "model",
}

// updateAgentHandler serves PUT /agents/{id}: merges the provided draft
// fields onto the agent's live configuration within the caller's tenant.
func updateAgentHandler(service *agents.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if strings.TrimSpace(agentID) == "" {
			writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil || strings.TrimSpace(claims.OrganizationID) == "" {
			writeErrorAL(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth context")
			return
		}
		orgID := claims.OrganizationID

		// Decode the body as a key set first so unsupported/immutable keys are
		// detected explicitly instead of being silently dropped by a naive
		// struct decode.
		fields := map[string]json.RawMessage{}
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			writeErrorAL(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		rejected := make([]string, 0, len(fields))
		for key := range fields {
			if _, ok := agentDraftUpdateFields[key]; !ok {
				rejected = append(rejected, key)
			}
		}
		if len(rejected) > 0 {
			sort.Strings(rejected)
			writeErrorAL(w, http.StatusBadRequest, "IMMUTABLE_FIELDS",
				fmt.Sprintf("immutable or unsupported fields cannot be updated via PUT /agents/{id}: %s (published version snapshots are immutable; only name, description, instructions (system_prompt) and model are draft-updatable)", strings.Join(rejected, ", ")))
			return
		}

		var name, description, instructions, model string
		var haveName, haveDescription, haveInstructions, haveModel bool
		for key, raw := range fields {
			canonical := agentDraftUpdateFields[key]
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				writeErrorAL(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", canonical+" must be a string")
				return
			}
			switch canonical {
			case "name":
				name, haveName = value, true
			case "description":
				description, haveDescription = value, true
			case "model":
				model, haveModel = value, true
			case "instructions":
				// instructions and its system_prompt alias must agree when both
				// are present; otherwise the request is ambiguous.
				if haveInstructions && instructions != value {
					writeErrorAL(w, http.StatusBadRequest, "INVALID_REQUEST", "instructions and system_prompt carry conflicting values")
					return
				}
				instructions, haveInstructions = value, true
			}
		}
		if !haveName && !haveDescription && !haveInstructions && !haveModel {
			writeErrorAL(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"at least one of name, description, instructions (system_prompt) or model is required")
			return
		}
		if (haveName && strings.TrimSpace(name) == "") ||
			(haveInstructions && strings.TrimSpace(instructions) == "") ||
			(haveModel && strings.TrimSpace(model) == "") {
			writeErrorAL(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"name, instructions (system_prompt) and model must be non-blank when provided")
			return
		}

		// Tenant guard: the lookup requires the agent's organization_id to
		// match the caller's tenant; foreign agents surface as 404.
		current, err := service.GetAgentCtx(r.Context(), orgID, agentID)
		if err != nil {
			if errors.Is(err, agents.ErrAgentNotFound) {
				writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
				return
			}
			writeErrorAL(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		// Struct copy before mutating: in-memory mode returns the shared cache
		// pointer, so in-place mutation would leak partial updates past
		// validation failures.
		updated := *current
		changed := make([]string, 0, 4)
		if haveName {
			updated.Name = name
			changed = append(changed, "name")
		}
		if haveDescription {
			updated.Description = description
			changed = append(changed, "description")
		}
		if haveInstructions {
			updated.Instructions = instructions
			changed = append(changed, "instructions")
		}
		if haveModel {
			updated.Model = model
			changed = append(changed, "model")
		}
		updated.UpdatedAt = time.Now().UTC()
		if err := service.UpdateAgentCtx(r.Context(), orgID, &updated); err != nil {
			if errors.Is(err, agents.ErrAgentNotFound) {
				writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
				return
			}
			writeErrorAL(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "agent.updated", orgID, "agents/"+agentID, map[string]any{"fields": changed})
		}
		// Same wire shape as GET /agents/{id}: the agent struct (PascalCase).
		writeJSONAL(w, http.StatusOK, &updated)
	}
}

// deleteAgentHandler serves DELETE /agents/{id}: a guarded HARD delete within
// the caller's tenant (see the file-level delete-semantics notes).
func deleteAgentHandler(service *agents.Service, runsSvc *runs.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if strings.TrimSpace(agentID) == "" {
			writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil || strings.TrimSpace(claims.OrganizationID) == "" {
			writeErrorAL(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth context")
			return
		}
		orgID := claims.OrganizationID

		// Tenant guard + existence: foreign/unknown agents surface as 404
		// before any destructive work happens.
		agent, err := service.GetAgentCtx(r.Context(), orgID, agentID)
		if err != nil {
			if errors.Is(err, agents.ErrAgentNotFound) {
				writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
				return
			}
			writeErrorAL(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}

		// Reference guard mirroring the durable schema: runs.agent_id has no
		// ON DELETE action (migration 001, fk_runs_agent), so Postgres refuses
		// the row delete while runs exist. Surface that as a structured 409
		// instead of a raw FK-violation 500. runsSvc may be nil in minimal
		// test wirings; production wiring always passes the app's runs service.
		if runsSvc != nil {
			orgRuns, err := runsSvc.ListRunsCtx(r.Context(), orgID)
			if err != nil {
				writeErrorAL(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
				return
			}
			runCount := 0
			for _, run := range orgRuns {
				if run.AgentID == agentID {
					runCount++
				}
			}
			if runCount > 0 {
				writeErrorAL(w, http.StatusConflict, "AGENT_HAS_RUNS",
					fmt.Sprintf("agent has %d run(s); deletion is blocked while runs reference the agent (delete the runs first)", runCount))
				return
			}
		}

		// Hard delete per the service's actual semantics: the agents row is
		// removed; versions/deployments/schedules/memory cascade via their ON
		// DELETE CASCADE foreign keys.
		if err := service.DeleteAgentCtx(r.Context(), orgID, agentID); err != nil {
			if errors.Is(err, agents.ErrAgentNotFound) {
				writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
				return
			}
			writeErrorAL(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "agent.deleted", orgID, "agents/"+agentID, map[string]any{"name": agent.Name})
		}
		writeJSONAL(w, http.StatusOK, map[string]any{"deleted": true, "id": agentID})
	}
}
