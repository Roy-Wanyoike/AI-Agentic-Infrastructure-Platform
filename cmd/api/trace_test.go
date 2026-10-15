package main

// trace_test.go — issue #114 HTTP surface, exercised through the REAL
// middleware chain (RequireAuthOrAPIKey -> RequirePermission(runs.read)):
// 401 unauthenticated, 404 (trace_not_available) for unknown AND foreign-org
// runs (no existence leak), and the happy {run_id, trace_id} shape.
// Companion test of cmd/api/trace.go (residency_test.go precedent).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	authpkg "agentos/internal/auth"
	"agentos/internal/observability"
	"agentos/internal/runs"

	"go.opentelemetry.io/otel/trace"
)

func newTraceTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *runs.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	runSvc := runs.NewService()
	apiMux := http.NewServeMux()
	registerRunTraceRoutes(apiMux, runSvc, authSvc, keysSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, runSvc
}

func trTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&authpkg.User{ID: "user-" + email, Organization: orgID, Email: email, Role: role})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

func trReq(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRunTraceAuthRequired(t *testing.T) {
	h, _, _ := newTraceTestRouter(t)
	if rr := trReq(t, h, "/api/v1/runs/run-1/trace", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated trace: want 401, got %d", rr.Code)
	}
}

func TestRunTraceUnknownAndCrossOrgAre404(t *testing.T) {
	h, authSvc, _ := newTraceTestRouter(t)
	orgA := trTokenFor(t, authSvc, "a@x.io", "org-a", "OWNER")
	orgB := trTokenFor(t, authSvc, "b@x.io", "org-b", "OWNER")

	// Unknown run: 404 trace_not_available (structured error envelope).
	rr := trReq(t, h, "/api/v1/runs/does-not-exist/trace", orgA)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown run: want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, rr.Body.String())
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok || errObj["code"] != "trace_not_available" {
		t.Fatalf("404 shape wrong: %v", body)
	}

	// Cross-org: the foreign run is indistinguishable from a trace miss.
	tid, terr := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if terr != nil {
		t.Fatalf("trace id: %v", terr)
	}
	observability.RecordRunTraceID("run-org-a", tid)
	rr = trReq(t, h, "/api/v1/runs/run-org-a/trace", orgB)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org trace: want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunTraceHappyPathShape(t *testing.T) {
	h, authSvc, runSvc := newTraceTestRouter(t)
	token := trTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	run, err := runSvc.CreateRunCtx(context.Background(), "org-a", "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx: %v", err)
	}
	tid, terr := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if terr != nil {
		t.Fatalf("trace id: %v", terr)
	}
	observability.RecordRunTraceID(run.ID, tid)

	rr := trReq(t, h, "/api/v1/runs/"+run.ID+"/trace", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("happy path: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, rr.Body.String())
	}
	if got["run_id"] != run.ID || got["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace body wrong: %v", got)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
}
