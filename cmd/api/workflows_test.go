package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/workflows"
)

// wfTestSetup builds an isolated in-memory service graph with the workflow
// routes registered exactly like the orchestrator will register them.
func wfTestSetup(t *testing.T) (*http.ServeMux, *auth.Service, *runs.Service, *workflows.Service, *approvals.Service) {
	t.Helper()
	authSvc := auth.NewService("wf-test-secret")
	apiKeysSvc := apikeys.NewService()
	runsSvc := runs.NewService()
	q := queue.NewQueue()
	apSvc := approvals.NewService()
	wfSvc := workflows.NewService()
	wfSvc.SetEngine(workflows.Engine{Runs: runsSvc, Queue: q, Approvals: apSvc})
	apSvc.SetRunController(runsSvc)

	mux := http.NewServeMux()
	registerWorkflowsRoutes(mux, wfSvc, apSvc, runsSvc, q, authSvc, apiKeysSvc)
	return mux, authSvc, runsSvc, wfSvc, apSvc
}

func wfRegisterOwner(t *testing.T, authSvc *auth.Service, email string) (orgID, userID, token string) {
	t.Helper()
	_, user, err := authSvc.Register("Acme", email, "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err = authSvc.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	return user.Organization, user.ID, token
}

// wfViewerToken mints a VIEWER-role token. The ghost email is not registered
// with the auth service, so the RBAC middleware falls back to the token role
// claims (VIEWER can read but not write/execute/decide).
func wfViewerToken(t *testing.T, authSvc *auth.Service, orgID string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&auth.User{
		ID:           "viewer-user",
		Organization: orgID,
		Email:        "ghost-viewer@acme.test",
		Role:         "VIEWER",
	})
	if err != nil {
		t.Fatalf("GenerateToken (viewer) returned error: %v", err)
	}
	return token
}

func wfDo(t *testing.T, mux http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func wfDecode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response should be valid JSON: %v body=%s", err, rr.Body.String())
	}
	return payload
}

const wfTestDSL = `{
  "nodes": [
    {"id":"n1","type":"agent","name":"Planner","config":{"agent_id":"agent-1","input":"{{input}}"}},
    {"id":"n2","type":"approval","name":"Gate","config":{"action":"deploy","reason":"needs sign-off","risk":"high"}},
    {"id":"n3","type":"agent","name":"Executor","config":{"agent_id":"agent-2"}}
  ],
  "edges": [
    {"from":"n1","to":"n2","condition":"on_success"},
    {"from":"n2","to":"n3","condition":"always"}
  ]
}`

func TestWorkflowsRequireAuthentication(t *testing.T) {
	mux, _, _, _, _ := wfTestSetup(t)

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/workflows"},
		{http.MethodPost, "/workflows/create"},
		{http.MethodGet, "/workflows/wf-1"},
		{http.MethodPost, "/workflows/wf-1/publish"},
		{http.MethodPost, "/workflows/wf-1/execute"},
		{http.MethodGet, "/workflow-runs/wr-1"},
		{http.MethodGet, "/approvals"},
		{http.MethodGet, "/approvals/ap-1"},
		{http.MethodPost, "/approvals/ap-1/decide"},
		{http.MethodPost, "/runs/run-1/cancel"},
		{http.MethodPost, "/runs/run-1/pause"},
		{http.MethodPost, "/runs/run-1/resume"},
	}
	for _, tc := range cases {
		rr := wfDo(t, mux, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without credentials: expected %d, got %d body=%s", tc.method, tc.path, http.StatusUnauthorized, rr.Code, rr.Body.String())
		}
	}
}

func TestWorkflowsViewerReadOnly(t *testing.T) {
	mux, authSvc, _, wfSvc, _ := wfTestSetup(t)
	orgID, _, ownerToken := wfRegisterOwner(t, authSvc, "viewer-owner@example.com")
	viewerToken := wfViewerToken(t, authSvc, orgID)

	// Seed one workflow with the owner so the viewer has something to read.
	rr := wfDo(t, mux, http.MethodPost, "/workflows/create", ownerToken, `{"name":"Support Flow","description":"d","dsl":`+wfTestDSL+`}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("owner create: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	workflowID := wfDecode(t, rr)["workflow"].(map[string]any)["id"].(string)

	// Reads are allowed for VIEWER (workflows.read / approvals.read).
	rr = wfDo(t, mux, http.MethodGet, "/workflows", viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer list: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	rr = wfDo(t, mux, http.MethodGet, "/workflows/"+workflowID, viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer detail: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	rr = wfDo(t, mux, http.MethodGet, "/approvals", viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer approvals list: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}

	// Writes/execute/decide/control are forbidden for VIEWER.
	forbidden := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/workflows/create", `{"name":"x"}`},
		{http.MethodPost, "/workflows/" + workflowID + "/publish", ""},
		{http.MethodPost, "/workflows/" + workflowID + "/execute", `{"input":"hi"}`},
		{http.MethodPost, "/approvals/ap-1/decide", `{"decision":"approved"}`},
		{http.MethodPost, "/runs/run-1/cancel", ""},
		{http.MethodPost, "/runs/run-1/pause", ""},
		{http.MethodPost, "/runs/run-1/resume", ""},
	}
	for _, tc := range forbidden {
		rr := wfDo(t, mux, tc.method, tc.path, viewerToken, tc.body)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("viewer %s %s: expected %d, got %d body=%s", tc.method, tc.path, http.StatusForbidden, rr.Code, rr.Body.String())
		}
	}
	_ = wfSvc
}

func TestWorkflowOwnerHappyPath(t *testing.T) {
	mux, authSvc, runsSvc, _, _ := wfTestSetup(t)
	orgID, userID, token := wfRegisterOwner(t, authSvc, "owner@example.com")

	// 1. create (draft)
	rr := wfDo(t, mux, http.MethodPost, "/workflows/create", token, `{"name":"Deploy Flow","description":"e2e","dsl":`+wfTestDSL+`}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	created := wfDecode(t, rr)["workflow"].(map[string]any)
	workflowID, _ := created["id"].(string)
	if workflowID == "" {
		t.Fatal("create response missing workflow.id")
	}
	if created["status"] != "draft" || created["current_version"] != float64(0) {
		t.Fatalf("unexpected draft state: %#v", created)
	}
	if _, ok := created["dsl"].(map[string]any); !ok {
		t.Fatalf("create response should embed the dsl, got %#v", created)
	}

	// 2. list shows the workflow without embedding the dsl
	rr = wfDo(t, mux, http.MethodGet, "/workflows", token, "")
	list := wfDecode(t, rr)["workflows"].([]any)
	var listed map[string]any
	for _, item := range list {
		if m := item.(map[string]any); m["id"] == workflowID {
			listed = m
		}
	}
	if listed == nil {
		t.Fatalf("list response missing workflow %s", workflowID)
	}
	if _, ok := listed["dsl"]; ok {
		t.Fatal("list item should be the summary shape without dsl")
	}

	// 3. validate
	rr = wfDo(t, mux, http.MethodPost, "/workflows/"+workflowID+"/validate", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("validate: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if wfDecode(t, rr)["valid"] != true {
		t.Fatalf("validate should report valid=true, got %s", rr.Body.String())
	}

	// 4. publish (immutable version 1)
	rr = wfDo(t, mux, http.MethodPost, "/workflows/"+workflowID+"/publish", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("publish: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	published := wfDecode(t, rr)
	if version := published["version"].(map[string]any); version["version"] != float64(1) || version["status"] != "published" {
		t.Fatalf("unexpected published version: %#v", version)
	}

	// 5. detail with versions
	rr = wfDo(t, mux, http.MethodGet, "/workflows/"+workflowID, token, "")
	detail := wfDecode(t, rr)["workflow"].(map[string]any)
	if detail["current_version"] != float64(1) || detail["status"] != "published" {
		t.Fatalf("unexpected published workflow state: %#v", detail)
	}
	versions := detail["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(versions))
	}
	snapshot := versions[0].(map[string]any)
	if snapshot["version"] != float64(1) || snapshot["status"] != "published" {
		t.Fatalf("unexpected version entry: %#v", snapshot)
	}
	if _, ok := snapshot["dsl_snapshot"].(map[string]any); !ok {
		t.Fatalf("version should embed dsl_snapshot, got %#v", snapshot)
	}

	// 6. execute -> DAG expansion
	rr = wfDo(t, mux, http.MethodPost, "/workflows/"+workflowID+"/execute", token, `{"input":"hello world"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("execute: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	execution := wfDecode(t, rr)
	workflowRunID, _ := execution["workflow_run_id"].(string)
	if workflowRunID == "" {
		t.Fatal("execute response missing workflow_run_id")
	}
	if execution["status"] != "pending" {
		t.Fatalf("execute status should be pending, got %#v", execution["status"])
	}
	runIDs := execution["run_ids"].([]any)
	if len(runIDs) != 2 {
		t.Fatalf("expected 2 agent runs (n1+n3), got %d: %s", len(runIDs), rr.Body.String())
	}

	// 7. workflow run detail with node_runs
	rr = wfDo(t, mux, http.MethodGet, "/workflow-runs/"+workflowRunID, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow run detail: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	runDetail := wfDecode(t, rr)
	if runDetail["workflow_id"] != workflowID {
		t.Fatalf("workflow_id mismatch: %#v", runDetail)
	}
	nodeRuns := runDetail["node_runs"].([]any)
	if len(nodeRuns) != 3 {
		t.Fatalf("expected 3 node runs, got %d", len(nodeRuns))
	}

	// 8. pending approval created by the approval node
	rr = wfDo(t, mux, http.MethodGet, "/approvals?status=pending", token, "")
	approvals := wfDecode(t, rr)["approvals"].([]any)
	if len(approvals) != 1 {
		t.Fatalf("expected 1 pending approval, got %d body=%s", len(approvals), rr.Body.String())
	}
	approval := approvals[0].(map[string]any)
	approvalID := approval["id"].(string)
	if approval["status"] != "pending" || approval["workflow_run_id"] != workflowRunID {
		t.Fatalf("unexpected approval: %#v", approval)
	}
	linkedRunID := approval["run_id"].(string)
	if linkedRunID == "" || linkedRunID != runIDs[0].(string) {
		t.Fatalf("approval should link the paused run %v, got %#v", runIDs[0], approval["run_id"])
	}

	// 9. decide (approve) -> linked paused run resumes
	rr = wfDo(t, mux, http.MethodPost, "/approvals/"+approvalID+"/decide", token, `{"decision":"approved","reason":"ship it"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("decide: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	decided := wfDecode(t, rr)["approval"].(map[string]any)
	if decided["status"] != "approved" || decided["approver"] != userID {
		t.Fatalf("unexpected decided approval: %#v (want approver %s)", decided, userID)
	}

	resumed, err := runsSvc.GetRunCtx(context.Background(), orgID, linkedRunID)
	if err != nil {
		t.Fatalf("GetRunCtx returned error: %v", err)
	}
	if resumed.Status != "pending" {
		t.Fatalf("approved approval should resume the paused run to pending, got %q", resumed.Status)
	}

	// 10. double decide -> 409
	rr = wfDo(t, mux, http.MethodPost, "/approvals/"+approvalID+"/decide", token, `{"decision":"rejected","reason":"too late"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("double decide: expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
}

func TestWorkflowCreateValidation422(t *testing.T) {
	mux, authSvc, _, _, _ := wfTestSetup(t)
	_, _, token := wfRegisterOwner(t, authSvc, "validator@example.com")

	cases := []struct {
		name string
		body string
	}{
		{"cycle", `{"name":"cyclic","dsl":{"nodes":[{"id":"a","type":"agent","config":{"agent_id":"x"}},{"id":"b","type":"agent","config":{"agent_id":"y"}}],"edges":[{"from":"a","to":"b"},{"from":"b","to":"a"}]}}`},
		{"missing edge ref", `{"name":"dangling","dsl":{"nodes":[{"id":"a","type":"agent","config":{"agent_id":"x"}}],"edges":[{"from":"a","to":"ghost"}]}}`},
		{"unknown node type", `{"name":"weird","dsl":{"nodes":[{"id":"a","type":"quantum"}],"edges":[]}}`},
		{"missing config", `{"name":"noconfig","dsl":{"nodes":[{"id":"a","type":"agent"}],"edges":[]}}`},
		{"empty graph", `{"name":"empty","dsl":{"nodes":[],"edges":[]}}`},
	}
	for _, tc := range cases {
		rr := wfDo(t, mux, http.MethodPost, "/workflows/create", token, tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: expected %d, got %d body=%s", tc.name, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		}
		payload := wfDecode(t, rr)
		errs, ok := payload["errors"].([]any)
		if !ok || len(errs) == 0 {
			t.Fatalf("%s: expected errors array, got %s", tc.name, rr.Body.String())
		}
		first := errs[0].(map[string]any)
		if first["code"] == "" || first["message"] == "" {
			t.Fatalf("%s: validation error should carry code+message, got %#v", tc.name, first)
		}
	}

	// missing name -> 422 structured error
	rr := wfDo(t, mux, http.MethodPost, "/workflows/create", token, `{"name":"","dsl":`+wfTestDSL+`}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing name: expected %d, got %d body=%s", http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	}
	if code := wfDecode(t, rr)["error"].(map[string]any)["code"]; code != "VALIDATION_ERROR" {
		t.Fatalf("expected VALIDATION_ERROR, got %#v", code)
	}

	// malformed JSON -> 400
	rr = wfDo(t, mux, http.MethodPost, "/workflows/create", token, `{not json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestWorkflowsCrossTenant404(t *testing.T) {
	mux, authSvc, runsSvc, wfSvc, apSvc := wfTestSetup(t)
	orgA, _, tokenA := wfRegisterOwner(t, authSvc, "tenant-a@example.com")

	// Seed tenant A resources directly through the services.
	wf, err := wfSvc.CreateWorkflow(context.Background(), orgA, "A Flow", "", workflows.DSL{
		Nodes: []workflows.Node{{ID: "n1", Type: workflows.NodeAgent, Config: map[string]any{"agent_id": "agent-1"}}},
	})
	if err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	run, err := runsSvc.CreateRunCtx(context.Background(), orgA, "agent-1", "hi")
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	approval, err := apSvc.Request(context.Background(), orgA, approvals.RequestInput{RunID: run.ID, Action: "deploy"})
	if err != nil {
		t.Fatalf("seed approval: %v", err)
	}

	// Tenant B cannot see or touch tenant A resources.
	_, _, tokenB := wfRegisterOwner(t, authSvc, "tenant-b@example.com")

	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/workflows/" + wf.ID, ""},
		{http.MethodPost, "/workflows/" + wf.ID + "/publish", ""},
		{http.MethodPost, "/workflows/" + wf.ID + "/execute", `{"input":"x"}`},
		{http.MethodGet, "/approvals/" + approval.ID, ""},
		{http.MethodPost, "/approvals/" + approval.ID + "/decide", `{"decision":"approved"}`},
		{http.MethodPost, "/runs/" + run.ID + "/cancel", ""},
		{http.MethodPost, "/runs/" + run.ID + "/pause", ""},
		{http.MethodPost, "/runs/" + run.ID + "/resume", ""},
	}
	for _, tc := range cases {
		rr := wfDo(t, mux, tc.method, tc.path, tokenB, tc.body)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("tenant B %s %s: expected %d, got %d body=%s", tc.method, tc.path, http.StatusNotFound, rr.Code, rr.Body.String())
		}
	}

	// Tenant A still sees its own resources.
	rr := wfDo(t, mux, http.MethodGet, "/workflows/"+wf.ID, tokenA, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("tenant A should read its workflow, got %d", rr.Code)
	}
	rr = wfDo(t, mux, http.MethodGet, "/approvals/"+approval.ID, tokenA, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("tenant A should read its approval, got %d", rr.Code)
	}
}

func TestRunControlEndpoints(t *testing.T) {
	mux, authSvc, runsSvc, _, _ := wfTestSetup(t)
	orgID, _, token := wfRegisterOwner(t, authSvc, "controller@example.com")

	run, err := runsSvc.CreateRunCtx(context.Background(), orgID, "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}

	// pause
	rr := wfDo(t, mux, http.MethodPost, "/runs/"+run.ID+"/pause", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("pause: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if wfDecode(t, rr)["Status"] != "paused" {
		t.Fatalf("expected paused status, got %s", rr.Body.String())
	}

	// resume -> pending
	rr = wfDo(t, mux, http.MethodPost, "/runs/"+run.ID+"/resume", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("resume: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if wfDecode(t, rr)["Status"] != "pending" {
		t.Fatalf("expected pending status after resume, got %s", rr.Body.String())
	}

	// cancel (idempotent)
	for i := 0; i < 2; i++ {
		rr = wfDo(t, mux, http.MethodPost, "/runs/"+run.ID+"/cancel", token, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("cancel #%d: expected %d, got %d body=%s", i+1, http.StatusOK, rr.Code, rr.Body.String())
		}
		if wfDecode(t, rr)["Status"] != "cancelled" {
			t.Fatalf("expected cancelled status, got %s", rr.Body.String())
		}
	}

	// pausing a cancelled run -> 409 INVALID_STATE
	rr = wfDo(t, mux, http.MethodPost, "/runs/"+run.ID+"/pause", token, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("pause after cancel: expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
	if code := wfDecode(t, rr)["error"].(map[string]any)["code"]; code != "INVALID_STATE" {
		t.Fatalf("expected INVALID_STATE, got %#v", code)
	}

	// unknown run -> 404
	rr = wfDo(t, mux, http.MethodPost, "/runs/does-not-exist/cancel", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown run: expected %d, got %d body=%s", http.StatusNotFound, rr.Code, rr.Body.String())
	}
}

func TestWorkflowsExecuteEngineNotWired503(t *testing.T) {
	authSvc := auth.NewService("wf-test-secret")
	apiKeysSvc := apikeys.NewService()
	runsSvc := runs.NewService()
	apSvc := approvals.NewService()
	wfSvc := workflows.NewService()
	// Register WITHOUT runs+queue services: no queue means no engine wiring.
	mux := http.NewServeMux()
	registerWorkflowsRoutes(mux, wfSvc, apSvc, runsSvc, nil, authSvc, apiKeysSvc)

	_, _, token := wfRegisterOwner(t, authSvc, "unwired@example.com")
	rr := wfDo(t, mux, http.MethodPost, "/workflows/create", token, `{"name":"Flow","dsl":`+wfTestDSL+`}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	workflowID := wfDecode(t, rr)["workflow"].(map[string]any)["id"].(string)

	rr = wfDo(t, mux, http.MethodPost, "/workflows/"+workflowID+"/execute", token, `{"input":"x"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("execute without engine: expected %d, got %d body=%s", http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	}
	if code := wfDecode(t, rr)["error"].(map[string]any)["code"]; code != "ENGINE_NOT_WIRED" {
		t.Fatalf("expected ENGINE_NOT_WIRED, got %#v", code)
	}
}
