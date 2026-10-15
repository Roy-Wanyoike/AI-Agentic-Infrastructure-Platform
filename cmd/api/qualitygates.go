package main

// qualitygates.go — issue #152 HTTP wiring: the unified release assurance
// surface (versioned gate policies + deterministic recommendations) on top
// of internal/qualitygates.
//
// Endpoints (registered on apiMux by registerQualityGateRoutes; served
// under BOTH /v1 and /api/v1 like every versioned surface):
//
//      POST   /quality-gates/policies        create policy v1            (OWNER/ADMIN)
//      GET    /quality-gates/policies        list latest versions        (all roles)
//      GET    /quality-gates/policies/{id}   one version (?version=N)    (all roles)
//      PUT    /quality-gates/policies/{id}   append new version          (OWNER/ADMIN)
//      DELETE /quality-gates/policies/{id}   delete every version        (OWNER/ADMIN)
//      POST   /quality-gates/evaluate        {policy_id, evidence_refs}  (all roles)
//      POST   /quality-gates/exceptions      approve time-bounded waiver (OWNER/ADMIN)
//
// Design (mirrors cmd/api/residency.go / agent_keys.go):
//   - RBAC uses the NEW issue #152 grants (internal/auth/permissions_qualitygates.go):
//     reads + evaluate are qualitygates.read (all roles — release verdicts
//     are org-wide facts); writes and exception approval are
//     qualitygates.write (OWNER/ADMIN — release policy is an account-
//     administration decision, same tier as policies.write).
//   - Tenant scope comes EXCLUSIVELY from the auth claims; policy/evidence
//     ids are tenant-scoped at the service layer (foreign ids are 404 —
//     never 403 — so tenants cannot probe each other's release posture).
//   - Validation failures are 422 VALIDATION_ERROR (house 422 convention,
//     cmd/api/a2a.go); inactive-policy evaluation is 422 POLICY_INACTIVE;
//     tampered evidence is 422 EVIDENCE_TAMPERED (integrity, not silence).
//   - Wire unit for staleness windows is max_age_ms (milliseconds — JSON
//     has no duration type); converted to time.Duration at the boundary.
//   - Exception approval FORCES approved_by to the authenticated actor:
//     waiver attribution can never be forged via the request body.
//   - Audit: policy lifecycle, exception approvals and every decision are
//     emitted through the service's AuditSink seam (best-effort, house
//     style) wired to THIS process's audit service here.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/qualitygates"
)

// registerQualityGateRoutes mounts the quality-gates surface and wires the
// service-level audit seam to THIS api process's audit service.
func registerQualityGateRoutes(apiMux *http.ServeMux, svc *qualitygates.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// Issue #152 seam: every policy mutation, exception approval and
	// decision flows through the injected AuditSink (best-effort — audit
	// failures never block release operations). The adapter bridges the
	// service's narrow seam onto the audit service's LogCtx.
	if auditSvc != nil {
		svc.SetAuditSink(func(ctx context.Context, actor, action, orgID, resource string, metadata map[string]any) {
			_, _ = auditSvc.LogCtx(ctx, actor, action, orgID, resource, metadata)
		})
	}
	// auth wrap pattern from cmd/api/main.go / residency.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	// Reads + evaluate: qualitygates.read (all roles).
	apiMux.Handle("GET /quality-gates/policies", wrap(auth.PermissionQualityGatesRead, http.HandlerFunc(qualityGatePolicyListHandler(svc))))
	apiMux.Handle("GET /quality-gates/policies/{id}", wrap(auth.PermissionQualityGatesRead, http.HandlerFunc(qualityGatePolicyGetHandler(svc))))
	apiMux.Handle("POST /quality-gates/evaluate", wrap(auth.PermissionQualityGatesRead, http.HandlerFunc(qualityGateEvaluateHandler(svc))))
	// Writes: qualitygates.write (OWNER/ADMIN).
	apiMux.Handle("POST /quality-gates/policies", wrap(auth.PermissionQualityGatesWrite, http.HandlerFunc(qualityGatePolicyCreateHandler(svc))))
	apiMux.Handle("PUT /quality-gates/policies/{id}", wrap(auth.PermissionQualityGatesWrite, http.HandlerFunc(qualityGatePolicyUpdateHandler(svc))))
	apiMux.Handle("DELETE /quality-gates/policies/{id}", wrap(auth.PermissionQualityGatesWrite, http.HandlerFunc(qualityGatePolicyDeleteHandler(svc))))
	apiMux.Handle("POST /quality-gates/exceptions", wrap(auth.PermissionQualityGatesWrite, http.HandlerFunc(qualityGateExceptionApproveHandler(svc))))
}

// --- serialization helpers (distinct names; same envelope as residency.go) ---

func writeQGJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeQGError(w http.ResponseWriter, status int, code, message string) {
	writeQGJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// decodeQGBody decodes the JSON request body bounded to 1 MiB (the
// decodeResBody convention). Returns false when the response was written.
func decodeQGBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	defer r.Body.Close() //nolint:errcheck // read path; nothing actionable
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeQGError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeQGError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return false
	}
	return true
}

// qualityGateClaims extracts the caller claims or writes the 401 shape,
// returning ok=false when the response was already written.
func qualityGateClaims(w http.ResponseWriter, r *http.Request) (auth.UserClaims, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeQGError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return auth.UserClaims{}, false
	}
	if claims.OrganizationID == "" {
		writeQGError(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
		return auth.UserClaims{}, false
	}
	return claims, true
}

// mapQualityGateError renders service errors with the documented status
// matrix (404 unknown/foreign; 422 validation-class; 500 otherwise).
func mapQualityGateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, qualitygates.ErrPolicyNotFound), errors.Is(err, qualitygates.ErrEvidenceNotFound):
		writeQGError(w, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, qualitygates.ErrInactivePolicy):
		writeQGError(w, http.StatusUnprocessableEntity, "POLICY_INACTIVE", err.Error())
	case errors.Is(err, qualitygates.ErrTamperedEvidence):
		writeQGError(w, http.StatusUnprocessableEntity, "EVIDENCE_TAMPERED", "evidence failed its integrity check")
	case errors.Is(err, qualitygates.ErrOrgRequired):
		writeQGError(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
	case errors.Is(err, qualitygates.ErrInvalidPolicy), errors.Is(err, qualitygates.ErrInvalidException):
		writeQGError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	default:
		writeQGError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// --- wire DTOs (the boundary owns unit conversion) ---------------------------

// qualityGateSuiteInput is the wire shape of one suite requirement
// (max_age_ms in milliseconds; converted to time.Duration in the domain).
type qualityGateSuiteInput struct {
	Kind               string   `json:"kind"`
	MinCoveragePct     *float64 `json:"min_coverage_pct"`
	MinPassRate        *float64 `json:"min_pass_rate"`
	MaxP95Ms           *int64   `json:"max_p95_ms"`
	MaxSeverityAllowed string   `json:"max_severity_allowed"`
	MaxAgeMs           *int64   `json:"max_age_ms"`
	FailClosed         bool     `json:"fail_closed"`
}

// qualityGatePolicyInput is the create/update wire shape.
type qualityGatePolicyInput struct {
	Name           string                  `json:"name"`
	RequiredSuites []qualityGateSuiteInput `json:"required_suites"`
	Active         *bool                   `json:"active"`
}

// toDomain converts the wire shape to the domain policy (unset thresholds
// stay zero = disabled, per the SuiteRequirement contract).
func (in qualityGatePolicyInput) toDomain() qualitygates.GatePolicy {
	p := qualitygates.GatePolicy{
		Name:   in.Name,
		Active: in.Active == nil || *in.Active, // default ACTIVE (a gate policy exists to be evaluated)
	}
	for _, s := range in.RequiredSuites {
		req := qualitygates.SuiteRequirement{
			Kind:               qualitygates.SuiteKind(s.Kind),
			MaxSeverityAllowed: qualitygates.Severity(s.MaxSeverityAllowed),
			FailClosed:         s.FailClosed,
		}
		if s.MinCoveragePct != nil {
			req.MinCoveragePct = *s.MinCoveragePct
		}
		if s.MinPassRate != nil {
			req.MinPassRate = *s.MinPassRate
		}
		if s.MaxP95Ms != nil {
			req.MaxP95Ms = *s.MaxP95Ms
		}
		if s.MaxAgeMs != nil && *s.MaxAgeMs > 0 {
			req.MaxAge = time.Duration(*s.MaxAgeMs) * time.Millisecond
		}
		p.RequiredSuites = append(p.RequiredSuites, req)
	}
	return p
}

// --- policy lifecycle handlers ------------------------------------------------

// qualityGatePolicyCreateHandler creates policy version 1 (201).
func qualityGatePolicyCreateHandler(svc *qualitygates.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := qualityGateClaims(w, r)
		if !ok {
			return
		}
		var in qualityGatePolicyInput
		if !decodeQGBody(w, r, &in) {
			return
		}
		policy, err := svc.CreatePolicy(r.Context(), claims.OrganizationID, claims.UserID, in.toDomain())
		if err != nil {
			mapQualityGateError(w, err)
			return
		}
		writeQGJSON(w, http.StatusCreated, map[string]any{"policy": policy})
	}
}

// qualityGatePolicyListHandler lists the latest version of every policy.
func qualityGatePolicyListHandler(svc *qualitygates.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := qualityGateClaims(w, r)
		if !ok {
			return
		}
		policies, err := svc.ListPolicies(r.Context(), claims.OrganizationID)
		if err != nil {
			mapQualityGateError(w, err)
			return
		}
		writeQGJSON(w, http.StatusOK, map[string]any{"policies": policies})
	}
}

// qualityGatePolicyGetHandler returns one version (?version=N pins an
// exact version; default latest).
func qualityGatePolicyGetHandler(svc *qualitygates.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := qualityGateClaims(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		version := 0
		if raw := r.URL.Query().Get("version"); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 1 {
				writeQGError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "version must be a positive integer")
				return
			}
			version = v
		}
		policy, err := svc.GetPolicy(r.Context(), claims.OrganizationID, id, version)
		if err != nil {
			mapQualityGateError(w, err)
			return
		}
		writeQGJSON(w, http.StatusOK, map[string]any{"policy": policy})
	}
}

// qualityGatePolicyUpdateHandler appends a new immutable version (200).
func qualityGatePolicyUpdateHandler(svc *qualitygates.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := qualityGateClaims(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		var in qualityGatePolicyInput
		if !decodeQGBody(w, r, &in) {
			return
		}
		policy, err := svc.UpdatePolicy(r.Context(), claims.OrganizationID, id, claims.UserID, in.toDomain())
		if err != nil {
			mapQualityGateError(w, err)
			return
		}
		writeQGJSON(w, http.StatusOK, map[string]any{"policy": policy})
	}
}

// qualityGatePolicyDeleteHandler removes every version of the policy.
func qualityGatePolicyDeleteHandler(svc *qualitygates.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := qualityGateClaims(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if err := svc.DeletePolicy(r.Context(), claims.OrganizationID, id, claims.UserID); err != nil {
			mapQualityGateError(w, err)
			return
		}
		writeQGJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	}
}

// --- evaluate + exceptions handlers -------------------------------------------

// qualityGateEvaluateHandler resolves {policy_id, evidence_refs} and
// returns the deterministic Decision (200). The decision is a
// RECOMMENDATION — applying it to live traffic is the orchestrator's
// CanaryAdvisor wiring (internal/qualitygates/canary_bridge.go).
func qualityGateEvaluateHandler(svc *qualitygates.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := qualityGateClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			PolicyID     string   `json:"policy_id"`
			EvidenceRefs []string `json:"evidence_refs"`
		}
		if !decodeQGBody(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.PolicyID) == "" {
			writeQGError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "policy_id is required")
			return
		}
		decision, err := svc.Evaluate(r.Context(), claims.OrganizationID, body.PolicyID, body.EvidenceRefs)
		if err != nil {
			mapQualityGateError(w, err)
			return
		}
		writeQGJSON(w, http.StatusOK, map[string]any{"decision": decision})
	}
}

// qualityGateExceptionApproveHandler approves a time-bounded waiver on a
// policy (new version, audited). approved_by is ALWAYS the authenticated
// actor — the request body cannot attribute a waiver to someone else.
func qualityGateExceptionApproveHandler(svc *qualitygates.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := qualityGateClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			PolicyID  string `json:"policy_id"`
			Suite     string `json:"suite"`
			Reason    string `json:"reason"`
			ExpiresAt string `json:"expires_at"` // RFC3339; required (time-bounded only)
		}
		if !decodeQGBody(w, r, &body) {
			return
		}
		ex := qualitygates.Exception{
			Suite:  qualitygates.SuiteKind(strings.TrimSpace(body.Suite)),
			Reason: body.Reason,
		}
		if strings.TrimSpace(body.ExpiresAt) != "" {
			exp, err := time.Parse(time.RFC3339, body.ExpiresAt)
			if err != nil {
				writeQGError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "expires_at must be an RFC3339 timestamp")
				return
			}
			ex.ExpiresAt = exp
		}
		policy, err := svc.ApproveException(r.Context(), claims.OrganizationID, body.PolicyID, claims.UserID, ex)
		if err != nil {
			mapQualityGateError(w, err)
			return
		}
		writeQGJSON(w, http.StatusOK, map[string]any{"policy": policy})
	}
}
