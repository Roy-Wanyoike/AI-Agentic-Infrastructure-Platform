package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
	"agentos/internal/scheduler"
)

// newSchedulesTestRouter builds a mux with only the scheduler routes mounted,
// exactly the way main.go mounts them (StripPrefix under /api/v1), plus the
// auth services needed to obtain credentials.
func newSchedulesTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *apikeys.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	schedSvc := scheduler.NewService()
	apiMux := http.NewServeMux()
	registerSchedulesRoutes(apiMux, schedSvc, authSvc, keysSvc, audit.NewService())
	return http.StripPrefix("/api/v1", apiMux), authSvc, keysSvc
}

// schedulesTokenFor issues a bearer token for a user in the given org with the
// given role (claims carry the role; RequirePermission falls back to it when
// the user is not registered in memory).
func schedulesTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&authpkg.User{
		ID:           "user-" + email,
		Organization: orgID,
		Email:        email,
		Role:         role,
	})
	if err != nil {
		t.Fatalf("generate token failed: %v", err)
	}
	return token
}

func doSchedReq(t *testing.T, h http.Handler, method, path, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeSchedBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

func TestSchedulesAuthRequired(t *testing.T) {
	h, _, _ := newSchedulesTestRouter(t)
	rr := doSchedReq(t, h, http.MethodGet, "/api/v1/schedules", "", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/create", "", `{"agent_id":"a","kind":"once"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on create, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSchedulesViewerReadOnly(t *testing.T) {
	h, authSvc, _ := newSchedulesTestRouter(t)
	viewer := schedulesTokenFor(t, authSvc, "viewer@sched.test", "org-view", "VIEWER")

	rr := doSchedReq(t, h, http.MethodGet, "/api/v1/schedules", viewer, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer read should be allowed, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeSchedBody(t, rr)
	if _, ok := body["schedules"].([]any); !ok {
		t.Fatalf("expected schedules array, got %#v", body)
	}

	rr = doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/create", viewer, `{"agent_id":"a","kind":"once","run_at":"2030-01-01T00:00:00Z"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer write should be forbidden, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = doSchedReq(t, h, http.MethodDelete, "/api/v1/schedules/some-id", viewer, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer delete should be forbidden, got %d", rr.Code)
	}
}

func TestSchedulesAPIKeyAuth(t *testing.T) {
	h, _, keysSvc := newSchedulesTestRouter(t)
	key, err := keysSvc.Create("org-key", "key-user", "ci")
	if err != nil {
		t.Fatalf("api key create failed: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/schedules", nil)
	req.Header.Set("X-API-Key", key.Value)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with X-API-Key, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSchedulesCreateValidationErrors(t *testing.T) {
	h, authSvc, _ := newSchedulesTestRouter(t)
	owner := schedulesTokenFor(t, authSvc, "owner@sched.test", "org-1", "OWNER")

	cases := []struct {
		name string
		body string
	}{
		{"once missing run_at", `{"agent_id":"a","kind":"once"}`},
		{"once bad run_at", `{"agent_id":"a","kind":"once","run_at":"not-a-time"}`},
		{"recurring small interval", `{"agent_id":"a","kind":"recurring","interval_seconds":30}`},
		{"cron missing expr", `{"agent_id":"a","kind":"cron","timezone":"UTC"}`},
		{"cron invalid expr", `{"agent_id":"a","kind":"cron","cron_expr":"60 * * * *","timezone":"UTC"}`},
		{"cron missing timezone", `{"agent_id":"a","kind":"cron","cron_expr":"0 9 * * *"}`},
		{"cron bad timezone", `{"agent_id":"a","kind":"cron","cron_expr":"0 9 * * *","timezone":"Mars/Olympus"}`},
		{"unknown kind", `{"agent_id":"a","kind":"hourly"}`},
		{"missing agent", `{"kind":"once","run_at":"2030-01-01T00:00:00Z"}`},
	}
	for _, tc := range cases {
		rr := doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/create", owner, tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: expected 422, got %d body=%s", tc.name, rr.Code, rr.Body.String())
			continue
		}
		body := decodeSchedBody(t, rr)
		errObj, ok := body["error"].(map[string]any)
		if !ok || errObj["code"] != "VALIDATION_ERROR" {
			t.Errorf("%s: expected VALIDATION_ERROR envelope, got %#v", tc.name, body)
		}
	}

	// malformed JSON -> 400 INVALID_REQUEST
	rr := doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/create", owner, `{not-json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeSchedBody(t, rr)
	if errObj := body["error"].(map[string]any); errObj["code"] != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %#v", body)
	}
}

func TestSchedulesOwnerLifecycle(t *testing.T) {
	h, authSvc, _ := newSchedulesTestRouter(t)
	owner := schedulesTokenFor(t, authSvc, "owner@sched.test", "org-1", "OWNER")

	// create kind=once
	runAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	rr := doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/create", owner,
		`{"agent_id":"agent-1","input":"hello","kind":"once","run_at":"`+runAt+`"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create once: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeSchedBody(t, rr)
	sched, ok := body["schedule"].(map[string]any)
	if !ok {
		t.Fatalf("expected schedule object, got %#v", body)
	}
	id, _ := sched["id"].(string)
	if id == "" {
		t.Fatalf("missing schedule id: %#v", sched)
	}
	if sched["kind"] != "once" || sched["status"] != "active" {
		t.Fatalf("unexpected kind/status: %#v", sched)
	}
	if sched["run_at"] != runAt {
		t.Fatalf("run_at mismatch: got %v want %v", sched["run_at"], runAt)
	}
	if sched["next_run_at"] != runAt {
		t.Fatalf("next_run_at should equal run_at for once: %#v", sched)
	}
	if sched["timezone"] != "UTC" {
		t.Fatalf("default timezone should be UTC, got %#v", sched)
	}

	// create kind=recurring
	rr = doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/create", owner,
		`{"agent_id":"agent-1","kind":"recurring","interval_seconds":120}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create recurring: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	recur := decodeSchedBody(t, rr)["schedule"].(map[string]any)
	nextAt, ok := recur["next_run_at"].(string)
	if !ok || nextAt == "" {
		t.Fatalf("recurring must compute next_run_at, got %#v", recur)
	}
	if _, err := time.Parse(time.RFC3339, nextAt); err != nil {
		t.Fatalf("next_run_at not RFC3339: %v", err)
	}

	// create kind=cron (explicit timezone)
	rr = doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/create", owner,
		`{"agent_id":"agent-1","kind":"cron","cron_expr":"30 9 * * 1-5","timezone":"Europe/Berlin"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create cron: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	cron := decodeSchedBody(t, rr)["schedule"].(map[string]any)
	if cron["timezone"] != "Europe/Berlin" || cron["cron_expr"] != "30 9 * * 1-5" {
		t.Fatalf("cron echo mismatch: %#v", cron)
	}

	// list shows all three
	rr = doSchedReq(t, h, http.MethodGet, "/api/v1/schedules", owner, "")
	body = decodeSchedBody(t, rr)
	list, _ := body["schedules"].([]any)
	if len(list) != 3 {
		t.Fatalf("expected 3 schedules in list, got %d", len(list))
	}

	// get by id
	rr = doSchedReq(t, h, http.MethodGet, "/api/v1/schedules/"+id, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get by id: expected 200, got %d", rr.Code)
	}

	// pause -> paused, second pause -> 409
	rr = doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/"+id+"/pause", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("pause: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := decodeSchedBody(t, rr)["schedule"].(map[string]any)["status"]; got != "paused" {
		t.Fatalf("expected paused, got %#v", got)
	}
	rr = doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/"+id+"/pause", owner, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("double pause: expected 409, got %d", rr.Code)
	}

	// resume -> active, second resume -> 409
	rr = doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/"+id+"/resume", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("resume: expected 200, got %d", rr.Code)
	}
	if got := decodeSchedBody(t, rr)["schedule"].(map[string]any)["status"]; got != "active" {
		t.Fatalf("expected active after resume, got %#v", got)
	}
	rr = doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/"+id+"/resume", owner, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("double resume: expected 409, got %d", rr.Code)
	}

	// delete -> {"deleted": true}; get afterwards -> 404; delete again -> 404
	rr = doSchedReq(t, h, http.MethodDelete, "/api/v1/schedules/"+id, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", rr.Code)
	}
	if got := decodeSchedBody(t, rr)["deleted"]; got != true {
		t.Fatalf("expected deleted:true, got %#v", got)
	}
	rr = doSchedReq(t, h, http.MethodGet, "/api/v1/schedules/"+id, owner, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", rr.Code)
	}
	rr = doSchedReq(t, h, http.MethodDelete, "/api/v1/schedules/"+id, owner, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("double delete: expected 404, got %d", rr.Code)
	}
}

func TestSchedulesCrossTenantIsolation(t *testing.T) {
	h, authSvc, _ := newSchedulesTestRouter(t)
	owner1 := schedulesTokenFor(t, authSvc, "o1@sched.test", "org-a", "OWNER")
	owner2 := schedulesTokenFor(t, authSvc, "o2@sched.test", "org-b", "OWNER")

	rr := doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/create", owner1,
		`{"agent_id":"a","kind":"recurring","interval_seconds":60}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d body=%s", rr.Code, rr.Body.String())
	}
	id := decodeSchedBody(t, rr)["schedule"].(map[string]any)["id"].(string)

	// org-b cannot see or mutate org-a's schedule (404s, never 403 leaks).
	rr = doSchedReq(t, h, http.MethodGet, "/api/v1/schedules/"+id, owner2, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get: expected 404, got %d", rr.Code)
	}
	rr = doSchedReq(t, h, http.MethodPost, "/api/v1/schedules/"+id+"/pause", owner2, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant pause: expected 404, got %d", rr.Code)
	}
	rr = doSchedReq(t, h, http.MethodDelete, "/api/v1/schedules/"+id, owner2, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete: expected 404, got %d", rr.Code)
	}
	// listing for org-b must be empty
	rr = doSchedReq(t, h, http.MethodGet, "/api/v1/schedules", owner2, "")
	if list := decodeSchedBody(t, rr)["schedules"].([]any); len(list) != 0 {
		t.Fatalf("cross-tenant list must be empty, got %#v", list)
	}
}

func TestSchedulesMethodNotAllowed(t *testing.T) {
	h, authSvc, _ := newSchedulesTestRouter(t)
	owner := schedulesTokenFor(t, authSvc, "owner@sched.test", "org-1", "OWNER")
	rr := doSchedReq(t, h, http.MethodPatch, "/api/v1/schedules/x", owner, "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH on detail route: expected 405, got %d", rr.Code)
	}
}
