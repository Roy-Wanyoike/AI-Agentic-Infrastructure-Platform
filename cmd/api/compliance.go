package main

// compliance.go — wave-9 issue #138 HTTP wiring: the compliance EVIDENCE
// PACK endpoints over the read-only aggregation service (internal/compliance).
//
// Endpoints (registered on apiMux by registerComplianceRoutes; served under
// BOTH /v1 and /api/v1 through the shared StripPrefix mounts in main.go):
//
//	GET /compliance/controls                      control catalog × frameworks
//	                                              × satisfied-by × live evidence
//	GET /compliance/agents/{id}/technical-doc     per-agent Annex-IV-style doc
//	                                              ?format=json|md (default json)
//	GET /compliance/evidence?from&to              deterministic JSONL bundle
//
// RBAC: audit.read — the SAME evidence-level grant the audit chain surface
// (cmd/api/audit_chain.go) requires: OWNER/ADMIN only; MEMBER/VIEWER are
// locked out. No new permission enum was introduced (the mcp_trust.go
// convention of reusing the closest existing grant).
//
// Tenant scope: resolved exclusively from the auth claims (never from client
// input). The technical-doc endpoint answers 404 COMPLIANCE_AGENT_NOT_FOUND
// for unknown AND foreign agent ids (no existence leak); the other endpoints
// have no client-supplied tenant parameters at all.
//
// Read-only posture: NEITHER endpoint appends audit rows or otherwise
// mutates state — the compliance service consumes its collaborators through
// write-free reader interfaces only (a GET that mutates the trail it
// evidences would be self-defeating; same rationale as audit_chain.go).
// Pure aggregation: NO database migration backs these endpoints.

import (
	"errors"
	"net/http"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/compliance"
)

// newComplianceService constructs the read-only compliance aggregation
// service from the app's EXISTING services (pure consumption — none of the
// referenced services is modified or written to). Called from newApp after
// every collaborator is constructed. The multiple narrow interfaces the
// apikeys service satisfies are wired to the SAME concrete service, which is
// exactly the point of the interface seams.
func newComplianceService(a *app) *compliance.Service {
	if a == nil {
		return nil
	}
	return compliance.NewService(compliance.Deps{
		Audit:       a.auditSvc,                // chain head + trail listing (read-only)
		Agents:      a.agentsSvc,               // agent identity + org listing
		Versions:    a.versionsSvc,             // config-version history
		Deployments: a.deploymentsSvc,          // deployments + canary/promotion state
		Evaluations: a.evalSvc,                 // datasets + completed-run samples
		Policies:    a.sharedPoliciesService(), // the ONE shared policies service
		OrgAPIKeys:  a.apiKeysSvc,              // org-scoped key inventory
		AgentKeys:   a.apiKeysSvc,              // agent-scoped keys + tool allow-lists
		Approvals:   a.apSvc,                   // human-oversight approvals
		MCPRegistry: a.mcpRegSvc,               // MCP server registry (supply chain)
	})
}

// registerComplianceRoutes mounts the compliance evidence-pack endpoints
// behind audit.read (OWNER/ADMIN only).
func registerComplianceRoutes(apiMux *http.ServeMux, svc *compliance.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go / mcp_registry.go: auth or API
	// key outermost, permission check inside.
	wrap := func(h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
			auth.RequirePermission(authSvc, auth.PermissionAuditRead)(h))
	}
	apiMux.Handle("GET /compliance/controls", wrap(http.HandlerFunc(complianceControlsHandler(svc))))
	apiMux.Handle("GET /compliance/agents/{id}/technical-doc", wrap(http.HandlerFunc(complianceTechDocHandler(svc))))
	apiMux.Handle("GET /compliance/evidence", wrap(http.HandlerFunc(complianceEvidenceHandler(svc))))
}

// complianceControlsHandler serves GET /v1/compliance/controls: the static
// control catalog with live evidence pointers for the caller's org.
func complianceControlsHandler(svc *compliance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDAud(w, r)
		if !ok {
			return
		}
		report, err := svc.Controls(r.Context(), orgID)
		if err != nil {
			mapComplianceError(w, err)
			return
		}
		writeAudJSON(w, http.StatusOK, report)
	}
}

// complianceTechDocHandler serves GET /v1/compliance/agents/{id}/technical-doc.
// format=json (default) renders the structured doc; format=md renders the
// deterministic Markdown document (text/markdown). Unknown/foreign agents
// are 404 without an existence leak.
func complianceTechDocHandler(svc *compliance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDAud(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if strings.TrimSpace(id) == "" {
			writeComplianceAgentNotFound(w)
			return
		}
		format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
		if format == "" {
			format = "json"
		}
		if format != "json" && format != "md" {
			writeAudError(w, http.StatusBadRequest, "INVALID_REQUEST",
				"format must be json or md, got "+format)
			return
		}
		doc, err := svc.TechnicalDoc(r.Context(), orgID, id)
		if err != nil {
			mapComplianceError(w, err)
			return
		}
		if format == "md" {
			body := svc.RenderMarkdown(doc)
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		writeAudJSON(w, http.StatusOK, doc)
	}
}

// complianceEvidenceHandler serves GET /v1/compliance/evidence?from&to: the
// deterministic JSONL bundle. from/to accept RFC3339 or YYYY-MM-DD (the
// window is inclusive; date-only ends resolve to the day boundaries).
func complianceEvidenceHandler(svc *compliance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrgIDAud(w, r)
		if !ok {
			return
		}
		from, to, err := compliance.ParseWindow(
			r.URL.Query().Get("from"), r.URL.Query().Get("to"))
		if err != nil {
			writeAudError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		bundle, err := svc.EvidenceBundle(r.Context(), orgID, from, to)
		if err != nil {
			mapComplianceError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bundle)
	}
}

// writeComplianceAgentNotFound renders the 404 shared by unknown and foreign
// agents (no existence leak).
func writeComplianceAgentNotFound(w http.ResponseWriter) {
	writeAudError(w, http.StatusNotFound, "COMPLIANCE_AGENT_NOT_FOUND", "agent not found")
}

// mapComplianceError converts compliance service errors into the contract
// error envelope. Org-required cannot surface from the HTTP path (the tenant
// comes from verified claims) but is kept for defense in depth.
func mapComplianceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, compliance.ErrAgentNotFound):
		writeComplianceAgentNotFound(w)
	case errors.Is(err, compliance.ErrOrgRequired):
		writeAudError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		writeAudError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}
