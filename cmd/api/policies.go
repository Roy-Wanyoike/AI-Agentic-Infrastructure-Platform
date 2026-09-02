package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/policies"
)

// Task 2-c (governance): policy CRUD + evaluation endpoints.
//
// Routes (mounted on apiMux, served under both /v1 and /api/v1):
//
//	GET    /policies            -> {"policies":[...]}           (policies.read)
//	POST   /policies/create     -> {"policy":{...}}             (policies.write)
//	GET    /policies/{id}       -> {"policy":{...}}             (policies.read)
//	PUT    /policies/{id}       -> {"policy":{...}}             (policies.write)
//	DELETE /policies/{id}       -> {"deleted":true}             (policies.write)
//	POST   /policies/evaluate   -> {"decision","matched_policy_id","reason"} (policies.read)
//
// Errors use the structured envelope {"error":{"code","message"}}. Tenant
// scope always comes from the authenticated claims, never from the body.

// registerPoliciesRoutes mounts every policy route on apiMux.
func registerPoliciesRoutes(apiMux *http.ServeMux, polSvc *policies.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	readWrap := func(next http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
			auth.RequirePermission(authSvc, auth.PermissionPoliciesRead)(next))
	}
	writeWrap := func(next http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
			auth.RequirePermission(authSvc, auth.PermissionPoliciesWrite)(next))
	}

	apiMux.Handle("GET /policies", readWrap(http.HandlerFunc(listPoliciesHandler(polSvc))))
	apiMux.Handle("POST /policies/create", writeWrap(http.HandlerFunc(createPoliciesHandler(polSvc))))
	apiMux.Handle("POST /policies/evaluate", readWrap(http.HandlerFunc(evaluatePoliciesHandler(polSvc))))
	apiMux.Handle("GET /policies/{id}", readWrap(http.HandlerFunc(policyDetailHandler(polSvc))))
	apiMux.Handle("PUT /policies/{id}", writeWrap(http.HandlerFunc(policyDetailHandler(polSvc))))
	apiMux.Handle("DELETE /policies/{id}", writeWrap(http.HandlerFunc(policyDetailHandler(polSvc))))
}

// writeJSONPol emits a JSON response (distinct name to avoid clashing with
// other track helpers in package main).
func writeJSONPol(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeErrorPol emits the structured error envelope.
func writeErrorPol(w http.ResponseWriter, status int, code, message string) {
	writeJSONPol(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// claimsOrganizationPol resolves the caller's tenant from the auth context.
func claimsOrganizationPol(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeErrorPol(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return "", false
	}
	if strings.TrimSpace(claims.OrganizationID) == "" {
		writeErrorPol(w, http.StatusUnauthorized, "unauthorized", "missing organization claim")
		return "", false
	}
	return claims.OrganizationID, true
}

// writePoliciesServiceError maps service errors onto HTTP responses.
func writePoliciesServiceError(w http.ResponseWriter, err error) {
	if errors.Is(err, policies.ErrPolicyNotFound) {
		writeErrorPol(w, http.StatusNotFound, "not_found", "policy not found")
		return
	}
	if errors.Is(err, policies.ErrInvalidPolicy) {
		writeErrorPol(w, http.StatusUnprocessableEntity, "invalid_policy", err.Error())
		return
	}
	slog.Error("policies service error", "error", err.Error())
	writeErrorPol(w, http.StatusInternalServerError, "internal_error", "policies service error")
}

func listPoliciesHandler(polSvc *policies.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrganizationPol(w, r)
		if !ok {
			return
		}
		// Tenant guard: the listing filters on organization_id.
		list, err := polSvc.ListPoliciesCtx(r.Context(), orgID)
		if err != nil {
			writePoliciesServiceError(w, err)
			return
		}
		writeJSONPol(w, http.StatusOK, map[string]any{"policies": list})
	}
}

func createPoliciesHandler(polSvc *policies.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrganizationPol(w, r)
		if !ok {
			return
		}
		var policy policies.Policy
		if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
			writeErrorPol(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
			return
		}
		// Tenant scope comes from the claims; client-supplied ids are ignored.
		policy.ID = ""
		policy.OrganizationID = orgID
		created, err := polSvc.CreatePolicyCtx(r.Context(), orgID, &policy)
		if err != nil {
			writePoliciesServiceError(w, err)
			return
		}
		writeJSONPol(w, http.StatusCreated, map[string]any{"policy": created})
	}
}

func policyDetailHandler(polSvc *policies.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrganizationPol(w, r)
		if !ok {
			return
		}
		policyID := r.PathValue("id")
		if strings.TrimSpace(policyID) == "" {
			writeErrorPol(w, http.StatusNotFound, "not_found", "policy id is required")
			return
		}
		switch r.Method {
		case http.MethodGet:
			policy, err := polSvc.GetPolicyCtx(r.Context(), orgID, policyID)
			if err != nil {
				writePoliciesServiceError(w, err)
				return
			}
			writeJSONPol(w, http.StatusOK, map[string]any{"policy": policy})
		case http.MethodPut:
			var policy policies.Policy
			if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
				writeErrorPol(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
				return
			}
			updated, err := polSvc.UpdatePolicyCtx(r.Context(), orgID, policyID, &policy)
			if err != nil {
				writePoliciesServiceError(w, err)
				return
			}
			writeJSONPol(w, http.StatusOK, map[string]any{"policy": updated})
		case http.MethodDelete:
			if err := polSvc.DeletePolicyCtx(r.Context(), orgID, policyID); err != nil {
				writePoliciesServiceError(w, err)
				return
			}
			writeJSONPol(w, http.StatusOK, map[string]any{"deleted": true})
		default:
			writeErrorPol(w, http.StatusMethodNotAllowed, "method_not_allowed", "unsupported method")
		}
	}
}

func evaluatePoliciesHandler(polSvc *policies.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrganizationPol(w, r)
		if !ok {
			return
		}
		var req policies.EvaluateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErrorPol(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
			return
		}
		if strings.TrimSpace(req.Action) == "" {
			writeErrorPol(w, http.StatusUnprocessableEntity, "invalid_request", "action is required")
			return
		}
		// The engine evaluates against the caller's tenant only; the
		// resource.tenant_id field is informational.
		decision, err := polSvc.EvaluateCtx(r.Context(), orgID, req)
		if err != nil {
			writePoliciesServiceError(w, err)
			return
		}
		writeJSONPol(w, http.StatusOK, decision)
	}
}
