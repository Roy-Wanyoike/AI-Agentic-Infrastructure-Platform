package main

// approvals_enterprise.go — wave-9 issue #137 HTTP wiring: the enterprise
// human-in-the-loop oversight surface on top of the existing approvals
// endpoints (workflows.go: GET /approvals, GET /approvals/{id}, POST
// /approvals/{id}/decide — dual control is enforced INSIDE the service, so
// the existing decide handler needs no changes).
//
// Endpoints (registered on apiMux by registerApprovalEnterpriseRoutes; served
// under BOTH /v1 and /api/v1 through the shared StripPrefix mounts):
//
//      POST /approvals/{id}/delegate        {to_user, note, expires_at}
//      POST /approvals/escalate-due         SLA sweep (explicit, no timers)
//      GET  /approvals/escalation-policy    per-org escalation SLA policy
//      PUT  /approvals/escalation-policy    {target_status, sla_minutes}
//      GET  /approvals/evidence?from&to     byte-deterministic NDJSON oversight
//                                           snapshot (format=jsonl default)
//
// RBAC (house matrix, mirrored from the wave-8 surfaces):
//   - delegate: approvals.decide — the task's "approval-decide-level"
//     permission (exactly OWNER/ADMIN).
//   - escalate-due + escalation-policy: approvals.decide (OWNER/ADMIN
//     read/write per the issue).
//   - evidence: approvals.read at the middleware level PLUS an in-handler
//     OWNER/ADMIN role gate — "OWNER/ADMIN; approvals.read-level" per the
//     issue. MEMBER/VIEWER pass the middleware and are refused 403.
//
// Tenant scope comes exclusively from the auth claims; cross-tenant ids
// surface as 404 without an existence leak (service-level guard). The
// delegatee must be an org member (verified against the organizations
// service — same membership source as the member-management surface).
//
// Audit: the service writes the state-change rows through the Auditor seam
// (approval.delegated, approval.delegation_expired, approval.escalated,
// approval.first_decision, approval.completed_dual — wired once below, the
// mcp registry precedent). The evidence export writes its OWN access row
// (approval.evidence_exported) best-effort AFTER the snapshot bytes are
// rendered: the export reads only the immutable approval_events log, so the
// audit row can never perturb export determinism.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/organizations"
)

// registerApprovalEnterpriseRoutes mounts the issue #137 oversight endpoints.
// Wiring note: the Auditor seam is attached here (single construction site,
// exactly like registerWorkflowsRoutes wires the run controller), so main.go
// needs only this one registration line.
func registerApprovalEnterpriseRoutes(apiMux *http.ServeMux, apSvc *approvals.Service, orgsSvc *organizations.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || apSvc == nil {
		return
	}
	if auditSvc != nil {
		apSvc.SetAuditor(auditSvc) // best-effort oversight audit rows (service-side)
	}
	// auth wrap pattern from cmd/api/main.go / mcp_trust.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("POST /approvals/{id}/delegate", wrap(auth.PermissionApprovalsDecide, http.HandlerFunc(approvalDelegateHandler(apSvc, orgsSvc))))
	apiMux.Handle("POST /approvals/escalate-due", wrap(auth.PermissionApprovalsDecide, http.HandlerFunc(approvalEscalateDueHandler(apSvc))))
	apiMux.Handle("GET /approvals/escalation-policy", wrap(auth.PermissionApprovalsDecide, http.HandlerFunc(approvalGetEscalationPolicyHandler(apSvc))))
	apiMux.Handle("PUT /approvals/escalation-policy", wrap(auth.PermissionApprovalsDecide, http.HandlerFunc(approvalPutEscalationPolicyHandler(apSvc))))
	// evidence: approvals.read at the middleware + explicit OWNER/ADMIN role
	// gate inside (see requireOrgAdminRole).
	apiMux.Handle("GET /approvals/evidence", wrap(auth.PermissionApprovalsRead, requireOrgAdminRole(http.HandlerFunc(approvalEvidenceExportHandler(apSvc, auditSvc)))))
}

// requireOrgAdminRole is the additive role gate for the evidence endpoint:
// the middleware already validated the session; this restricts the effective
// matrix to OWNER/ADMIN exactly as the issue pins it.
func requireOrgAdminRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeErrorWF(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		switch strings.ToUpper(strings.TrimSpace(claims.Role)) {
		case "OWNER", "ADMIN":
			next.ServeHTTP(w, r)
		default:
			writeErrorWF(w, http.StatusForbidden, "FORBIDDEN", "evidence export requires OWNER or ADMIN")
		}
	})
}

// approvalDelegateHandler serves POST /approvals/{id}/delegate.
func approvalDelegateHandler(apSvc *approvals.Service, orgsSvc *organizations.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := wfPathID(r)
		if id == "" {
			writeErrorWF(w, http.StatusNotFound, "APPROVAL_NOT_FOUND", "approval not found")
			return
		}
		var req struct {
			ToUser    string `json:"to_user"`
			Note      string `json:"note"`
			ExpiresAt string `json:"expires_at"` // RFC3339, optional
		}
		if err := readJSONWF(r, &req); err != nil {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeErrorWF(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		if strings.TrimSpace(req.ToUser) == "" {
			writeErrorWF(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "to_user is required")
			return
		}
		// The delegatee must be an org member (membership is the organizations
		// service's source of truth; unknown/foreign users are not members).
		if orgsSvc == nil {
			writeErrorWF(w, http.StatusServiceUnavailable, "ORG_SERVICE_UNAVAILABLE", "organization service is not available")
			return
		}
		members, err := orgsSvc.MembersCtx(r.Context(), claims.OrganizationID)
		if err != nil {
			writeErrorWF(w, http.StatusInternalServerError, "INTERNAL_ERROR", "membership lookup failed")
			return
		}
		member := false
		for _, m := range members {
			if m.UserID == strings.TrimSpace(req.ToUser) {
				member = true
				break
			}
		}
		if !member {
			writeErrorWF(w, http.StatusUnprocessableEntity, "DELEGATEE_NOT_MEMBER", "the delegatee must be an organization member")
			return
		}
		var expiresAt *time.Time
		if raw := strings.TrimSpace(req.ExpiresAt); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "expires_at must be RFC3339")
				return
			}
			expiresAt = &t
		}
		approval, delegation, err := apSvc.Delegate(r.Context(), claims.OrganizationID, id, req.ToUser, req.Note, expiresAt, claims.UserID)
		if err != nil {
			mapApprovalEnterpriseError(w, err)
			return
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"approval": approval, "delegation": delegation})
	}
}

// approvalEscalateDueHandler serves POST /approvals/escalate-due: the
// explicit SLA sweep. Body is ignored; the response carries the approvals
// escalated by THIS call.
func approvalEscalateDueHandler(apSvc *approvals.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeErrorWF(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		escalated, err := apSvc.EscalateDue(r.Context(), claims.OrganizationID, claims.UserID)
		if err != nil {
			mapApprovalEnterpriseError(w, err)
			return
		}
		if escalated == nil {
			escalated = []*approvals.Approval{}
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"escalated": escalated, "count": len(escalated)})
	}
}

// approvalGetEscalationPolicyHandler serves GET /approvals/escalation-policy.
func approvalGetEscalationPolicyHandler(apSvc *approvals.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeErrorWF(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		policy, err := apSvc.GetEscalationPolicy(r.Context(), claims.OrganizationID)
		if err != nil {
			mapApprovalEnterpriseError(w, err)
			return
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"policy": policy})
	}
}

// approvalPutEscalationPolicyHandler serves PUT /approvals/escalation-policy.
func approvalPutEscalationPolicyHandler(apSvc *approvals.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TargetStatus string `json:"target_status"` // pending | partially_approved (default pending)
			SLAMinutes   int    `json:"sla_minutes"`   // 0 disables the sweep
		}
		if err := readJSONWF(r, &req); err != nil {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeErrorWF(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		policy, err := apSvc.SetEscalationPolicy(r.Context(), claims.OrganizationID, req.TargetStatus, req.SLAMinutes, claims.UserID)
		if err != nil {
			mapApprovalEnterpriseError(w, err)
			return
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"policy": policy})
	}
}

// approvalEvidenceExportHandler serves GET /approvals/evidence?from&to&format=jsonl:
// the byte-deterministic NDJSON oversight snapshot. The export is a pure read
// (it never mutates approval state); the approval.evidence_exported audit row
// is written AFTER the bytes are rendered — the export reads only the
// immutable approval_events log, so determinism is unaffected.
func approvalEvidenceExportHandler(apSvc *approvals.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apSvc == nil {
			writeErrorWF(w, http.StatusServiceUnavailable, "APPROVALS_UNAVAILABLE", "approvals service is not available")
			return
		}
		if format := strings.TrimSpace(r.URL.Query().Get("format")); format != "" && format != "jsonl" {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "format must be jsonl")
			return
		}
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeErrorWF(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		from, err := parseEvidenceTime(r.URL.Query().Get("from"))
		if err != nil {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "from must be RFC3339")
			return
		}
		to, err := parseEvidenceTime(r.URL.Query().Get("to"))
		if err != nil {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "to must be RFC3339")
			return
		}
		data, err := apSvc.ExportEvidenceJSONL(r.Context(), claims.OrganizationID, from, to)
		if err != nil {
			mapApprovalEnterpriseError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, approvals.AuditEvidenceExported,
				claims.OrganizationID, "approvals/evidence",
				map[string]any{"from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339), "count": evidenceCount(data)})
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Evidence-Count", strconv.Itoa(evidenceCount(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// mapApprovalEnterpriseError maps the oversight service errors onto the
// contract envelope; the base approval errors fall through to the shared
// wave-2 mapping (404 / 422 / 409 / 500).
func mapApprovalEnterpriseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, approvals.ErrDualSelfApproval):
		// machine-readable two-person-rule conflict (issue #137)
		writeErrorWF(w, http.StatusConflict, "dual_control_self_approval", err.Error())
	case errors.Is(err, approvals.ErrInvalidDelegation),
		errors.Is(err, approvals.ErrInvalidEscalationPolicy),
		errors.Is(err, approvals.ErrInvalidEvidenceRange),
		errors.Is(err, approvals.ErrOrgRequired):
		writeErrorWF(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	default:
		writeApprovalErrorWF(w, err)
	}
}

// parseEvidenceTime parses an optional RFC3339 query bound (empty -> zero).
func parseEvidenceTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, raw)
}

// evidenceCount reads the count out of the export's trailing evidence_head
// line (self-consistent: the header can never disagree with the bytes).
func evidenceCount(data []byte) int {
	line := lastJSONLLine(data)
	if line == nil {
		return 0
	}
	var head struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(line, &head); err != nil || head.Type != "evidence_head" {
		return 0
	}
	return head.Count
}
