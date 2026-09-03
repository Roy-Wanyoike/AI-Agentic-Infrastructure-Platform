package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/runs"
)

// Usage-costs HTTP surface (wave 3, track 3-b).
//
// Contract endpoint (mounted on apiMux, served under BOTH /v1 and /api/v1):
//
//      GET /usage/costs?from=&to=&group_by=day|agent|model   (usage.read)
//
// Aggregates runs.cost_cents for the caller's organization over the half-open
// [from, to) window. Response (snake_case):
//
//      {"total_cost_cents": 0, "series": [{"bucket": "2026-09-03",
//        "agent_id": "…", "model": "…", "cost_cents": 0, "runs": 0}]}
//
// `bucket` is present for group_by=day; `agent_id`/`model` for the other
// groupings (omitted otherwise). Machine errors use the standard error
// envelope: INVALID_GROUP_BY (400, unknown group_by) and INVALID_TIME_RANGE
// (400, unparsable or inverted window).

const (
	// usageCostsDefaultWindow bounds the report when from/to are omitted.
	usageCostsDefaultWindow = 30 * 24 * time.Hour
	// usageCostsMaxDays caps a single report window so one request cannot
	// scan the whole history of a very large tenant (defense in depth).
	usageCostsMaxDays = 366
)

// registerUsageCostsRoutes mounts GET /usage/costs behind usage.read.
func registerUsageCostsRoutes(apiMux *http.ServeMux, runsSvc *runs.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	apiMux.Handle("/usage/costs", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionUsageRead)(http.HandlerFunc(usageCostsHandler(runsSvc)))))
}

func writeUsgJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeUsgError(w http.ResponseWriter, status int, code, message string) {
	writeUsgJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// parseUsgWindowTime accepts RFC3339 timestamps and bare UTC dates
// ("2006-01-02"); anything else is an invalid window.
func parseUsgWindowTime(raw string) (time.Time, bool) {
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

func usageCostsHandler(runsSvc *runs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if runsSvc == nil {
			writeUsgError(w, http.StatusServiceUnavailable, "USAGE_UNAVAILABLE", "runs service not available")
			return
		}

		groupBy := runs.CostGroupBy(strings.TrimSpace(r.URL.Query().Get("group_by")))
		if groupBy == "" {
			groupBy = runs.CostGroupByDay
		}
		switch groupBy {
		case runs.CostGroupByDay, runs.CostGroupByAgent, runs.CostGroupByModel:
		default:
			writeUsgError(w, http.StatusBadRequest, "INVALID_GROUP_BY",
				"group_by must be one of day, agent, model")
			return
		}

		now := time.Now().UTC()
		to := now
		if raw := r.URL.Query().Get("to"); strings.TrimSpace(raw) != "" {
			parsed, ok := parseUsgWindowTime(raw)
			if !ok {
				writeUsgError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
					"to must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			to = parsed
		}
		from := to.Add(-usageCostsDefaultWindow)
		if raw := r.URL.Query().Get("from"); strings.TrimSpace(raw) != "" {
			parsed, ok := parseUsgWindowTime(raw)
			if !ok {
				writeUsgError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
					"from must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			from = parsed
		}
		if !to.After(from) {
			writeUsgError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
				"to must be after from")
			return
		}
		if to.Sub(from).Hours()/24 > usageCostsMaxDays {
			writeUsgError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
				"window exceeds the maximum of 366 days")
			return
		}

		// Tenant guard: the aggregation filters on the caller's organization_id.
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}

		series, total, err := runsSvc.AggregateCostsCtx(r.Context(), orgID, from, to, groupBy)
		if err != nil {
			if errors.Is(err, runs.ErrInvalidGroupBy) {
				writeUsgError(w, http.StatusBadRequest, "INVALID_GROUP_BY", err.Error())
				return
			}
			writeUsgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		if series == nil {
			series = []runs.CostBucket{}
		}
		// Contract shape (snake_case, exact):
		// {"total_cost_cents": 0, "series": [{"bucket": "2026-09-03",
		//   "agent_id": "…", "model": "…", "cost_cents": 0, "runs": 0}]}
		writeUsgJSON(w, http.StatusOK, map[string]any{
			"total_cost_cents": total,
			"series":           series,
		})
	}
}
