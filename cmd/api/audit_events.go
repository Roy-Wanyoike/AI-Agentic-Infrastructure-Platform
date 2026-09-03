package main

// Issue #18 (wave 5-b) HTTP handlers — audit trail half.
//
// Endpoint (registered on apiMux by registerAuditEventsRoutes; served under
// BOTH /v1 and /api/v1):
//
//	GET /audit-events?limit=&cursor=  (audit.read — OWNER/ADMIN only)
//
// Org-scoped, keyset-paginated (newest first):
//
//	{"events":[{"id","actor","action","resource","metadata","created_at"}],
//	 "next_cursor":""}
//
// next_cursor is "" when the trail is exhausted; otherwise it is an opaque
// token to pass back as ?cursor= (created_at/id keyset, so appends between
// pages can neither duplicate nor skip rows). The tenant comes from the auth
// claims ONLY — client-supplied organization ids are never trusted. The
// organization id itself stays server-side (the tenant is implied by the
// caller's claims), matching the other track views.
//
// audit.read is the dedicated permission added in internal/auth
// (permissions_audit.go); MEMBER/VIEWER are intentionally locked out of the
// security-sensitive trail.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
)

// writeAudJSON renders a JSON response (local helper, distinct name avoids
// collisions with other tracks' helpers in package main).
func writeAudJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeAudError renders the shared structured error envelope.
func writeAudError(w http.ResponseWriter, status int, code, message string) {
	writeAudJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// claimsOrgIDAud resolves the caller's tenant from the auth context (never
// from client input).
func claimsOrgIDAud(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeAudError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return "", false
	}
	if strings.TrimSpace(claims.OrganizationID) == "" {
		writeAudError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing organization claim")
		return "", false
	}
	return claims.OrganizationID, true
}

// auditEventView is the wire shape of one audit entry (no organization_id:
// the tenant is implied by the caller's claims).
type auditEventView struct {
	ID        string         `json:"id"`
	Actor     string         `json:"actor"`
	Action    string         `json:"action"`
	Resource  string         `json:"resource,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt string         `json:"created_at"`
}

func newAuditEventView(entry *audit.Entry) auditEventView {
	return auditEventView{
		ID:        entry.ID,
		Actor:     entry.Actor,
		Action:    entry.Action,
		Resource:  entry.Resource,
		Metadata:  entry.Metadata,
		CreatedAt: entry.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// listAuditEventsHandler serves GET /audit-events: the caller's org-scoped
// audit trail, newest first, keyset-paginated. The page size is clamped by
// the audit service itself (audit.NormalizeLimit) — the handler validates the
// syntax of limit/cursor and maps the typed store errors onto the contract
// error envelope.
func listAuditEventsHandler(svc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeAudError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "audit service not available")
			return
		}
		// Tenant guard: the listing filters on the caller's organization_id.
		orgID, ok := claimsOrgIDAud(w, r)
		if !ok {
			return
		}
		limit := audit.DefaultListLimit
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				writeAudError(w, http.StatusBadRequest, "INVALID_REQUEST",
					fmt.Sprintf("limit must be an integer, got %q", raw))
				return
			}
			if parsed <= 0 {
				writeAudError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be positive")
				return
			}
			limit = parsed
		}
		cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))

		entries, next, err := svc.ListEntriesPaged(r.Context(), orgID, limit, cursor)
		if err != nil {
			if errors.Is(err, audit.ErrInvalidCursor) {
				writeAudError(w, http.StatusBadRequest, "INVALID_CURSOR", "cursor is malformed or stale")
				return
			}
			writeAudError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		views := make([]auditEventView, 0, len(entries))
		for _, entry := range entries {
			views = append(views, newAuditEventView(entry))
		}
		writeAudJSON(w, http.StatusOK, map[string]any{"events": views, "next_cursor": next})
	}
}

// registerAuditEventsRoutes mounts the org-scoped audit trail on apiMux
// behind the dedicated audit.read permission (OWNER/ADMIN only).
func registerAuditEventsRoutes(apiMux *http.ServeMux, svc *audit.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	apiMux.Handle("GET /audit-events", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionAuditRead)(http.HandlerFunc(listAuditEventsHandler(svc)))))
}
