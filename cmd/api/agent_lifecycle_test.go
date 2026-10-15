package main

// Issue #106 handler tests — the agent lifecycle transition endpoint and the
// run-creation state guard.
//
// Coverage (mirroring the cmd/api harness conventions of
// agents_lifecycle_test.go / apikeys_test.go):
//   - in-memory transition contract: valid move 200, illegal move 409 with
//     the typed transition message, unknown target_state 422, cross-org 404,
//     unauthenticated 401, RBAC 403 (MEMBER holds agents.read but not
//     agents.write);
//   - audit: agent.state_changed lands through the service-level auditor
//     seam (the same seam newApp wires), rejected moves audit nothing;
//   - run-creation state guard: DRAFT agents refuse POST /runs with 409
//     AGENT_NOT_ACTIVE (before any run row / queue task), the ACTIVE agent
//     accepts runs, nil wiring keeps the legacy behavior, and the refusal
//     writes the run.blocked_inactive_agent audit entry;
//   - store mode (sqlmock): the durable transition — org-scoped SELECT then
//     exactly one guarded UPDATE binding the new status.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	authpkg "agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// newAgentLifecycleTestRouter mounts the transition route the way main.go
// does (registerAgentsLifecycleRoutes on apiMux, served under /api/v1 via
// StripPrefix).
func newAgentLifecycleTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *agents.Service, *audit.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	agentsSvc := agents.NewService()
	auditSvc := audit.NewService()
	apiMux := http.NewServeMux()
	registerAgentsLifecycleRoutes(apiMux, agentsSvc, nil, authSvc, apikeys.NewService(), nil)
	// legacy catch-all registration from main.go — GET /agents/{id}
	apiMux.Handle("/agents/", authpkg.RequireAuthOrAPIKey(authSvc, apikeys.NewService())(http.HandlerFunc(agentDetailHandler(agentsSvc))))
	return http.StripPrefix("/api/v1", apiMux), authSvc, agentsSvc, auditSvc
}

// agentLifecycleTokenFor issues a bearer token for a user in the given org
// with the given role (claims carry the role; RequirePermission falls back
// to it when the user is not registered in memory).
func agentLifecycleTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
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

func doAgentLifecycleReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func agentLifecycleError(t *testing.T, rr *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	body := rr.Body.String()
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, body)
	}
	return out.Error.Code, out.Error.Message
}

// seedLifecycleAgent creates a DRAFT agent in orgID.
func seedLifecycleAgent(t *testing.T, svc *agents.Service, orgID, name string) *agents.Agent {
	t.Helper()
	agent, err := svc.CreateAgentCtx(t.Context(), orgID, name, "desc", "be helpful", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("seed agent failed: %v", err)
	}
	return agent
}

// TestTransitionEndpointContract pins the HTTP contract of POST
// /agents/{id}/transition: 200 on legal moves, 409 with the typed transition
// message on illegal ones, 422 for unknown states, 404 cross-org, 401
// unauthenticated, 403 for agents.write-less roles.
func TestTransitionEndpointContract(t *testing.T) {
	h, authSvc, agentsSvc, _ := newAgentLifecycleTestRouter(t)
	org := "org-a"
	seed := seedLifecycleAgent(t, agentsSvc, org, "Support Agent")
	owner := agentLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")
	member := agentLifecycleTokenFor(t, authSvc, "member@al.test", org, "MEMBER")
	foreign := agentLifecycleTokenFor(t, authSvc, "owner@other.test", "org-b", "OWNER")

	// Unauthenticated: 401 before anything else.
	if rr := doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", "", `{"target_state":"building"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated transition = %d, want 401", rr.Code)
	}

	// RBAC: MEMBER lacks agents.write (OWNER/ADMIN only) — 403 from the
	// permission middleware (plain-text body), before any handler logic.
	if rr := doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", member, `{"target_state":"building"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("member transition = %d, want 403", rr.Code)
	}

	// Cross-org: 404 with no existence leak.
	rr := doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", foreign, `{"target_state":"building"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org transition = %d, want 404", rr.Code)
	}
	if code, _ := agentLifecycleError(t, rr); code != "AGENT_NOT_FOUND" {
		t.Fatalf("cross-org error code = %s, want AGENT_NOT_FOUND", code)
	}

	// Unknown target_state: 422 VALIDATION_ERROR.
	rr = doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", owner, `{"target_state":"published"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown target = %d, want 422", rr.Code)
	}
	if code, message := agentLifecycleError(t, rr); code != "VALIDATION_ERROR" || !strings.Contains(message, "published") {
		t.Fatalf("unknown target envelope = (%s, %s), want VALIDATION_ERROR naming the state", code, message)
	}

	// Illegal direct move draft → active: 409 with current+target in the message.
	rr = doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", owner, `{"target_state":"active"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("illegal transition = %d, want 409", rr.Code)
	}
	if code, message := agentLifecycleError(t, rr); code != "INVALID_TRANSITION" || !strings.Contains(message, "DRAFT") || !strings.Contains(message, "ACTIVE") {
		t.Fatalf("illegal transition envelope = (%s, %s), want INVALID_TRANSITION naming DRAFT and ACTIVE", code, message)
	}

	// Case-insensitive valid move: draft → building (lowercase payload).
	rr = doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", owner, `{"target_state":"building"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid transition = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	body := decodeAgentsLifecycleBody(t, rr)
	if body["Status"] != "BUILDING" {
		t.Fatalf("transition response status = %v, want BUILDING", body["Status"])
	}

	// Second hop building → active ("ACTIVE" uppercase payload this time).
	rr = doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", owner, `{"target_state":"ACTIVE"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second transition = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}

	// The state is durable in the service: GET detail reflects it.
	rr = doAgentLifecycleReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("detail after transitions = %d, want 200", rr.Code)
	}
	if got := decodeAgentsLifecycleBody(t, rr)["Status"]; got != "ACTIVE" {
		t.Fatalf("detail status = %v, want ACTIVE", got)
	}

	// active → building is illegal: 409.
	rr = doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", owner, `{"target_state":"building"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("active → building = %d, want 409", rr.Code)
	}
}

// TestTransitionAuditsThroughServiceSeam pins the audit emission:
// agent.state_changed is recorded via the service-level auditor seam (the
// seam newApp wires with a.agentsSvc.SetAuditor), and rejected moves write
// nothing.
func TestTransitionAuditsThroughServiceSeam(t *testing.T) {
	h, authSvc, agentsSvc, auditSvc := newAgentLifecycleTestRouter(t)
	org := "org-audit"
	seed := seedLifecycleAgent(t, agentsSvc, org, "Audited Agent")
	agentsSvc.SetAuditor(auditSvc) // production wiring (mirrors newApp)
	owner := agentLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	rr := doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", owner, `{"target_state":"building"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("transition = %d, want 200", rr.Code)
	}

	entries, err := auditSvc.ListCtx(t.Context(), org)
	if err != nil {
		t.Fatalf("audit list failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 audit entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Action != "agent.state_changed" {
		t.Fatalf("audit action = %q, want agent.state_changed", entry.Action)
	}
	if entry.Metadata["from"] != "DRAFT" || entry.Metadata["to"] != "BUILDING" {
		t.Fatalf("audit metadata = %v, want from=DRAFT to=BUILDING", entry.Metadata)
	}

	// Rejected move: no additional audit entry.
	rr = doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/transition", owner, `{"target_state":"building"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("building → building = %d, want 409", rr.Code)
	}
	entries, err = auditSvc.ListCtx(t.Context(), org)
	if err != nil {
		t.Fatalf("audit list failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("rejected transitions must not audit, got %d entries", len(entries))
	}
}

// lifecycleAuditActions lists the audit actions recorded for orgID.
func lifecycleAuditActions(t *testing.T, auditSvc *audit.Service, orgID string) []string {
	t.Helper()
	entries, err := auditSvc.ListCtx(t.Context(), orgID)
	if err != nil {
		t.Fatalf("audit ListCtx failed: %v", err)
	}
	actions := make([]string, 0, len(entries))
	for _, entry := range entries {
		actions = append(actions, entry.Action)
	}
	return actions
}

// TestRunCreationRequiresActiveAgent pins the issue #106 run-creation guard:
// DRAFT agents refuse runs (409 AGENT_NOT_ACTIVE) before any run row or
// queue task exists, the ACTIVE agent accepts them, and the refusal is
// audited. agentsServiceVar is the production wiring; the nil case keeps the
// legacy behavior for minimal test wirings.
func TestRunCreationRequiresActiveAgent(t *testing.T) {
	// Isolate the package-level wiring vars this test swaps.
	oldAgents, oldRuns, oldPolicy := agentsServiceVar, runsServiceVar, policyEnforcementVar
	t.Cleanup(func() { agentsServiceVar, runsServiceVar, policyEnforcementVar = oldAgents, oldRuns, oldPolicy })

	authSvc := authpkg.NewService("test-secret")
	agentsSvc := agents.NewService()
	runsSvc := runs.NewService()
	auditSvc := audit.NewService()
	agentsServiceVar = agentsSvc
	runsServiceVar = runsSvc
	policyEnforcementVar = nil

	org := "org-runs"
	seed := seedLifecycleAgent(t, agentsSvc, org, "Runnable Agent")
	owner := agentLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")
	createRun := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
			strings.NewReader(`{"organization_id":"`+org+`","agent_id":"`+seed.ID+`","input":"hello"}`))
		req.Header.Set("Authorization", "Bearer "+owner)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		auth.RequireAuthOrAPIKey(authSvc, apikeys.NewService())(createRunHandler(queue.NewQueue(), auditSvc)).ServeHTTP(rr, req)
		return rr
	}

	// DRAFT: refused before any run row or queue task exists.
	if rr := createRun(); rr.Code != http.StatusConflict {
		t.Fatalf("draft run creation = %d, want 409 body=%s", rr.Code, rr.Body.String())
	} else if code, message := agentLifecycleError(t, rr); code != "AGENT_NOT_ACTIVE" || !strings.Contains(message, "DRAFT") {
		t.Fatalf("draft refusal envelope = (%s, %s), want AGENT_NOT_ACTIVE naming DRAFT", code, message)
	}
	if len(runsSvc.List()) != 0 {
		t.Fatal("refused run creation must not leave a run row behind")
	}
	blocked := false
	for _, action := range lifecycleAuditActions(t, auditSvc, org) {
		if action == "run.blocked_inactive_agent" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("refused run creation must write the run.blocked_inactive_agent audit entry")
	}

	// Walk draft → building → active through the service (the same move the
	// transition endpoint performs).
	for _, target := range []agents.AgentState{agents.StateBuilding, agents.StateActive} {
		if err := agentsSvc.TransitionState(t.Context(), org, seed.ID, target); err != nil {
			t.Fatalf("activate to %s failed: %v", target, err)
		}
	}
	rr := createRun()
	if rr.Code != http.StatusCreated {
		t.Fatalf("active run creation = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	if len(runsSvc.List()) != 1 {
		t.Fatalf("active agent run creation must persist a run, got %d", len(runsSvc.List()))
	}

	// paused refuses again (active → paused → refused).
	if err := agentsSvc.TransitionState(t.Context(), org, seed.ID, agents.StatePaused); err != nil {
		t.Fatalf("pause failed: %v", err)
	}
	if rr := createRun(); rr.Code != http.StatusConflict {
		t.Fatalf("paused run creation = %d, want 409", rr.Code)
	}
}

// TestRunCreationNilAgentsWiringKeepsLegacy pins the documented nil-var
// behavior: without the agents wiring the guard degrades to the legacy
// always-allow (minimal test wirings, partial rollouts).
func TestRunCreationNilAgentsWiringKeepsLegacy(t *testing.T) {
	oldAgents, oldRuns, oldPolicy := agentsServiceVar, runsServiceVar, policyEnforcementVar
	t.Cleanup(func() { agentsServiceVar, runsServiceVar, policyEnforcementVar = oldAgents, oldRuns, oldPolicy })

	authSvc := authpkg.NewService("test-secret")
	agentsSvc := agents.NewService()
	runsSvc := runs.NewService()
	agentsServiceVar = nil // guard disabled
	runsServiceVar = runsSvc
	policyEnforcementVar = nil

	org := "org-legacy"
	seed := seedLifecycleAgent(t, agentsSvc, org, "Legacy Agent")
	token := agentLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
		strings.NewReader(`{"organization_id":"`+org+`","agent_id":"`+seed.ID+`","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	auth.RequireAuthOrAPIKey(authSvc, apikeys.NewService())(createRunHandler(queue.NewQueue(), nil)).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("nil-wiring run creation = %d, want 201 (legacy behavior) body=%s", rr.Code, rr.Body.String())
	}
}

// --- store mode (sqlmock): the durable transition through the HTTP layer ----

func TestTransitionStoreModeBindsScopedUpdate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	authSvc := authpkg.NewService("test-secret")
	agentsSvc := agents.NewServiceWithStore(agents.NewPostgresStore(db))
	apiMux := http.NewServeMux()
	registerAgentsLifecycleRoutes(apiMux, agentsSvc, nil, authSvc, apikeys.NewService(), nil)
	h := http.StripPrefix("/api/v1", apiMux)

	org := "org-store"
	owner := agentLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")
	now := time.Now().UTC()

	// Org-scoped SELECT behind TransitionStateWithAgent.
	mock.ExpectQuery("FROM agents WHERE id = \\$1 AND organization_id = \\$2").
		WithArgs("agent-1", org).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "name", "description", "instructions", "model", "status", "current_version_id", "created_at", "updated_at"}).
			AddRow("agent-1", org, "Support Agent", "d", "p", "gpt-4o-mini", "DRAFT", "v1", now, now))
	// Exactly one guarded UPDATE binding the NEW status + tenant scope.
	mock.ExpectExec("UPDATE agents SET").
		WithArgs("Support Agent", "d", "p", "gpt-4o-mini", "BUILDING", "v1", sqlmock.AnyArg(), "agent-1", org).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rr := doAgentLifecycleReq(t, h, http.MethodPost, "/api/v1/agents/agent-1/transition", owner, `{"target_state":"building"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("store transition = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	if got := decodeAgentsLifecycleBody(t, rr)["Status"]; got != "BUILDING" {
		t.Fatalf("store transition response status = %v, want BUILDING", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
