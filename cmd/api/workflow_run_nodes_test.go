package main

import (
	"context"
	"net/http"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/workflows"
)

// wfNodesTestSetup registers the wave-2 workflow routes plus the track 3-c
// node-timeline route exactly like the orchestrator will register them.
func wfNodesTestSetup(t *testing.T) (*http.ServeMux, *auth.Service, *workflows.Service) {
	t.Helper()
	authSvc := auth.NewService("wf-nodes-test-secret")
	apiKeysSvc := apikeys.NewService()
	runsSvc := runs.NewService()
	q := queue.NewQueue()
	apSvc := approvals.NewService()
	wfSvc := workflows.NewService()
	wfSvc.SetEngine(workflows.Engine{Runs: runsSvc, Queue: q, Approvals: apSvc})
	apSvc.SetRunController(runsSvc)

	mux := http.NewServeMux()
	registerWorkflowsRoutes(mux, wfSvc, apSvc, runsSvc, q, authSvc, apiKeysSvc)
	registerWorkflowRunNodeRoutes(mux, wfSvc, authSvc, apiKeysSvc)
	return mux, authSvc, wfSvc
}

// wfNodesTimeline executes a two-node workflow as the given owner and drives
// the first node through a full checkpoint lifecycle (begin -> finish), the
// way a worker would.
func wfNodesTimeline(t *testing.T, mux *http.ServeMux, wfSvc *workflows.Service, orgID, token string) string {
	t.Helper()
	ctx := context.Background()

	rr := wfDo(t, mux, http.MethodPost, "/workflows/create", token, `{"name":"Durable Flow","dsl":{"nodes":[
		{"id":"n1","type":"agent","name":"First","config":{"agent_id":"agent-1"}},
		{"id":"n2","type":"agent","name":"Second","config":{"agent_id":"agent-2"}}
	],"edges":[{"from":"n1","to":"n2","condition":"on_success"}]}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workflow: got %d body=%s", rr.Code, rr.Body.String())
	}
	created := wfDecode(t, rr)
	wfID, _ := created["workflow"].(map[string]any)["id"].(string)

	rr = wfDo(t, mux, http.MethodPost, "/workflows/"+wfID+"/execute", token, `{"input":"hi"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("execute workflow: got %d body=%s", rr.Code, rr.Body.String())
	}
	result := wfDecode(t, rr)
	runID, _ := result["workflow_run_id"].(string)
	runIDs, _ := result["run_ids"].([]any)
	if runID == "" || len(runIDs) != 2 {
		t.Fatalf("unexpected execute result: %#v", result)
	}

	n1, err := wfSvc.BeginNodeRun(ctx, orgID, runID, "n1", runIDs[0].(string))
	if err != nil {
		t.Fatalf("BeginNodeRun: %v", err)
	}
	if err := wfSvc.FinishNodeRun(ctx, orgID, n1.ID, workflows.RunStatusCompleted, ""); err != nil {
		t.Fatalf("FinishNodeRun: %v", err)
	}
	return runID
}

func TestWorkflowRunNodesRequireAuth(t *testing.T) {
	mux, _, _ := wfNodesTestSetup(t)

	rr := wfDo(t, mux, http.MethodGet, "/workflow-runs/wr-1/nodes", "", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request should be 401, got %d", rr.Code)
	}
}

func TestWorkflowRunNodesTimelineShape(t *testing.T) {
	mux, authSvc, wfSvc := wfNodesTestSetup(t)
	orgID, _, token := wfRegisterOwner(t, authSvc, "owner@wf-nodes.test")
	runID := wfNodesTimeline(t, mux, wfSvc, orgID, token)

	rr := wfDo(t, mux, http.MethodGet, "/workflow-runs/"+runID+"/nodes", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET nodes: got %d body=%s", rr.Code, rr.Body.String())
	}
	payload := wfDecode(t, rr)
	rawNodes, ok := payload["nodes"].([]any)
	if !ok || len(rawNodes) != 2 {
		t.Fatalf("expected 2 nodes in the timeline, got %#v", payload)
	}

	first, _ := rawNodes[0].(map[string]any)
	if first["node_id"] != "n1" {
		t.Fatalf("timeline should be oldest-first, got %#v", rawNodes)
	}
	if _, hasID := first["id"]; !hasID {
		t.Fatalf("node view must carry the checkpoint id, got %#v", first)
	}
	if first["status"] != workflows.RunStatusCompleted {
		t.Fatalf("finished node should be completed, got %#v", first)
	}
	if attempt, ok := first["attempt"].(float64); !ok || attempt != 1 {
		t.Fatalf("checkpoint attempt should be 1, got %#v", first["attempt"])
	}
	if first["started_at"] == nil || first["finished_at"] == nil {
		t.Fatalf("finished node should carry started_at/finished_at, got %#v", first)
	}
	if errCode, exists := first["error_code"]; !exists || errCode != nil {
		t.Fatalf("empty error_code must serialize as a present null, got %#v (exists=%v)", errCode, exists)
	}

	second, _ := rawNodes[1].(map[string]any)
	if second["node_id"] != "n2" || second["status"] != workflows.RunStatusPending {
		t.Fatalf("untouched node should still be its pending placeholder, got %#v", second)
	}
	if second["finished_at"] != nil {
		t.Fatalf("pending node should have null finished_at, got %#v", second["finished_at"])
	}
}

func TestWorkflowRunNodesTenantGuardAndViewerRead(t *testing.T) {
	mux, authSvc, wfSvc := wfNodesTestSetup(t)
	orgID, _, ownerToken := wfRegisterOwner(t, authSvc, "owner2@wf-nodes.test")
	runID := wfNodesTimeline(t, mux, wfSvc, orgID, ownerToken)

	// A VIEWER token of another org cannot see the run (tenant guard -> 404,
	// indistinguishable from a missing run).
	foreignViewer := wfViewerToken(t, authSvc, "org-foreign")
	rr := wfDo(t, mux, http.MethodGet, "/workflow-runs/"+runID+"/nodes", foreignViewer, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("foreign-org viewer should get 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if code := wfDecode(t, rr)["error"].(map[string]any)["code"]; code != "WORKFLOW_RUN_NOT_FOUND" {
		t.Fatalf("expected WORKFLOW_RUN_NOT_FOUND, got %#v", code)
	}

	// A VIEWER of the owning org can read the timeline (workflows:read).
	ownViewer := wfViewerToken(t, authSvc, orgID)
	rr = wfDo(t, mux, http.MethodGet, "/workflow-runs/"+runID+"/nodes", ownViewer, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("owning-org viewer should read the timeline, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestWorkflowRunNodesUnknownRun404(t *testing.T) {
	mux, authSvc, _ := wfNodesTestSetup(t)
	_, _, token := wfRegisterOwner(t, authSvc, "owner3@wf-nodes.test")

	rr := wfDo(t, mux, http.MethodGet, "/workflow-runs/does-not-exist/nodes", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown run should be 404, got %d", rr.Code)
	}
	if code := wfDecode(t, rr)["error"].(map[string]any)["code"]; code != "WORKFLOW_RUN_NOT_FOUND" {
		t.Fatalf("expected WORKFLOW_RUN_NOT_FOUND, got %#v", code)
	}
}
