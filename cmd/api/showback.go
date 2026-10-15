package main

// Showback HTTP surface (wave-8 issue C: showback/chargeback reports).
//
// Endpoint (registered on apiMux by registerShowbackRoutes; served under BOTH
// /v1 and /api/v1):
//
//      GET /showback?from=&to=&group_by=agent|team
//
// Aggregates the caller's organization metered usage over the half-open
// [from, to) window from the EXISTING runs cost ledger (the same numbers as
// GET /usage/costs?group_by=agent of the same window). Response (snake_case):
//
//      {"from": "...", "to": "...", "group_by": "agent",
//       "rows": [{"scope_ref": "…", "runs": 0, "tool_calls": 0,
//                 "tokens_in": 0, "tokens_out": 0, "cost_cents": 0}],
//       "notes": ["…"]}
//
// from/to accept RFC3339 timestamps or bare UTC dates (mirroring
// usage_costs.go); omitted values default to a trailing 30-day window. The
// window is validated (from < to, max 92 days) and unknown group_by is a 400
// INVALID_GROUP_BY. Honest limitations travel in "notes" (team fallback,
// unattributable tool-call/token counters — see internal/billing/showback.go).
//
// RBAC: usage.read (the GET /usage/costs grant — OWNER/ADMIN/MEMBER/VIEWER).
// Showback is a READ-ONLY cost report in the usage-costs family; the budget
// write tier (users.manage) does not apply because there is nothing to
// write. The tenant comes from the auth claims ONLY.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/billing"
)

const (
	// showbackDefaultWindow bounds the report when from/to are omitted
	// (mirrors usageCostsDefaultWindow).
	showbackDefaultWindow = 30 * 24 * time.Hour
)

// registerShowbackRoutes mounts GET /showback behind usage.read.
func registerShowbackRoutes(apiMux *http.ServeMux, source billing.ShowbackSource, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	svc := billing.NewShowbackService(source)
	apiMux.Handle("/showback", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionUsageRead)(http.HandlerFunc(showbackHandler(svc)))))
}

func writeSbJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeSbError(w http.ResponseWriter, status int, code, message string) {
	writeSbJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// parseSbWindowTime accepts RFC3339 timestamps and bare UTC dates
// ("2006-01-02"); anything else is an invalid window (mirrors
// parseUsgWindowTime).
func parseSbWindowTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func showbackRowView(row *billing.ShowbackRow) map[string]any {
	return map[string]any{
		"scope_ref":  row.ScopeRef,
		"runs":       row.Runs,
		"tool_calls": row.ToolCalls,
		"tokens_in":  row.TokensIn,
		"tokens_out": row.TokensOut,
		"cost_cents": row.CostCents,
	}
}

func showbackHandler(svc *billing.ShowbackService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if svc == nil {
			writeSbError(w, http.StatusServiceUnavailable, "SHOWBACK_UNAVAILABLE", "showback service not available")
			return
		}

		groupBy := strings.TrimSpace(r.URL.Query().Get("group_by"))
		if groupBy == "" {
			groupBy = billing.ShowbackGroupByAgent
		}

		now := time.Now().UTC()
		to := now
		if raw := r.URL.Query().Get("to"); strings.TrimSpace(raw) != "" {
			parsed, ok := parseSbWindowTime(raw)
			if !ok {
				writeSbError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
					"to must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			to = parsed
		}
		from := to.Add(-showbackDefaultWindow)
		if raw := r.URL.Query().Get("from"); strings.TrimSpace(raw) != "" {
			parsed, ok := parseSbWindowTime(raw)
			if !ok {
				writeSbError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
					"from must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			from = parsed
		}
		if !to.After(from) {
			writeSbError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
				"to must be after from")
			return
		}
		if to.Sub(from).Hours()/24 > billing.ShowbackMaxWindowDays {
			writeSbError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
				"window exceeds the maximum of 92 days")
			return
		}

		// Tenant guard: the aggregation filters on the caller's organization_id.
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSbError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}

		report, err := svc.ShowbackReport(r.Context(), claims.OrganizationID, from, to, groupBy)
		if err != nil {
			switch {
			case errors.Is(err, billing.ErrInvalidShowbackGroupBy):
				writeSbError(w, http.StatusBadRequest, "INVALID_GROUP_BY", err.Error())
			case errors.Is(err, billing.ErrInvalidPeriod):
				writeSbError(w, http.StatusBadRequest, "INVALID_TIME_RANGE", err.Error())
			default:
				writeSbError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		rows := make([]map[string]any, 0, len(report.Rows))
		for _, row := range report.Rows {
			rows = append(rows, showbackRowView(row))
		}
		notes := report.Notes
		if notes == nil {
			notes = []string{}
		}
		writeSbJSON(w, http.StatusOK, map[string]any{
			"from":     report.From.UTC().Format(time.RFC3339Nano),
			"to":       report.To.UTC().Format(time.RFC3339Nano),
			"group_by": report.GroupBy,
			"rows":     rows,
			"notes":    notes,
		})
	}
}
