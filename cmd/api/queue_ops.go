package main

// Issue #77 (wave 8-c) HTTP handlers — the queue introspection + dead-letter
// requeue half, served on apiMux by registerQueueOpsRoutes under BOTH /v1 and
// /api/v1:
//
//      GET  /queue/tasks?status=&limit=&cursor=   (runs.read — all roles)
//      GET  /queue/tasks/{id}                     (runs.read — all roles)
//      GET  /queue/stats                          (runs.read — all roles)
//      POST /queue/tasks/{id}/requeue             (queue.manage — OWNER/ADMIN)
//
// Org-scoped: the tenant comes from the auth claims ONLY — client-supplied
// organization ids are never trusted and the organization id itself never
// appears in responses (the tenant is implied by the caller's claims, same
// convention as the audit-events and events views). Listing is keyset-
// paginated newest-enqueue-first: {"tasks":[...],"next_cursor":""} with
// next_cursor "" when exhausted; limit is clamped by the queue package
// (queue.NormalizeTaskListLimit: default 50, cap 200) and the status filter
// accepts exactly the four engine states (queued/running/dead_letter/
// completed). The detail view carries attempts, last error and timestamps.
//
// REQUEUE: POST /queue/tasks/{id}/requeue revives a dead_letter task —
// attempts reset to 0, last_error cleared, requeues counter incremented,
// requeued_at stamped, created_at preserved — and appends it back to the work
// list so workers pick it up. Any other current state is a 409 CONFLICT;
// unknown AND foreign-tenant ids are the same 404 (no existence leak). Every
// successful requeue writes a queue.task_requeued audit row.
//
// DUAL MODE: all four operations dispatch through queue.Queue's Inspector
// implementation, so the endpoints transparently serve the in-memory backend
// (dev mode) and the Redis backend (REDIS_QUEUE=redis) with identical
// contracts.

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
	"agentos/internal/queue"
)

// queueTaskView is the wire shape of one task record (no organization_id:
// the tenant is implied by the caller's claims; the payload may legitimately
// echo the caller's own organization_id inside its keys).
type queueTaskView struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Status     string         `json:"status"`
	Attempts   int            `json:"attempts"`
	Requeues   int            `json:"requeues"`
	LastError  string         `json:"last_error,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
	RequeuedAt string         `json:"requeued_at,omitempty"`
}

func newQueueTaskView(rec *queue.TaskRecord) queueTaskView {
	view := queueTaskView{
		ID:        rec.ID,
		Type:      rec.Type,
		Status:    rec.Status,
		Attempts:  rec.Attempts,
		Requeues:  rec.Requeues,
		LastError: rec.LastError,
		Payload:   rec.Payload,
		CreatedAt: rec.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: rec.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if rec.RequeuedAt != nil {
		view.RequeuedAt = rec.RequeuedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

// claimsOrgIDQops resolves the caller's tenant from the auth context (never
// from client input).
func claimsOrgIDQops(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeQopsError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return "", false
	}
	if strings.TrimSpace(claims.OrganizationID) == "" {
		writeQopsError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing organization claim")
		return "", false
	}
	return claims.OrganizationID, true
}

// writeQopsJSON renders a JSON response (local helper, distinct name avoids
// collisions with other tracks' helpers in package main).
func writeQopsJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeQopsError renders the shared structured error envelope.
func writeQopsError(w http.ResponseWriter, status int, code, message string) {
	writeQopsJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// mapQopsError translates the typed queue introspection errors onto the HTTP
// contract (404/409/400), with a conservative 500 fallback.
func mapQopsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, queue.ErrTaskNotFound):
		writeQopsError(w, http.StatusNotFound, "NOT_FOUND", "queue task not found")
	case errors.Is(err, queue.ErrNotRequeueable):
		writeQopsError(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, queue.ErrInvalidCursor):
		writeQopsError(w, http.StatusBadRequest, "INVALID_CURSOR", "cursor is malformed or stale")
	case errors.Is(err, queue.ErrInvalidStatus):
		writeQopsError(w, http.StatusBadRequest, "INVALID_REQUEST", fmt.Sprintf("status must be one of %q, %q, %q, %q",
			queue.StatusQueued, queue.StatusRunning, queue.StatusDeadLetter, queue.StatusCompleted))
	case errors.Is(err, queue.ErrOrgRequired):
		writeQopsError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "missing organization scope")
	default:
		writeQopsError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
}

// listQueueTasksHandler serves GET /queue/tasks: the caller's org-scoped task
// records, newest enqueue first, keyset-paginated.
func listQueueTasksHandler(q *queue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q == nil {
			writeQopsError(w, http.StatusServiceUnavailable, "QUEUE_UNAVAILABLE", "task queue not available")
			return
		}
		orgID, ok := claimsOrgIDQops(w, r)
		if !ok {
			return
		}
		limit := queue.DefaultTaskListLimit
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				writeQopsError(w, http.StatusBadRequest, "INVALID_REQUEST",
					fmt.Sprintf("limit must be an integer, got %q", raw))
				return
			}
			if parsed <= 0 {
				writeQopsError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be positive")
				return
			}
			limit = parsed
		}
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))

		records, next, err := q.ListTasks(r.Context(), orgID, status, limit, cursor)
		if err != nil {
			mapQopsError(w, err)
			return
		}
		views := make([]queueTaskView, 0, len(records))
		for _, rec := range records {
			views = append(views, newQueueTaskView(rec))
		}
		writeQopsJSON(w, http.StatusOK, map[string]any{"tasks": views, "next_cursor": next})
	}
}

// getQueueTaskHandler serves GET /queue/tasks/{id}: one org-scoped task
// record incl. attempts, last error and timestamps.
func getQueueTaskHandler(q *queue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q == nil {
			writeQopsError(w, http.StatusServiceUnavailable, "QUEUE_UNAVAILABLE", "task queue not available")
			return
		}
		orgID, ok := claimsOrgIDQops(w, r)
		if !ok {
			return
		}
		record, err := q.GetTask(r.Context(), orgID, r.PathValue("id"))
		if err != nil {
			mapQopsError(w, err)
			return
		}
		writeQopsJSON(w, http.StatusOK, newQueueTaskView(record))
	}
}

// requeueQueueTaskHandler serves POST /queue/tasks/{id}/requeue: revives a
// dead_letter task (attempts reset, last_error cleared, requeued_at stamped,
// work-list append) and audits the operation. OWNER/ADMIN only.
func requeueQueueTaskHandler(q *queue.Queue, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q == nil {
			writeQopsError(w, http.StatusServiceUnavailable, "QUEUE_UNAVAILABLE", "task queue not available")
			return
		}
		orgID, ok := claimsOrgIDQops(w, r)
		if !ok {
			return
		}
		taskID := r.PathValue("id")
		record, err := q.RequeueTask(r.Context(), orgID, taskID)
		if err != nil {
			mapQopsError(w, err)
			return
		}
		// Best-effort audit trail entry (tenant-scoped insert), mirroring the
		// runs.create pattern: an audit failure never fails the operation.
		if auditSvc != nil {
			if claims, claimsErr := auth.ExtractClaims(r.Context()); claimsErr == nil {
				metadata := map[string]any{"task_type": record.Type, "requeues": record.Requeues}
				if record.RequeuedAt != nil {
					metadata["requeued_at"] = record.RequeuedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
				}
				_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "queue.task_requeued", orgID,
					"queue/tasks/"+taskID, metadata)
			}
		}
		writeQopsJSON(w, http.StatusOK, newQueueTaskView(record))
	}
}

// queueStatsHandler serves GET /queue/stats: the caller's org task depth per
// status (all four engine states always present) plus the total.
func queueStatsHandler(q *queue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q == nil {
			writeQopsError(w, http.StatusServiceUnavailable, "QUEUE_UNAVAILABLE", "task queue not available")
			return
		}
		orgID, ok := claimsOrgIDQops(w, r)
		if !ok {
			return
		}
		stats, total, err := q.TaskStats(r.Context(), orgID)
		if err != nil {
			mapQopsError(w, err)
			return
		}
		writeQopsJSON(w, http.StatusOK, map[string]any{"stats": stats, "total": total})
	}
}

// registerQueueOpsRoutes mounts the queue introspection + requeue surface on
// apiMux: reads behind runs.read (all roles), requeue behind the dedicated
// queue.manage permission (OWNER/ADMIN; MEMBER/VIEWER denied).
func registerQueueOpsRoutes(apiMux *http.ServeMux, q *queue.Queue, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil {
		return
	}
	readGuard := func(next http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
			auth.RequirePermission(authSvc, auth.PermissionRunsRead)(next))
	}
	manageGuard := func(next http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
			auth.RequirePermission(authSvc, auth.PermissionQueueManage)(next))
	}
	apiMux.Handle("GET /queue/tasks", readGuard(http.HandlerFunc(listQueueTasksHandler(q))))
	apiMux.Handle("GET /queue/tasks/{id}", readGuard(http.HandlerFunc(getQueueTaskHandler(q))))
	apiMux.Handle("GET /queue/stats", readGuard(http.HandlerFunc(queueStatsHandler(q))))
	apiMux.Handle("POST /queue/tasks/{id}/requeue", manageGuard(http.HandlerFunc(requeueQueueTaskHandler(q, auditSvc))))
}
