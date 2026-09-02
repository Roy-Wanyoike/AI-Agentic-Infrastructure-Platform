package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/scheduler"
)

// schedules.go mounts the wave-2 scheduler track (2-f) endpoints:
//
//      GET    /schedules            (schedules.read)
//      POST   /schedules/create     (schedules.write)
//      GET    /schedules/{id}       (schedules.read)
//      POST   /schedules/{id}/pause  (schedules.write)
//      POST   /schedules/{id}/resume (schedules.write)
//      DELETE /schedules/{id}       (schedules.write)
//
// Routes are registered on apiMux so main.go serves them under both /v1 and
// /api/v1 (StripPrefix mounting). Tenant scope comes exclusively from auth
// claims — client-supplied organization ids are never trusted.

// registerSchedulesRoutes mounts all schedule routes on apiMux.
func registerSchedulesRoutes(apiMux *http.ServeMux, schedSvc *scheduler.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	// auth wrap pattern from cmd/api/main.go: RequireAuthOrAPIKey outer,
	// RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("GET /schedules", wrap(auth.PermissionSchedulesRead, http.HandlerFunc(listSchedulesHandler(schedSvc))))
	apiMux.Handle("POST /schedules/create", wrap(auth.PermissionSchedulesWrite, http.HandlerFunc(createScheduleHandler(schedSvc, auditSvc))))
	apiMux.Handle("GET /schedules/{id}", wrap(auth.PermissionSchedulesRead, http.HandlerFunc(getScheduleHandler(schedSvc))))
	apiMux.Handle("POST /schedules/{id}/pause", wrap(auth.PermissionSchedulesWrite, http.HandlerFunc(pauseScheduleHandler(schedSvc))))
	apiMux.Handle("POST /schedules/{id}/resume", wrap(auth.PermissionSchedulesWrite, http.HandlerFunc(resumeScheduleHandler(schedSvc))))
	apiMux.Handle("DELETE /schedules/{id}", wrap(auth.PermissionSchedulesWrite, http.HandlerFunc(deleteScheduleHandler(schedSvc, auditSvc))))
}

// writeJSONSched serializes v with the given status (distinct name to avoid
// clashing with helpers in other handler files).
func writeJSONSched(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeSchedError emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeSchedError(w http.ResponseWriter, status int, code, message string) {
	writeJSONSched(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readSchedJSON decodes the request body into dst, writing a 400 envelope on
// malformed JSON. Returns false when the response is already written.
func readSchedJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeSchedError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// mapScheduleError converts scheduler service errors into contract error
// responses (404 / 409 / 422 / 500).
func mapScheduleError(w http.ResponseWriter, err error) {
	var verr *scheduler.ValidationError
	switch {
	case errors.Is(err, scheduler.ErrScheduleNotFound):
		writeSchedError(w, http.StatusNotFound, "SCHEDULE_NOT_FOUND", "schedule not found")
	case errors.As(err, &verr),
		errors.Is(err, scheduler.ErrAgentRequired),
		errors.Is(err, scheduler.ErrInvalidKind),
		errors.Is(err, scheduler.ErrRunAtRequired),
		errors.Is(err, scheduler.ErrInvalidRunAt),
		errors.Is(err, scheduler.ErrIntervalTooSmall),
		errors.Is(err, scheduler.ErrInvalidInterval),
		errors.Is(err, scheduler.ErrCronExprRequired),
		errors.Is(err, scheduler.ErrCronNeverFires),
		errors.Is(err, scheduler.ErrTimezoneRequired),
		errors.Is(err, scheduler.ErrInvalidTimezone):
		writeSchedError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, scheduler.ErrScheduleNotActive):
		writeSchedError(w, http.StatusConflict, "INVALID_STATE", scheduler.ErrScheduleNotActive.Error())
	case errors.Is(err, scheduler.ErrScheduleNotPaused):
		writeSchedError(w, http.StatusConflict, "INVALID_STATE", scheduler.ErrScheduleNotPaused.Error())
	case errors.Is(err, scheduler.ErrScheduleCompleted):
		writeSchedError(w, http.StatusConflict, "SCHEDULE_COMPLETED", scheduler.ErrScheduleCompleted.Error())
	default:
		writeSchedError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// scheduleJSON renders one schedule with the contract field names (RFC3339
// UTC timestamps; unset optional timestamps serialize as null). last_fired_at
// and updated_at are additive fields beyond the contract shape.
func scheduleJSON(s *scheduler.Schedule) map[string]any {
	out := map[string]any{
		"id":               s.ID,
		"agent_id":         s.AgentID,
		"input":            s.Input,
		"kind":             s.Kind,
		"run_at":           nil,
		"interval_seconds": s.IntervalSeconds,
		"cron_expr":        s.CronExpr,
		"timezone":         s.Timezone,
		"status":           s.Status,
		"next_run_at":      nil,
		"last_run_id":      s.LastRunID,
		"last_fired_at":    nil,
		"created_at":       s.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":       s.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if s.RunAt != nil {
		out["run_at"] = s.RunAt.UTC().Format(time.RFC3339)
	}
	if s.NextRunAt != nil {
		out["next_run_at"] = s.NextRunAt.UTC().Format(time.RFC3339)
	}
	if s.LastFiredAt != nil {
		out["last_fired_at"] = s.LastFiredAt.UTC().Format(time.RFC3339)
	}
	return out
}

func listSchedulesHandler(svc *scheduler.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSchedError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		// Tenant guard: the listing filters on the caller's organization_id.
		list, err := svc.List(r.Context(), claims.OrganizationID)
		if err != nil {
			mapScheduleError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(list))
		for _, sched := range list {
			items = append(items, scheduleJSON(sched))
		}
		writeJSONSched(w, http.StatusOK, map[string]any{"schedules": items})
	}
}

func createScheduleHandler(svc *scheduler.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSchedError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		var req struct {
			AgentID         string `json:"agent_id"`
			Input           string `json:"input"`
			Kind            string `json:"kind"`
			RunAt           string `json:"run_at"`
			IntervalSeconds int    `json:"interval_seconds"`
			CronExpr        string `json:"cron_expr"`
			Timezone        string `json:"timezone"`
		}
		if !readSchedJSON(w, r, &req) {
			return
		}
		// Tenant guard: the schedule is created with the caller's organization_id;
		// client-supplied org ids are ignored by design.
		sched, err := svc.Create(r.Context(), claims.OrganizationID, scheduler.CreateInput{
			AgentID:         req.AgentID,
			Input:           req.Input,
			Kind:            req.Kind,
			RunAt:           req.RunAt,
			IntervalSeconds: req.IntervalSeconds,
			CronExpr:        req.CronExpr,
			Timezone:        req.Timezone,
		})
		if err != nil {
			mapScheduleError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "schedule.created",
				claims.OrganizationID, "schedules/"+sched.ID, map[string]any{"kind": sched.Kind})
		}
		writeJSONSched(w, http.StatusCreated, map[string]any{"schedule": scheduleJSON(sched)})
	}
}

func getScheduleHandler(svc *scheduler.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSchedError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		if id == "" {
			writeSchedError(w, http.StatusNotFound, "SCHEDULE_NOT_FOUND", "schedule not found")
			return
		}
		// Tenant guard: the lookup requires the schedule's organization_id to
		// match the caller's tenant; foreign schedules surface as 404.
		sched, err := svc.Get(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapScheduleError(w, err)
			return
		}
		writeJSONSched(w, http.StatusOK, map[string]any{"schedule": scheduleJSON(sched)})
	}
}

func pauseScheduleHandler(svc *scheduler.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSchedError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		// Tenant guard: transition methods are scoped to the caller's org.
		sched, err := svc.Pause(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapScheduleError(w, err)
			return
		}
		writeJSONSched(w, http.StatusOK, map[string]any{"schedule": scheduleJSON(sched)})
	}
}

func resumeScheduleHandler(svc *scheduler.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSchedError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		// Tenant guard: transition methods are scoped to the caller's org.
		sched, err := svc.Resume(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapScheduleError(w, err)
			return
		}
		writeJSONSched(w, http.StatusOK, map[string]any{"schedule": scheduleJSON(sched)})
	}
}

func deleteScheduleHandler(svc *scheduler.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeSchedError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := r.PathValue("id")
		// Tenant guard: deletes require a matching organization_id.
		if err := svc.Delete(r.Context(), claims.OrganizationID, id); err != nil {
			mapScheduleError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "schedule.deleted",
				claims.OrganizationID, "schedules/"+id, nil)
		}
		writeJSONSched(w, http.StatusOK, map[string]any{"deleted": true})
	}
}
