package main

// Agent-scoped API keys (wave-8, agent identity v1) — the management surface
// for the non-human credentials minted for ONE agent (internal/apikeys
// agent_keys.go). Endpoints (registered on apiMux by registerAgentKeyRoutes;
// served under BOTH /v1 and /api/v1):
//
//	POST   /agents/{id}/keys           mint (plaintext exactly once)
//	GET    /agents/{id}/keys           metadata list (prefix + allow-list, never the value)
//	PATCH  /agents/{id}/keys/{keyID}   name / tool allow-list / expiry
//	DELETE /agents/{id}/keys/{keyID}   revoke (soft tombstone, idempotent)
//
// RBAC — OWNER/ADMIN ONLY for ALL FOUR endpoints (agents.write, whose grant
// matrix is exactly OWNER/ADMIN). This is deliberately stricter than the user
// API-key surface (cmd/api/apikeys.go), whose metadata list is MEMBER+: agent
// keys are non-human credentials — long-lived, embedded in configs and CI,
// never protected by a human's MFA or step-up — and their allow-list IS the
// blast radius of a leak, so even read access to the list is restricted to
// the roles that can already mint credentials. Agent keys themselves
// authenticate as the least-privilege AGENT principal (auth.AgentRole) and
// therefore can never manage agent keys, including their own.
//
// Tenant guard: org scope comes exclusively from the auth claims —
// client-supplied organization ids are never trusted. Every handler first
// resolves the agent within the caller's organization (svc.EnsureAgentInOrg);
// an agent of another tenant is indistinguishable from an unknown one (404,
// no existence leak). Key ids are additionally resolved within the
// (organization_id, agent_id) pair by the store, so foreign keys are 404 too.
//
// SECURITY: the plaintext key value exists in exactly one wire place — the
// response body of POST /agents/{id}/keys, returned EXACTLY ONCE as the
// top-level "value" sibling of the metadata object (the POST /api-keys and
// POST /scim/tokens one-time credential shape). It is never echoed by
// list/patch/revoke responses, never logged, and never written to audit
// metadata. At rest only the SHA-256 hash is persisted (agent_api_keys).
// The allow-list is not sensitive and IS echoed so operators can audit the
// key's blast radius: nil (JSON null) = allow-list disabled (all tools),
// [] = no tools at all.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
)

// registerAgentKeyRoutes mounts the agent-key management surface on apiMux.
func registerAgentKeyRoutes(apiMux *http.ServeMux, svc *apikeys.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	// auth wrap pattern from cmd/api/main.go: RequireAuthOrAPIKey outer,
	// RequirePermission inner. agents.write is the OWNER/ADMIN-only matrix;
	// non-human credential management never falls to MEMBER.
	wrap := func(h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionAgentsWrite)(h))
	}

	apiMux.Handle("POST /agents/{id}/keys", wrap(http.HandlerFunc(createAgentKeyHandler(svc, auditSvc))))
	apiMux.Handle("GET /agents/{id}/keys", wrap(http.HandlerFunc(listAgentKeysHandler(svc))))
	apiMux.Handle("PATCH /agents/{id}/keys/{keyID}", wrap(http.HandlerFunc(updateAgentKeyHandler(svc, auditSvc))))
	apiMux.Handle("DELETE /agents/{id}/keys/{keyID}", wrap(http.HandlerFunc(revokeAgentKeyHandler(svc, auditSvc))))
}

// writeJSONAgentKey serializes v with the given status (distinct name to
// avoid clashing with helpers in other handler files).
func writeJSONAgentKey(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAgentKeyError emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeAgentKeyError(w http.ResponseWriter, status int, code, message string) {
	writeJSONAgentKey(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readAgentKeyJSON decodes the request body into dst, writing a 400 envelope
// on malformed JSON. Returns false when the response is already written.
func readAgentKeyJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeAgentKeyError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// agentKeyJSON renders one agent key METADATA projection. There is
// deliberately no `value` and no `hash`: the plaintext leaves the API exactly
// once (the create response) and only the SHA-256 hash is ever persisted.
// tool_allow_list preserves the nil-vs-empty contract (null = disabled,
// [] = no tools).
func agentKeyJSON(k *apikeys.AgentKey) map[string]any {
	var allowList any
	if k.ToolAllowList != nil {
		allowList = k.ToolAllowList
	}
	var expiresAt, revokedAt, lastUsedAt any
	if k.ExpiresAt != nil {
		expiresAt = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if k.RevokedAt != nil {
		revokedAt = k.RevokedAt.UTC().Format(time.RFC3339)
	}
	if k.LastUsedAt != nil {
		lastUsedAt = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id":              k.ID,
		"agent_id":        k.AgentID,
		"name":            k.Name,
		"prefix":          k.KeyPrefix,
		"tool_allow_list": allowList,
		"expires_at":      expiresAt,
		"revoked":         k.Revoked(),
		"revoked_at":      revokedAt,
		"last_used_at":    lastUsedAt,
		"created_at":      k.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// parseAgentKeyAllowListPatch decodes the PATCH tool_allow_list tri-state:
// absent (empty raw) = leave unchanged; null = DISABLE the allow-list (all
// tools); array = replace it ([] = no tools, named entries = those tools).
func parseAgentKeyAllowListPatch(raw json.RawMessage) (*[]string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, true
	}
	if bytes.Equal(trimmed, []byte("null")) {
		disabled := []string(nil)
		return &disabled, true
	}
	var list []string
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return nil, false
	}
	return &list, true
}

// parseAgentKeyExpiryPatch decodes the PATCH expires_at tri-state: absent =
// leave unchanged; null = clear the expiry; timestamp = replace it.
func parseAgentKeyExpiryPatch(raw json.RawMessage) (*time.Time, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, true
	}
	if bytes.Equal(trimmed, []byte("null")) {
		cleared := time.Time{}
		return &cleared, true
	}
	var t time.Time
	if err := json.Unmarshal(trimmed, &t); err != nil {
		return nil, false
	}
	return &t, true
}

// createAgentKeyHandler mints a new key for ONE agent of the caller's
// organization. The plaintext value is returned EXACTLY ONCE (top-level
// "value" sibling of the metadata object) and the minting writes an audit
// entry without key material.
func createAgentKeyHandler(svc *apikeys.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := apiKeyClaims(w, r)
		if !ok {
			return
		}
		agentID := r.PathValue("id")
		if strings.TrimSpace(agentID) == "" {
			writeAgentKeyError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		var req struct {
			Name          string     `json:"name"`
			ToolAllowList []string   `json:"tool_allow_list"`
			ExpiresAt     *time.Time `json:"expires_at"`
		}
		if !readAgentKeyJSON(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeAgentKeyError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "key name is required")
			return
		}
		// Tenant guard: the agent must belong to the caller's organization
		// (foreign/unknown agent = 404, no existence leak). The service
		// re-runs the same check before minting.
		if err := svc.EnsureAgentInOrg(r.Context(), claims.OrganizationID, agentID); err != nil {
			writeAgentKeyError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		key, plaintext, err := svc.CreateAgentKey(r.Context(), claims.OrganizationID, agentID, apikeys.AgentKeyCreateRequest{
			Name:          req.Name,
			ToolAllowList: req.ToolAllowList,
			ExpiresAt:     req.ExpiresAt,
		})
		if err != nil {
			switch {
			case errors.Is(err, apikeys.ErrAgentNotFound):
				writeAgentKeyError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			case errors.Is(err, apikeys.ErrAgentKeyNotFound):
				writeAgentKeyError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			default:
				// blank name / empty allow-list entries / past expiry
				writeAgentKeyError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			}
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert; no key material)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "agent_key.created",
				claims.OrganizationID, "agents/"+agentID+"/keys/"+key.ID, map[string]any{"name": req.Name})
		}
		writeJSONAgentKey(w, http.StatusCreated, map[string]any{
			"agent_key": agentKeyJSON(key),
			"value":     plaintext,
		})
	}
}

// listAgentKeysHandler returns metadata ONLY for the keys of one agent of
// the caller's organization, newest first.
func listAgentKeysHandler(svc *apikeys.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := apiKeyClaims(w, r)
		if !ok {
			return
		}
		agentID := r.PathValue("id")
		// Tenant guard: a foreign/unknown agent is 404, never an empty list.
		if err := svc.EnsureAgentInOrg(r.Context(), claims.OrganizationID, agentID); err != nil {
			writeAgentKeyError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		keys, err := svc.ListAgentKeys(r.Context(), claims.OrganizationID, agentID)
		if err != nil {
			writeAgentKeyError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		items := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			items = append(items, agentKeyJSON(k))
		}
		writeJSONAgentKey(w, http.StatusOK, map[string]any{"agent_keys": items})
	}
}

// updateAgentKeyHandler PATCHes name / tool allow-list / expiry of one key.
// Absent fields are left unchanged; "tool_allow_list": null disables the
// allow-list (all tools); "expires_at": null clears the expiry.
func updateAgentKeyHandler(svc *apikeys.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := apiKeyClaims(w, r)
		if !ok {
			return
		}
		agentID := r.PathValue("id")
		keyID := r.PathValue("keyID")
		if strings.TrimSpace(keyID) == "" {
			writeAgentKeyError(w, http.StatusNotFound, "AGENT_KEY_NOT_FOUND", "agent key not found")
			return
		}
		var req struct {
			Name          *string         `json:"name"`
			ToolAllowList json.RawMessage `json:"tool_allow_list"`
			ExpiresAt     json.RawMessage `json:"expires_at"`
		}
		if !readAgentKeyJSON(w, r, &req) {
			return
		}
		allowList, okList := parseAgentKeyAllowListPatch(req.ToolAllowList)
		if !okList {
			writeAgentKeyError(w, http.StatusBadRequest, "INVALID_REQUEST", "tool_allow_list must be an array of tool names or null")
			return
		}
		expiresAt, okExpiry := parseAgentKeyExpiryPatch(req.ExpiresAt)
		if !okExpiry {
			writeAgentKeyError(w, http.StatusBadRequest, "INVALID_REQUEST", "expires_at must be an RFC3339 timestamp or null")
			return
		}
		// Tenant guard: agent membership first (foreign agent = 404), then
		// the (org, agent)-scoped key resolution (foreign/unknown key = 404).
		if err := svc.EnsureAgentInOrg(r.Context(), claims.OrganizationID, agentID); err != nil {
			writeAgentKeyError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		key, err := svc.UpdateAgentKey(r.Context(), claims.OrganizationID, agentID, keyID, apikeys.AgentKeyUpdate{
			Name:          req.Name,
			ToolAllowList: allowList,
			ExpiresAt:     expiresAt,
		})
		if err != nil {
			switch {
			case errors.Is(err, apikeys.ErrAgentKeyNotFound):
				writeAgentKeyError(w, http.StatusNotFound, "AGENT_KEY_NOT_FOUND", "agent key not found")
			default:
				writeAgentKeyError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			}
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert; no key material)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "agent_key.updated",
				claims.OrganizationID, "agents/"+agentID+"/keys/"+key.ID, map[string]any{"key_id": key.ID})
		}
		writeJSONAgentKey(w, http.StatusOK, map[string]any{"agent_key": agentKeyJSON(key)})
	}
}

// revokeAgentKeyHandler soft-revokes one key within the caller's organization
// and agent. Unknown/foreign ids surface as 404 without an existence leak;
// revoking an already-revoked key stays 200 (idempotent per service
// semantics). The revoke writes an audit entry without key material.
func revokeAgentKeyHandler(svc *apikeys.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := apiKeyClaims(w, r)
		if !ok {
			return
		}
		agentID := r.PathValue("id")
		keyID := r.PathValue("keyID")
		if strings.TrimSpace(keyID) == "" {
			writeAgentKeyError(w, http.StatusNotFound, "AGENT_KEY_NOT_FOUND", "agent key not found")
			return
		}
		// Tenant guard: agent membership first (foreign agent = 404), then
		// the (org, agent)-scoped revocation (foreign/unknown key = 404).
		if err := svc.EnsureAgentInOrg(r.Context(), claims.OrganizationID, agentID); err != nil {
			writeAgentKeyError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		if err := svc.RevokeAgentKey(r.Context(), claims.OrganizationID, agentID, keyID); err != nil {
			if errors.Is(err, apikeys.ErrAgentKeyNotFound) {
				writeAgentKeyError(w, http.StatusNotFound, "AGENT_KEY_NOT_FOUND", "agent key not found")
				return
			}
			writeAgentKeyError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert; no key material)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "agent_key.revoked",
				claims.OrganizationID, "agents/"+agentID+"/keys/"+keyID, map[string]any{"key_id": keyID})
		}
		writeJSONAgentKey(w, http.StatusOK, map[string]any{"revoked": true})
	}
}
