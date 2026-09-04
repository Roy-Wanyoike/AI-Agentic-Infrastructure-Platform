package main

// Issue #56 (wave 6-l) HTTP handlers — events read half.
//
// Endpoint (registered on apiMux by registerEventsRoutes; served under BOTH
// /v1 and /api/v1):
//
//      GET /events?limit=&cursor=&type=&entity_type=&entity_id=&since=
//          (runs.execute — MEMBER+: OWNER/ADMIN/MEMBER; VIEWER is 403)
//
// Org-scoped, keyset-paginated (newest first), shaped like GET /audit-events:
//
//      {"events":[{"id","type","entity_type","entity_id","payload","timestamp",...}],
//       "next_cursor":""}
//
// next_cursor is "" when the trail is exhausted; otherwise it is an opaque
// token to pass back as ?cursor= (created_at/id keyset, so appends between
// pages can neither duplicate nor skip rows). The tenant comes from the auth
// claims ONLY — client-supplied organization ids are never trusted, and the
// organization id itself stays server-side (the tenant is implied by the
// caller's claims), matching the other track views.
//
// RBAC note: the issue contract pins MEMBER+ with viewers explicitly denied.
// The natural "runs.read" permission ALSO grants VIEWER in the base role
// matrix (internal/auth/service.go), so this listing reuses runs.execute —
// the established MEMBER+ read grant (GET /secrets list convention). No new
// permission enum was introduced.
//
// Filters (all optional, AND-composed, stable across cursor pages):
//   - type        — exact contract event type (validated, 400 when unknown)
//   - entity_type — exact resource_type match (the domain object the event
//     is about, e.g. "run", "agent")
//   - entity_id   — exact resource_id match
//   - since       — RFC3339 lower bound, inclusive (created_at >= since)
//
// Dual mode (mirrors the AppendEvent memory/store split): the listing is
// served through the events.PagedStore resolved by eventsLister — the
// Postgres events table when a database is configured, otherwise whatever
// the process retains in memory (MemoryStore behind the AuditPublisher, or
// the MemoryPublisher ring). Nothing wired to retain events answers
// 503 EVENTS_UNAVAILABLE (loud, never a silent fake).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/events"
)

// writeEvJSON renders a JSON response (local helper, distinct name avoids
// collisions with other tracks' helpers in package main).
func writeEvJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeEvError renders the shared structured error envelope.
func writeEvError(w http.ResponseWriter, status int, code, message string) {
	writeEvJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// claimsOrgIDEv resolves the caller's tenant from the auth context (never
// from client input).
func claimsOrgIDEv(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeEvError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return "", false
	}
	if strings.TrimSpace(claims.OrganizationID) == "" {
		writeEvError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing organization claim")
		return "", false
	}
	return claims.OrganizationID, true
}

// eventView is the wire shape of one published event (no organization_id:
// the tenant is implied by the caller's claims). Resource is flattened into
// entity_type/entity_id to mirror the query filters.
type eventView struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	EntityType  string         `json:"entity_type,omitempty"`
	EntityID    string         `json:"entity_id,omitempty"`
	ProjectID   string         `json:"project_id,omitempty"`
	ExecutionID string         `json:"execution_id,omitempty"`
	TraceID     string         `json:"trace_id,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`
	Timestamp   string         `json:"timestamp"`
}

func newEventView(event *events.Event) eventView {
	return eventView{
		ID:          event.ID,
		Type:        event.Type,
		EntityType:  event.Resource.Type,
		EntityID:    event.Resource.ID,
		ProjectID:   event.ProjectID,
		ExecutionID: event.ExecutionID,
		TraceID:     event.TraceID,
		Payload:     event.Payload,
		Timestamp:   event.Timestamp.UTC().Format(time.RFC3339Nano),
	}
}

// listEventsHandler serves GET /events: the caller's org-scoped event
// stream, newest first, keyset-paginated. The page size is clamped by the
// events package itself (events.NormalizeLimit) — the handler validates the
// syntax of the query parameters and maps the typed store errors onto the
// contract error envelope.
func listEventsHandler(lister events.PagedStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if lister == nil {
			writeEvError(w, http.StatusServiceUnavailable, "EVENTS_UNAVAILABLE", "events listing is not available")
			return
		}
		// Tenant guard: the listing filters on the caller's organization_id.
		orgID, ok := claimsOrgIDEv(w, r)
		if !ok {
			return
		}

		limit := events.DefaultListLimit
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				writeEvError(w, http.StatusBadRequest, "INVALID_REQUEST",
					fmt.Sprintf("limit must be an integer, got %q", raw))
				return
			}
			if parsed <= 0 {
				writeEvError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be positive")
				return
			}
			limit = parsed
		}

		filter := events.EventFilter{}
		if raw := strings.TrimSpace(r.URL.Query().Get("type")); raw != "" {
			// The event vocabulary is a pinned contract; anything else is a
			// caller bug, not an empty page.
			if !events.IsValidEventType(raw) {
				writeEvError(w, http.StatusBadRequest, "INVALID_REQUEST",
					fmt.Sprintf("unknown event type %q", raw))
				return
			}
			filter.Type = raw
		}
		filter.EntityType = strings.TrimSpace(r.URL.Query().Get("entity_type"))
		filter.EntityID = strings.TrimSpace(r.URL.Query().Get("entity_id"))
		if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeEvError(w, http.StatusBadRequest, "INVALID_REQUEST",
					fmt.Sprintf("since must be an RFC3339 timestamp, got %q", raw))
				return
			}
			filter.Since = parsed
		}
		cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))

		items, next, err := lister.ListEventsPaged(r.Context(), orgID, filter, limit, cursor)
		if err != nil {
			switch {
			case errors.Is(err, events.ErrInvalidCursor):
				writeEvError(w, http.StatusBadRequest, "INVALID_CURSOR", "cursor is malformed or stale")
			case errors.Is(err, events.ErrListUnsupported):
				writeEvError(w, http.StatusServiceUnavailable, "EVENTS_UNAVAILABLE", "events store does not support listing")
			case errors.Is(err, events.ErrOrgRequired):
				writeEvError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "missing tenant scope")
			default:
				writeEvError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		views := make([]eventView, 0, len(items))
		for i := range items {
			views = append(views, newEventView(&items[i]))
		}
		writeEvJSON(w, http.StatusOK, map[string]any{"events": views, "next_cursor": next})
	}
}

// eventsLister resolves the read side of the events store split (issue #56).
// With a database, a dedicated read handle over the shared append-only
// `events` table serves the listing (the AuditPublisher chain owns the write
// side; SQL reads need no instance sharing). Zero-infrastructure modes read
// through the publisher when it can serve listings — a MemoryStore behind an
// AuditPublisher, or a MemoryPublisher ring buffer.
func eventsLister(db *sql.DB, pub events.Publisher) events.PagedStore {
	if db != nil {
		if paged, ok := events.NewPostgresStore(db).(events.PagedStore); ok {
			return paged
		}
		return nil
	}
	if paged, ok := pub.(events.PagedStore); ok {
		return paged
	}
	return nil
}

// registerEventsRoutes mounts the org-scoped event stream on apiMux behind
// runs.execute (MEMBER+: OWNER/ADMIN/MEMBER; viewers denied per the issue
// contract — see the RBAC note in the file header).
func registerEventsRoutes(apiMux *http.ServeMux, lister events.PagedStore, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil {
		return
	}
	apiMux.Handle("GET /events", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionRunsExecute)(http.HandlerFunc(listEventsHandler(lister)))))
}
