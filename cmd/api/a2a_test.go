package main

// a2a_test.go — issue #136 HTTP surface, exercised through the REAL
// middleware chain (RequireAuthOrAPIKey -> RequirePermission): the RBAC
// matrix, the registry CRUD + canonical discovery contract, tenant
// isolation, the inbound message happy path over the REAL runs + queue
// services (exported API), and the governed delegation (allow + gate
// denial). Reuses the mcp_registry_test.go fixture style.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentos/internal/a2a"
	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// newA2ATestRouter wires the A2A routes against in-memory services: a real
// agents service (agent-link validation), a real runs service + queue (the
// inbound message happy path), and an audit service.
func newA2ATestRouter(t *testing.T) (http.Handler, *authpkg.Service, *apikeys.Service, *audit.Service, *a2a.Service, *agents.Service, *runs.Service, *queue.Queue) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	auditSvc := audit.NewService()
	agentsSvc := agents.NewService()
	runsSvc := runs.NewService()
	workQueue := queue.NewQueue()

	svc := a2a.NewService()
	svc.SetAgentChecker(agentsSvc)
	svc.SetAuditor(auditSvc)

	apiMux := http.NewServeMux()
	registerA2ARoutes(apiMux, svc, runsSvc, workQueue, authSvc, keysSvc, auditSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, keysSvc, auditSvc, svc, agentsSvc, runsSvc, workQueue
}

func a2aTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
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

func doA2AReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func decodeA2ABody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

func a2aCardBody(agentID, endpoint string) string {
	return fmt.Sprintf(`{"agent_id":%q,"name":"Weather Agent","description":"Talks about weather","version":"1.0.0","endpoint_url":%q,"capabilities":{"streaming":true,"push_notifications":false,"state_transition_history":false},"skills":[{"id":"forecast","name":"Forecast","description":"5-day forecast","tags":["weather"]}],"auth_scheme":"bearer","preferred_transport":"JSONRPC"}`, agentID, endpoint)
}

// seedA2AAgent creates one platform agent in the org (cards must link to a
// real agent) and returns its id.
func seedA2AAgent(t *testing.T, agentsSvc *agents.Service, orgID, name string) string {
	t.Helper()
	agent, err := agentsSvc.CreateAgentCtx(context.Background(), orgID, name, "desc", "instructions", "test-model")
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return agent.ID
}

// createA2ACardViaHTTP publishes one card for org (owner token) and fails
// the test on any non-201.
func createA2ACardViaHTTP(t *testing.T, h http.Handler, token, agentID, endpoint string) map[string]any {
	t.Helper()
	rr := doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents", token, a2aCardBody(agentID, endpoint))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create card: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	return decodeA2ABody(t, rr)
}

func TestA2ARoutesAuthRequired(t *testing.T) {
	h, _, _, _, _, agentsSvc, _, _ := newA2ATestRouter(t)
	agentID := seedA2AAgent(t, agentsSvc, "org-a", "seeded")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/a2a/agents"},
		{http.MethodPost, "/api/v1/a2a/agents"},
		{http.MethodGet, "/api/v1/a2a/agents/some-id"},
		{http.MethodGet, "/api/v1/a2a/agents/some-id/card"},
		{http.MethodPatch, "/api/v1/a2a/agents/some-id"},
		{http.MethodDelete, "/api/v1/a2a/agents/some-id"},
		{http.MethodPost, "/api/v1/a2a/agents/" + agentID + "/message"},
		{http.MethodPost, "/api/v1/a2a/delegate"},
	} {
		rr := doA2AReq(t, h, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestA2ARoutesRoleMatrix(t *testing.T) {
	h, authSvc, _, _, svc, agentsSvc, _, _ := newA2ATestRouter(t)
	const org = "org-m"
	agentID := seedA2AAgent(t, agentsSvc, org, "matrix-agent")

	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = a2aTokenFor(t, authSvc, strings.ToLower(role)+"@x.io", org, role)
	}

	// Seed one card (service-level, RBAC-agnostic) for read targets.
	card, err := svc.CreateCard(context.Background(), org, a2a.CreateCardInput{
		AgentID: agentID, Name: "seeded", Version: "1.0.0", CreatedBy: "seeder",
	})
	if err != nil {
		t.Fatalf("seed card: %v", err)
	}

	// Discovery card reads: agents.read = EVERY authenticated org member
	// (VIEWER included) — the discovery surface.
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		rr := doA2AReq(t, h, http.MethodGet, "/api/v1/a2a/agents/"+card.ID+"/card", tokens[role], "")
		if rr.Code != http.StatusOK {
			t.Errorf("GET card as %s: expected 200, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}

	// Registry reads: connectors.read = MEMBER+ (VIEWER 403).
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER"} {
		rr := doA2AReq(t, h, http.MethodGet, "/api/v1/a2a/agents", tokens[role], "")
		if rr.Code != http.StatusOK {
			t.Errorf("GET list as %s: expected 200, got %d", role, rr.Code)
		}
	}
	if rr := doA2AReq(t, h, http.MethodGet, "/api/v1/a2a/agents", tokens["VIEWER"], ""); rr.Code != http.StatusForbidden {
		t.Errorf("GET list as VIEWER: expected 403, got %d", rr.Code)
	}

	// Writes: agents.write = OWNER/ADMIN only.
	for _, role := range []string{"OWNER", "ADMIN"} {
		rr := doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents", tokens[role], a2aCardBody(agentID, "https://x.example/"+role))
		if rr.Code != http.StatusCreated {
			t.Errorf("POST as %s: expected 201, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}
	for _, role := range []string{"MEMBER", "VIEWER"} {
		rr := doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents", tokens[role], a2aCardBody(agentID, "https://x.example/"+role))
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST as %s: expected 403, got %d", role, rr.Code)
		}
		rr = doA2AReq(t, h, http.MethodPatch, "/api/v1/a2a/agents/"+card.ID, tokens[role], `{"name":"x"}`)
		if rr.Code != http.StatusForbidden {
			t.Errorf("PATCH as %s: expected 403, got %d", role, rr.Code)
		}
		rr = doA2AReq(t, h, http.MethodDelete, "/api/v1/a2a/agents/"+card.ID, tokens[role], "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("DELETE as %s: expected 403, got %d", role, rr.Code)
		}
	}

	// Invocation surfaces: runs.execute = OWNER/ADMIN/MEMBER (VIEWER 403).
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER"} {
		rr := doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents/"+card.ID+"/message", tokens[role], `{"message":"hi"}`)
		if rr.Code != http.StatusAccepted {
			t.Errorf("message as %s: expected 202, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}
	if rr := doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents/"+card.ID+"/message", tokens["VIEWER"], `{"message":"hi"}`); rr.Code != http.StatusForbidden {
		t.Errorf("message as VIEWER: expected 403, got %d", rr.Code)
	}
}

func TestA2ACardLifecycleHTTP(t *testing.T) {
	h, authSvc, _, auditSvc, _, agentsSvc, _, _ := newA2ATestRouter(t)
	const org = "org-a"
	agentID := seedA2AAgent(t, agentsSvc, org, "weather")
	token := a2aTokenFor(t, authSvc, "owner@x.io", org, "OWNER")

	created := createA2ACardViaHTTP(t, h, token, agentID, "https://agents.example/weather")
	card := created["agent_card"].(map[string]any)
	id := card["id"].(string)
	if card["agent_id"] != agentID || card["auth_scheme"] != "bearer" || card["preferred_transport"] != "JSONRPC" {
		t.Fatalf("registry row wrong: %v", card)
	}
	if card["organization_id"] != org {
		t.Fatalf("org scope wrong: %v", card["organization_id"])
	}
	if skills := card["skills"].([]any); len(skills) != 1 {
		t.Fatalf("skills echo wrong: %v", skills)
	}

	// Canonical discovery card: camelCase A2A keys, no platform internals.
	rr := doA2AReq(t, h, http.MethodGet, "/api/v1/a2a/agents/"+id+"/card", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("discovery: expected 200, got %d", rr.Code)
	}
	var canonical map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &canonical); err != nil {
		t.Fatalf("discovery body: %v", err)
	}
	for _, key := range []string{"name", "description", "version", "provider", "url", "capabilities", "skills", "authentication", "preferredTransport"} {
		if _, ok := canonical[key]; !ok {
			t.Errorf("canonical card must carry %q (body=%s)", key, rr.Body.String())
		}
	}
	for _, key := range []string{"id", "organization_id", "agent_id"} {
		if _, ok := canonical[key]; ok {
			t.Errorf("canonical card must not carry %q", key)
		}
	}
	if caps := canonical["capabilities"].(map[string]any); caps["streaming"] != true || caps["pushNotifications"] != false {
		t.Errorf("canonical capabilities wrong: %v", caps)
	}
	if auth := canonical["authentication"].(map[string]any); auth["schemes"].([]any)[0] != "bearer" {
		t.Errorf("canonical authentication wrong: %v", auth)
	}

	// PATCH: partial update.
	rr = doA2AReq(t, h, http.MethodPatch, "/api/v1/a2a/agents/"+id, token, `{"name":"Weather v2","version":"1.1.0"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	patched := decodeA2ABody(t, rr)["agent_card"].(map[string]any)
	if patched["name"] != "Weather v2" || patched["version"] != "1.1.0" || patched["agent_id"] != agentID {
		t.Fatalf("patch mismatch: %v", patched)
	}

	// DELETE: gone for everyone afterwards.
	rr = doA2AReq(t, h, http.MethodDelete, "/api/v1/a2a/agents/"+id, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", rr.Code)
	}
	if out := decodeA2ABody(t, rr); out["deleted"] != true || out["id"] != id {
		t.Fatalf("delete payload wrong: %v", out)
	}
	rr = doA2AReq(t, h, http.MethodGet, "/api/v1/a2a/agents/"+id, token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("deleted card must 404, got %d", rr.Code)
	}
	rr = doA2AReq(t, h, http.MethodGet, "/api/v1/a2a/agents/"+id+"/card", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("deleted discovery card must 404, got %d", rr.Code)
	}

	// Audit rows for the lifecycle.
	entries, err := auditSvc.ListCtx(context.Background(), org)
	if err != nil {
		t.Fatalf("audit list failed: %v", err)
	}
	actions := map[string]int{}
	for _, e := range entries {
		actions[e.Action]++
	}
	if actions["a2a.card_published"] != 1 || actions["a2a.card_updated"] != 1 || actions["a2a.card_deleted"] != 1 {
		t.Fatalf("audit actions wrong: %v", actions)
	}
}

func TestA2ACardValidationHTTP(t *testing.T) {
	h, authSvc, _, _, _, agentsSvc, _, _ := newA2ATestRouter(t)
	const org = "org-a"
	agentID := seedA2AAgent(t, agentsSvc, org, "weather")
	token := a2aTokenFor(t, authSvc, "owner@x.io", org, "OWNER")

	// Malformed JSON -> 400.
	if rr := doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents", token, "{nope"); rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d", rr.Code)
	}
	// Missing name -> 422 VALIDATION_ERROR.
	rr := doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents", token, `{"agent_id":"x","version":"1.0.0"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing name: expected 422, got %d", rr.Code)
	}
	if code := decodeA2ABody(t, rr)["error"].(map[string]any)["code"]; code != "VALIDATION_ERROR" {
		t.Fatalf("error code = %v", code)
	}
	// Unknown agent -> 422 (agent must exist in the org).
	rr = doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents", token, a2aCardBody("agent-missing", "https://x.example"))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown agent: expected 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	// Bad endpoint scheme -> 422.
	rr = doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents", token, a2aCardBody(agentID, "ftp://x.example"))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad endpoint: expected 422, got %d", rr.Code)
	}
	// Unknown card -> 404 (no existence leak).
	rr = doA2AReq(t, h, http.MethodGet, "/api/v1/a2a/agents/nope/card", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown discovery: expected 404, got %d", rr.Code)
	}
	if code := decodeA2ABody(t, rr)["error"].(map[string]any)["code"]; code != "AGENT_CARD_NOT_FOUND" {
		t.Fatalf("error code = %v", code)
	}
}

func TestA2ACrossOrgIsolation(t *testing.T) {
	h, authSvc, _, _, svc, agentsSvc, _, _ := newA2ATestRouter(t)
	agentA := seedA2AAgent(t, agentsSvc, "org-a", "a-agent")
	orgA := a2aTokenFor(t, authSvc, "a@x.io", "org-a", "OWNER")
	orgB := a2aTokenFor(t, authSvc, "b@x.io", "org-b", "OWNER")

	created := createA2ACardViaHTTP(t, h, orgA, agentA, "https://agents.example/a")
	id := created["agent_card"].(map[string]any)["id"].(string)

	// Org B cannot read, discover, update or delete org A's card (404, no
	// existence leak) — and org B's list never contains it.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/a2a/agents/" + id, ""},
		{http.MethodGet, "/api/v1/a2a/agents/" + id + "/card", ""},
		{http.MethodPatch, "/api/v1/a2a/agents/" + id, `{"name":"hijack"}`},
		{http.MethodDelete, "/api/v1/a2a/agents/" + id, ""},
		{http.MethodPost, "/api/v1/a2a/agents/" + id + "/message", `{"message":"hi"}`},
	} {
		rr := doA2AReq(t, h, tc.method, tc.path, orgB, tc.body)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s cross-org: expected 404, got %d body=%s", tc.method, rr.Code, rr.Body.String())
		}
	}
	rr := doA2AReq(t, h, http.MethodGet, "/api/v1/a2a/agents", orgB, "")
	if items := decodeA2ABody(t, rr)["agent_cards"].([]any); len(items) != 0 {
		t.Errorf("org B list must be empty, got %v", items)
	}
	_ = svc
}

func TestA2AMessageEndpointHappyPath(t *testing.T) {
	h, authSvc, _, auditSvc, svc, agentsSvc, runsSvc, workQueue := newA2ATestRouter(t)
	const org = "org-a"
	agentID := seedA2AAgent(t, agentsSvc, org, "weather")
	owner := a2aTokenFor(t, authSvc, "owner@x.io", org, "OWNER")
	token := a2aTokenFor(t, authSvc, "member@x.io", org, "MEMBER")

	created := createA2ACardViaHTTP(t, h, owner, agentID, "https://agents.example/weather")
	id := created["agent_card"].(map[string]any)["id"].(string)
	_ = svc

	// MEMBER can message the card: a REAL run is created and the REAL task
	// is enqueued (both via the services' exported API).
	rr := doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents/"+id+"/message", token, `{"message":"ship the forecast"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("message: expected 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	out := decodeA2ABody(t, rr)
	if out["status"] != "submitted" {
		t.Fatalf("status = %v, want submitted", out["status"])
	}
	runID, _ := out["run_id"].(string)
	taskID, _ := out["task_id"].(string)
	if runID == "" || taskID == "" {
		t.Fatalf("run_id/task_id must be present: %v", out)
	}

	// The run really exists (QUEUED, org-scoped, right agent + input).
	run, err := runsSvc.GetRunCtx(context.Background(), org, runID)
	if err != nil {
		t.Fatalf("run must exist: %v", err)
	}
	if run.AgentID != agentID || run.Input != "ship the forecast" {
		t.Fatalf("run fields wrong: %+v", run)
	}
	if string(run.Status) != "QUEUED" {
		t.Fatalf("run status = %v, want QUEUED", run.Status)
	}

	// The task really sits on the queue, org-scoped.
	task := workQueue.Peek()
	if task == nil {
		t.Fatal("a task must be queued")
	}
	if task.Type != "agent.run" || task.OrganizationID != org {
		t.Fatalf("task wrong: %+v", task)
	}
	if task.Payload["run_id"] != runID || task.Payload["agent_id"] != agentID {
		t.Fatalf("task payload wrong: %v", task.Payload)
	}
	if task.ID != taskID {
		t.Fatalf("task_id echo mismatch: %v vs %v", task.ID, taskID)
	}

	// Audit: a2a.message_received with metadata ONLY (never the payload).
	entries, err := auditSvc.ListCtx(context.Background(), org)
	if err != nil {
		t.Fatalf("audit list failed: %v", err)
	}
	var found *audit.Entry
	for _, e := range entries {
		if e.Action == "a2a.message_received" {
			found = e
		}
	}
	if found == nil {
		t.Fatal("expected an a2a.message_received audit row")
	}
	if strings.Contains(fmt.Sprintf("%v", found.Metadata), "ship the forecast") {
		t.Fatal("the audit row must never carry the message payload")
	}
	// message_bytes is an int in the in-memory metadata map (queue_ops_test
	// precedent: compare against the untyped constant, not float64 —
	// float64 is the JSON-decoded body convention, not audit metadata).
	if found.Metadata["run_id"] != runID || found.Metadata["message_bytes"] != len("ship the forecast") {
		t.Fatalf("audit metadata wrong: %v", found.Metadata)
	}

	// Empty message -> 422.
	rr = doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/agents/"+id+"/message", token, `{"message":"  "}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty message: expected 422, got %d", rr.Code)
	}
}

// a2aDelegateFixture is the remote A2A endpoint the delegation tests POST
// to (records bodies, answers 200).
type a2aDelegateFixture struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies []string
}

func newA2ADelegateFixture() *a2aDelegateFixture {
	f := &a2aDelegateFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(raw))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return f
}

func (f *a2aDelegateFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func TestA2ADelegateEndpointHTTP(t *testing.T) {
	fixture := newA2ADelegateFixture()
	defer fixture.srv.Close()

	h, authSvc, _, auditSvc, svc, agentsSvc, _, _ := newA2ATestRouter(t)
	const org = "org-a"
	agentID := seedA2AAgent(t, agentsSvc, org, "weather")
	owner := a2aTokenFor(t, authSvc, "owner@x.io", org, "OWNER")
	token := a2aTokenFor(t, authSvc, "member@x.io", org, "MEMBER")

	created := createA2ACardViaHTTP(t, h, owner, agentID, fixture.srv.URL)
	id := created["agent_card"].(map[string]any)["id"].(string)

	// Allow path (nil gate -> allowed_no_gate verdict): 200 + result fields.
	rr := doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/delegate", token,
		fmt.Sprintf(`{"card_id":%q,"message":"delegate me"}`, id))
	if rr.Code != http.StatusOK {
		t.Fatalf("delegate: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	out := decodeA2ABody(t, rr)
	if out["status"] != "submitted" || out["http_status"] != float64(http.StatusOK) {
		t.Fatalf("delegate result wrong: %v", out)
	}
	if _, ok := out["latency_ms"]; !ok {
		t.Fatalf("latency_ms must be present: %v", out)
	}
	if out["verdict"] != "allowed_no_gate" {
		t.Fatalf("nil gate verdict = %v, want allowed_no_gate", out["verdict"])
	}
	if fixture.count() != 1 {
		t.Fatalf("expected one POST to the remote, got %d", fixture.count())
	}
	// The delegation itself is audited by the service via the Auditor seam.
	entries, _ := auditSvc.ListCtx(context.Background(), org)
	var delegated int
	for _, e := range entries {
		if e.Action == "a2a.delegated" {
			delegated++
			if strings.Contains(fmt.Sprintf("%v", e.Metadata), "delegate me") {
				t.Fatal("delegation audit must never carry the message payload")
			}
		}
	}
	if delegated != 1 {
		t.Fatalf("expected one a2a.delegated row, got %d", delegated)
	}

	// Gate denial: 403 with the policy-style delegation_denied verdict, and
	// NOTHING hits the remote.
	svc.SetDelegationGate(a2a.DelegationGateFunc(func(_ context.Context, _ string, _ *a2a.AgentCard, _ string) a2a.DelegationVerdict {
		return a2a.DelegationVerdict{Allowed: false, Reason: "not on the delegation allowlist"}
	}))
	rr = doA2AReq(t, h, http.MethodPost, "/api/v1/a2a/delegate", token,
		fmt.Sprintf(`{"card_id":%q,"message":"blocked"}`, id))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("denied delegate: expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	out = decodeA2ABody(t, rr)
	if out["error"].(map[string]any)["code"] != "delegation_denied" {
		t.Fatalf("denial code wrong: %v", out)
	}
	if verdict := out["verdict"].(map[string]any); verdict["decision"] != "delegation_denied" || verdict["reason"] != "not on the delegation allowlist" {
		t.Fatalf("denial verdict wrong: %v", verdict)
	}
	if fixture.count() != 1 {
		t.Fatalf("a denied delegation must not reach the remote, got %d POSTs", fixture.count())
	}
	entries, _ = auditSvc.ListCtx(context.Background(), org)
	var denialRows int
	for _, e := range entries {
		if e.Action == "a2a.delegation_denied" {
			denialRows++
		}
	}
	if denialRows != 1 {
		t.Fatalf("expected one a2a.delegation_denied row, got %d", denialRows)
	}
}
