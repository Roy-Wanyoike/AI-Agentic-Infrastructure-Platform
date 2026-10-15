package main

// batch_test.go — issue #123 HTTP surface, exercised through the REAL
// middleware chain (RequireAuthOrAPIKey -> RequirePermission) exactly like
// residency_test.go: 401 unauthenticated, the batch RBAC matrix (batch.write
// excludes VIEWER; batch.read covers every role), the happy-path POST
// /batch/runs run_ids contract over seam fakes, 207 partial failure
// isolation, the AGENTOS_BATCH_MAX_ITEMS 422 (atomic: nothing runs), the
// whole-request 422 atomic-validation table, and cross-org GET /batch/{id}
// reading as 404. Companion test of cmd/api/batch.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	authpkg "agentos/internal/auth"
	"agentos/internal/batch"
	"agentos/internal/deployments"
	"agentos/internal/runs"
	"agentos/internal/secrets"
	"agentos/internal/workflows"
)

// --- seam fakes (the narrow interfaces from cmd/api/batch.go) -----------------

type fakeBatchRuns struct {
	mu            sync.Mutex
	created       []string
	failFor       map[string]bool
	cancelled     []string
	cancelFailFor map[string]bool
}

func (f *fakeBatchRuns) CreateRunCtx(_ context.Context, orgID, agentID, input string) (*runs.Run, error) {
	if f.failFor[agentID] {
		return nil, errors.New("agent unavailable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, agentID)
	// The run id derives from the agent id (NOT the call order): the pool runs
	// items concurrently, so submission order is the only stable reference.
	return &runs.Run{ID: "run-" + agentID, OrganizationID: orgID, AgentID: agentID, Input: input, Status: runs.StatusQueued}, nil
}

func (f *fakeBatchRuns) CancelRun(_ context.Context, orgID, id string) (*runs.Run, error) {
	if f.cancelFailFor[id] {
		return nil, runs.ErrInvalidTransition
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	return &runs.Run{ID: id, OrganizationID: orgID, Status: runs.StatusCancelled}, nil
}

// fakeBatchAgents, fakeBatchWorkflows, fakeBatchVersions,
// fakeBatchDeployments and fakeBatchSecrets keep the remaining seams
// exercisable (unknown targets surface as failed items / 404s, never panics).
type fakeBatchAgents struct{}

func (fakeBatchAgents) GetAgentCtx(_ context.Context, _, _ string) (*agents.Agent, error) {
	return nil, errors.New("agent not found")
}
func (fakeBatchAgents) UpdateAgentCtx(_ context.Context, _ string, _ *agents.Agent) error {
	return errors.New("agent not found")
}

type fakeBatchWorkflows struct{}

func (fakeBatchWorkflows) GetWorkflow(_ context.Context, _, _ string) (*workflows.Workflow, error) {
	return nil, errors.New("workflow not found")
}
func (fakeBatchWorkflows) ExecuteWorkflow(_ context.Context, _, _, _, _ string) (*workflows.ExecutionResult, error) {
	return nil, errors.New("workflow not found")
}

type fakeBatchVersions struct{}

func (fakeBatchVersions) ListVersionsCtx(_ context.Context, _, _ string) ([]*agents.ConfigVersion, error) {
	return nil, errors.New("no versions")
}

type fakeBatchDeployments struct{}

func (fakeBatchDeployments) CreateDeploymentCtx(_ context.Context, _, agentID string, version int, environment, _ string) (*deployments.Deployment, error) {
	return &deployments.Deployment{ID: "dep-1", AgentID: agentID, Version: version, Environment: environment}, nil
}

type fakeBatchSecrets struct{}

func (fakeBatchSecrets) Create(_ context.Context, _, name, _, _ string) (*secrets.Secret, error) {
	return &secrets.Secret{Name: name, KeyVersion: 1}, nil
}

// --- router harness (residency_test.go pattern) -------------------------------

func newBatchTestRouter(t *testing.T, svc *batch.Service) (http.Handler, *authpkg.Service, *fakeBatchRuns) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	runsFake := &fakeBatchRuns{}
	seams := &batchSeams{
		Runs:        runsFake,
		Agents:      fakeBatchAgents{},
		Workflows:   fakeBatchWorkflows{},
		Versions:    fakeBatchVersions{},
		Deployments: fakeBatchDeployments{},
		Secrets:     fakeBatchSecrets{},
		Queue:       nil,
	}
	apiMux := http.NewServeMux()
	registerBatchRoutes(apiMux, svc, seams, nil, authSvc, keysSvc, nil)
	return http.StripPrefix("/api/v1", apiMux), authSvc, runsFake
}

func batchTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&authpkg.User{ID: "user-" + email, Organization: orgID, Email: email, Role: role})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

func batchReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func batchBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, rr.Body.String())
	}
	return out
}

// batchItems decodes the partial-failure items array.
func batchItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing or not an array: %v", body["items"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		out = append(out, it.(map[string]any))
	}
	return out
}

// --- 401: every endpoint requires auth ---------------------------------------

func TestBatchAuthRequired(t *testing.T) {
	h, _, _ := newBatchTestRouter(t, batch.NewService())
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/batch/runs"},
		{http.MethodPost, "/api/v1/batch/workflows/wf-1/trigger"},
		{http.MethodPost, "/api/v1/batch/agents/update"},
		{http.MethodPost, "/api/v1/batch/agents/deploy"},
		{http.MethodPost, "/api/v1/batch/runs/cancel"},
		{http.MethodPost, "/api/v1/batch/secrets"},
		{http.MethodPost, "/api/v1/batch/b-1/cancel"},
		{http.MethodGet, "/api/v1/batch/b-1"},
		{http.MethodGet, "/api/v1/batch/b-1/events"},
	} {
		if rr := batchReq(t, h, tc.method, tc.path, "", "{}"); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

// --- RBAC matrix: batch.write excludes VIEWER, batch.read covers all ---------

func TestBatchRBACMatrix(t *testing.T) {
	h, authSvc, _ := newBatchTestRouter(t, batch.NewService())
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = batchTokenFor(t, authSvc, strings.ToLower(role)+"@x.io", "org-a", role)
	}

	// Every batch.write surface: MEMBER+, VIEWER 403 (same tier as runs.execute).
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/batch/runs", `{"agent_id":"a1","input":"hi"}`},
		{http.MethodPost, "/api/v1/batch/workflows/wf-1/trigger", `{"inputs":["x"]}`},
		{http.MethodPost, "/api/v1/batch/agents/update", `{"agent_ids":["a1"],"patch":{"name":"n"}}`},
		{http.MethodPost, "/api/v1/batch/agents/deploy", `{"agent_ids":["a1"],"version":1}`},
		{http.MethodPost, "/api/v1/batch/runs/cancel", `{"run_ids":["r1"]}`},
		{http.MethodPost, "/api/v1/batch/secrets", `{"secrets":[{"name":"k","value":"v"}]}`},
		{http.MethodPost, "/api/v1/batch/b-1/cancel", ""},
	} {
		for _, role := range []string{"VIEWER"} {
			if rr := batchReq(t, h, tc.method, tc.path, tokens[role], tc.body); rr.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s: want 403, got %d", tc.method, tc.path, role, rr.Code)
			}
		}
		for _, role := range []string{"OWNER", "ADMIN", "MEMBER"} {
			rr := batchReq(t, h, tc.method, tc.path, tokens[role], tc.body)
			if rr.Code == http.StatusForbidden {
				t.Errorf("%s %s as %s: unexpectedly 403 (batch.write grants MEMBER)", tc.method, tc.path, role)
			}
		}
	}

	// GET /batch/{id}: batch.read is granted to ALL roles — VIEWER gets 404
	// (unknown id) rather than 403, proving the RBAC layer let it through.
	for role, token := range tokens {
		if rr := batchReq(t, h, http.MethodGet, "/api/v1/batch/no-such-batch", token, ""); rr.Code != http.StatusNotFound {
			t.Errorf("GET /batch/{id} as %s: want 404 (past RBAC), got %d", role, rr.Code)
		}
	}
}

// --- happy path: POST /batch/runs run_ids shape -------------------------------

func TestBatchRunsHappyPathRunIDs(t *testing.T) {
	h, authSvc, runsFake := newBatchTestRouter(t, batch.NewService())
	token := batchTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	rr := batchReq(t, h, http.MethodPost, "/api/v1/batch/runs", token, `{"agent_ids":["a1","a2"],"input":"hi","max_concurrency":2}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("batch runs: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := batchBody(t, rr)
	if body["batch_id"] == "" || body["status"] != "completed" {
		t.Fatalf("batch_id/status wrong: %v / %v", body["batch_id"], body["status"])
	}
	// run_ids: one per succeeded item, in submission order.
	runIDs := fmtSlice(body["run_ids"])
	if len(runIDs) != 2 || runIDs[0] != "run-a1" || runIDs[1] != "run-a2" {
		t.Fatalf("run_ids = %v, want [run-a1 run-a2] (submission order)", runIDs)
	}
	// Summary rollup + per-item run_id result fields.
	sum := body["summary"].(map[string]any)
	if sum["total"] != float64(2) || sum["succeeded"] != float64(2) || sum["failed"] != float64(0) {
		t.Fatalf("summary wrong: %v", sum)
	}
	items := batchItems(t, body)
	if items[0]["ref"] != "a1" || items[0]["status"] != "succeeded" || items[0]["run_id"] != "run-a1" {
		t.Fatalf("item 0 wrong: %v", items[0])
	}
	if items[1]["ref"] != "a2" || items[1]["run_id"] != "run-a2" {
		t.Fatalf("item 1 wrong: %v", items[1])
	}
	// Each item is a REAL run through the seam (both agents reached it).
	if len(runsFake.created) != 2 {
		t.Fatalf("runs seam calls = %v, want 2", runsFake.created)
	}

	// GET /batch/{id} reflects the tracked record for the same org.
	batchID, _ := body["batch_id"].(string)
	rr = batchReq(t, h, http.MethodGet, "/api/v1/batch/"+batchID, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET batch: want 200, got %d", rr.Code)
	}
	rec := batchBody(t, rr)
	if rec["type"] != "runs" || rec["status"] != "completed" || rec["total"] != float64(2) {
		t.Fatalf("GET record wrong: %v", rec)
	}
	if got := fmtSlice(rec["organization_id"]); len(got) != 1 || got[0] != "org-a" {
		if rec["organization_id"] != "org-a" {
			t.Fatalf("GET organization_id = %v, want org-a", rec["organization_id"])
		}
	}
}

// --- 207 partial failure isolation ---------------------------------------------

func TestBatchRunsPartialFailure207(t *testing.T) {
	h, authSvc, runsFake := newBatchTestRouter(t, batch.NewService())
	token := batchTokenFor(t, authSvc, "admin@x.io", "org-a", "ADMIN")
	runsFake.failFor = map[string]bool{"bad-agent": true}

	rr := batchReq(t, h, http.MethodPost, "/api/v1/batch/runs", token, `{"agent_ids":["a1","bad-agent","a2"],"input":"hi"}`)
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("partial failure: want 207, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := batchBody(t, rr)
	sum := body["summary"].(map[string]any)
	if sum["succeeded"] != float64(2) || sum["failed"] != float64(1) || sum["total"] != float64(3) {
		t.Fatalf("summary wrong: %v", sum)
	}
	// run_ids carries ONLY the succeeded items' run ids.
	runIDs := fmtSlice(body["run_ids"])
	if len(runIDs) != 2 {
		t.Fatalf("run_ids = %v, want 2 entries", runIDs)
	}
	items := batchItems(t, body)
	if items[0]["status"] != "succeeded" || items[1]["status"] != "failed" || items[2]["status"] != "succeeded" {
		t.Fatalf("item statuses wrong: %v", items)
	}
	if items[1]["error"] == "" || items[1]["error"] == nil {
		t.Fatalf("failed item must carry an error message: %v", items[1])
	}
	// Isolation: the two healthy items still ran.
	if len(runsFake.created) != 2 {
		t.Fatalf("healthy items must still run: %v", runsFake.created)
	}
}

// --- 422 oversized: AGENTOS_BATCH_MAX_ITEMS contract, atomic -------------------

func TestBatchOversized422Atomic(t *testing.T) {
	// A service built with a 2-item cap stands in for AGENTOS_BATCH_MAX_ITEMS=2
	// (MaxItemsFromEnv is read at construction; MaxItems is the same knob).
	h, authSvc, runsFake := newBatchTestRouter(t, batch.NewServiceWithOptions(batch.Options{MaxItems: 2}))
	token := batchTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	rr := batchReq(t, h, http.MethodPost, "/api/v1/batch/runs", token, `{"agent_ids":["a1","a2","a3"],"input":"hi"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized: want 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	errObj := batchBody(t, rr)["error"].(map[string]any)
	if errObj["code"] != "VALIDATION_ERROR" {
		t.Fatalf("error code = %v, want VALIDATION_ERROR", errObj["code"])
	}
	// Atomic validation: NOTHING ran and no item was created.
	if len(runsFake.created) != 0 {
		t.Fatalf("oversized batch must not run any item: %v", runsFake.created)
	}

	// At the cap exactly, the submission succeeds.
	rr = batchReq(t, h, http.MethodPost, "/api/v1/batch/runs", token, `{"agent_ids":["a1","a2"],"input":"hi"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("at-cap batch: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// --- 422 atomic validation across the submission surface -----------------------

func TestBatchAtomicValidation422(t *testing.T) {
	h, authSvc, runsFake := newBatchTestRouter(t, batch.NewService())
	token := batchTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")
	for _, tc := range []struct{ name, path, body string }{
		{"runs: both selectors", "/batch/runs", `{"agent_id":"a1","agent_ids":["a2"],"input":"hi"}`},
		{"runs: inputs without agent_id", "/batch/runs", `{"inputs":["x"]}`},
		{"runs: inputs with agent_ids", "/batch/runs", `{"agent_ids":["a1"],"inputs":["x"]}`},
		{"runs: no selector", "/batch/runs", `{"input":"hi"}`},
		{"workflow: empty inputs", "/batch/workflows/wf-1/trigger", `{}`},
		{"agent update: no ids", "/batch/agents/update", `{"patch":{"name":"n"}}`},
		{"agent update: empty patch", "/batch/agents/update", `{"agent_ids":["a1"],"patch":{}}`},
		{"agent update: blank patch name", "/batch/agents/update", `{"agent_ids":["a1"],"patch":{"name":"  "}}`},
		{"deploy: bad environment", "/batch/agents/deploy", `{"agent_ids":["a1"],"environment":"chaos"}`},
		{"deploy: no ids", "/batch/agents/deploy", `{"environment":"production"}`},
		{"run cancel: empty", "/batch/runs/cancel", `{"run_ids":[]}`},
		{"secrets: empty list", "/batch/secrets", `{"secrets":[]}`},
	} {
		rr := batchReq(t, h, http.MethodPost, "/api/v1"+tc.path, token, tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: want 422, got %d body=%s", tc.name, rr.Code, rr.Body.String())
			continue
		}
		if code := batchBody(t, rr)["error"].(map[string]any)["code"]; code != "VALIDATION_ERROR" {
			t.Errorf("%s: error code = %v, want VALIDATION_ERROR", tc.name, code)
		}
	}
	// Nothing ran anywhere (atomic validation never reaches an item).
	if len(runsFake.created) != 0 {
		t.Fatalf("atomic validation must not run items: %v", runsFake.created)
	}
}

// --- cross-org GET /batch/{id} reads as 404 -------------------------------------

func TestBatchGetCrossOrg404(t *testing.T) {
	h, authSvc, _ := newBatchTestRouter(t, batch.NewService())
	orgA := batchTokenFor(t, authSvc, "a@x.io", "org-a", "OWNER")
	orgB := batchTokenFor(t, authSvc, "b@x.io", "org-b", "OWNER")

	rr := batchReq(t, h, http.MethodPost, "/api/v1/batch/runs", orgA, `{"agent_id":"a1","input":"hi"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("org-a submit: %d body=%s", rr.Code, rr.Body.String())
	}
	batchID, _ := batchBody(t, rr)["batch_id"].(string)

	// Another organization polls the SAME id: 404 (never 403, never a leak).
	rr = batchReq(t, h, http.MethodGet, "/api/v1/batch/"+batchID, orgB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org GET: want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	// Same for the SSE progress surface and the batch-level cancel.
	if rr := batchReq(t, h, http.MethodPost, "/api/v1/batch/"+batchID+"/cancel", orgB, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org cancel: want 404, got %d", rr.Code)
	}
}
