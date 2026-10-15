package main

// audit_chain.go — wave 8 issue A HTTP wiring: tamper-evidence endpoints over
// the org-scoped audit trail (SOC 2 monitoring evidence + EU AI Act Art. 12
// record-keeping story; the chain/evidence engine itself lives in
// internal/audit/chain.go and export.go).
//
// Endpoints (registered on apiMux by registerAuditChainRoutes; served under
// BOTH /v1 and /api/v1 through the shared StripPrefix mounts in main.go):
//
//      GET /audit/verify              -> 200 ChainReport JSON
//                                         {"valid":true,"length":N,"head_hash":"..."}
//                                         (broken trails add broken_at_index +
//                                         expected_head_hash)
//      GET /audit/export?format=jsonl -> 200 Content-Type application/x-ndjson,
//                                         X-Audit-Head-Hash header, JSONL body
//                                         ending in the chain_head line
//
// RBAC: audit.read — the SAME grant the audit list endpoint (issue #18,
// audit_events.go) requires: OWNER/ADMIN only; MEMBER/VIEWER are locked out
// of the security-sensitive trail. Auth wrap order replicates the repo
// convention: RequireAuthOrAPIKey outer, RequirePermission inner.
//
// Tenant scope: resolved exclusively from the auth claims (never from client
// input). An explicit ?organization_id= that disagrees with the claims is a
// cross-tenant probe and answers 404 AUDIT_NOT_FOUND — no existence leak;
// the claims remain the only scope that can ever be widened to.
//
// Read-only posture: NEITHER endpoint appends audit rows — a GET that
// mutates the trail it is evidencing would recursively grow the chain and
// make every snapshot differ by the fact of its own creation (the same
// posture as the audit list endpoint). Access evidence lives in the
// deployment's access logs, not in the chain.
//
// JSON helpers (writeAudJSON/writeAudError/claimsOrgIDAud) are shared with
// audit_events.go — same package, intentionally reused to keep one error
// envelope shape across the audit surface.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
)

// registerAuditChainRoutes mounts the tamper-evidence endpoints on apiMux
// behind the audit.read permission (OWNER/ADMIN only).
func registerAuditChainRoutes(apiMux *http.ServeMux, svc *audit.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go (see mcp_registry.go): auth or
	// API key outermost, permission check inside.
	wrap := func(h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
			auth.RequirePermission(authSvc, auth.PermissionAuditRead)(h))
	}
	apiMux.Handle("GET /audit/verify", wrap(http.HandlerFunc(verifyAuditChainHandler(svc))))
	apiMux.Handle("GET /audit/export", wrap(http.HandlerFunc(exportAuditChainHandler(svc))))
}

// auditChainOrgScope resolves the request's tenant. The auth claims are the
// only scope source; an explicit ?organization_id= is tolerated only when it
// MATCHES the claims (making cross-tenant probing an explicit 404 instead of
// a silent re-scope). Returns false when the response is already written.
func auditChainOrgScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	orgID, ok := claimsOrgIDAud(w, r)
	if !ok {
		return "", false
	}
	if requested := strings.TrimSpace(r.URL.Query().Get("organization_id")); requested != "" && requested != orgID {
		// Cross-tenant probe: 404 without an existence leak.
		writeAudError(w, http.StatusNotFound, "AUDIT_NOT_FOUND", "audit trail not found")
		return "", false
	}
	return orgID, true
}

// verifyAuditChainHandler serves GET /audit/verify: the caller's org chain
// verdict as ChainReport JSON (200 even when the chain is BROKEN — a failed
// verification is a valid, actionable answer; only transport/store errors
// map to the 5xx envelope).
func verifyAuditChainHandler(svc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeAudError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "audit service not available")
			return
		}
		orgID, ok := auditChainOrgScope(w, r)
		if !ok {
			return
		}
		report, err := svc.VerifyChain(r.Context(), orgID)
		if err != nil {
			mapAuditChainError(w, err)
			return
		}
		// NO self-audit row here: verify is a read-only evidence endpoint.
		// A GET that appends rows would (a) mutate the very trail it is
		// verdicting, (b) recursively grow the chain on every read, and (c)
		// make the reported length depend on how many times someone checked
		// it. Reads are evidenced by the access logs of the deployment, not
		// by the audit chain (same posture as the audit list endpoint).
		writeAudJSON(w, http.StatusOK, report)
	}
}

// exportAuditChainHandler serves GET /audit/export?format=jsonl: the NDJSON
// evidence export. The format parameter is required and must be jsonl —
// explicit versioning keeps future export formats (signed bundles, SIEM
// shards) from being silently confused with this one. X-Audit-Head-Hash is
// parsed FROM the body's own chain_head line so the header and the bytes it
// anchors can never disagree.
func exportAuditChainHandler(svc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeAudError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "audit service not available")
			return
		}
		if format := strings.TrimSpace(r.URL.Query().Get("format")); format != "jsonl" {
			writeAudError(w, http.StatusBadRequest, "INVALID_REQUEST", fmt.Sprintf("format must be jsonl, got %q", format))
			return
		}
		orgID, ok := auditChainOrgScope(w, r)
		if !ok {
			return
		}
		data, err := svc.ExportJSONL(r.Context(), orgID)
		if err != nil {
			mapAuditChainError(w, err)
			return
		}
		head := chainHeadFromExport(data)
		// NO self-audit row here: the export must be a pure snapshot of the
		// trail AS IT EXISTS — appending an audit.export row during the read
		// would make every snapshot differ from the next by the fact of its
		// own creation, and a GET must not mutate audited state.
		w.Header().Set("Content-Type", "application/x-ndjson")
		if head != "" {
			w.Header().Set("X-Audit-Head-Hash", head)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// chainHeadFromExport extracts the head hash from the export's final
// chain_head line (self-consistent anchor for the X-Audit-Head-Hash header).
func chainHeadFromExport(data []byte) string {
	line := lastJSONLLine(data)
	if line == nil {
		return ""
	}
	var head struct {
		Type     string `json:"type"`
		HeadHash string `json:"head_hash"`
	}
	if err := json.Unmarshal(line, &head); err != nil || head.Type != "chain_head" {
		return ""
	}
	return head.HeadHash
}

// lastJSONLLine returns the last non-empty line of an NDJSON payload.
func lastJSONLLine(data []byte) []byte {
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 0 {
		return nil
	}
	return []byte(lines[len(lines)-1])
}

// mapAuditChainError maps audit service errors onto the contract envelope.
// ErrOrgRequired cannot surface from the HTTP path (the tenant comes from
// verified claims) but is kept for defense in depth.
func mapAuditChainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, audit.ErrOrgRequired):
		writeAudError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		writeAudError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}
