package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/streaming"
)

func TestCreateRunAndPostEventRoundtrip(t *testing.T) {
	// prepare services
	q := queue.NewQueue()
	rs := runs.NewService()
	s := streaming.NewService()
	rs.SetStreamer(s)
	// expose global for handlers that use it
	runsServiceVar = rs

	// create an auth service and user so we can call the handler with auth
	authService := authpkg.NewService("dev-secret")
	org, user, err := authService.Register("org-x", "tester@example.com", "pw")
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	token, err := authService.GenerateToken(user)
	if err != nil {
		t.Fatalf("generate token failed: %v", err)
	}

	// create run via handler with auth middleware
	payload := `{"organization_id":"` + org.ID + `","agent_id":"agent-1","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	authpkg.RequireAuth(authService)(createRunHandler(q, audit.NewService())).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected created, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	runID, _ := resp["run_id"].(string)
	if runID == "" {
		t.Fatalf("missing run_id in response")
	}

	// post an event via runEventsHandler
	ev := `{"type":"status","name":"status.changed","payload":{"status":"RUNNING"}}`
	postReq := httptest.NewRequest(http.MethodPost, "/v1/runs/"+runID+"/events", strings.NewReader(ev))
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	runEventsHandler(s).ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusNoContent {
		t.Fatalf("expected no content, got %d body=%s", postRec.Code, postRec.Body.String())
	}

	// fetch history via GET
	getReq := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/events", nil)
	getRec := httptest.NewRecorder()
	runEventsHandler(s).ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", getRec.Code, getRec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	events, ok := got["events"].([]any)
	if !ok || len(events) == 0 {
		t.Fatalf("expected events in history, got %#v", got)
	}
}
