package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"agentos/internal/agents"
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

func listAgentsHandler(service *agents.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		orgID := r.URL.Query().Get("organization_id")
		if orgID == "" {
			claims, err := auth.ExtractClaims(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			orgID = claims.OrganizationID
		}
		if !requireOrganizationAccess(w, r, orgID) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(service.List(orgID))
	}
}

func createAgentHandler(service *agents.Service) http.HandlerFunc {
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
		agent, err := service.Create(req.OrganizationID, req.Name, req.Description, req.Instructions, req.Model)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(agent)
	}
}

func agentDetailHandler(service *agents.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
		if path == "" || strings.Contains(path, "/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		agent, ok := service.Get(path)
		if !ok {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agent)
	}
}
