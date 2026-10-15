package main

// Issue #122 handler tests — agent export / import / clone, exercised through
// the REAL middleware chain (RequireAuthOrAPIKey -> RequirePermission),
// mirroring the cmd/api harness conventions of residency_test.go /
// agent_lifecycle_test.go.
//
// Coverage:
//   - 401 without credentials on all three routes;
//   - RBAC matrix: export is agents.read (every role incl. VIEWER);
//     import/clone are agents.write (exactly OWNER/ADMIN — MEMBER/VIEWER 403);
//   - export contract: 200 configuration-only bundle (schema_version "1",
//     NO org id / agent id / status / credentials in the body), 404
//     cross-org/unknown with no existence leak, audit agent.exported;
//   - import contract: 201 fresh identity in the CALLER's tenant (payload
//     ids/org/status/created_at can never be smuggled in), 422 VALIDATION_ERROR
//     typed rejections (unknown model / unknown tool / schema version /
//     oversized instructions), 400 malformed JSON, audit agent.imported;
//   - clone contract: 201 same-org deep copy (name "-copy" suffix, status
//     reset DRAFT, source untouched), 404 cross-org/unknown, audit
//     agent.cloned.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
)

// newAgentPortabilityTestRouter mounts the portability routes the way
// main.go does (registerAgentPortabilityRoutes on apiMux, served under
// /api/v1 via StripPrefix) plus the legacy GET detail catch-all.
func newAgentPortabilityTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *agents.Service, *audit.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	agentsSvc := agents.NewService()
	auditSvc := audit.NewService()
	apiMux := http.NewServeMux()
	registerAgentPortabilityRoutes(apiMux, agentsSvc, authSvc, apikeys.NewService(), auditSvc)
	// legacy catch-all registration from main.go — GET /agents/{id}
	apiMux.Handle("/agents/", authpkg.RequireAuthOrAPIKey(authSvc, apikeys.NewService())(http.HandlerFunc(agentDetailHandler(agentsSvc))))
	return http.StripPrefix("/api/v1", apiMux), authSvc, agentsSvc, auditSvc
}

// portabilityTokenFor issues a bearer token for a user in the given org with
// the given role.
func portabilityTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
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

func doPortabilityReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

// seedPortabilityAgent creates a DRAFT agent in orgID.
func seedPortabilityAgent(t *testing.T, svc *agents.Service, orgID, name string) *agents.Agent {
	t.Helper()
	agent, err := svc.CreateAgentCtx(t.Context(), orgID, name, "front-line helper", "be helpful", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("seed agent failed: %v", err)
	}
	return agent
}

func portabilityAuditActions(t *testing.T, auditSvc *audit.Service, orgID string) []audit.Entry {
	t.Helper()
	entries, err := auditSvc.ListCtx(t.Context(), orgID)
	if err != nil {
		t.Fatalf("audit ListCtx failed: %v", err)
	}
	out := make([]audit.Entry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, *entry)
	}
	return out
}

// portabilityErrorEnvelope decodes the {"error":{"code","message"}} shape.
func portabilityErrorEnvelope(t *testing.T, rr *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(rr.Body.String()), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, rr.Body.String())
	}
	return out.Error.Code, out.Error.Message
}

// validPortabilityBundle returns an importable bundle payload for orgs.
func validPortabilityBundle(name string) string {
	return `{"schema_version":"1","exported_at":"2026-01-01T00:00:00Z","agent":{"name":"` + name +
		`","description":"d","instructions":"be helpful","model":"gpt-4o-mini","tools":[]}}`
}

func TestAgentPortabilityAuthRequired(t *testing.T) {
	h, _, agentsSvc, _ := newAgentPortabilityTestRouter(t)
	seed := seedPortabilityAgent(t, agentsSvc, "org-a", "Support Agent")
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/agents/" + seed.ID + "/export"},
		{http.MethodPost, "/api/v1/agents/import"},
		{http.MethodPost, "/api/v1/agents/" + seed.ID + "/clone"},
	} {
		if rr := doPortabilityReq(t, h, tc.method, tc.path, "", validPortabilityBundle("X")); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d", tc.method, tc.path, rr.Code)
		}
	}
}

func TestAgentPortabilityRBACMatrix(t *testing.T) {
	h, authSvc, agentsSvc, _ := newAgentPortabilityTestRouter(t)
	seed := seedPortabilityAgent(t, agentsSvc, "org-rbac", "Support Agent")
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = portabilityTokenFor(t, authSvc, strings.ToLower(role)+"@x.io", "org-rbac", role)
	}
	// Export is agents.read: every role (VIEWER included) may read the bundle.
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		if rr := doPortabilityReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID+"/export", tokens[role], ""); rr.Code != http.StatusOK {
			t.Errorf("export as %s: want 200, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}
	// Import/clone are agents.write: exactly OWNER/ADMIN.
	for _, role := range []string{"MEMBER", "VIEWER"} {
		if rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/import", tokens[role], validPortabilityBundle("Imported")); rr.Code != http.StatusForbidden {
			t.Errorf("import as %s: want 403, got %d", role, rr.Code)
		}
		if rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/clone", tokens[role], ""); rr.Code != http.StatusForbidden {
			t.Errorf("clone as %s: want 403, got %d", role, rr.Code)
		}
	}
	agents, _ := agentsSvc.ListAgentsCtx(t.Context(), "org-rbac")
	if len(agents) != 1 {
		t.Fatalf("forbidden writes must leave no agents behind, got %d", len(agents))
	}
	for _, role := range []string{"OWNER", "ADMIN"} {
		if rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/import", tokens[role], validPortabilityBundle("Imported "+role)); rr.Code != http.StatusCreated {
			t.Errorf("import as %s: want 201, got %d body=%s", role, rr.Code, rr.Body.String())
		}
		if rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/clone", tokens[role], ""); rr.Code != http.StatusCreated {
			t.Errorf("clone as %s: want 201, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}
}

func TestAgentExportEndpointContract(t *testing.T) {
	h, authSvc, agentsSvc, auditSvc := newAgentPortabilityTestRouter(t)
	org := "org-export"
	seed := seedPortabilityAgent(t, agentsSvc, org, "Support Agent")
	owner := portabilityTokenFor(t, authSvc, "owner@x.io", org, "OWNER")
	foreign := portabilityTokenFor(t, authSvc, "owner@other.io", "org-foreign", "OWNER")

	rr := doPortabilityReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID+"/export", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("export = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	var bundle agents.ExportBundle
	if err := json.Unmarshal([]byte(body), &bundle); err != nil {
		t.Fatalf("export body is not a bundle: %v body=%s", err, body)
	}
	if bundle.SchemaVersion != "1" || bundle.Agent.Name != "Support Agent" || bundle.Agent.Model != "gpt-4o-mini" {
		t.Fatalf("bundle mismatch: %+v", bundle)
	}
	// Configuration-only: no identity, no lifecycle state, no credentials.
	for _, forbidden := range []string{seed.ID, org, `"status"`, `"organization`, `"created_at"`, seed.Status, `"id"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("export body leaked %q: %s", forbidden, body)
		}
	}

	// Cross-org / unknown: 404 with no existence leak.
	if rr := doPortabilityReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID+"/export", foreign, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org export = %d, want 404", rr.Code)
	} else if code, _ := portabilityErrorEnvelope(t, rr); code != "AGENT_NOT_FOUND" {
		t.Fatalf("cross-org export code = %s, want AGENT_NOT_FOUND", code)
	}
	if rr := doPortabilityReq(t, h, http.MethodGet, "/api/v1/agents/does-not-exist/export", owner, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown export = %d, want 404", rr.Code)
	}

	// Audit: exactly one agent.exported (the two 404s audit nothing).
	actions := portabilityAuditActions(t, auditSvc, org)
	if len(actions) != 1 || actions[0].Action != "agent.exported" {
		t.Fatalf("audit actions = %v, want exactly one agent.exported", actions)
	}
	if actions[0].Resource != "agents/"+seed.ID {
		t.Fatalf("audit resource = %q, want agents/%s", actions[0].Resource, seed.ID)
	}
}

func TestAgentImportEndpointContract(t *testing.T) {
	h, authSvc, agentsSvc, auditSvc := newAgentPortabilityTestRouter(t)
	org := "org-import"
	owner := portabilityTokenFor(t, authSvc, "owner@x.io", org, "OWNER")
	foreign := portabilityTokenFor(t, authSvc, "owner@other.io", "org-foreign", "OWNER")

	rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/import", owner, validPortabilityBundle("Imported Agent"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("import = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var created agents.Agent
	if err := json.Unmarshal([]byte(rr.Body.String()), &created); err != nil {
		t.Fatalf("import response is not an agent: %v body=%s", err, rr.Body.String())
	}
	if created.OrganizationID != org || created.Status != "DRAFT" || created.ID == "" {
		t.Fatalf("imported agent identity mismatch: %+v", created)
	}
	if created.Name != "Imported Agent" || created.Instructions != "be helpful" || created.Model != "gpt-4o-mini" {
		t.Fatalf("imported agent config mismatch: %+v", created)
	}
	// Durable: GET detail (legacy catch-all) reflects the import.
	rr = doPortabilityReq(t, h, http.MethodGet, "/api/v1/agents/"+created.ID, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("detail after import = %d, want 200", rr.Code)
	}

	// Validation rejections: 422 VALIDATION_ERROR, no agent left behind.
	before, _ := agentsSvc.ListAgentsCtx(t.Context(), org)
	baseCount := len(before)
	for _, tc := range []struct{ name, body, wantInMessage string }{
		{"unknown model", `{"schema_version":"1","agent":{"name":"A","instructions":"be helpful","model":"totally-made-up-model"}}`, "UNKNOWN_MODEL"},
		{"unknown tool", `{"schema_version":"1","agent":{"name":"A","instructions":"be helpful","model":"gpt-4o-mini","tools":["shell_exec"]}}`, "UNKNOWN_TOOL"},
		{"schema version", `{"schema_version":"2","agent":{"name":"A","instructions":"be helpful","model":"gpt-4o-mini"}}`, "BUNDLE_SCHEMA_VERSION"},
		{"blank name", `{"schema_version":"1","agent":{"name":"  ","instructions":"be helpful","model":"gpt-4o-mini"}}`, "NAME_REQUIRED"},
		{"oversized instructions", `{"schema_version":"1","agent":{"name":"A","instructions":"` + strings.Repeat("x", 64<<10) + `x","model":"gpt-4o-mini"}}`, "INSTRUCTIONS_TOO_LARGE"},
	} {
		rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/import", owner, tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: import = %d, want 422 body=%s", tc.name, rr.Code, rr.Body.String())
		}
		code, message := portabilityErrorEnvelope(t, rr)
		if code != "VALIDATION_ERROR" || !strings.Contains(message, tc.wantInMessage) {
			t.Fatalf("%s: envelope = (%s, %s), want VALIDATION_ERROR naming %s", tc.name, code, message, tc.wantInMessage)
		}
	}
	after, _ := agentsSvc.ListAgentsCtx(t.Context(), org)
	if len(after) != baseCount {
		t.Fatalf("rejected imports left agents behind: %d -> %d", baseCount, len(after))
	}

	// Malformed / missing body: 400 INVALID_REQUEST.
	for _, body := range []string{"", "{not json}"} {
		if rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/import", owner, body); rr.Code != http.StatusBadRequest {
			t.Fatalf("body %q: import = %d, want 400", body, rr.Code)
		}
	}

	// Audit: one agent.imported for the successful create; failures audit nothing.
	imports := 0
	for _, entry := range portabilityAuditActions(t, auditSvc, org) {
		if entry.Action == "agent.imported" {
			imports++
			if entry.Metadata["agent_id"] != created.ID {
				t.Fatalf("import audit metadata mismatch: %v", entry.Metadata)
			}
		}
	}
	if imports != 1 {
		t.Fatalf("expected exactly 1 agent.imported entry, got %d", imports)
	}

	// The foreign org's tenant is never addressable: importing as the foreign
	// owner creates in the FOREIGN org, never in ours (claims-scoped tenant).
	if rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/import", foreign, validPortabilityBundle("Foreign Import")); rr.Code != http.StatusCreated {
		t.Fatalf("foreign import = %d, want 201", rr.Code)
	}
	orgAgents, _ := agentsSvc.ListAgentsCtx(t.Context(), org)
	if len(orgAgents) != baseCount { // only the original import landed here
		t.Fatalf("foreign import leaked into caller org: %d agents", len(orgAgents))
	}
}

func TestAgentImportNeverTrustsClientIdentity(t *testing.T) {
	h, authSvc, _, _ := newAgentPortabilityTestRouter(t)
	org := "org-honest"
	owner := portabilityTokenFor(t, authSvc, "owner@x.io", org, "OWNER")

	payload := `{
                "schema_version":"1",
                "id":"evil-bundle","organization_id":"org-evil","created_at":"1999-01-01T00:00:00Z","status":"ARCHIVED",
                "agent":{"id":"evil-agent","organization_id":"org-evil","name":"Smuggled","description":"d",
                         "instructions":"be helpful","model":"gpt-4o-mini","status":"ACTIVE",
                         "current_version_id":"evil-version","created_at":"1999-01-01T00:00:00Z"}
        }`
	rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/import", owner, payload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("smuggled import = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var created agents.Agent
	if err := json.Unmarshal([]byte(rr.Body.String()), &created); err != nil {
		t.Fatalf("import response decode failed: %v", err)
	}
	if created.OrganizationID != org {
		t.Fatalf("client organization_id was trusted: %q", created.OrganizationID)
	}
	if created.Status != "DRAFT" {
		t.Fatalf("client status was trusted: %q", created.Status)
	}
	if created.ID == "evil-agent" || created.ID == "evil-bundle" || created.CurrentVersionID == "evil-version" {
		t.Fatalf("client ids were trusted: %+v", created)
	}
	if created.CreatedAt.Before(time.Now().UTC().Add(-time.Minute)) {
		t.Fatalf("client created_at was trusted: %v", created.CreatedAt)
	}
}

func TestAgentCloneEndpointContract(t *testing.T) {
	h, authSvc, agentsSvc, auditSvc := newAgentPortabilityTestRouter(t)
	org := "org-clone"
	seed := seedPortabilityAgent(t, agentsSvc, org, "Support Agent")
	owner := portabilityTokenFor(t, authSvc, "owner@x.io", org, "OWNER")
	foreign := portabilityTokenFor(t, authSvc, "owner@other.io", "org-foreign", "OWNER")
	// Move the source to ACTIVE: the clone must reset to DRAFT.
	for _, target := range []agents.AgentState{agents.StateBuilding, agents.StateActive} {
		if err := agentsSvc.TransitionState(t.Context(), org, seed.ID, target); err != nil {
			t.Fatalf("activate failed: %v", err)
		}
	}

	rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/clone", owner, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("clone = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var clone agents.Agent
	if err := json.Unmarshal([]byte(rr.Body.String()), &clone); err != nil {
		t.Fatalf("clone response decode failed: %v body=%s", err, rr.Body.String())
	}
	if clone.Name != "Support Agent-copy" {
		t.Fatalf("clone name = %q, want %q", clone.Name, "Support Agent-copy")
	}
	if clone.Status != "DRAFT" || clone.OrganizationID != org || clone.ID == seed.ID {
		t.Fatalf("clone identity mismatch: %+v", clone)
	}
	if clone.Instructions != "be helpful" || clone.Model != "gpt-4o-mini" || clone.Description != "front-line helper" {
		t.Fatalf("clone config mismatch: %+v", clone)
	}
	// The source is untouched: still ACTIVE.
	rr = doPortabilityReq(t, h, http.MethodGet, "/api/v1/agents/"+seed.ID, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("source detail = %d, want 200", rr.Code)
	}
	var source agents.Agent
	_ = json.Unmarshal([]byte(rr.Body.String()), &source)
	if source.Status != "ACTIVE" || source.Name != "Support Agent" {
		t.Fatalf("source mutated by clone: %+v", source)
	}

	// Cross-org / unknown: 404 with no existence leak.
	if rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/"+seed.ID+"/clone", foreign, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org clone = %d, want 404", rr.Code)
	} else if code, _ := portabilityErrorEnvelope(t, rr); code != "AGENT_NOT_FOUND" {
		t.Fatalf("cross-org clone code = %s, want AGENT_NOT_FOUND", code)
	}
	if rr := doPortabilityReq(t, h, http.MethodPost, "/api/v1/agents/does-not-exist/clone", owner, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown clone = %d, want 404", rr.Code)
	}

	// Audit: exactly one agent.cloned carrying both ids.
	clones := 0
	for _, entry := range portabilityAuditActions(t, auditSvc, org) {
		if entry.Action == "agent.cloned" {
			clones++
			if entry.Metadata["agent_id"] != clone.ID || entry.Metadata["source_agent_id"] != seed.ID {
				t.Fatalf("clone audit metadata mismatch: %v", entry.Metadata)
			}
		}
	}
	if clones != 1 {
		t.Fatalf("expected exactly 1 agent.cloned entry, got %d", clones)
	}
}
