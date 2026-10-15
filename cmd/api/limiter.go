package main

// limiter.go — issue #124 HTTP wiring: the granular rate-limit surface
// (management API) plus the create-run enforcement gate, on top of
// internal/limiter (sliding-window engine, dual-mode config store).
//
// Endpoints (registered on apiMux by registerLimiterRoutes; served under
// BOTH /v1 and /api/v1 like every versioned surface):
//
//	GET    /rate-limits                list the org's resolved limits (all roles)
//	PUT    /rate-limits/{scope}        upsert the scope's ORG DEFAULT row (OWNER/ADMIN)
//	PUT    /rate-limits/{scope}/{id}   upsert a subject-specific row (OWNER/ADMIN)
//	DELETE /rate-limits/{scope}        delete the scope's ORG DEFAULT row (OWNER/ADMIN)
//	DELETE /rate-limits/{scope}/{id}   delete a subject-specific row (OWNER/ADMIN)
//
// RBAC: NEW permissions limiter.read (all roles) / limiter.write
// (OWNER/ADMIN) registered in internal/auth/permissions_limiter.go via the
// init() append pattern.
//
// Tenant scope comes EXCLUSIVELY from the auth claims (the residency.go
// convention): there are no org-addressable path ids, so a caller can never
// name a foreign tenant. Validation failures answer 422 VALIDATION_ERROR
// (the versions.go convention); a DELETE of an absent row answers 404
// not_found; the run-creation gate answers 429 with the structured error
// envelope {"error":{"code":"rate_limited","scope":...,"retry_after":s}} and
// a Retry-After header, mirroring the global limiter's 429 shape.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/limiter"
)

// limiterGate is the consumer-side hook seam for run creation (issue #124:
// "hooks behind a Limiter interface with a no-op default"). *limiter.Enforcer
// satisfies it. When limiterGateVar is nil (tests that construct handlers
// without opting in, or a half-wired rollout) run creation keeps the legacy
// always-allow behavior.
type limiterGate interface {
	AllowRun(ctx context.Context, orgID, agentID string) limiter.Decision
}

// limiterGateVar mirrors the runsServiceVar/billingServiceVar/policyEnforcementVar
// wiring precedent: newApp assigns it after construction and tests wire it
// directly. There is deliberately no package-level no-op fallback: a nil var
// IS the no-op default (no checks, no behavior change).
var limiterGateVar limiterGate

// enforceRunRateLimits is the create-run granular rate-limit gate
// (issue #124, org + agent scopes). It runs AFTER the governance policy and
// billing gates and BEFORE any run row or queue task exists, so a refusal
// leaves no run behind. Decision matrix:
//
//   - seam unwired (limiterGateVar nil) -> allow (legacy behavior)
//   - allowed                           -> proceed
//   - denied                            -> 429 with the structured error
//     envelope {error:{code:"rate_limited",scope,retry_after}}, a
//     Retry-After header (seconds, ceil, >= 1 — the httpx convention) and
//     NO run row / queue task
//
// Infrastructure failures inside the gate are already resolved by the
// engine's fail mode (AGENTOS_LIMITER_FAIL_MODE, default open — see
// internal/limiter), so this seam never turns a limiter outage into a 5xx.
func enforceRunRateLimits(w http.ResponseWriter, r *http.Request, orgID, agentID string) bool {
	gate := limiterGateVar
	if gate == nil {
		return true
	}
	decision := gate.AllowRun(r.Context(), orgID, agentID)
	if decision.Allowed {
		return true
	}
	writeRateLimitedError(w, r, decision)
	return false
}

// writeRateLimitedError emits the 429 envelope: the house error shape with
// the two issue-#124 additions (scope, retry_after) inside error, plus the
// Retry-After header.
func writeRateLimitedError(w http.ResponseWriter, r *http.Request, decision limiter.Decision) {
	seconds := limiter.CeilSeconds(decision.RetryAfter)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":        limiter.ReasonRateLimited,
			"message":     rateLimitedMessage(decision),
			"scope":       string(decision.Scope),
			"retry_after": seconds,
		},
	})
}

// rateLimitedMessage renders the human-facing denial text (never empty).
func rateLimitedMessage(decision limiter.Decision) string {
	msg := strings.TrimSpace(decision.Reason)
	if msg == "" {
		msg = "rate limit exceeded"
	}
	if decision.RetryAfter > 0 {
		msg += "; retry in " + strconv.Itoa(limiter.CeilSeconds(decision.RetryAfter)) + " seconds"
	}
	return msg
}

// registerLimiterRoutes mounts the granular rate-limit management surface
// (nil-service guard: a nil Enforcer registers nothing, the mcp registry
// precedent, so partial wirings cannot panic).
func registerLimiterRoutes(apiMux *http.ServeMux, svc *limiter.Enforcer, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// auth wrap pattern from cmd/api/residency.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	// Reads: every role (limiter.read) — limits are configuration, not
	// credentials; dashboards and runbooks render them.
	apiMux.Handle("GET /rate-limits", wrap(auth.PermissionLimiterRead, http.HandlerFunc(rateLimitsListHandler(svc))))
	// Writes: OWNER/ADMIN (limiter.write) — a change re-throttles the org.
	apiMux.Handle("PUT /rate-limits/{scope}", wrap(auth.PermissionLimiterWrite, http.HandlerFunc(rateLimitUpsertHandler(svc))))
	apiMux.Handle("PUT /rate-limits/{scope}/{id}", wrap(auth.PermissionLimiterWrite, http.HandlerFunc(rateLimitUpsertHandler(svc))))
	apiMux.Handle("DELETE /rate-limits/{scope}", wrap(auth.PermissionLimiterWrite, http.HandlerFunc(rateLimitDeleteHandler(svc))))
	apiMux.Handle("DELETE /rate-limits/{scope}/{id}", wrap(auth.PermissionLimiterWrite, http.HandlerFunc(rateLimitDeleteHandler(svc))))
}

// --- serialization (distinct names; the residency.go envelope convention) ---

func writeLimJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeLimError(w http.ResponseWriter, status int, code, message string) {
	writeLimJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// decodeLimBody decodes the JSON request body bounded to 1 MiB (the
// decodeResBody convention). Returns false when the response was written.
func decodeLimBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	defer r.Body.Close() //nolint:errcheck // read path; nothing actionable
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeLimError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeLimError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return false
	}
	return true
}

// rateLimitConfigJSON renders one row (explicit field allowlist; the
// org-default row surfaces subject_id null — the wire form of the SQL NULL).
func rateLimitConfigJSON(cfg limiter.Config) map[string]any {
	var subject any
	if cfg.Subject != "" {
		subject = cfg.Subject
	}
	return map[string]any{
		"scope":      string(cfg.Scope),
		"subject_id": subject,
		"rpm":        cfg.RPM,
		"rpd":        cfg.RPD,
		"tpm":        cfg.TPM,
		"enabled":    cfg.Enabled,
		"created_at": cfg.CreatedAt,
		"updated_at": cfg.UpdatedAt,
	}
}

// rateLimitsListHandler answers GET /rate-limits: every stored row of the
// caller's org (org defaults + subject-specific rows; a scope with no rows
// is unlimited by omission). Rows carry an "org_default" flag so callers can
// tell the resolution tiers apart without re-deriving them.
func rateLimitsListHandler(svc *limiter.Enforcer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeLimError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		if claims.OrganizationID == "" {
			writeLimError(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
			return
		}
		rows, err := svc.ListConfigs(r.Context(), claims.OrganizationID)
		if err != nil {
			writeLimError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list rate limits")
			return
		}
		items := make([]map[string]any, 0, len(rows))
		for _, cfg := range rows {
			item := rateLimitConfigJSON(cfg)
			item["org_default"] = cfg.Subject == ""
			items = append(items, item)
		}
		writeLimJSON(w, http.StatusOK, map[string]any{"limits": items})
	}
}

// rateLimitUpsertHandler answers PUT /rate-limits/{scope}/{id?}: insert or
// update one row. Validation (422 VALIDATION_ERROR): unknown scope, a
// subject id on the org scope, negative limits, and an all-zero enabled row
// (stage disabled rows instead — zero limits mean unlimited, so a zero row
// would be a confusing no-op).
func rateLimitUpsertHandler(svc *limiter.Enforcer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeLimError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		if claims.OrganizationID == "" {
			writeLimError(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
			return
		}
		scope, ok := limiter.ParseScope(r.PathValue("scope"))
		if !ok {
			writeLimError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"scope must be one of org|agent|tool|model")
			return
		}
		subject := strings.TrimSpace(r.PathValue("id"))
		if scope == limiter.ScopeOrg && subject != "" {
			writeLimError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"the org scope has no subject id (use PUT /rate-limits/org)")
			return
		}
		var body struct {
			RPM     *int  `json:"rpm"`
			RPD     *int  `json:"rpd"`
			TPM     *int  `json:"tpm"`
			Enabled *bool `json:"enabled"`
		}
		if !decodeLimBody(w, r, &body) {
			return
		}
		cfg := limiter.Config{
			Scope:   scope,
			Subject: subject,
			Enabled: true,
		}
		if body.Enabled != nil {
			cfg.Enabled = *body.Enabled
		}
		for _, f := range []struct {
			val   *int
			field string
			dst   *int
		}{
			{body.RPM, "rpm", &cfg.RPM},
			{body.RPD, "rpd", &cfg.RPD},
			{body.TPM, "tpm", &cfg.TPM},
		} {
			if f.val == nil {
				continue
			}
			if *f.val < 0 {
				writeLimError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
					f.field+" must be >= 0 (0 = unlimited)")
				return
			}
			*f.dst = *f.val
		}
		if cfg.Enabled && cfg.RPM == 0 && cfg.RPD == 0 && cfg.TPM == 0 {
			writeLimError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"at least one of rpm, rpd, tpm must be positive (or set enabled=false to stage the row)")
			return
		}
		if err := svc.UpsertConfig(r.Context(), claims.OrganizationID, cfg); err != nil {
			if errors.Is(err, limiter.ErrInvalidConfig) {
				writeLimError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
				return
			}
			writeLimError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to store rate limit")
			return
		}
		// Read-back keeps the response honest (timestamps, normalized shape).
		stored, err := svc.ListConfigs(r.Context(), claims.OrganizationID)
		if err != nil {
			writeLimError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to reload rate limits")
			return
		}
		for _, row := range stored {
			if row.Scope == scope && row.Subject == subject {
				writeLimJSON(w, http.StatusOK, map[string]any{"limit": rateLimitConfigJSON(row)})
				return
			}
		}
		slog.Warn("rate limit upsert read-back missed", "scope", string(scope), "subject", subject)
		writeLimJSON(w, http.StatusOK, map[string]any{"limit": rateLimitConfigJSON(cfg)})
	}
}

// rateLimitDeleteHandler answers DELETE /rate-limits/{scope}/{id?}: removes
// the row (the org default when no id). Absent rows answer 404 not_found.
func rateLimitDeleteHandler(svc *limiter.Enforcer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeLimError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		if claims.OrganizationID == "" {
			writeLimError(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
			return
		}
		scope, ok := limiter.ParseScope(r.PathValue("scope"))
		if !ok {
			writeLimError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"scope must be one of org|agent|tool|model")
			return
		}
		subject := strings.TrimSpace(r.PathValue("id"))
		if scope == limiter.ScopeOrg && subject != "" {
			writeLimError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"the org scope has no subject id (use DELETE /rate-limits/org)")
			return
		}
		if err := svc.DeleteConfig(r.Context(), claims.OrganizationID, scope, subject); err != nil {
			if errors.Is(err, limiter.ErrConfigNotFound) {
				writeLimError(w, http.StatusNotFound, "NOT_FOUND", "rate limit not found")
				return
			}
			writeLimError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete rate limit")
			return
		}
		writeLimJSON(w, http.StatusOK, map[string]any{"deleted": true, "scope": string(scope), "subject_id": subject})
	}
}
