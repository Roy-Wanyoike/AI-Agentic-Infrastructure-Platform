package main

// Budget HTTP surface (wave-8 issue C: per-scope budget gates + showback).
//
// Endpoints (registered on apiMux by registerBudgetRoutes; served under BOTH
// /v1 and /api/v1):
//
//      POST /budgets                       -> create a budget (org or agent scope)
//      GET  /budgets[?active=true|false]   -> list the org's budgets
//      GET  /budgets/evaluate              -> dry-run decision (?agent_id&runs&cost)
//      GET  /budgets/{id}                  -> one budget
//      PUT  /budgets/{id}                  -> full update
//      DELETE /budgets/{id}                -> remove
//
// RBAC (documented REUSE of the existing billing grants — internal/auth is
// outside this track's ownership, mirroring cmd/api/billing.go):
//
//      writes (POST/PUT/DELETE)  users.manage (OWNER/ADMIN — the billing
//                                subscription/cancel tier; spend ceilings are an
//                                account-administration decision)
//      reads  (GET, evaluate)    runs.execute (OWNER/ADMIN/MEMBER — the billing
//                                read tier: those who can trigger metered runs
//                                can read the gates that stop them)
//
// The tenant is taken from the auth claims ONLY; budget reads are
// store-guarded by organization_id so a foreign budget id is a plain 404
// (cross-tenant 404 convention). Agent-scoped budgets additionally verify the
// referenced agent exists INSIDE the caller's tenant (agents service wired
// from main; nil skips the check — zero-infrastructure tests).
//
// RUN-CREATION GATE: enforceBudget below is wired into createRunHandler
// immediately AFTER the quota gate (cmd/api/runs.go). It is additive billing
// enforcement under the SAME flag (AGENTOS_BILLING_ENFORCEMENT via
// billing.EnforcementFromEnv — budgets are part of billing enforcement, one
// flag so the two gates can never drift). Decision matrix mirrors
// enforceQuota/enforceRunPolicy:
//
//   - enforcement OFF            -> allow (today's behavior)
//   - budget service not wired   -> allow (budgets are a NEW additive
//     surface: no budget rows can exist without the wiring, mirroring the
//     unwired-policy-seam allow of enforceRunPolicy — unlike billing, whose
//     quota gate predates the flag and therefore fails closed)
//   - evaluation fails           -> 500 internal_error (fail closed: a
//     failing consumption source must surface, never fake availability —
//     the same rule as quotaFor)
//   - decision denied            -> 402 budget_exceeded, audit-logged, NO run
//     row / queue task is created

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/billing"
)

// budgetServiceVar exposes the budget service to the create-run gate and to
// registerBudgetRoutes (which assigns it), mirroring the billingServiceVar /
// runsServiceVar wiring precedent. nil = budget surface not deployed (the
// gate then allows — see the decision matrix above).
var budgetServiceVar *billing.BudgetService

// newBudgetService constructs the dual-mode budget service (Postgres store +
// runs-ledger consumption when db is non-nil, in-memory otherwise), mirroring
// newPoliciesService. The consumption source is wired in BOTH modes: the
// in-memory mode is used by the e2e/dev wiring and its budget math must see
// the same metered ledger the Postgres mode sees (zero consumption only when
// the caller passes nil).
func newBudgetService(db *sql.DB, consumption billing.ConsumptionSource) *billing.BudgetService {
	if db != nil {
		return billing.NewBudgetServiceWithStore(billing.NewBudgetPostgresStore(db), consumption)
	}
	svc := billing.NewBudgetService()
	svc.SetConsumptionSource(consumption)
	return svc
}

// registerBudgetRoutes mounts the budget endpoints on apiMux and assigns
// budgetServiceVar so the create-run gate evaluates the SAME instance the
// API serves (one source of truth, like sharedPoliciesService).
func registerBudgetRoutes(apiMux *http.ServeMux, db *sql.DB, consumption billing.ConsumptionSource, agentsSvc *agents.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	svc := newBudgetService(db, consumption)
	budgetServiceVar = svc

	// Collection: GET (billing read tier) vs POST (billing write tier).
	apiMux.Handle("/budgets", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perm := auth.PermissionRunsExecute
		if r.Method == http.MethodPost {
			perm = auth.PermissionUsersManage
		}
		auth.RequirePermission(authSvc, perm)(http.HandlerFunc(budgetsCollectionHandler(svc, agentsSvc))).ServeHTTP(w, r)
	})))
	// Dry-run evaluate: read tier (dashboard use case).
	apiMux.Handle("/budgets/evaluate", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionRunsExecute)(http.HandlerFunc(budgetsEvaluateHandler(svc)))))
	// Item: GET (read tier) vs PUT/DELETE (write tier).
	apiMux.Handle("/budgets/", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perm := auth.PermissionRunsExecute
		if r.Method != http.MethodGet {
			perm = auth.PermissionUsersManage
		}
		auth.RequirePermission(authSvc, perm)(http.HandlerFunc(budgetsItemHandler(svc, agentsSvc))).ServeHTTP(w, r)
	})))
}

func writeBdgJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeBdgError(w http.ResponseWriter, status int, code, message string) {
	writeBdgJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// writeBudgetServiceError maps billing budget errors onto the contract's
// error envelope; returns false for unmapped errors (caller decides).
func writeBudgetServiceError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, billing.ErrBudgetNotFound):
		writeBdgError(w, http.StatusNotFound, "BUDGET_NOT_FOUND", err.Error())
	case errors.Is(err, billing.ErrBudgetConflict):
		writeBdgError(w, http.StatusConflict, "BUDGET_CONFLICT", err.Error())
	case errors.Is(err, billing.ErrInvalidBudget), errors.Is(err, billing.ErrInvalidPeriod):
		writeBdgError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, agents.ErrAgentNotFound):
		writeBdgError(w, http.StatusNotFound, "AGENT_NOT_FOUND", err.Error())
	default:
		return false
	}
	return true
}

// decodeBdgBody decodes the JSON request body (bounded to 1 MiB).
func decodeBdgBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst); err != nil {
		writeBdgError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// bdgGuard rejects misconfigured wiring (nil service) with an honest 503 and
// enforces the method.
func bdgGuard(w http.ResponseWriter, r *http.Request, svc *billing.BudgetService, method string) bool {
	if r.Method != method {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	if svc == nil {
		writeBdgError(w, http.StatusServiceUnavailable, "BUDGET_UNAVAILABLE", "budget service not available")
		return false
	}
	return true
}

// bdgRequireAgent verifies that an agent-scoped budget references an agent
// INSIDE the caller's tenant (cross-tenant ref -> 404, never 403: the
// referenced agent must not even be revealed to exist). A nil agents service
// skips the check (zero-infrastructure wiring).
func bdgRequireAgent(w http.ResponseWriter, r *http.Request, agentsSvc *agents.Service, orgID, refID string) bool {
	if agentsSvc == nil {
		return true
	}
	if _, err := agentsSvc.GetAgentCtx(r.Context(), orgID, refID); err != nil {
		if errors.Is(err, agents.ErrAgentNotFound) {
			writeBdgError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return false
		}
		writeBdgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return false
	}
	return true
}

func budgetView(b *billing.Budget) map[string]any {
	return map[string]any{
		"id":               b.ID,
		"scope":            b.Scope,
		"ref_id":           b.RefID,
		"period":           b.Period,
		"limit_runs":       b.LimitRuns,
		"limit_cost_cents": b.LimitCostCents,
		"is_active":        b.IsActive,
		"created_at":       b.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":       b.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func budgetDecisionView(d *billing.BudgetDecision) map[string]any {
	return map[string]any{
		"allowed":          d.Allowed,
		"reason":           d.Reason,
		"denied_budget_id": d.DeniedBudgetID,
		"denied_scope":     d.DeniedScope,
		"message":          d.Message,
	}
}

// budgetsCollectionHandler serves POST /budgets (create) and
// GET /budgets?active=true|false (list).
func budgetsCollectionHandler(svc *billing.BudgetService, agentsSvc *agents.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeBdgError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		orgID := claims.OrganizationID
		switch r.Method {
		case http.MethodPost:
			var req struct {
				Scope          string `json:"scope"`
				RefID          string `json:"ref_id"`
				Period         string `json:"period"`
				LimitRuns      int64  `json:"limit_runs"`
				LimitCostCents int64  `json:"limit_cost_cents"`
				// IsActive defaults to TRUE when omitted (pointer).
				IsActive *bool `json:"is_active"`
			}
			if !decodeBdgBody(w, r, &req) {
				return
			}
			active := true
			if req.IsActive != nil {
				active = *req.IsActive
			}
			in := billing.BudgetInput{
				Scope:          strings.TrimSpace(req.Scope),
				RefID:          strings.TrimSpace(req.RefID),
				Period:         strings.TrimSpace(req.Period),
				LimitRuns:      req.LimitRuns,
				LimitCostCents: req.LimitCostCents,
				IsActive:       active,
			}
			// Validate the request shape FIRST (empty ref -> 400 via the
			// service), then check agent existence (404) only for a
			// non-empty ref — a missing ref is a validation error, not a
			// tenant lookup miss.
			if in.Scope == billing.BudgetScopeAgent && in.RefID != "" {
				if !bdgRequireAgent(w, r, agentsSvc, orgID, in.RefID) {
					return
				}
			}
			budget, err := svc.CreateBudgetCtx(r.Context(), orgID, in)
			if err != nil {
				if !writeBudgetServiceError(w, err) {
					writeBdgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
			writeBdgJSON(w, http.StatusCreated, map[string]any{"budget": budgetView(budget)})
		case http.MethodGet:
			activeOnly := false
			switch strings.TrimSpace(r.URL.Query().Get("active")) {
			case "true", "1":
				activeOnly = true
			case "", "false", "0":
			default:
				writeBdgError(w, http.StatusBadRequest, "VALIDATION_ERROR", "active must be true or false")
				return
			}
			budgets, err := svc.ListBudgetsCtx(r.Context(), orgID, activeOnly)
			if err != nil {
				if !writeBudgetServiceError(w, err) {
					writeBdgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
			out := make([]map[string]any, 0, len(budgets))
			for _, b := range budgets {
				out = append(out, budgetView(b))
			}
			writeBdgJSON(w, http.StatusOK, map[string]any{"budgets": out})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

// budgetsEvaluateHandler serves GET /budgets/evaluate?agent_id&runs&cost —
// the dry-run decision behind dashboards: would the requested work be denied
// by a budget right now? agent_id is optional (org-scope-only evaluation).
// runs defaults to 1 (a single run); cost is USD cents (fractional allowed,
// rounded half away from zero like invoice lines).
func budgetsEvaluateHandler(svc *billing.BudgetService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bdgGuard(w, r, svc, http.MethodGet) {
			return
		}
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeBdgError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
		projectedRuns := int64(1)
		if raw := strings.TrimSpace(r.URL.Query().Get("runs")); raw != "" {
			n, perr := strconv.ParseInt(raw, 10, 64)
			if perr != nil || n < 0 {
				writeBdgError(w, http.StatusBadRequest, "VALIDATION_ERROR", "runs must be a non-negative integer")
				return
			}
			projectedRuns = n
		}
		projectedCost := int64(0)
		if raw := strings.TrimSpace(r.URL.Query().Get("cost")); raw != "" {
			f, perr := strconv.ParseFloat(raw, 64)
			if perr != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
				writeBdgError(w, http.StatusBadRequest, "VALIDATION_ERROR", "cost must be a non-negative number of cents")
				return
			}
			projectedCost = int64(math.Round(f))
		}
		decision, err := svc.EvaluateBudget(r.Context(), claims.OrganizationID, agentID, projectedRuns, projectedCost)
		if err != nil {
			if !writeBudgetServiceError(w, err) {
				writeBdgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeBdgJSON(w, http.StatusOK, map[string]any{"decision": budgetDecisionView(decision)})
	}
}

// budgetsItemHandler serves GET/PUT/DELETE /budgets/{id}.
func budgetsItemHandler(svc *billing.BudgetService, agentsSvc *agents.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeBdgError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		orgID := claims.OrganizationID
		id := trimRoutePrefix(r.URL.Path, "/budgets/")
		if id == "" || strings.Contains(id, "/") {
			writeBdgError(w, http.StatusNotFound, "BUDGET_NOT_FOUND", "budget not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			budget, err := svc.GetBudgetCtx(r.Context(), orgID, id)
			if err != nil {
				if !writeBudgetServiceError(w, err) {
					writeBdgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
			writeBdgJSON(w, http.StatusOK, map[string]any{"budget": budgetView(budget)})
		case http.MethodPut:
			var req struct {
				Scope          string `json:"scope"`
				RefID          string `json:"ref_id"`
				Period         string `json:"period"`
				LimitRuns      int64  `json:"limit_runs"`
				LimitCostCents int64  `json:"limit_cost_cents"`
				IsActive       *bool  `json:"is_active"`
			}
			if !decodeBdgBody(w, r, &req) {
				return
			}
			// Full PUT semantics: an omitted is_active keeps the stored value.
			in := billing.BudgetInput{
				Scope:          strings.TrimSpace(req.Scope),
				RefID:          strings.TrimSpace(req.RefID),
				Period:         strings.TrimSpace(req.Period),
				LimitRuns:      req.LimitRuns,
				LimitCostCents: req.LimitCostCents,
			}
			current, err := svc.GetBudgetCtx(r.Context(), orgID, id)
			if err != nil {
				if !writeBudgetServiceError(w, err) {
					writeBdgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
			in.IsActive = current.IsActive
			if req.IsActive != nil {
				in.IsActive = *req.IsActive
			}
			if in.Scope == billing.BudgetScopeAgent && !bdgRequireAgent(w, r, agentsSvc, orgID, in.RefID) {
				return
			}
			budget, err := svc.UpdateBudgetCtx(r.Context(), orgID, id, in)
			if err != nil {
				if !writeBudgetServiceError(w, err) {
					writeBdgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
			writeBdgJSON(w, http.StatusOK, map[string]any{"budget": budgetView(budget)})
		case http.MethodDelete:
			if err := svc.DeleteBudgetCtx(r.Context(), orgID, id); err != nil {
				if !writeBudgetServiceError(w, err) {
					writeBdgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

// enforceBudget is the create-run budget gate (wave-8 issue C). It runs in
// createRunHandler immediately AFTER the quota gate and BEFORE any run row /
// queue task exists. projectedRuns is always 1 (one run is being created);
// projectedCostCents is the caller-supplied estimate (0 when omitted) — the
// same input the policy engine's max_cost_cents condition consumes.
//
// See the file-header decision matrix for the OFF/unwired/failure semantics.
func enforceBudget(w http.ResponseWriter, r *http.Request, orgID, agentID string, projectedCostCents int64, auditSvc *audit.Service) bool {
	if !billing.EnforcementFromEnv() {
		return true
	}
	if budgetServiceVar == nil {
		return true // additive surface not deployed: nothing to enforce
	}
	decision, err := budgetServiceVar.EvaluateBudget(r.Context(), orgID, agentID, 1, projectedCostCents)
	if err != nil {
		// Never fake budget availability when the consumption source fails.
		writeRunError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return false
	}
	if decision.Allowed {
		return true
	}
	if auditSvc != nil {
		// Best-effort denial audit — same claims-scoped pattern as the quota
		// denial (resource "runs/-": no run row exists yet).
		metadata := map[string]any{
			"reason":    billing.ReasonBudgetExceeded,
			"agent_id":  agentID,
			"budget_id": decision.DeniedBudgetID,
			"scope":     decision.DeniedScope,
			"message":   billing.BudgetExceededMessage(decision),
		}
		if claims, claimsErr := auth.ExtractClaims(r.Context()); claimsErr == nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "run.budget_denied", orgID, "runs/-", metadata)
		}
	}
	writeRunError(w, http.StatusPaymentRequired, billing.ReasonBudgetExceeded, billing.BudgetExceededMessage(decision))
	return false
}
