package main

// residency.go — wave-9 issue #140 HTTP wiring: the org data-residency
// surface (region pinning, posture, explicit checks) on top of
// internal/tenant/residency.go.
//
// Endpoints (registered on apiMux by registerResidencyRoutes; served under
// BOTH /v1 and /api/v1 like every versioned surface):
//
//      GET  /residency/policy    effective policy + pinned/unpinned mode (MEMBER+)
//      PUT  /residency/policy    {pinned_region, allowed_regions, strict_mode} (OWNER/ADMIN)
//      GET  /residency/posture   {mode, pinned_region, allowed_regions, strict_mode, last_violation} (OWNER/ADMIN)
//      POST /residency/check     {url} -> verdict; strict deny = 403 residency_violation (OWNER/ADMIN)
//
// Design (mirrors cmd/api/mcp_trust.go / budgets.go):
//   - RBAC reuses EXISTING grants (internal/auth is outside this track's
//     ownership — the permissions_connectors.go / budgets.go documented-reuse
//     pattern): policy reads are connectors.read (MEMBER+; integration
//     topology tier, VIEWER excluded); policy writes, posture and explicit
//     checks are users.manage (OWNER/ADMIN; org security configuration is an
//     account-administration decision, same tier as budget ceilings).
//   - Tenant scope comes EXCLUSIVELY from the auth claims: there are no
//     org-addressable path ids on this surface, so a caller can never name a
//     foreign tenant (cross-tenant 404 convention is enforced one layer down
//     by the org-guarded Store, exercised in internal/tenant tests).
//   - Response rendering is an explicit field allowlist. Violation evidence
//     carries url_host ONLY — never the full URL or its query string (the
//     service layer already strips it; this surface never re-derives it).
//   - Strict-deny verdicts surface as 403 {"error":{"code":
//     "residency_violation", ...}} — the issue #140 contract shape, mirroring
//     the policies denial style (403 {"error":{"code":"policy_denied"}}).
//   - Audit: the SERVICE owns every residency event (residency.check /
//     residency.violation / residency.policy_updated) through its injected
//     AuditPublisher — wired here (SetAuditPublisher) so future MCP/A2A
//     guard wiring emits through the same seam. Best-effort, house style.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/tenant"
)

// registerResidencyRoutes mounts the residency surface and wires the
// service-level seams (audit publisher + actor resolver) to THIS api
// process's audit service — one wiring point for every future Check caller.
func registerResidencyRoutes(apiMux *http.ServeMux, svc *tenant.ResidencyService, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// Issue #140 seam: all residency evidence flows through the injected
	// AuditPublisher (LogCtx convention), covering HTTP checks today and the
	// MCP/A2A guard wiring in a follow-up.
	svc.SetAuditPublisher(auditSvc)
	svc.SetActorResolver(func(ctx context.Context) string {
		if claims, err := auth.ExtractClaims(ctx); err == nil {
			return claims.UserID
		}
		return ""
	})

	// auth wrap pattern from cmd/api/main.go / mcp_trust.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	// Policy reads: MEMBER+ (integration topology tier, connectors.read).
	apiMux.Handle("GET /residency/policy", wrap(auth.PermissionConnectorsRead, http.HandlerFunc(residencyPolicyGetHandler(svc))))
	// Writes: OWNER/ADMIN (users.manage — org security configuration).
	apiMux.Handle("PUT /residency/policy", wrap(auth.PermissionUsersManage, http.HandlerFunc(residencyPolicyPutHandler(svc))))
	// Posture + explicit check: OWNER/ADMIN (issue #140 contract).
	apiMux.Handle("GET /residency/posture", wrap(auth.PermissionUsersManage, http.HandlerFunc(residencyPostureHandler(svc))))
	apiMux.Handle("POST /residency/check", wrap(auth.PermissionUsersManage, http.HandlerFunc(residencyCheckHandler(svc))))
}

// --- serialization helpers (distinct names; same envelope as budgets.go) ----

func writeResJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeResError(w http.ResponseWriter, status int, code, message string) {
	writeResJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// decodeResBody decodes the JSON request body bounded to 1 MiB (the
// decodeBdgBody convention). Returns false when the response was written.
func decodeResBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	defer r.Body.Close() //nolint:errcheck // read path; nothing actionable
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeResError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeResError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return false
	}
	return true
}

// residencyClaims extracts the caller claims or writes the 401/404 shape,
// returning ok=false when the response was already written.
func residencyClaims(w http.ResponseWriter, r *http.Request) (auth.UserClaims, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeResError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return auth.UserClaims{}, false
	}
	if claims.OrganizationID == "" {
		writeResError(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
		return auth.UserClaims{}, false
	}
	return claims, true
}

// --- rendering (explicit field allowlists) -----------------------------------

func residencyPolicyJSON(p *tenant.ResidencyPolicy) map[string]any {
	allowed := []string{}
	if p != nil {
		allowed = append(allowed, p.AllowedRegions...)
	}
	out := map[string]any{
		"org_id":          "",
		"pinned_region":   "",
		"allowed_regions": allowed,
		"strict_mode":     false,
		"created_at":      time.Time{}.UTC().Format(time.RFC3339),
		"updated_at":      time.Time{}.UTC().Format(time.RFC3339),
	}
	if p == nil {
		return out
	}
	out["org_id"] = p.OrgID
	out["pinned_region"] = string(p.PinnedRegion)
	out["strict_mode"] = p.StrictMode
	if !p.CreatedAt.IsZero() {
		out["created_at"] = p.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !p.UpdatedAt.IsZero() {
		out["updated_at"] = p.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func residencyViolationJSON(v *tenant.ViolationRecord) any {
	if v == nil {
		return nil
	}
	return map[string]any{
		"at":       v.At.UTC().Format(time.RFC3339),
		"url_host": v.URLHost,
		"region":   string(v.Region),
		"reason":   v.Reason,
	}
}

// --- handlers -----------------------------------------------------------------

// residencyPolicyGetHandler renders the effective policy (never 404: an org
// without a stored policy IS the documented unpinned default).
func residencyPolicyGetHandler(svc *tenant.ResidencyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := residencyClaims(w, r)
		if !ok {
			return
		}
		policy, err := svc.GetPolicy(r.Context(), claims.OrganizationID)
		if err != nil {
			writeResError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		mode := tenant.ModeUnpinned
		if policy.IsPinned() {
			mode = tenant.ModePinned
		}
		writeResJSON(w, http.StatusOK, map[string]any{"policy": residencyPolicyJSON(policy), "mode": mode})
	}
}

// residencyPolicyPutHandler upserts the org policy (OWNER/ADMIN). Validation
// failures are 400 VALIDATION_ERROR; the service emits the
// residency.policy_updated audit row.
func residencyPolicyPutHandler(svc *tenant.ResidencyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := residencyClaims(w, r)
		if !ok {
			return
		}
		var in tenant.PolicyUpdate
		if !decodeResBody(w, r, &in) {
			return
		}
		policy, err := svc.UpdatePolicy(r.Context(), claims.OrganizationID, in)
		if err != nil {
			switch {
			case errors.Is(err, tenant.ErrOrgRequired):
				writeResError(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
			case errors.Is(err, tenant.ErrInvalidRegion), errors.Is(err, tenant.ErrInvalidPolicy):
				writeResError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			default:
				writeResError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
			return
		}
		mode := tenant.ModeUnpinned
		if policy.IsPinned() {
			mode = tenant.ModePinned
		}
		writeResJSON(w, http.StatusOK, map[string]any{"policy": residencyPolicyJSON(policy), "mode": mode})
	}
}

// residencyPostureHandler renders the compliance view: policy, mode and the
// last out-of-region evidence (null when the org has none).
func residencyPostureHandler(svc *tenant.ResidencyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := residencyClaims(w, r)
		if !ok {
			return
		}
		posture, err := svc.Posture(r.Context(), claims.OrganizationID)
		if err != nil {
			writeResError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		writeResJSON(w, http.StatusOK, map[string]any{
			"pinned_region":   string(posture.Policy.PinnedRegion),
			"allowed_regions": append([]string(nil), posture.Policy.AllowedRegions...),
			"strict_mode":     posture.Policy.StrictMode,
			"mode":            posture.Mode,
			"last_violation":  residencyViolationJSON(posture.LastViolation),
		})
	}
}

// residencyCheckHandler is the explicit data-flow check (dashboards, tests,
// and the reference mapping for the future MCP/A2A guard wiring). An allowed
// verdict is 200 {"verdict": ...}; a denied verdict is the contract 403
// {"error":{"code":"residency_violation", ...}}. Both outcomes are audited
// by the service (residency.check / residency.violation).
func residencyCheckHandler(svc *tenant.ResidencyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := residencyClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			URL string `json:"url"`
		}
		if !decodeResBody(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.URL) == "" {
			writeResError(w, http.StatusBadRequest, "VALIDATION_ERROR", tenant.ErrURLRequired.Error())
			return
		}
		verdict := svc.Check(r.Context(), claims.OrganizationID, body.URL)
		if verdict.Allowed {
			writeResJSON(w, http.StatusOK, map[string]any{"verdict": map[string]any{
				"allowed":     verdict.Allowed,
				"region":      string(verdict.Region),
				"reason":      verdict.Reason,
				"strict_mode": verdict.StrictMode,
				"mode":        verdict.Mode,
			}})
			return
		}
		writeResJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
			"code":        "residency_violation",
			"message":     residencyDenialMessage(verdict),
			"url_host":    tenant.HostOf(body.URL),
			"verdict":     "denied",
			"region":      string(verdict.Region),
			"reason":      verdict.Reason,
			"strict_mode": verdict.StrictMode,
			"mode":        verdict.Mode,
		}})
	}
}

// residencyDenialMessage renders the human-facing half of the 403 envelope.
func residencyDenialMessage(v tenant.Verdict) string {
	switch v.Reason {
	case tenant.ReasonUnclassifiable:
		return "endpoint region could not be classified; strict mode denies unclassifiable endpoints (fail-closed)"
	case tenant.ReasonPolicyUnavailable:
		return "residency policy could not be evaluated; the check fails closed"
	default:
		return "endpoint region " + string(v.Region) + " is outside the organization's pinned residency regions"
	}
}
