package main

// Issue #49 (wave 6-g) handler tests — agent update + delete endpoints.
//
// Coverage mirrors the apikeys harness conventions (cmd/api/apikeys_test.go):
//   - in-memory mode: full contract matrix over the mounted routes
//     (update reflected in GET, immutable-field rejection, delete guard when
//     runs exist, clean delete, cross-org 404, RBAC OWNER/ADMIN vs
//     MEMBER/VIEWER, audit rows, nil-audit variant);
//   - store mode (sqlmock): the durable path — the tenant-scoped UPDATE binds
//     the merged fields + organization_id, the runs guard runs BEFORE any
//     DELETE (a stray DELETE would fail the test), a clean delete issues
//     exactly one org-scoped DELETE, and cross-org requests perform zero
//     writes.

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
	authpkg "agentos/internal/auth"
	"agentos/internal/runs"
)

// newAgentsLifecycleTestRouter mounts the issue #49 routes plus the existing
// agent detail (GET) handler on one mux, exactly the way main.go mounts them
// (StripPrefix under /api/v1; the catch-all "/agents/" serves GET detail).
func newAgentsLifecycleTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *agents.Service, *runs.Service, *audit.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	agentsSvc := agents.NewService()
	runsSvc := runs.NewService()
	auditSvc := audit.NewService()
	apiMux := http.NewServeMux()
	registerAgentsLifecycleRoutes(apiMux, agentsSvc, runsSvc, authSvc, apikeys.NewService(), auditSvc)
	// legacy catch-all registration from main.go — GET /agents/{id}
	apiMux.Handle("/agents/", authpkg.RequireAuthOrAPIKey(authSvc, apikeys.NewService())(http.HandlerFunc(agentDetailHandler(agentsSvc))))
	return http.StripPrefix("/api/v1", apiMux), authSvc, agentsSvc, runsSvc, auditSvc
}

// agentsLifecycleTokenFor issues a bearer token for a user in the given org
// with the given role (claims carry the role; RequirePermission falls back to
// it when the user is not registered in memory).
func agentsLifecycleTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
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

func doAgentsLifecycleReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func decodeAgentsLifecycleBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

// agentsLifecycleErrorEnvelope extracts {"error":{"code","message"}}.
func agentsLifecycleErrorEnvelope(t *testing.T, rr *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	body := decodeAgentsLifecycleBody(t, rr)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected structured error envelope, got %s", rr.Body.String())
	}
	code, _ := errObj["code"].(string)
	message, _ := errObj["message"].(string)
	return code, message
}

// agentsLifecycleAuditActions returns the audit actions recorded for orgID.
func agentsLifecycleAuditActions(t *testing.T, auditSvc *audit.Service, orgID string) []string {
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

func agentsLifecycleSeedAgent(t *testing.T, svc *agents.Service, orgID, name string) *agents.Agent {
	t.Helper()
	agent, err := svc.CreateAgentCtx(t.Context(), orgID, name, "old-desc", "old-prompt", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("seed agent failed: %v", err)
	}
	return agent
}

// --- in-memory mode ---

func TestAgentsLifecycleUnauthenticated401(t *testing.T) {
	h, _, _, _, _ := newAgentsLifecycleTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/agents/agent-1"},
		{http.MethodDelete, "/api/v1/agents/agent-1"},
	} {
		rr := doAgentsLifecycleReq(t, h, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestAgentsLifecycleUpdateReflectsInGet(t *testing.T) {
	h, authSvc, agentsSvc, _, auditSvc := newAgentsLifecycleTestRouter(t)
	org := "org-a"
	seed := agentsLifecycleSeedAgent(t, agentsSvc, org, "Support Agent")
	owner := agentsLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	// Partial update: only name + description change; everything else passes through.
	rr := doAgentsLifecycleReq(t, h, http.MethodPut, "/api/v1/agents/"+seed.ID, owner,
		`{"name":"Renamed Agent","description":"new-desc"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeAgentsLifecycleBody(t, rr)
	if body["Name"] != "Renamed Agent" || body["Description"] != "new-desc" {
		t.Fatalf("update response not reflected: %s", rr.Body.String())
	}
	if body["Instructions"] != "old-prompt" || body["Model"] != "gpt-4o-mini" || body["Status"] != "DRAFT" {
		t.Fatalf("unmodified fields must pass through: %s", rr.Body.String())
	}
	if body["ID"] != seed.ID || body["OrganizationID"] != org {
		t.Fatalf("identity fields must be untouched: %s", rr.Body.String())
	}

	// GET reflects the update.
	rr = doAgentsLifecycleReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get after update: expected 200, got %d", rr.Code)
	}
	if got := decodeAgentsLifecycleBody(t, rr)["Name"]; got != "Renamed Agent" {
		t.Fatalf("GET must reflect the update, got %#v", got)
	}

	// system_prompt alias updates the instructions.
	rr = doAgentsLifecycleReq(t, h, http.MethodPut, "/api/v1/agents/"+seed.ID, owner, `{"system_prompt":"new-prompt"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("system_prompt alias: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = doAgentsLifecycleReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID, owner, "")
	if got := decodeAgentsLifecycleBody(t, rr)["Instructions"]; got != "new-prompt" {
		t.Fatalf("system_prompt alias must update instructions, got %#v", got)
	}

	// Audit: exactly two agent.updated rows for this org (one per PUT).
	actions := agentsLifecycleAuditActions(t, auditSvc, org)
	updates := 0
	for _, action := range actions {
		if action == "agent.updated" {
			updates++
		}
	}
	if updates != 2 {
		t.Fatalf("expected 2 agent.updated audit rows, got %v", actions)
	}
}

func TestAgentsLifecycleUpdateImmutableFieldsRejected(t *testing.T) {
	h, authSvc, agentsSvc, _, auditSvc := newAgentsLifecycleTestRouter(t)
	org := "org-a"
	seed := agentsLifecycleSeedAgent(t, agentsSvc, org, "Support Agent")
	owner := agentsLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	// tools + config are published-version (snapshot-domain) fields: explicit
	// 400, never silently dropped.
	rr := doAgentsLifecycleReq(t, h, http.MethodPut, "/api/v1/agents/"+seed.ID, owner,
		`{"tools":["web.search"],"config":{"temperature":0.7},"name":"Try Rename"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("immutable fields: expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	code, message := agentsLifecycleErrorEnvelope(t, rr)
	if code != "IMMUTABLE_FIELDS" {
		t.Fatalf("expected IMMUTABLE_FIELDS code, got %q (%s)", code, message)
	}
	for _, field := range []string{"config", "tools"} {
		if !strings.Contains(message, field) {
			t.Fatalf("rejection message must enumerate %q: %s", field, message)
		}
	}
	// name was part of the same rejected payload: nothing may be applied.
	rr = doAgentsLifecycleReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID, owner, "")
	if got := decodeAgentsLifecycleBody(t, rr)["Name"]; got != "Support Agent" {
		t.Fatalf("rejected payload must not partially apply, got %#v", got)
	}

	// status/version/temperature are also outside the draft update surface.
	rr = doAgentsLifecycleReq(t, h, http.MethodPut, "/api/v1/agents/"+seed.ID, owner,
		`{"status":"PUBLISHED","version":2,"temperature":0.5}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("immutable fields #2: expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	code, message = agentsLifecycleErrorEnvelope(t, rr)
	if code != "IMMUTABLE_FIELDS" {
		t.Fatalf("expected IMMUTABLE_FIELDS code, got %q (%s)", code, message)
	}
	if !strings.Contains(message, "status") || !strings.Contains(message, "temperature") || !strings.Contains(message, "version") {
		t.Fatalf("rejection message must enumerate all offending keys: %s", message)
	}

	// No agent.updated audit row for a rejected request.
	for _, action := range agentsLifecycleAuditActions(t, auditSvc, org) {
		if action == "agent.updated" {
			t.Fatal("rejected updates must not write agent.updated audit rows")
		}
	}
}

func TestAgentsLifecycleUpdateValidationErrors(t *testing.T) {
	h, authSvc, agentsSvc, _, _ := newAgentsLifecycleTestRouter(t)
	org := "org-a"
	seed := agentsLifecycleSeedAgent(t, agentsSvc, org, "Support Agent")
	owner := agentsLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	cases := []struct {
		name, body, wantCode string
		wantStatus           int
	}{
		{"blank name", `{"name":"   "}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"blank model", `{"model":"  "}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"blank instructions", `{"instructions":" "}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"no updatable fields", `{}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"non-string value", `{"name":42}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"conflicting prompt keys", `{"instructions":"a","system_prompt":"b"}`, "INVALID_REQUEST", http.StatusBadRequest},
		{"malformed json", `{"name":`, "INVALID_REQUEST", http.StatusBadRequest},
	}
	for _, tc := range cases {
		rr := doAgentsLifecycleReq(t, h, http.MethodPut, "/api/v1/agents/"+seed.ID, owner, tc.body)
		if rr.Code != tc.wantStatus {
			t.Fatalf("%s: expected %d, got %d body=%s", tc.name, tc.wantStatus, rr.Code, rr.Body.String())
		}
		if code, _ := agentsLifecycleErrorEnvelope(t, rr); code != tc.wantCode {
			t.Fatalf("%s: expected code %s, got %s", tc.name, tc.wantCode, code)
		}
	}
}

func TestAgentsLifecycleDeleteGuardWhenRunsExist(t *testing.T) {
	h, authSvc, agentsSvc, runsSvc, auditSvc := newAgentsLifecycleTestRouter(t)
	org := "org-a"
	seed := agentsLifecycleSeedAgent(t, agentsSvc, org, "Busy Agent")
	if _, err := runsSvc.CreateRunCtx(t.Context(), org, seed.ID, "hello"); err != nil {
		t.Fatalf("seed run failed: %v", err)
	}
	owner := agentsLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	rr := doAgentsLifecycleReq(t, h, http.MethodDelete, "/api/v1/agents/"+seed.ID, owner, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete with runs: expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	code, message := agentsLifecycleErrorEnvelope(t, rr)
	if code != "AGENT_HAS_RUNS" {
		t.Fatalf("expected AGENT_HAS_RUNS code, got %q (%s)", code, message)
	}
	if !strings.Contains(message, "1 run") {
		t.Fatalf("conflict message must carry the structured reason (run count): %s", message)
	}

	// The agent must still exist.
	rr = doAgentsLifecycleReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("agent must survive a guarded delete, got %d", rr.Code)
	}
	for _, action := range agentsLifecycleAuditActions(t, auditSvc, org) {
		if action == "agent.deleted" {
			t.Fatal("guarded deletes must not write agent.deleted audit rows")
		}
	}
}

func TestAgentsLifecycleCleanDelete(t *testing.T) {
	h, authSvc, agentsSvc, _, auditSvc := newAgentsLifecycleTestRouter(t)
	org := "org-a"
	seed := agentsLifecycleSeedAgent(t, agentsSvc, org, "Doomed Agent")
	owner := agentsLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	rr := doAgentsLifecycleReq(t, h, http.MethodDelete, "/api/v1/agents/"+seed.ID, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("clean delete: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeAgentsLifecycleBody(t, rr)
	if body["deleted"] != true || body["id"] != seed.ID {
		t.Fatalf("unexpected delete response: %s", rr.Body.String())
	}

	// GET now 404s and the service listing no longer contains the agent.
	rr = doAgentsLifecycleReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID, owner, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", rr.Code)
	}
	list, err := agentsSvc.ListAgentsCtx(t.Context(), org)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("agent must be gone after delete, got %d rows", len(list))
	}

	// Audit row exists, tenant-scoped, with the agent name in metadata.
	entries, err := auditSvc.ListCtx(t.Context(), org)
	if err != nil {
		t.Fatalf("audit ListCtx failed: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Action == "agent.deleted" {
			found = true
			if entry.Resource != "agents/"+seed.ID {
				t.Fatalf("audit resource mismatch: %s", entry.Resource)
			}
			if entry.Metadata["name"] != "Doomed Agent" {
				t.Fatalf("audit metadata should carry the agent name, got %#v", entry.Metadata)
			}
		}
	}
	if !found {
		t.Fatal("expected an agent.deleted audit row")
	}
}

func TestAgentsLifecycleCrossOrgIs404(t *testing.T) {
	h, authSvc, agentsSvc, _, _ := newAgentsLifecycleTestRouter(t)
	seed := agentsLifecycleSeedAgent(t, agentsSvc, "org-a", "Support Agent")
	ownerB := agentsLifecycleTokenFor(t, authSvc, "b@al.test", "org-b", "OWNER")

	rr := doAgentsLifecycleReq(t, h, http.MethodPut, "/api/v1/agents/"+seed.ID, ownerB, `{"name":"Hijack"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org update: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if code, _ := agentsLifecycleErrorEnvelope(t, rr); code != "AGENT_NOT_FOUND" {
		t.Fatalf("expected AGENT_NOT_FOUND, got %s", rr.Body.String())
	}
	rr = doAgentsLifecycleReq(t, h, http.MethodDelete, "/api/v1/agents/"+seed.ID, ownerB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org delete: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = doAgentsLifecycleReq(t, h, http.MethodDelete, "/api/v1/agents/does-not-exist", ownerB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id delete: expected 404, got %d", rr.Code)
	}

	// org-a still sees the agent, untouched.
	list, err := agentsSvc.ListAgentsCtx(t.Context(), "org-a")
	if err != nil || len(list) != 1 || list[0].Name != "Support Agent" {
		t.Fatalf("cross-org requests must not mutate the agent (list=%v err=%v)", list, err)
	}
}

func TestAgentsLifecycleRBACOwnerAdminMemberViewer(t *testing.T) {
	h, authSvc, agentsSvc, _, _ := newAgentsLifecycleTestRouter(t)
	org := "org-a"
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = agentsLifecycleTokenFor(t, authSvc, strings.ToLower(role)+"@al.test", org, role)
	}

	// update: agents.write -> OWNER/ADMIN only
	seed := agentsLifecycleSeedAgent(t, agentsSvc, org, "Support Agent")
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 200, "MEMBER": 403, "VIEWER": 403} {
		rr := doAgentsLifecycleReq(t, h, http.MethodPut, "/api/v1/agents/"+seed.ID, tokens[role], `{"description":"d"}`)
		if rr.Code != want {
			t.Fatalf("update as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
	}

	// delete: agents.write -> OWNER/ADMIN only (fresh agent per denied role so
	// the assertions are order-independent)
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 200, "MEMBER": 403, "VIEWER": 403} {
		agent := agentsLifecycleSeedAgent(t, agentsSvc, org, "Doomed "+role)
		rr := doAgentsLifecycleReq(t, h, http.MethodDelete, "/api/v1/agents/"+agent.ID, tokens[role], "")
		if rr.Code != want {
			t.Fatalf("delete as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
	}
}

func TestAgentsLifecycleNilAuditStillWorks(t *testing.T) {
	authSvc := authpkg.NewService("test-secret")
	agentsSvc := agents.NewService()
	apiMux := http.NewServeMux()
	registerAgentsLifecycleRoutes(apiMux, agentsSvc, runs.NewService(), authSvc, apikeys.NewService(), nil)
	h := http.StripPrefix("/api/v1", apiMux)
	seed := agentsLifecycleSeedAgent(t, agentsSvc, "org-a", "Support Agent")
	owner := agentsLifecycleTokenFor(t, authSvc, "owner@al.test", "org-a", "OWNER")

	if rr := doAgentsLifecycleReq(t, h, http.MethodPut, "/api/v1/agents/"+seed.ID, owner, `{"name":"X"}`); rr.Code != http.StatusOK {
		t.Fatalf("update with nil audit: expected 200, got %d", rr.Code)
	}
	if rr := doAgentsLifecycleReq(t, h, http.MethodDelete, "/api/v1/agents/"+seed.ID, owner, ""); rr.Code != http.StatusOK {
		t.Fatalf("delete with nil audit: expected 200, got %d", rr.Code)
	}
}

// --- store mode (sqlmock): the durable path mirrors the pg stores — the
// UPDATE re-checks organization_id, the runs guard precedes any DELETE, and
// cross-org requests perform zero writes.

// newAgentsLifecycleStoreTestEnv wires the lifecycle routes against
// sqlmock-backed agents + runs services (the stores are the source of truth,
// like the Postgres mode). Audit stays nil so expectations cover only the
// statements under test.
func newAgentsLifecycleStoreTestEnv(t *testing.T) (http.Handler, *authpkg.Service, *agents.Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	authSvc := authpkg.NewService("test-secret")
	agentsSvc := agents.NewServiceWithStore(agents.NewPostgresStore(db))
	runsSvc := runs.NewServiceWithStore(runs.NewPostgresStore(db))
	apiMux := http.NewServeMux()
	registerAgentsLifecycleRoutes(apiMux, agentsSvc, runsSvc, authSvc, apikeys.NewService(), nil)
	closeDB := func() { _ = db.Close() }
	return http.StripPrefix("/api/v1", apiMux), authSvc, agentsSvc, mock, closeDB
}

// agentsLifecycleSQLAgentRows lists the agent SELECT columns in scanAgent order.
func agentsLifecycleSQLAgentRows() []string {
	return []string{"id", "organization_id", "name", "description", "instructions", "model", "status", "current_version_id", "created_at", "updated_at"}
}

// agentsLifecycleSQLRunRows lists the run SELECT columns in scanRun order.
func agentsLifecycleSQLRunRows() []string {
	return []string{"id", "organization_id", "agent_id", "input", "output", "status", "cost_cents", "created_at", "updated_at"}
}

func TestAgentsLifecycleStoreModeUpdateBindsScopedUpdate(t *testing.T) {
	h, authSvc, _, mock, closeDB := newAgentsLifecycleStoreTestEnv(t)
	defer closeDB()
	org := "org-a"
	owner := agentsLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	// The scoped SELECT behind GetAgentCtx (WHERE id = $1 AND organization_id = $2).
	now := time.Now().UTC()
	mock.ExpectQuery("FROM agents WHERE id").
		WithArgs(sqlmock.AnyArg(), org).
		WillReturnRows(sqlmock.NewRows(agentsLifecycleSQLAgentRows()).
			AddRow("agent-1", org, "Support Agent", "old-desc", "old-prompt", "gpt-4o-mini", "DRAFT", "version-agent-1-1", now, now))
	// The guarded UPDATE: merged name binds, unchanged fields pass through,
	// and the organization guard binds the caller's org last.
	mock.ExpectExec("UPDATE agents SET").
		WithArgs("Renamed Agent", "new-desc", "old-prompt", "gpt-4o-mini", "DRAFT", "version-agent-1-1", sqlmock.AnyArg(), "agent-1", org).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rr := doAgentsLifecycleReq(t, h, http.MethodPut, "/api/v1/agents/agent-1", owner, `{"name":"Renamed Agent","description":"new-desc"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("store update: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeAgentsLifecycleBody(t, rr)
	if body["Name"] != "Renamed Agent" || body["Instructions"] != "old-prompt" || body["Status"] != "DRAFT" {
		t.Fatalf("store update response mismatch: %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestAgentsLifecycleStoreModeDeleteGuardRunsExist(t *testing.T) {
	h, authSvc, _, mock, closeDB := newAgentsLifecycleStoreTestEnv(t)
	defer closeDB()
	org := "org-a"
	owner := agentsLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	now := time.Now().UTC()
	// GetAgentCtx resolves the agent within the tenant first.
	mock.ExpectQuery("FROM agents WHERE id").
		WithArgs("agent-1", org).
		WillReturnRows(sqlmock.NewRows(agentsLifecycleSQLAgentRows()).
			AddRow("agent-1", org, "Busy Agent", "d", "p", "gpt-4o-mini", "DRAFT", "v1", now, now))
	// The runs guard lists the tenant's runs; one references the agent.
	mock.ExpectQuery("FROM runs WHERE organization_id").
		WithArgs(org).
		WillReturnRows(sqlmock.NewRows(agentsLifecycleSQLRunRows()).
			AddRow("run-1", org, "agent-1", "hello", "", "COMPLETED", 0.0, now, now))

	rr := doAgentsLifecycleReq(t, h, http.MethodDelete, "/api/v1/agents/agent-1", owner, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("store delete guard: expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if code, message := agentsLifecycleErrorEnvelope(t, rr); code != "AGENT_HAS_RUNS" || !strings.Contains(message, "1 run") {
		t.Fatalf("expected AGENT_HAS_RUNS with run count, got %s", rr.Body.String())
	}
	// No DELETE expectation is registered: a stray DELETE would 500 the
	// request and fail both the status assertion and ExpectationsWereMet.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations (no writes may happen while runs exist): %v", err)
	}
}

func TestAgentsLifecycleStoreModeCleanDeleteBindsScope(t *testing.T) {
	h, authSvc, _, mock, closeDB := newAgentsLifecycleStoreTestEnv(t)
	defer closeDB()
	org := "org-a"
	owner := agentsLifecycleTokenFor(t, authSvc, "owner@al.test", org, "OWNER")

	now := time.Now().UTC()
	mock.ExpectQuery("FROM agents WHERE id").
		WithArgs("agent-1", org).
		WillReturnRows(sqlmock.NewRows(agentsLifecycleSQLAgentRows()).
			AddRow("agent-1", org, "Doomed Agent", "d", "p", "gpt-4o-mini", "DRAFT", "v1", now, now))
	// The runs guard finds nothing referencing the agent.
	mock.ExpectQuery("FROM runs WHERE organization_id").
		WithArgs(org).
		WillReturnRows(sqlmock.NewRows(agentsLifecycleSQLRunRows()))
	// Exactly one org-scoped hard delete.
	mock.ExpectExec("DELETE FROM agents").
		WithArgs("agent-1", org).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rr := doAgentsLifecycleReq(t, h, http.MethodDelete, "/api/v1/agents/agent-1", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("store clean delete: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeAgentsLifecycleBody(t, rr)
	if body["deleted"] != true || body["id"] != "agent-1" {
		t.Fatalf("unexpected delete response: %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestAgentsLifecycleStoreModeCrossOrg404NoWrites pins the tenant guard on
// the durable path: the org-scoped SELECT 404s BEFORE the runs guard or the
// DELETE runs — the missing DELETE/runs expectations make any stray write
// fail the test.
func TestAgentsLifecycleStoreModeCrossOrg404NoWrites(t *testing.T) {
	h, authSvc, _, mock, closeDB := newAgentsLifecycleStoreTestEnv(t)
	defer closeDB()
	ownerB := agentsLifecycleTokenFor(t, authSvc, "b@al.test", "org-b", "OWNER")

	mock.ExpectQuery("FROM agents WHERE id").
		WithArgs("agent-1", "org-b").
		WillReturnRows(sqlmock.NewRows(agentsLifecycleSQLAgentRows()))

	rr := doAgentsLifecycleReq(t, h, http.MethodDelete, "/api/v1/agents/agent-1", ownerB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org store delete: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if code, _ := agentsLifecycleErrorEnvelope(t, rr); code != "AGENT_NOT_FOUND" {
		t.Fatalf("expected AGENT_NOT_FOUND envelope, got %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations (no writes may happen for a foreign agent): %v", err)
	}
}
