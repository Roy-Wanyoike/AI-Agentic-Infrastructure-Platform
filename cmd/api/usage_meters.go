package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/billing"
)

// Usage-meter + margin HTTP surface (issue #57).
//
// Endpoints (registered on apiMux by registerUsageMetersRoutes; served under
// BOTH /v1 and /api/v1):
//
//      GET /usage/meters?from=&to= -> aggregated usage meters for the caller's
//                                   org over the half-open [from, to) window
//                                   (runs_count, tool_calls_count; see the
//                                   sandbox note below)
//      GET /billing/margin         -> cost basis vs plan price for the CURRENT
//                                   subscription period (owner-only finance view)
//
// RBAC (documented permission REUSE — internal/auth is outside this track's
// ownership, so no dedicated permission could be introduced; the convention
// mirrors cmd/api/billing.go exactly):
//
//      GET /usage/meters  runs.execute (the metered-action permission: exactly
//                         OWNER/ADMIN/MEMBER, i.e. MEMBER+ per the issue — the
//                         same grant set as every billing read)
//      GET /billing/margin organization.manage (OWNER only — margin is the
//                         org's financial data)
//
// Stripe usage sync (env-driven, ASYNC, never on the run path): after a
// successful meters read the handler fires syncer.SyncUsage in a
// recover-guarded goroutine — enabled only when STRIPE_API_KEY is set
// (billing.NewStripeSyncerFromEnv returns a zero-network NoopSyncer
// otherwise). There is deliberately NO manual POST trigger: v1 stays
// read-only for meters/margin, so the sync is system behavior triggered by a
// read, not a user-initiated state change, and is LOGGED rather than audited
// (the audit trail records user actions; per-read sync noise would bury it).
// The syncer is idempotent per (org, period, meter) — see
// internal/billing/stripe.go — so repeated reads of the same window cannot
// double count on Stripe's side.
//
// SANDBOX SECONDS — documented omission (never invent data): the platform
// records no durable sandbox execution duration today, so no sandbox_seconds
// meter exists; see internal/billing/meters.go.
//
// Response envelopes are {"from","to","meters":{...}} and {"margin":{...}};
// errors use the shared {"error":{"code","message"}} envelope via the
// writeBillError helper from billing.go (same package).

// usageMeterSyncTimeout bounds one async Stripe sync (the client adds its own
// per-request HTTP timeout; this covers the whole retry schedule).
const usageMeterSyncTimeout = 30 * time.Second

// registerUsageMetersRoutes mounts the two endpoints on apiMux. meterSrc and
// syncer are optional (nil meterSrc degrades the meters read to an honest
// 503; a nil/disabled syncer makes the trigger a no-op — production wiring
// passes billing.NewStripeSyncerFromEnv which returns the NoopSyncer unless
// STRIPE_API_KEY is set).
func registerUsageMetersRoutes(apiMux *http.ServeMux, billingSvc *billing.Service, meterSrc billing.MeterSource, syncer billing.StripeSyncer, authSvc *auth.Service, apiKeysSvc *apikeys.Service, logr *slog.Logger) {
	if apiMux == nil {
		return
	}
	apiMux.Handle("/usage/meters", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionRunsExecute)(http.HandlerFunc(usageMetersHandler(billingSvc, meterSrc, syncer, logr)))))
	apiMux.Handle("/billing/margin", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionOrgManage)(http.HandlerFunc(billingMarginHandler(billingSvc)))))
}

// usageMetersHandler serves GET /usage/meters?from=&to=: the org's aggregated
// meters over the half-open [from, to) window. Window parsing mirrors
// GET /usage/costs (RFC3339 timestamps or bare UTC dates; default last 30
// days; max 366 days) so both metering reads behave identically.
func usageMetersHandler(billingSvc *billing.Service, meterSrc billing.MeterSource, syncer billing.StripeSyncer, logr *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// billGuard (billing.go): method check, nil-service 503, claims org.
		orgID, ok := billGuard(w, r, billingSvc, http.MethodGet)
		if !ok {
			return
		}
		if meterSrc == nil {
			writeBillError(w, http.StatusServiceUnavailable, "METER_SOURCE_UNAVAILABLE", "usage meter source not available")
			return
		}

		now := time.Now().UTC()
		to := now
		if raw := r.URL.Query().Get("to"); strings.TrimSpace(raw) != "" {
			parsed, ok := parseUsgWindowTime(raw)
			if !ok {
				writeBillError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
					"to must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			to = parsed
		}
		from := to.Add(-usageCostsDefaultWindow)
		if raw := r.URL.Query().Get("from"); strings.TrimSpace(raw) != "" {
			parsed, ok := parseUsgWindowTime(raw)
			if !ok {
				writeBillError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
					"from must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			from = parsed
		}
		if !to.After(from) {
			writeBillError(w, http.StatusBadRequest, "INVALID_TIME_RANGE", "to must be after from")
			return
		}
		if to.Sub(from).Hours()/24 > usageCostsMaxDays {
			writeBillError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
				"window exceeds the maximum of 366 days")
			return
		}

		// Tenant guard: the aggregation reads exactly the caller's organization.
		meters, err := meterSrc.MetersForPeriod(r.Context(), orgID, from, to)
		if err != nil {
			if !writeBillServiceError(w, err) {
				writeBillError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}

		// Env-driven Stripe sync: fire-and-forget AFTER the read succeeded,
		// never on the run path, never blocking this response. Disabled
		// syncers (NoopSyncer) skip the goroutine entirely.
		triggerStripeUsageSync(syncer, logr, orgID, from, to, meters)

		// Contract shape (snake_case; sandbox_seconds intentionally absent —
		// see the header note): {"from","to","meters":{"runs_count":0,
		// "tool_calls_count":0}}
		writeBillJSON(w, http.StatusOK, map[string]any{
			"from": from.UTC().Format(time.RFC3339Nano),
			"to":   to.UTC().Format(time.RFC3339Nano),
			"meters": map[string]any{
				billing.MeterRunsCount:      meters.RunsCount,
				billing.MeterToolCallsCount: meters.ToolCallsCount,
			},
		})
	}
}

// billingMarginHandler serves GET /billing/margin (OWNER only): the margin of
// the org's current subscription period — plan price (revenue side) vs the
// metered usage cost (cost basis), computed by billing.Service.ComputeMarginCtx
// (exact formula documented there and in the OpenAPI fragment).
func billingMarginHandler(billingSvc *billing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := billGuard(w, r, billingSvc, http.MethodGet)
		if !ok {
			return
		}
		report, err := billingSvc.ComputeMarginCtx(r.Context(), orgID)
		if err != nil {
			if !writeBillServiceError(w, err) {
				writeBillError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeBillJSON(w, http.StatusOK, map[string]any{"margin": billMarginView(report)})
	}
}

// billMarginView renders the margin report (snake_case; no organization_id —
// the caller IS the tenant). margin_percent is omitted when the plan price is
// 0 (a percentage of zero revenue is undefined, never fabricated).
func billMarginView(m *billing.MarginReport) map[string]any {
	view := map[string]any{
		"subscription_id":  m.SubscriptionID,
		"status":           m.Status,
		"period_start":     m.PeriodStart.UTC().Format(time.RFC3339Nano),
		"period_end":       m.PeriodEnd.UTC().Format(time.RFC3339Nano),
		"currency":         m.Currency,
		"price_cents":      m.PriceCents,
		"usage_cost_cents": m.UsageCostCents,
		"margin_cents":     m.MarginCents,
		"plan": map[string]any{
			"id":             m.PlanID,
			"name":           m.PlanName,
			"price_cents":    m.PriceCents,
			"currency":       m.Currency,
			"included_quota": m.IncludedQuota,
			"unlimited":      m.Unlimited,
		},
	}
	if m.MarginPercent != nil {
		view["margin_percent"] = *m.MarginPercent
	}
	return view
}

// triggerStripeUsageSync fires the env-gated async sync. Recover-guarded:
// a panicking syncer must never take the API process down from a read path.
func triggerStripeUsageSync(syncer billing.StripeSyncer, logr *slog.Logger, orgID string, from, to time.Time, meters *billing.Meters) {
	if syncer == nil || !billing.SyncerEnabled(syncer) {
		return
	}
	if logr == nil {
		logr = slog.Default()
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				logr.Error("stripe usage sync panicked", "org_id", orgID, "panic", rec)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), usageMeterSyncTimeout)
		defer cancel()
		if err := syncer.SyncUsage(ctx, orgID, from, to, meters); err != nil {
			logr.Warn("stripe usage sync failed", "org_id", orgID, "error", err.Error())
			return
		}
		logr.Debug("stripe usage sync complete", "org_id", orgID)
	}()
}
