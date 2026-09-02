package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agentos/internal/agents"
	"agentos/internal/audit"
	"agentos/internal/auth"
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
