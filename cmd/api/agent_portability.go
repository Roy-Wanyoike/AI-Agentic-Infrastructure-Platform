package main

// agent_portability.go — issue #122 HTTP wiring: agent export / import /
// clone on top of internal/agents/portability.go.
//
// Endpoints (registered on apiMux by registerAgentPortabilityRoutes; served
// under BOTH /v1 and /api/v1 like every versioned surface):
//
//      GET  /agents/{id}/export  portable bundle, configuration only (agents.read — every role)
//      POST /agents/import       bundle -> NEW agent, fresh identity (agents.write — OWNER/ADMIN)
//      POST /agents/{id}/clone   deep copy within the SAME org (agents.write — OWNER/ADMIN)
//
// Design (mirrors cmd/api/residency.go / agents.go):
//   - RBAC reuses the EXISTING agents grants: reads are agents.read (the
//     bundle is configuration, but it IS the org's prompt IP — every role
//     holds agents.read, VIEWER included, matching GET /agents/{id}); writes
//     are agents.write (exactly OWNER/ADMIN), matching create/update.
//   - The tenant comes EXCLUSIVELY from the auth claims. Import never trusts
//     client identity: the typed bundle decode drops unknown keys, and the
//     service reassigns id/organization/created_at and resets status to
//     DRAFT. Cross-org export/clone surface as 404 with no existence leak.
//   - Import validation failures are typed *PortabilityError (wrapping
//     agents.ErrInvalidBundle) rendered as 422 VALIDATION_ERROR with the
//     typed code embedded in the message (schema version, unknown model,
//     unknown tool, oversized instructions...).
//   - Secret VALUES are never exported: the bundle schema carries
//     configuration and tool NAMES only (internal/agents/portability.go);
//     audit metadata carries ids, never bundle content.
//   - Audit: best-effort agent.exported / agent.imported / agent.cloned
//     entries through the shared audit service (house style).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
)

// registerAgentPortabilityRoutes mounts the issue #122 agent export/import/
// clone routes. The method-prefixed patterns are more specific than the
// legacy "/agents/" catch-all registered by main.go, so existing behavior is
// unchanged (wiring is one additive line in cmd/api/main.go routes()).
func registerAgentPortabilityRoutes(apiMux *http.ServeMux, agentsSvc *agents.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || agentsSvc == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go / agents.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	// Reads: agents.read (every role — same grant as GET /agents/{id}).
	apiMux.Handle("GET /agents/{id}/export", wrap(auth.PermissionAgentsRead, http.HandlerFunc(exportAgentHandler(agentsSvc, auditSvc))))
	// Writes: agents.write (exactly OWNER/ADMIN).
	apiMux.Handle("POST /agents/import", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(importAgentHandler(agentsSvc, auditSvc))))
	apiMux.Handle("POST /agents/{id}/clone", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(cloneAgentHandler(agentsSvc, auditSvc))))
}

// decodePortBody decodes a REQUIRED JSON request body bounded to 1 MiB (the
// decodeResBody convention). Unknown keys are ignored by the typed decode —
// that is the never-trust-client guarantee: identity-shaped fields smuggled
// into the payload (ids, organization_id, created_at, status) simply never
// reach the service. Returns false when the response was written.
func decodePortBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close() //nolint:errcheck // read path; nothing actionable
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(raw) == 0 {
		writeErrorAL(w, http.StatusBadRequest, "INVALID_REQUEST", "a JSON request body is required")
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeErrorAL(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body")
		return false
	}
	return true
}

// portabilityActor resolves the audit actor from the request context
// (best-effort, house style).
func portabilityActor(r *http.Request) string {
	if claims, err := auth.ExtractClaims(r.Context()); err == nil {
		return claims.UserID
	}
	return ""
}

// exportAgentHandler serves GET /agents/{id}/export: the portable bundle
// (schema_version "1", configuration only) for one agent of the caller's
// tenant. Foreign/unknown agents answer 404 with no existence leak. The
// response body never carries the org id, agent id, status or credentials —
// the bundle schema is configuration-only by construction.
func exportAgentHandler(agentsSvc *agents.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if strings.TrimSpace(agentID) == "" {
			writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		org := claimsOrg(r)
		if org == "" {
			writeErrorAL(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth context")
			return
		}
		bundle, err := agentsSvc.ExportAgent(r.Context(), org, agentID)
		if err != nil {
			if errors.Is(err, agents.ErrAgentNotFound) {
				// Cross-organization and unknown agents are the same 404 —
				// no existence leak across tenants.
				writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
				return
			}
			writeErrorAL(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		if auditSvc != nil {
			// Best-effort; metadata carries ids only, never bundle content.
			_, _ = auditSvc.LogCtx(r.Context(), portabilityActor(r), "agent.exported", org, "agents/"+agentID, map[string]any{
				"agent_id":       agentID,
				"schema_version": bundle.SchemaVersion,
			})
		}
		writeJSONAL(w, http.StatusOK, bundle)
	}
}

// importAgentHandler serves POST /agents/import: validates the bundle and
// creates a NEW agent in the CALLER's tenant with a fresh identity (new id,
// fresh created_at, status DRAFT, fresh v1 version). Client-supplied
// identity fields are never trusted (dropped by the typed decode; the
// service reassigns the rest). Validation failures (schema version, unknown
// model, unknown tool, oversized instructions, missing fields) answer 422
// VALIDATION_ERROR with the typed *PortabilityError message and leave no
// agent behind.
func importAgentHandler(agentsSvc *agents.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org := claimsOrg(r)
		if org == "" {
			writeErrorAL(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth context")
			return
		}
		var bundle agents.ExportBundle
		if !decodePortBody(w, r, &bundle) {
			return
		}
		imported, err := agentsSvc.ImportAgent(r.Context(), org, &bundle)
		if err != nil {
			switch {
			case errors.Is(err, agents.ErrInvalidBundle):
				// Typed 422-able validation rejection (the message embeds the
				// PortabilityCode*, e.g. UNKNOWN_MODEL).
				writeErrorAL(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			case errors.Is(err, agents.ErrAgentNotFound):
				writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			default:
				writeErrorAL(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), portabilityActor(r), "agent.imported", org, "agents/"+imported.ID, map[string]any{
				"agent_id": imported.ID,
			})
		}
		// Same wire shape as POST /agents: the agent struct (PascalCase).
		writeJSONAL(w, http.StatusCreated, imported)
	}
}

// cloneAgentHandler serves POST /agents/{id}/clone: a deep copy of one agent
// WITHIN the caller's organization (new id, name suffix "-copy", status reset
// to DRAFT, fresh v1). The request body is optional and ignored — there is
// deliberately no client-controlled target tenant or name; a cross-org clone
// attempt is rejected as 404 with no existence leak.
func cloneAgentHandler(agentsSvc *agents.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if strings.TrimSpace(agentID) == "" {
			writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		org := claimsOrg(r)
		if org == "" {
			writeErrorAL(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth context")
			return
		}
		clone, err := agentsSvc.CloneAgent(r.Context(), org, agentID)
		if err != nil {
			if errors.Is(err, agents.ErrAgentNotFound) {
				// Cross-organization and unknown sources are the same 404 —
				// no existence leak across tenants.
				writeErrorAL(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
				return
			}
			writeErrorAL(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), portabilityActor(r), "agent.cloned", org, "agents/"+clone.ID, map[string]any{
				"agent_id":        clone.ID,
				"source_agent_id": agentID,
			})
		}
		writeJSONAL(w, http.StatusCreated, clone)
	}
}
