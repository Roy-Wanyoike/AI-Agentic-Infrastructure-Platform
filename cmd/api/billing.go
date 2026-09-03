package main

// Billing HTTP surface (issue #24, wave-5 track 5-c).
//
// Endpoints (registered on apiMux by registerBillingRoutes; served under BOTH
// /v1 and /api/v1):
//
//      GET  /billing/plans               -> list the global plan catalog
//      POST /billing/subscriptions       -> subscribe the caller's org to a plan
//      GET  /billing/subscription        -> current subscription + quota state
//      POST /billing/subscription/cancel -> cancel (immediate or at period end)
//      GET  /billing/invoices            -> list the org's invoices
//      GET  /billing/invoices/{id}       -> one invoice with its lines
//
// RBAC (documented permission REUSE — internal/auth is outside this track's
// ownership, so no dedicated billing permission could be introduced; the
// closest existing org-scoped grants are used per the internal/auth
// conventions, exactly like the knowledge/memory tracks reuse agents.*):
//
//      writes
//        POST /billing/subscriptions      organization.manage (OWNER only —
//                                         committing the org to a paid plan is an
//                                         org-level decision reserved to OWNER)
//        POST /billing/subscription/cancel users.manage (OWNER/ADMIN — account
//                                         administration tier)
//      reads (MEMBER+ per the issue: VIEWER excluded)
//        every GET                        runs.execute (the metered-action
//                                         permission: exactly the OWNER/ADMIN/
//                                         MEMBER set; those who can trigger
//                                         metered runs can read their quota,
//                                         subscription and invoices)
//
// The tenant is taken from the auth claims ONLY; client-supplied organization
// ids are never trusted and no view carries another tenant's data (the store
// additionally filters every subscription/invoice read by organization_id).
// Response envelopes are {"plan":{...}} / {"plans":[...]} /
// {"subscription":{...}} / {"invoices":[...]} etc.; errors use the shared
// {"error":{"code","message"}} envelope via the local writeBillError helper.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/billing"
)

// registerBillingRoutes mounts the six billing endpoints on apiMux.
func registerBillingRoutes(apiMux *http.ServeMux, svc *billing.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	read := func(next http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionRunsExecute)(next))
	}
	apiMux.Handle("/billing/plans", read(http.HandlerFunc(billingPlansHandler(svc))))
	apiMux.Handle("/billing/subscription", read(http.HandlerFunc(billingSubscriptionHandler(svc))))
	apiMux.Handle("/billing/invoices", read(http.HandlerFunc(billingInvoicesHandler(svc))))
	apiMux.Handle("/billing/invoices/", read(http.HandlerFunc(billingInvoiceDetailHandler(svc))))
	// Writes carry the stricter grants (see the RBAC comment above).
	apiMux.Handle("/billing/subscriptions", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionOrgManage)(http.HandlerFunc(billingSubscribeHandler(svc)))))
	apiMux.Handle("/billing/subscription/cancel", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionUsersManage)(http.HandlerFunc(billingCancelHandler(svc)))))
}

func writeBillJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeBillError(w http.ResponseWriter, status int, code, message string) {
	writeBillJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// writeBillServiceError maps billing service errors onto the contract's error
// envelope; returns false for unmapped errors (caller decides).
func writeBillServiceError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, billing.ErrPlanNotFound):
		writeBillError(w, http.StatusNotFound, "PLAN_NOT_FOUND", err.Error())
	case errors.Is(err, billing.ErrNoSubscription):
		writeBillError(w, http.StatusNotFound, "NO_SUBSCRIPTION", err.Error())
	case errors.Is(err, billing.ErrInvoiceNotFound):
		writeBillError(w, http.StatusNotFound, "INVOICE_NOT_FOUND", err.Error())
	case errors.Is(err, billing.ErrPlanExists), errors.Is(err, billing.ErrPlanInUse),
		errors.Is(err, billing.ErrSubscriptionExists):
		writeBillError(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, billing.ErrSubscriptionCanceled), errors.Is(err, billing.ErrInvalidTransition),
		errors.Is(err, billing.ErrInvalidInvoiceState):
		writeBillError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, billing.ErrInvalidPlan), errors.Is(err, billing.ErrInvalidPeriod),
		errors.Is(err, billing.ErrInvalidUsageRow):
		writeBillError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	default:
		return false
	}
	return true
}

// billGuard returns the tenant from the auth claims and rejects misconfigured
// wiring (nil service) with an honest 503.
func billGuard(w http.ResponseWriter, r *http.Request, svc *billing.Service, method string) (string, bool) {
	if r.Method != method {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return "", false
	}
	if svc == nil {
		writeBillError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "billing service not available")
		return "", false
	}
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeBillError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return "", false
	}
	return claims.OrganizationID, true
}

// decodeBillBody decodes the optional JSON request body (bounded to 1 MiB).
// An empty body is not an error: the destination keeps its zero values (e.g.
// POST /billing/subscription/cancel with no body = deferred cancel).
func decodeBillBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeBillError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Wire views (snake_case; no organization_id — the caller IS the tenant)
// ---------------------------------------------------------------------------

func billPlanView(p *billing.Plan) map[string]any {
	return map[string]any{
		"id":             p.ID,
		"name":           p.Name,
		"price_cents":    p.PriceCents,
		"currency":       p.Currency,
		"included_quota": p.IncludedQuota,
		"metadata":       p.Metadata,
		"created_at":     p.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":     p.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func billSubscriptionView(s *billing.Subscription) map[string]any {
	view := map[string]any{
		"id":                   s.ID,
		"plan_id":              s.PlanID,
		"status":               s.Status,
		"period_start":         s.PeriodStart.UTC().Format(time.RFC3339Nano),
		"period_end":           s.PeriodEnd.UTC().Format(time.RFC3339Nano),
		"cancel_at_period_end": s.CancelAtPeriodEnd,
		"created_at":           s.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":           s.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if s.CanceledAt != nil {
		view["canceled_at"] = s.CanceledAt.UTC().Format(time.RFC3339Nano)
	}
	return view
}

func billQuotaView(q *billing.QuotaStatus) map[string]any {
	return map[string]any{
		"subscription_id": q.SubscriptionID,
		"status":          q.Status,
		"included_runs":   q.IncludedRuns,
		"unlimited":       q.Unlimited,
		"consumed_runs":   q.ConsumedRuns,
		"remaining_runs":  q.RemainingRuns,
		"exceeded":        q.Exceeded,
		"period_start":    q.PeriodStart.UTC().Format(time.RFC3339Nano),
		"period_end":      q.PeriodEnd.UTC().Format(time.RFC3339Nano),
	}
}

func billInvoiceView(inv *billing.Invoice, withLines bool) map[string]any {
	view := map[string]any{
		"id":              inv.ID,
		"subscription_id": inv.SubscriptionID,
		"period_start":    inv.PeriodStart.UTC().Format(time.RFC3339Nano),
		"period_end":      inv.PeriodEnd.UTC().Format(time.RFC3339Nano),
		"subtotal_cents":  inv.SubtotalCents,
		"currency":        inv.Currency,
		"status":          inv.Status,
		"created_at":      inv.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":      inv.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if withLines {
		lines := make([]map[string]any, 0, len(inv.Lines))
		for _, line := range inv.Lines {
			lines = append(lines, map[string]any{
				"id":           line.ID,
				"source":       line.Source,
				"description":  line.Description,
				"quantity":     line.Quantity,
				"amount_cents": line.AmountCents,
				"refs":         line.Refs,
				"created_at":   line.CreatedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		view["lines"] = lines
	}
	return view
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// billingPlansHandler serves GET /billing/plans: the global (non-tenant) plan
// catalog.
func billingPlansHandler(svc *billing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := billGuard(w, r, svc, http.MethodGet); !ok {
			return
		}
		plans, err := svc.ListPlansCtx(r.Context())
		if err != nil {
			if !writeBillServiceError(w, err) {
				writeBillError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		out := make([]map[string]any, 0, len(plans))
		for _, p := range plans {
			out = append(out, billPlanView(p))
		}
		writeBillJSON(w, http.StatusOK, map[string]any{"plans": out})
	}
}

// billingSubscribeHandler serves POST /billing/subscriptions (OWNER only):
// {"plan_id": "..."} -> 201 {"subscription": {...}} in trial state.
func billingSubscribeHandler(svc *billing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := billGuard(w, r, svc, http.MethodPost)
		if !ok {
			return
		}
		var req struct {
			PlanID string `json:"plan_id"`
		}
		if !decodeBillBody(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.PlanID) == "" {
			writeBillError(w, http.StatusBadRequest, "VALIDATION_ERROR", "plan_id is required")
			return
		}
		sub, err := svc.SubscribeCtx(r.Context(), orgID, req.PlanID)
		if err != nil {
			if !writeBillServiceError(w, err) {
				writeBillError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeBillJSON(w, http.StatusCreated, map[string]any{"subscription": billSubscriptionView(sub)})
	}
}

// billingSubscriptionHandler serves GET /billing/subscription: the org's
// current subscription plus its monthly run-budget quota state. Canceled
// history is still readable (the read path falls back to the latest row).
func billingSubscriptionHandler(svc *billing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := billGuard(w, r, svc, http.MethodGet)
		if !ok {
			return
		}
		sub, err := svc.GetCurrentSubscriptionCtx(r.Context(), orgID)
		if err != nil {
			if !writeBillServiceError(w, err) {
				writeBillError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		quota, err := svc.CheckQuotaCtx(r.Context(), orgID)
		if err != nil {
			if !writeBillServiceError(w, err) {
				writeBillError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeBillJSON(w, http.StatusOK, map[string]any{
			"subscription": billSubscriptionView(sub),
			"quota":        billQuotaView(quota),
		})
	}
}

// billingCancelHandler serves POST /billing/subscription/cancel (OWNER/ADMIN):
// {"immediate": true} cancels NOW; {"immediate": false} (the default, empty
// body included) schedules the cancel for the end of the running period.
func billingCancelHandler(svc *billing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := billGuard(w, r, svc, http.MethodPost)
		if !ok {
			return
		}
		var req struct {
			Immediate bool `json:"immediate"`
		}
		if !decodeBillBody(w, r, &req) {
			return
		}
		sub, err := svc.CancelSubscriptionCtx(r.Context(), orgID, req.Immediate)
		if err != nil {
			if !writeBillServiceError(w, err) {
				writeBillError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeBillJSON(w, http.StatusOK, map[string]any{"subscription": billSubscriptionView(sub)})
	}
}

// billingInvoicesHandler serves GET /billing/invoices: the org's invoices,
// newest first (lines omitted — use the detail endpoint for the document).
func billingInvoicesHandler(svc *billing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := billGuard(w, r, svc, http.MethodGet)
		if !ok {
			return
		}
		invoices, err := svc.ListInvoicesCtx(r.Context(), orgID)
		if err != nil {
			if !writeBillServiceError(w, err) {
				writeBillError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		out := make([]map[string]any, 0, len(invoices))
		for _, inv := range invoices {
			out = append(out, billInvoiceView(inv, false))
		}
		writeBillJSON(w, http.StatusOK, map[string]any{"invoices": out})
	}
}

// billingInvoiceDetailHandler serves GET /billing/invoices/{id}: one
// tenant-scoped invoice with its lines.
func billingInvoiceDetailHandler(svc *billing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := billGuard(w, r, svc, http.MethodGet)
		if !ok {
			return
		}
		id := trimRoutePrefix(r.URL.Path, "/billing/invoices/")
		if id == "" || strings.Contains(id, "/") {
			writeBillError(w, http.StatusNotFound, "INVOICE_NOT_FOUND", "invoice not found")
			return
		}
		inv, err := svc.GetInvoiceCtx(r.Context(), orgID, id)
		if err != nil {
			if !writeBillServiceError(w, err) {
				writeBillError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeBillJSON(w, http.StatusOK, map[string]any{"invoice": billInvoiceView(inv, true)})
	}
}
