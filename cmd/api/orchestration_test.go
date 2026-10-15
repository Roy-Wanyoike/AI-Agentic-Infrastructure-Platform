package main

// orchestration_test.go — issue #108 HTTP surface, exercised through the REAL
// middleware chain (RequireAuthOrAPIKey -> RequirePermission): the 401/403/
// 404/422 matrix, the orchestration.read / orchestration.write RBAC matrix,
// the async delegate contract (202 run ids -> ledger terminal state via the
// comm bus response message), the approval gate, the supervisor routing
// surface and its typed failure codes, org isolation over the wire, and the
// registration nil-guards. Companion of cmd/api/orchestration.go
// (residency_test.go precedent). Deterministic throughout: every
// cross-goroutine assertion synchronizes on channels (the only wall-clock
// waits are bounded safety fallbacks that can never mask a correct result).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
	"agentos/internal/orchestration"
)

// --- fakes (the seams are interfaces; the HTTP layer gets fakes, not the
// real runtime, so outcomes are scripted and deterministic) -------------------

type orchCreateCall struct {
	orgID, agentID, input string
	meta                  map[string]any
}

type orchParentStepCall struct {
	orgID, parentRunID string
	meta               map[string]any
}

type orchFakeRuns struct {
	mu          sync.Mutex
	created     []orchCreateCall
	finished    int
	parentSteps []orchParentStepCall
	nextID      int
}

func (f *orchFakeRuns) CreateSubRun(ctx context.Context, orgID, agentID, input string, meta map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := "child-" + string(rune('0'+f.nextID))
	f.created = append(f.created, orchCreateCall{orgID: orgID, agentID: agentID, input: input, meta: meta})
	return id, nil
}

func (f *orchFakeRuns) FinishSubRun(ctx context.Context, orgID, runID, status, output string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished++
	return nil
}

func (f *orchFakeRuns) RecordParentDelegationStep(ctx context.Context, orgID, parentRunID string, meta map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parentSteps = append(f.parentSteps, orchParentStepCall{orgID: orgID, parentRunID: parentRunID, meta: meta})
	return nil
}

func (f *orchFakeRuns) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

func (f *orchFakeRuns) lastCreated() orchCreateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created[len(f.created)-1]
}

type orchExecCall struct {
	req orchestration.ExecuteRequest
	ctx context.Context
}

type orchFakeExecutor struct {
	mu    sync.Mutex
	calls []orchExecCall
	fn    func(ctx context.Context, req orchestration.ExecuteRequest) (*orchestration.ExecutionResult, error)
}

func (f *orchFakeExecutor) ExecuteRun(ctx context.Context, req orchestration.ExecuteRequest) (*orchestration.ExecutionResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, orchExecCall{req: req, ctx: ctx})
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	return &orchestration.ExecutionResult{
		RunID:  req.RunID,
		Status: "COMPLETED",
		Output: "sub-result",
		Tokens: orchestration.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5},
		Steps:  1,
	}, nil
}

func (f *orchFakeExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type orchFakeApprovals struct {
	mu       sync.Mutex
	requests []orchestration.ApprovalInput
}

func (f *orchFakeApprovals) Request(ctx context.Context, orgID string, in orchestration.ApprovalInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, in)
	return "approval-1", nil
}

// --- harness -----------------------------------------------------------------

type orchTestHarness struct {
	hdl       http.Handler
	authSvc   *authpkg.Service
	auditSvc  *audit.Service
	svc       *orchestration.Service
	runs      *orchFakeRuns
	executor  *orchFakeExecutor
	approvals *orchFakeApprovals
}

func newOrchestrationTestRouter(t *testing.T, mutate func(*orchestration.Options)) *orchTestHarness {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	auditSvc := audit.NewService()
	h := &orchTestHarness{
		authSvc:   authSvc,
		auditSvc:  auditSvc,
		runs:      &orchFakeRuns{},
		executor:  &orchFakeExecutor{},
		approvals: &orchFakeApprovals{},
	}
	opts := orchestration.Options{
		Runs:      h.runs,
		Executor:  h.executor,
		Approvals: h.approvals,
		Workers:   []orchestration.WorkerAgent{{AgentID: "worker-1", Capabilities: []string{"sql"}}},
		// Mirror newOrchestrationService: capability_match is what makes
		// the HTTP required_capability field meaningful (typed 409 when
		// no worker declares it).
		Strategy: orchestration.RouteCapabilityMatch,
	}
	if mutate != nil {
		mutate(&opts)
	}
	svc, err := orchestration.NewService(opts)
	if err != nil {
		t.Fatalf("orchestration.NewService: %v", err)
	}
	h.svc = svc
	apiMux := http.NewServeMux()
	registerOrchestrationRoutes(apiMux, svc, authSvc, keysSvc, auditSvc)
	h.hdl = http.StripPrefix("/api/v1", apiMux)
	return h
}

func orchTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&authpkg.User{ID: "user-" + email, Organization: orgID, Email: email, Role: role})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

func orchReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func orchBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, rr.Body.String())
	}
	return out
}

func orchErrCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	errObj, ok := orchBody(t, rr)["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}

// awaitInboxMessage blocks until one message arrives on the given inbox
// channel (bounded safety fallback: a correct delivery always wins the race).
func awaitInboxMessage(t *testing.T, ch <-chan orchestration.AgentMessage) orchestration.AgentMessage {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the inbox message")
		return orchestration.AgentMessage{}
	}
}

// --- auth + RBAC -------------------------------------------------------------

func TestOrchestrationAuthRequired(t *testing.T) {
	h := newOrchestrationTestRouter(t, nil)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/orchestration/delegate", `{"from_agent_id":"a","to_agent_id":"b","task":"t"}`},
		{http.MethodPost, "/api/v1/orchestration/messages", `{"from_agent_id":"a","to_agent_id":"b","message_type":"request"}`},
		{http.MethodGet, "/api/v1/orchestration/agents/agent-a/messages", ""},
		{http.MethodPost, "/api/v1/orchestration/supervisor/run", `{"task":"t"}`},
		{http.MethodGet, "/api/v1/orchestration/delegations", ""},
	} {
		if rr := orchReq(t, h.hdl, tc.method, tc.path, "", tc.body); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d", tc.method, tc.path, rr.Code)
		}
	}
}

func TestOrchestrationRBACMatrix(t *testing.T) {
	h := newOrchestrationTestRouter(t, nil)
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = orchTokenFor(t, h.authSvc, strings.ToLower(role)+"@x.io", "org-a", role)
	}

	// Writes (orchestration.write): OWNER/ADMIN only — delegations create
	// runs and consume model tokens.
	for _, role := range []string{"MEMBER", "VIEWER"} {
		if rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/delegate", tokens[role],
			`{"from_agent_id":"agent-a","to_agent_id":"agent-b","task":"t"}`); rr.Code != http.StatusForbidden {
			t.Errorf("delegate as %s: want 403, got %d", role, rr.Code)
		}
		if rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/messages", tokens[role],
			`{"from_agent_id":"agent-a","to_agent_id":"agent-b","message_type":"request"}`); rr.Code != http.StatusForbidden {
			t.Errorf("messages as %s: want 403, got %d", role, rr.Code)
		}
		if rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/supervisor/run", tokens[role],
			`{"task":"t"}`); rr.Code != http.StatusForbidden {
			t.Errorf("supervisor run as %s: want 403, got %d", role, rr.Code)
		}
	}
	for _, role := range []string{"OWNER", "ADMIN"} {
		if rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/delegate", tokens[role],
			`{"from_agent_id":"agent-a","to_agent_id":"agent-b","task":"t"}`); rr.Code != http.StatusAccepted {
			t.Errorf("delegate as %s: want 202, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}

	// Reads (orchestration.read): every role including VIEWER. The agent-a
	// inbox is pre-registered so the read is a clean 200.
	if _, err := h.svc.Bus.Receive(context.Background(), "org-a", "agent-a"); err != nil {
		t.Fatalf("inbox registration: %v", err)
	}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		if rr := orchReq(t, h.hdl, http.MethodGet, "/api/v1/orchestration/agents/agent-a/messages", tokens[role], ""); rr.Code != http.StatusOK {
			t.Errorf("messages as %s: want 200, got %d", role, rr.Code)
		}
		if rr := orchReq(t, h.hdl, http.MethodGet, "/api/v1/orchestration/delegations", tokens[role], ""); rr.Code != http.StatusOK {
			t.Errorf("delegations as %s: want 200, got %d", role, rr.Code)
		}
	}
}

// --- delegate ----------------------------------------------------------------

func TestOrchestrationDelegateHappyPathOverHTTP(t *testing.T) {
	h := newOrchestrationTestRouter(t, nil)
	token := orchTokenFor(t, h.authSvc, "owner@x.io", "org-a", "OWNER")

	rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/delegate", token,
		`{"from_agent_id":"agent-a","to_agent_id":"agent-b","task":"summarize the ledger","parent_run_id":"run-parent"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("delegate: want 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := orchBody(t, rr)
	delegationID, _ := body["delegation_id"].(string)
	childRunID, _ := body["child_run_id"].(string)
	if delegationID == "" || childRunID == "" {
		t.Fatalf("async run ids missing: %v", body)
	}
	if body["status"] != orchestration.DelegationStatusPending {
		t.Fatalf("status = %v, want pending", body["status"])
	}

	// The child run was created through the runs seam with full linkage.
	if h.runs.createdCount() != 1 {
		t.Fatalf("child runs created = %d, want 1", h.runs.createdCount())
	}
	created := h.runs.lastCreated()
	if created.orgID != "org-a" || created.agentID != "agent-b" || created.input != "summarize the ledger" {
		t.Fatalf("child run fields wrong: %+v", created)
	}
	if created.meta["parent_run_id"] != "run-parent" || created.meta["from_agent_id"] != "agent-a" {
		t.Fatalf("child run linkage wrong: %+v", created.meta)
	}

	// The outcome is delivered to the delegator's inbox as a response
	// message (channel-synchronized, not polled).
	ch, err := h.svc.Bus.Receive(context.Background(), "org-a", "agent-a")
	if err != nil {
		t.Fatalf("delegator inbox: %v", err)
	}
	msg := awaitInboxMessage(t, ch)
	if msg.MessageType != orchestration.MessageTypeResponse || msg.FromAgentID != "agent-b" || msg.ToAgentID != "agent-a" {
		t.Fatalf("response routing wrong: %+v", msg)
	}
	if msg.RunID != childRunID {
		t.Fatalf("response run_id = %q, want the child run id %q", msg.RunID, childRunID)
	}
	var respPayload map[string]any
	if err := json.Unmarshal(msg.Payload, &respPayload); err != nil {
		t.Fatalf("response payload invalid: %v", err)
	}
	if respPayload["status"] != orchestration.DelegationStatusCompleted || respPayload["output"] != "sub-result" {
		t.Fatalf("response payload wrong: %v", respPayload)
	}

	// The executor ran the pre-created child run with the parent context.
	if h.executor.callCount() != 1 {
		t.Fatalf("executor calls = %d, want 1", h.executor.callCount())
	}
	call := h.executor.calls[0]
	if call.req.RunID != childRunID || call.req.AgentID != "agent-b" || call.req.ParentRunID != "run-parent" || call.req.Depth != 1 {
		t.Fatalf("execute request wrong: %+v", call.req)
	}
	if _, ok := call.ctx.Deadline(); !ok {
		t.Fatal("execution context must carry the delegation deadline")
	}

	// The ledger is the source of truth for the terminal state (run_id
	// filter matches the child run).
	rr = orchReq(t, h.hdl, http.MethodGet, "/api/v1/orchestration/delegations?run_id="+childRunID, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delegations: want 200, got %d", rr.Code)
	}
	list := orchBody(t, rr)
	if recs, ok := list["delegations"].([]any); !ok || len(recs) != 1 {
		t.Fatalf("delegations = %v, want exactly the child record", list["delegations"])
	} else {
		rec := recs[0].(map[string]any)
		if rec["id"] != delegationID || rec["status"] != orchestration.DelegationStatusCompleted ||
			rec["result"] != "sub-result" || rec["child_run_id"] != childRunID || rec["depth"] != float64(1) {
			t.Fatalf("ledger record wrong: %v", rec)
		}
		if toks, ok := rec["tokens"].(map[string]any); !ok || toks["total_tokens"] != float64(5) {
			t.Fatalf("token attribution wrong: %v", rec["tokens"])
		}
	}

	// One audited delegation row.
	entries, err := h.auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Action]++
	}
	if counts["orchestration.delegate"] != 1 {
		t.Fatalf("audit counts wrong: %v", counts)
	}
}

func TestOrchestrationDelegateValidationAndApprovalGate(t *testing.T) {
	h := newOrchestrationTestRouter(t, nil)
	token := orchTokenFor(t, h.authSvc, "admin@x.io", "org-a", "ADMIN")
	const path = "/api/v1/orchestration/delegate"

	// 422 matrix: missing fields, self-delegation, depth limit.
	for name, body := range map[string]string{
		"missing to":      `{"from_agent_id":"agent-a","task":"t"}`,
		"missing from":    `{"to_agent_id":"agent-b","task":"t"}`,
		"missing task":    `{"from_agent_id":"agent-a","to_agent_id":"agent-b"}`,
		"self delegation": `{"from_agent_id":"agent-a","to_agent_id":"agent-a","task":"t"}`,
		"depth exceeded":  `{"from_agent_id":"agent-a","to_agent_id":"agent-b","task":"t","depth":7}`,
		"negative depth":  `{"from_agent_id":"agent-a","to_agent_id":"agent-b","task":"t","depth":-1}`,
	} {
		if rr := orchReq(t, h.hdl, http.MethodPost, path, token, body); rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: want 422, got %d body=%s", name, rr.Code, rr.Body.String())
		} else if code := orchErrCode(t, rr); code != "VALIDATION_ERROR" {
			t.Errorf("%s: error code = %q, want VALIDATION_ERROR", name, code)
		}
	}
	if h.executor.callCount() != 0 || h.runs.createdCount() != 0 {
		t.Fatal("rejected requests must have no side effects")
	}

	// Approval gate: parked, filed through the approvals seam, never executed.
	rr := orchReq(t, h.hdl, http.MethodPost, path, token,
		`{"from_agent_id":"agent-a","to_agent_id":"agent-b","task":"deploy the canary","require_approval":true}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("approval-gated delegate: want 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := orchBody(t, rr)
	if body["status"] != orchestration.DelegationStatusAwaitingApproval {
		t.Fatalf("status = %v, want awaiting_approval", body["status"])
	}
	if body["approval_id"] != "approval-1" || body["child_run_id"] == "" {
		t.Fatalf("approval response wrong: %v", body)
	}
	if h.executor.callCount() != 0 {
		t.Fatal("approval-gated delegation must not execute")
	}
	if len(h.approvals.requests) != 1 || h.approvals.requests[0].Action != orchestration.ActionOrchestrationDelegate {
		t.Fatalf("approval requests wrong: %+v", h.approvals.requests)
	}
	if delegationID, _ := body["delegation_id"].(string); delegationID == "" ||
		!strings.Contains(h.approvals.requests[0].Resource, delegationID) {
		t.Fatalf("approval resource must reference the delegation: %q vs %q", h.approvals.requests[0].Resource, delegationID)
	}
}

func TestOrchestrationDelegateApprovalUnavailable(t *testing.T) {
	h := newOrchestrationTestRouter(t, func(o *orchestration.Options) { o.Approvals = nil })
	token := orchTokenFor(t, h.authSvc, "owner@x.io", "org-a", "OWNER")
	rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/delegate", token,
		`{"from_agent_id":"agent-a","to_agent_id":"agent-b","task":"t","require_approval":true}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409 (fail-closed), got %d body=%s", rr.Code, rr.Body.String())
	}
	if code := orchErrCode(t, rr); code != "APPROVAL_UNAVAILABLE" {
		t.Fatalf("error code = %q, want APPROVAL_UNAVAILABLE", code)
	}
	if body := orchBody(t, rr); body["delegation_id"] == "" {
		t.Fatalf("the failed record must stay traceable: %v", body)
	}
	if h.runs.createdCount() != 0 || h.executor.callCount() != 0 {
		t.Fatal("fail-closed gate must have no side effects")
	}
}

// --- messages ----------------------------------------------------------------

func TestOrchestrationMessagesLifecycleOverHTTP(t *testing.T) {
	// Capacity 1 so the second direct send exercises the typed 409.
	h := newOrchestrationTestRouter(t, func(o *orchestration.Options) { o.BusChannelCapacity = 1 })
	token := orchTokenFor(t, h.authSvc, "owner@x.io", "org-a", "OWNER")
	if _, err := h.svc.Bus.Receive(context.Background(), "org-a", "agent-a"); err != nil {
		t.Fatalf("inbox registration: %v", err)
	}

	// 422 validation matrix.
	for name, body := range map[string]string{
		"missing from":  `{"to_agent_id":"agent-a","message_type":"request"}`,
		"missing to":    `{"from_agent_id":"agent-x","message_type":"request"}`,
		"unknown type":  `{"from_agent_id":"agent-x","to_agent_id":"agent-a","message_type":"gossip"}`,
		"bad JSON body": `{`,
	} {
		want := http.StatusUnprocessableEntity
		if name == "bad JSON body" {
			want = http.StatusBadRequest
		}
		if rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/messages", token, body); rr.Code != want {
			t.Errorf("%s: want %d, got %d body=%s", name, want, rr.Code, rr.Body.String())
		}
	}
	// 404: unknown target inbox (org-scoped).
	if rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/messages", token,
		`{"from_agent_id":"agent-x","to_agent_id":"agent-ghost","message_type":"request"}`); rr.Code != http.StatusNotFound {
		t.Errorf("unknown target: want 404, got %d", rr.Code)
	}

	// First delivery fills the capacity-1 inbox.
	rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/messages", token,
		`{"from_agent_id":"agent-x","to_agent_id":"agent-a","message_type":"request","payload":{"k":"v"},"run_id":"run-9"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("first send: want 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	// Second is a typed saturation -> 409 INBOX_FULL.
	if rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/messages", token,
		`{"from_agent_id":"agent-x","to_agent_id":"agent-a","message_type":"request"}`); rr.Code != http.StatusConflict {
		t.Fatalf("saturated inbox: want 409, got %d body=%s", rr.Code, rr.Body.String())
	} else if code := orchErrCode(t, rr); code != "INBOX_FULL" {
		t.Fatalf("error code = %q, want INBOX_FULL", code)
	}

	// The retained inbox is readable over HTTP (newest first, limit applied).
	rr = orchReq(t, h.hdl, http.MethodGet, "/api/v1/orchestration/agents/agent-a/messages?limit=1", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET messages: want 200, got %d", rr.Code)
	}
	list := orchBody(t, rr)
	if list["count"] != float64(1) {
		t.Fatalf("count = %v, want 1", list["count"])
	}
	msgs, _ := list["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	msg := msgs[0].(map[string]any)
	if msg["message_type"] != "request" || msg["from_agent_id"] != "agent-x" || msg["run_id"] != "run-9" {
		t.Fatalf("message fields wrong: %v", msg)
	}
	payload, ok := msg["payload"].(map[string]any)
	if !ok || payload["k"] != "v" {
		t.Fatalf("payload must round-trip as raw JSON: %v", msg["payload"])
	}

	// Malformed limit is a 422; unknown inbox is a 404.
	if rr := orchReq(t, h.hdl, http.MethodGet, "/api/v1/orchestration/agents/agent-a/messages?limit=abc", token, ""); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad limit: want 422, got %d", rr.Code)
	}
	if rr := orchReq(t, h.hdl, http.MethodGet, "/api/v1/orchestration/agents/agent-ghost/messages", token, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown inbox: want 404, got %d", rr.Code)
	}
}

// --- supervisor ----------------------------------------------------------------

func TestOrchestrationSupervisorRunOverHTTP(t *testing.T) {
	h := newOrchestrationTestRouter(t, nil)
	token := orchTokenFor(t, h.authSvc, "owner@x.io", "org-a", "OWNER")
	const path = "/api/v1/orchestration/supervisor/run"

	// Happy path: routed to worker-1, delegated, aggregated.
	rr := orchReq(t, h.hdl, http.MethodPost, path, token,
		`{"task":"crunch it","task_ref":"ref-9","from_agent_id":"agent-a"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("supervisor run: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := orchBody(t, rr)
	if body["worker_agent_id"] != "worker-1" || body["status"] != orchestration.DelegationStatusCompleted || body["output"] != "sub-result" {
		t.Fatalf("supervisor result wrong: %v", body)
	}
	childRunID, _ := body["child_run_id"].(string)
	if childRunID == "" || body["delegation_id"] == "" {
		t.Fatalf("run linkage missing: %v", body)
	}
	if toks, ok := body["tokens"].(map[string]any); !ok || toks["total_tokens"] != float64(5) {
		t.Fatalf("tokens wrong: %v", body["tokens"])
	}
	if attempts, ok := body["attempts"].([]any); !ok || len(attempts) != 0 {
		t.Fatalf("attempts = %v, want empty on first-try success", body["attempts"])
	}

	// The supervised delegation is visible in the ledger via the child run.
	rr = orchReq(t, h.hdl, http.MethodGet, "/api/v1/orchestration/delegations?run_id="+childRunID, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delegations: want 200, got %d", rr.Code)
	}
	if recs, _ := orchBody(t, rr)["delegations"].([]any); len(recs) != 1 {
		t.Fatalf("ledger must hold the supervised delegation: %v", orchBody(t, rr)["delegations"])
	}

	// Capability routing: sql matches worker-1; art is a typed 409.
	if rr := orchReq(t, h.hdl, http.MethodPost, path, token,
		`{"task":"migrate","required_capability":"sql"}`); rr.Code != http.StatusOK {
		t.Fatalf("capability match: want 200, got %d", rr.Code)
	}
	if rr := orchReq(t, h.hdl, http.MethodPost, path, token,
		`{"task":"paint","required_capability":"art"}`); rr.Code != http.StatusConflict {
		t.Fatalf("no capable worker: want 409, got %d body=%s", rr.Code, rr.Body.String())
	} else if code := orchErrCode(t, rr); code != "NO_CAPABLE_WORKER" {
		t.Fatalf("error code = %q, want NO_CAPABLE_WORKER", code)
	}

	// Missing task text is a 422.
	if rr := orchReq(t, h.hdl, http.MethodPost, path, token, `{"task":"  "}`); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing task: want 422, got %d", rr.Code)
	}

	// One audited supervisor row.
	entries, err := h.auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Action]++
	}
	if counts["orchestration.supervisor_run"] != 2 { // happy path + capability match
		t.Fatalf("audit counts wrong: %v", counts)
	}
}

func TestOrchestrationSupervisorFailureCodes(t *testing.T) {
	// All workers failed -> 502 with the attempt trace.
	h := newOrchestrationTestRouter(t, func(o *orchestration.Options) {
		o.Executor = failingOrchExecutor("model provider unreachable")
	})
	token := orchTokenFor(t, h.authSvc, "owner@x.io", "org-a", "OWNER")
	rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/supervisor/run", token,
		`{"task":"t","task_ref":"ref-1"}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("all workers failed: want 502, got %d body=%s", rr.Code, rr.Body.String())
	}
	if code := orchErrCode(t, rr); code != "ALL_WORKERS_FAILED" {
		t.Fatalf("error code = %q, want ALL_WORKERS_FAILED", code)
	}
	attempts, _ := orchBody(t, rr)["attempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %v, want the single worker trace", attempts)
	}
	if attempt := attempts[0].(map[string]any); attempt["worker_agent_id"] != "worker-1" ||
		!strings.Contains(attempt["error"].(string), "model provider unreachable") {
		t.Fatalf("attempt trace wrong: %v", attempt)
	}

	// No workers configured -> typed 409.
	h2 := newOrchestrationTestRouter(t, func(o *orchestration.Options) { o.Workers = nil })
	token2 := orchTokenFor(t, h2.authSvc, "owner@x.io", "org-a", "OWNER")
	if rr := orchReq(t, h2.hdl, http.MethodPost, "/api/v1/orchestration/supervisor/run", token2, `{"task":"t"}`); rr.Code != http.StatusConflict {
		t.Fatalf("no workers: want 409, got %d body=%s", rr.Code, rr.Body.String())
	} else if code := orchErrCode(t, rr); code != "NO_WORKERS" {
		t.Fatalf("error code = %q, want NO_WORKERS", code)
	}
}

// failingOrchExecutor builds an executor fake whose every call fails.
func failingOrchExecutor(cause string) *orchFakeExecutor {
	return &orchFakeExecutor{fn: func(ctx context.Context, req orchestration.ExecuteRequest) (*orchestration.ExecutionResult, error) {
		return nil, errors.New(cause)
	}}
}

// --- tenancy + wiring --------------------------------------------------------

func TestOrchestrationOrgIsolationOverHTTP(t *testing.T) {
	h := newOrchestrationTestRouter(t, nil)
	orgA := orchTokenFor(t, h.authSvc, "a@x.io", "org-a", "OWNER")
	orgB := orchTokenFor(t, h.authSvc, "b@x.io", "org-b", "OWNER")

	// org-a delegates and holds an inbox for agent-a.
	if rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/delegate", orgA,
		`{"from_agent_id":"agent-a","to_agent_id":"agent-b","task":"t"}`); rr.Code != http.StatusAccepted {
		t.Fatalf("org-a delegate: %d body=%s", rr.Code, rr.Body.String())
	}
	msg := awaitInboxMessage(t, mustOrchInbox(t, h, "org-a", "agent-a"))
	if msg.MessageType != orchestration.MessageTypeResponse {
		t.Fatalf("org-a inbox message wrong: %+v", msg)
	}

	// org-b sees none of it: empty ledger, unknown inbox, unknown send target.
	if rr := orchReq(t, h.hdl, http.MethodGet, "/api/v1/orchestration/delegations", orgB, ""); rr.Code != http.StatusOK {
		t.Fatalf("org-b delegations: %d", rr.Code)
	} else if body := orchBody(t, rr); body["count"] != float64(0) {
		t.Fatalf("org-a ledger leaked into org-b: %v", body)
	}
	if rr := orchReq(t, h.hdl, http.MethodGet, "/api/v1/orchestration/agents/agent-a/messages", orgB, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("org-b read of org-a inbox: want 404, got %d", rr.Code)
	}
	if rr := orchReq(t, h.hdl, http.MethodPost, "/api/v1/orchestration/messages", orgB,
		`{"from_agent_id":"agent-z","to_agent_id":"agent-a","message_type":"request"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("org-b cross-tenant send: want 404, got %d", rr.Code)
	}
}

func mustOrchInbox(t *testing.T, h *orchTestHarness, org, agent string) <-chan orchestration.AgentMessage {
	t.Helper()
	ch, err := h.svc.Bus.Receive(context.Background(), org, agent)
	if err != nil {
		t.Fatalf("Receive(%s, %s): %v", org, agent, err)
	}
	return ch
}

func TestOrchestrationDualMountAndNilGuards(t *testing.T) {
	// The surface is registered on the shared apiMux and is therefore served
	// under BOTH versioned prefixes (/v1 and /api/v1).
	h := newOrchestrationTestRouter(t, nil)
	token := orchTokenFor(t, h.authSvc, "o@x.io", "org-a", "OWNER")
	legacyMux := http.NewServeMux()
	registerOrchestrationRoutes(legacyMux, h.svc, h.authSvc, apikeys.NewService(), audit.NewService())
	if rr := orchReq(t, http.StripPrefix("/v1", legacyMux), http.MethodGet, "/v1/orchestration/delegations", token, ""); rr.Code != http.StatusOK {
		t.Fatalf("/v1 mount: want 200, got %d", rr.Code)
	}

	// Nil guards: registering against a nil mux or nil service must not panic.
	registerOrchestrationRoutes(nil, h.svc, h.authSvc, apikeys.NewService(), audit.NewService())
	registerOrchestrationRoutes(http.NewServeMux(), nil, h.authSvc, apikeys.NewService(), audit.NewService())

	// The main.go construction seam builds a fully wired service (offline
	// deterministic mode in tests — no OPENAI_API_KEY).
	svc := newOrchestrationService(nil, nil, approvals.NewService(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if svc == nil || svc.Bus == nil || svc.Delegation == nil || svc.Supervisor == nil {
		t.Fatalf("newOrchestrationService returned an incomplete service: %+v", svc)
	}
}
