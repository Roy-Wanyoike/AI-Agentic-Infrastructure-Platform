package main

// simulation_test.go — issue #133 HTTP surface, exercised through the REAL
// middleware chain (RequireAuthOrAPIKey -> RequirePermission), mirroring
// cmd/api/synth_test.go. Covers: 401 without credentials, the
// simulation.read/simulation.write RBAC matrix, 422 validation failures,
// 404 for unknown AND cross-org ids (no existence leak), the async
// create->run->completed lifecycle through the API, org-scoped listing,
// and one full-app E2E smoke proving the main.go registration line serves
// /simulations under /api/v1.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/apikeys"
	authpkg "agentos/internal/auth"
	"agentos/internal/simulation"
)

func newSimulationTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *simulation.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	// deterministic clock for the engine + fixed id source for assertions
	svc := simulation.NewService(
		simulation.WithRunner(simulation.NewRunner(simulation.WithClock(simulation.FixedClock{T: time.Date(2014, 6, 7, 18, 44, 0, 0, time.UTC)}))),
	)
	apiMux := http.NewServeMux()
	registerSimulationRoutes(apiMux, svc, authSvc, keysSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, svc
}

// simulationTokenFor mints a JWT for a role/org (synthTokenFor pattern).
func simulationTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&authpkg.User{ID: "user-" + email, Organization: orgID, Email: email, Role: role})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

// simReq issues one request against the stripped mux (synthReq pattern).
func simReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// simBody decodes the JSON envelope (synthBody pattern).
func simBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, rr.Body.String())
	}
	return out
}

// simErrorCode extracts the error code from the house envelope.
func simErrorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	body := simBody(t, rr)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope: %s", rr.Body.String())
	}
	code, _ := errObj["code"].(string)
	return code
}

// simConfigBody is a minimal valid POST /simulations payload.
const simConfigBody = `{"name":"cross-platform check","kind":"social","rounds":3,"seed":42,"location":"UTC",
 "agents":[{"core_identity":{"id":"a","name":"Ada","traits":{"extraversion":0.9}},
            "platform_behavior":{"social":{"weights":{"post":1}}}}]}`

// TestSimulationAuthRequired: no credentials -> 401 on every endpoint.
func TestSimulationAuthRequired(t *testing.T) {
	h, _, _ := newSimulationTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/simulations"},
		{http.MethodGet, "/api/v1/simulations"},
		{http.MethodGet, "/api/v1/simulations/sim-1"},
	} {
		rr := simReq(t, h, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without auth = %d, want 401", tc.method, tc.path, rr.Code)
		}
	}
}

// TestSimulationRBACMatrix: simulation.read is all roles (VIEWER included);
// simulation.write excludes VIEWER (403), grants MEMBER (202).
func TestSimulationRBACMatrix(t *testing.T) {
	h, authSvc, _ := newSimulationTestRouter(t)
	token := func(role string) string {
		return simulationTokenFor(t, authSvc, role+"@x.io", "org-1", role)
	}

	// reads: every role incl. VIEWER
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		rr := simReq(t, h, http.MethodGet, "/api/v1/simulations", token(role), "")
		if rr.Code != http.StatusOK {
			t.Fatalf("list as %s = %d, want 200", role, rr.Code)
		}
	}

	// writes: VIEWER is forbidden (the middleware's plain-text 403 —
	// house middleware renders middleware-level denials without the JSON
	// envelope; the handler-level errors below carry the envelope)
	rr := simReq(t, h, http.MethodPost, "/api/v1/simulations", token("VIEWER"), simConfigBody)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("create as VIEWER = %d, want 403", rr.Code)
	}

	// writes: MEMBER is granted
	rr = simReq(t, h, http.MethodPost, "/api/v1/simulations", token("MEMBER"), simConfigBody)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create as MEMBER = %d, want 202 body=%s", rr.Code, rr.Body.String())
	}
}

// TestSimulationValidation: malformed configs fail fast with 422 and no
// record is created (fail-fast before the async run exists).
func TestSimulationValidation(t *testing.T) {
	h, authSvc, svc := newSimulationTestRouter(t)
	token := simulationTokenFor(t, authSvc, "m@x.io", "org-1", "MEMBER")
	for name, body := range map[string]string{
		"empty body":    "",
		"invalid json":  "{not json",
		"unknown kind":  `{"kind":"quantum","rounds":2,"seed":1,"agents":[{"core_identity":{"id":"a"}}]}`,
		"zero rounds":   `{"kind":"social","rounds":0,"seed":1,"agents":[{"core_identity":{"id":"a"}}]}`,
		"no agents":     `{"kind":"social","rounds":2,"seed":1,"agents":[]}`,
		"bad timezone":  `{"kind":"social","rounds":2,"seed":1,"location":"Mars/Olympus","agents":[{"core_identity":{"id":"a"}}]}`,
		"missing agent": `{"kind":"social","rounds":2,"seed":1}`,
	} {
		rr := simReq(t, h, http.MethodPost, "/api/v1/simulations", token, body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: create = %d, want 422 body=%s", name, rr.Code, rr.Body.String())
		}
		if code := simErrorCode(t, rr); code != "VALIDATION_ERROR" {
			t.Fatalf("%s: error code = %q, want VALIDATION_ERROR", name, code)
		}
	}
	// nothing was created
	if sims, _ := svc.List("org-1"); len(sims) != 0 {
		t.Fatalf("invalid creates leaked records: %d", len(sims))
	}
}

// TestSimulationLifecycleAndIsolation: the async lifecycle through the API
// (202 queued -> completed with result) plus cross-org 404 + org-scoped list.
func TestSimulationLifecycleAndIsolation(t *testing.T) {
	h, authSvc, _ := newSimulationTestRouter(t)
	orgA := simulationTokenFor(t, authSvc, "a@x.io", "org-a", "MEMBER")
	orgB := simulationTokenFor(t, authSvc, "b@x.io", "org-b", "MEMBER")

	// create in org-a
	rr := simReq(t, h, http.MethodPost, "/api/v1/simulations", orgA, simConfigBody)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create = %d, want 202 body=%s", rr.Code, rr.Body.String())
	}
	sim, ok := simBody(t, rr)["simulation"].(map[string]any)
	if !ok {
		t.Fatalf("missing simulation: %s", rr.Body.String())
	}
	id, _ := sim["id"].(string)
	if id == "" {
		t.Fatalf("missing id: %s", rr.Body.String())
	}
	if status, _ := sim["status"].(string); status != "queued" {
		t.Fatalf("initial status = %q, want queued", status)
	}

	// wait for the async run to land (bounded)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rr = simReq(t, h, http.MethodGet, "/api/v1/simulations/"+id, orgA, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("get = %d, want 200 body=%s", rr.Code, rr.Body.String())
		}
		sim = simBody(t, rr)["simulation"].(map[string]any)
		if sim["status"] == "completed" || sim["status"] == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("simulation did not finish: %s", rr.Body.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if sim["status"] != "completed" {
		t.Fatalf("status = %v error=%v", sim["status"], sim["error"])
	}
	result, ok := sim["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result: %s", rr.Body.String())
	}
	rounds, _ := result["rounds"].([]any)
	if len(rounds) != 3 {
		t.Fatalf("rounds = %d, want 3", len(rounds))
	}
	notes, _ := result["emergent_notes"].([]any)
	if len(notes) == 0 {
		t.Fatal("missing emergent notes")
	}

	// org-b sees neither the record (404) nor it in a list (isolation)
	rr = simReq(t, h, http.MethodGet, "/api/v1/simulations/"+id, orgB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org get = %d, want 404", rr.Code)
	}
	if code := simErrorCode(t, rr); code != "NOT_FOUND" {
		t.Fatalf("error code = %q, want NOT_FOUND", code)
	}
	rr = simReq(t, h, http.MethodGet, "/api/v1/simulations", orgB, "")
	if got := int(simBody(t, rr)["count"].(float64)); got != 0 {
		t.Fatalf("org-b list count = %d, want 0", got)
	}

	// unknown id is the same 404
	rr = simReq(t, h, http.MethodGet, "/api/v1/simulations/nope", orgA, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown get = %d, want 404", rr.Code)
	}

	// org-a list contains exactly the one record
	rr = simReq(t, h, http.MethodGet, "/api/v1/simulations", orgA, "")
	if got := int(simBody(t, rr)["count"].(float64)); got != 1 {
		t.Fatalf("org-a list count = %d, want 1", got)
	}
}

// TestSimulationWiringE2E boots the REAL application (newApp in
// zero-infrastructure mode, issue #55 harness) and proves the main.go
// registration line serves /simulations under /api/v1 with real auth.
func TestSimulationWiringE2E(t *testing.T) {
	env := newE2EEnv(t)
	env.registerAndLogin(t)

	rr, resp := env.do(t, http.MethodPost, "/api/v1/simulations",
		`{"name":"e2e-cross-platform","kind":"market","rounds":2,"seed":7,
                  "agents":[{"core_identity":{"id":"m1","name":"Mo","traits":{"risk_appetite":0.9}},
                             "platform_behavior":{"market":{"weights":{"buy":1,"sell":0,"bid":0}}}}]}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("e2e create simulation = %d — body=%s", rr.Code, rr.Body.String())
	}
	sim, ok := resp["simulation"].(map[string]any)
	if !ok {
		t.Fatalf("e2e response missing simulation: %s", rr.Body.String())
	}
	id, _ := sim["id"].(string)

	// wait for the detached run to finish (bounded), through the full stack
	deadline := time.Now().Add(5 * time.Second)
	for {
		rr, resp = env.do(t, http.MethodGet, "/api/v1/simulations/"+id, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("e2e get simulation = %d — body=%s", rr.Code, rr.Body.String())
		}
		sim = resp["simulation"].(map[string]any)
		if sim["status"] == "completed" || sim["status"] == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("e2e simulation did not finish: %s", rr.Body.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if sim["status"] != "completed" {
		t.Fatalf("e2e status = %v (%v)", sim["status"], sim["error"])
	}
	// empty registry renders as an empty list (never null)
	rr, resp = env.do(t, http.MethodGet, "/api/v1/simulations", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("e2e list simulations = %d", rr.Code)
	}
	if _, ok := resp["simulations"].([]any); !ok {
		t.Fatalf("e2e list missing simulations array: %s", rr.Body.String())
	}
}
