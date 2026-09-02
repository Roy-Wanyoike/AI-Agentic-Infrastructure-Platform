package main

// Track 2-b handler tests: auth (401/403), full version + deployment lifecycles
// through the registered middleware chain, contract JSON shapes, and tenant
// isolation. All in-memory, no infrastructure.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/deployments"
)

// versionsHandlerEnv wires the full handler stack the way main.go routes() does
// (including the "/agents/" catch-all so route precedence is exercised) and
// returns the mux plus owner/viewer/member bearer tokens for one tenant.
type versionsHandlerEnv struct {
	mux         *http.ServeMux
	agentsSvc   *agents.Service
	versionsSvc *agents.VersionsService
	depSvc      *deployments.Service
	orgID       string
	agentID     string
	ownerToken  string
	viewerToken string
	memberToken string
	otherToken  string // authenticated OWNER of a DIFFERENT tenant
}

func newVersionsHandlerEnv(t *testing.T) *versionsHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	ownerToken, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken(owner) returned error: %v", err)
	}
	// VIEWER and MEMBER users exist only as claims (claims.Role path in
	// RequirePermission covers roles not registered in the user map).
	viewerToken, err := authSvc.GenerateToken(&auth.User{
		ID: "user-viewer", Organization: owner.Organization, Email: "viewer@acme.test", Role: "VIEWER",
	})
	if err != nil {
		t.Fatalf("GenerateToken(viewer) returned error: %v", err)
	}
	memberToken, err := authSvc.GenerateToken(&auth.User{
		ID: "user-member", Organization: owner.Organization, Email: "member@acme.test", Role: "MEMBER",
	})
	if err != nil {
		t.Fatalf("GenerateToken(member) returned error: %v", err)
	}
	_, foreign, err := authSvc.Register("OtherCo", "owner@other.test", "secret123")
	if err != nil {
		t.Fatalf("Register(foreign) returned error: %v", err)
	}
	otherToken, err := authSvc.GenerateToken(foreign)
	if err != nil {
		t.Fatalf("GenerateToken(foreign) returned error: %v", err)
	}

	agentsSvc := agents.NewService()
	agent, err := agentsSvc.CreateAgentCtx(context.Background(), owner.Organization, "Support Agent", "desc", "help users v1", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx returned error: %v", err)
	}
	versionsSvc := agents.NewVersionsService(agentsSvc)
	depSvc := deployments.NewService(versionsSvc)

	mux := http.NewServeMux()
	// Mirror main.go's catch-all so the more-specific registrations below must
	// win (ServeMux pattern precedence under real wiring).
	mux.Handle("/agents/", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionAgentsRead)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"catchall":true}`))
	}))))
	registerVersionsRoutes(mux, versionsSvc, authSvc, apiKeysSvc)
	registerDeploymentsRoutes(mux, depSvc, authSvc, apiKeysSvc)

	return &versionsHandlerEnv{
		mux:         mux,
		agentsSvc:   agentsSvc,
		versionsSvc: versionsSvc,
		depSvc:      depSvc,
		orgID:       owner.Organization,
		agentID:     agent.ID,
		ownerToken:  ownerToken,
		viewerToken: viewerToken,
		memberToken: memberToken,
		otherToken:  otherToken,
	}
}

func (e *versionsHandlerEnv) do(t *testing.T, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	out := map[string]any{}
	if strings.Contains(rr.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
	}
	return rr, out
}

func errCode(t *testing.T, body map[string]any) string {
	t.Helper()
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got %v", body)
	}
	code, _ := errObj["code"].(string)
	return code
}

func TestVersionsEndpointsRequireAuth(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	paths := []struct{ method, path string }{
		{http.MethodGet, "/agents/" + env.agentID + "/versions"},
		{http.MethodPost, "/agents/" + env.agentID + "/versions/create"},
		{http.MethodPost, "/agents/" + env.agentID + "/versions/2/publish"},
		{http.MethodPost, "/agents/" + env.agentID + "/rollback"},
		{http.MethodGet, "/deployments"},
		{http.MethodPost, "/deployments/create"},
		{http.MethodGet, "/deployments/dep-1"},
		{http.MethodPost, "/deployments/dep-1/promote"},
		{http.MethodPost, "/deployments/dep-1/rollback"},
	}
	for _, p := range paths {
		rr, _ := env.do(t, p.method, p.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without credentials: expected %d, got %d body=%s", p.method, p.path, http.StatusUnauthorized, rr.Code, rr.Body.String())
		}
	}
}

func TestVersionsViewerReadOnly(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	if _, err := env.versionsSvc.CreateVersionCtx(context.Background(), env.orgID, env.agentID, "user-1"); err != nil {
		t.Fatalf("seed version failed: %v", err)
	}

	// VIEWER has agents.read: listing versions is allowed.
	rr, _ := env.do(t, http.MethodGet, "/agents/"+env.agentID+"/versions", env.viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer list versions: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	// VIEWER lacks agents.write: mutations are 403.
	for _, p := range []struct{ method, path, body string }{
		{http.MethodPost, "/agents/" + env.agentID + "/versions/create", ""},
		{http.MethodPost, "/agents/" + env.agentID + "/versions/2/publish", ""},
		{http.MethodPost, "/agents/" + env.agentID + "/rollback", `{"target_version":2}`},
	} {
		rr, _ := env.do(t, p.method, p.path, env.viewerToken, p.body)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s as viewer: expected %d, got %d body=%s", p.method, p.path, http.StatusForbidden, rr.Code, rr.Body.String())
		}
	}
}

func TestVersionsHandlerLifecycle(t *testing.T) {
	env := newVersionsHandlerEnv(t)

	// Create a snapshot of the current config (legacy v1 exists -> version 2).
	rr, body := env.do(t, http.MethodPost, "/agents/"+env.agentID+"/versions/create", env.ownerToken, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create version: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	if v, _ := body["version"].(float64); v != 2 {
		t.Fatalf(`create version: expected {"version":2}, got %v`, body)
	}

	// Publish it.
	rr, body = env.do(t, http.MethodPost, "/agents/"+env.agentID+"/versions/2/publish", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("publish version: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if v, _ := body["version"].(float64); v != 2 {
		t.Fatalf(`publish version: expected {"version":2}, got %v`, body)
	}

	// List: contract shape {"versions":[{version,snapshot,published_at,published_by,status}]}.
	rr, body = env.do(t, http.MethodGet, "/agents/"+env.agentID+"/versions", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list versions: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	raw, _ := json.Marshal(body["versions"])
	var versions []struct {
		Version     int             `json:"version"`
		Snapshot    json.RawMessage `json:"snapshot"`
		PublishedAt *string         `json:"published_at"`
		PublishedBy string          `json:"published_by"`
		Status      string          `json:"status"`
	}
	if err := json.Unmarshal(raw, &versions); err != nil {
		t.Fatalf("versions payload is not an array: %v (%s)", err, raw)
	}
	if len(versions) != 1 || versions[0].Version != 2 || versions[0].Status != "published" || versions[0].PublishedAt == nil || versions[0].PublishedBy == "" {
		t.Fatalf("unexpected versions payload: %s", raw)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(versions[0].Snapshot, &snapshot); err != nil {
		t.Fatalf("snapshot must be an embedded JSON object, got %s", versions[0].Snapshot)
	}
	if snapshot["instructions"] != "help users v1" || snapshot["model"] != "gpt-4o-mini" {
		t.Fatalf("snapshot did not capture agent config: %v", snapshot)
	}

	// Drift the config, snapshot v3, publish, then roll back to v2.
	agent, _ := env.agentsSvc.GetAgentCtx(context.Background(), env.orgID, env.agentID)
	agent.Instructions = "help users v2"
	if err := env.agentsSvc.UpdateAgentCtx(context.Background(), env.orgID, agent); err != nil {
		t.Fatalf("UpdateAgentCtx returned error: %v", err)
	}
	rr, _ = env.do(t, http.MethodPost, "/agents/"+env.agentID+"/versions/create", env.ownerToken, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create v3: expected %d, got %d", http.StatusCreated, rr.Code)
	}
	rr, _ = env.do(t, http.MethodPost, "/agents/"+env.agentID+"/versions/3/publish", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("publish v3: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	rr, body = env.do(t, http.MethodPost, "/agents/"+env.agentID+"/rollback", env.ownerToken, `{"target_version":2}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("rollback: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if v, _ := body["current_version"].(float64); v != 2 {
		t.Fatalf(`rollback: expected {"current_version":2}, got %v`, body)
	}
	restored, _ := env.agentsSvc.GetAgentCtx(context.Background(), env.orgID, env.agentID)
	if restored.Instructions != "help users v1" {
		t.Fatalf("rollback should restore snapshot config, got %+v", restored)
	}
}

func TestVersionsHandlerErrors(t *testing.T) {
	env := newVersionsHandlerEnv(t)

	// Unknown agent surfaces as 404 AGENT_NOT_FOUND on every route.
	rr, body := env.do(t, http.MethodGet, "/agents/agent-missing/versions", env.ownerToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "AGENT_NOT_FOUND" {
		t.Fatalf("list for unknown agent: expected 404 AGENT_NOT_FOUND, got %d %v", rr.Code, body)
	}
	rr, body = env.do(t, http.MethodPost, "/agents/agent-missing/rollback", env.ownerToken, `{"target_version":2}`)
	if rr.Code != http.StatusNotFound || errCode(t, body) != "AGENT_NOT_FOUND" {
		t.Fatalf("rollback for unknown agent: expected 404 AGENT_NOT_FOUND, got %d %v", rr.Code, body)
	}

	// Unknown version -> 404 VERSION_NOT_FOUND.
	rr, body = env.do(t, http.MethodPost, "/agents/"+env.agentID+"/versions/9/publish", env.ownerToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "VERSION_NOT_FOUND" {
		t.Fatalf("publish unknown version: expected 404 VERSION_NOT_FOUND, got %d %v", rr.Code, body)
	}
	rr, body = env.do(t, http.MethodPost, "/agents/"+env.agentID+"/rollback", env.ownerToken, `{"target_version":9}`)
	if rr.Code != http.StatusNotFound || errCode(t, body) != "VERSION_NOT_FOUND" {
		t.Fatalf("rollback unknown version: expected 404 VERSION_NOT_FOUND, got %d %v", rr.Code, body)
	}

	// Malformed body / missing target_version.
	rr, body = env.do(t, http.MethodPost, "/agents/"+env.agentID+"/rollback", env.ownerToken, `{not json`)
	if rr.Code != http.StatusBadRequest || errCode(t, body) != "INVALID_REQUEST" {
		t.Fatalf("malformed rollback body: expected 400 INVALID_REQUEST, got %d %v", rr.Code, body)
	}
	rr, body = env.do(t, http.MethodPost, "/agents/"+env.agentID+"/rollback", env.ownerToken, `{"target_version":0}`)
	if rr.Code != http.StatusUnprocessableEntity || errCode(t, body) != "VALIDATION_ERROR" {
		t.Fatalf("rollback without target: expected 422 VALIDATION_ERROR, got %d %v", rr.Code, body)
	}

	// Non-integer version segment.
	rr, body = env.do(t, http.MethodPost, "/agents/"+env.agentID+"/versions/abc/publish", env.ownerToken, "")
	if rr.Code != http.StatusBadRequest || errCode(t, body) != "INVALID_REQUEST" {
		t.Fatalf("publish non-integer version: expected 400 INVALID_REQUEST, got %d %v", rr.Code, body)
	}

	// Cross-tenant access surfaces as 404 (tenant guard).
	rr, _ = env.do(t, http.MethodGet, "/agents/"+env.agentID+"/versions", env.otherToken, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant list: expected %d, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestDeploymentsViewerAndMemberRBAC(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	// Seed a published version so deployment creation can succeed.
	version, err := env.versionsSvc.CreateVersionCtx(context.Background(), env.orgID, env.agentID, "user-1")
	if err != nil {
		t.Fatalf("seed version failed: %v", err)
	}
	if _, err := env.versionsSvc.PublishVersionCtx(context.Background(), env.orgID, env.agentID, version.Version, "user-1"); err != nil {
		t.Fatalf("seed publish failed: %v", err)
	}
	payload := `{"agent_id":"` + env.agentID + `","version":` + strconv.Itoa(version.Version) + `,"environment":"development"}`

	// VIEWER can read but not create (deployments.write).
	rr, _ := env.do(t, http.MethodGet, "/deployments", env.viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer list deployments: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	rr, _ = env.do(t, http.MethodPost, "/deployments/create", env.viewerToken, payload)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer create deployment: expected %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}

	// MEMBER can create (deployments.write) but not promote/rollback (deployments.deploy).
	rr, _ = env.do(t, http.MethodPost, "/deployments/create", env.memberToken, payload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("member create deployment: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	rr, listBody := env.do(t, http.MethodGet, "/deployments", env.memberToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("member list deployments: expected %d, got %d", http.StatusOK, rr.Code)
	}
	deploymentsRaw, _ := json.Marshal(listBody["deployments"])
	var views []map[string]any
	if err := json.Unmarshal(deploymentsRaw, &views); err != nil || len(views) != 1 {
		t.Fatalf("expected exactly 1 deployment after member create, got %s (%v)", deploymentsRaw, err)
	}
	id, _ := views[0]["id"].(string)
	rr, _ = env.do(t, http.MethodPost, "/deployments/"+id+"/promote", env.memberToken, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member promote: expected %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
	rr, _ = env.do(t, http.MethodPost, "/deployments/"+id+"/rollback", env.memberToken, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member rollback: expected %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
}

func TestDeploymentsHandlerLifecycle(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	version, err := env.versionsSvc.CreateVersionCtx(context.Background(), env.orgID, env.agentID, "user-1")
	if err != nil {
		t.Fatalf("seed version failed: %v", err)
	}
	if _, err := env.versionsSvc.PublishVersionCtx(context.Background(), env.orgID, env.agentID, version.Version, "user-1"); err != nil {
		t.Fatalf("seed publish failed: %v", err)
	}

	rr, body := env.do(t, http.MethodPost, "/deployments/create", env.ownerToken,
		`{"agent_id":"`+env.agentID+`","version":`+strconv.Itoa(version.Version)+`,"environment":"staging"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create deployment: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	dep, ok := body["deployment"].(map[string]any)
	if !ok {
		t.Fatalf("create deployment: expected deployment object, got %v", body)
	}
	if dep["status"] != "requested" || dep["agent_id"] != env.agentID || dep["environment"] != "staging" {
		t.Fatalf("created deployment should be requested in staging, got %v", dep)
	}
	if _, ok := dep["health"].(map[string]any); !ok {
		t.Fatalf("deployment must carry a health object, got %v", dep)
	}
	id, _ := dep["id"].(string)
	if strings.TrimSpace(id) == "" {
		t.Fatalf("deployment id missing: %v", dep)
	}

	// Promote walks requested -> validated -> deploying -> healthy, one step at a time.
	for _, want := range []string{"validated", "deploying", "healthy"} {
		rr, body = env.do(t, http.MethodPost, "/deployments/"+id+"/promote", env.ownerToken, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("promote to %s: expected %d, got %d body=%s", want, http.StatusOK, rr.Code, rr.Body.String())
		}
		dep, _ = body["deployment"].(map[string]any)
		if dep["status"] != want {
			t.Fatalf("promote: expected status %q, got %v", want, dep["status"])
		}
	}
	if health, _ := dep["health"].(map[string]any); health["last_check_at"] == nil {
		t.Fatalf("healthy deployment should stamp health.last_check_at, got %v", dep["health"])
	}

	// Terminal statuses cannot promote (409 INVALID_STATE).
	rr, body = env.do(t, http.MethodPost, "/deployments/"+id+"/promote", env.ownerToken, "")
	if rr.Code != http.StatusConflict || errCode(t, body) != "INVALID_STATE" {
		t.Fatalf("promote healthy: expected 409 INVALID_STATE, got %d %v", rr.Code, body)
	}

	// GET single returns the same deployment.
	rr, body = env.do(t, http.MethodGet, "/deployments/"+id, env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get deployment: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	dep, _ = body["deployment"].(map[string]any)
	if dep["status"] != "healthy" {
		t.Fatalf("get deployment: expected healthy, got %v", dep)
	}

	// agent_id filter on the list endpoint.
	rr, body = env.do(t, http.MethodGet, "/deployments?agent_id="+env.agentID, env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list filtered: expected %d, got %d", http.StatusOK, rr.Code)
	}
	raw, _ := json.Marshal(body["deployments"])
	var views []map[string]any
	if err := json.Unmarshal(raw, &views); err != nil || len(views) != 1 {
		t.Fatalf("expected 1 filtered deployment, got %s (%v)", raw, err)
	}
	rr, body = env.do(t, http.MethodGet, "/deployments?agent_id=agent-unknown", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list unknown agent filter: expected %d, got %d", http.StatusOK, rr.Code)
	}
	raw, _ = json.Marshal(body["deployments"])
	views = nil
	if err := json.Unmarshal(raw, &views); err != nil || len(views) != 0 {
		t.Fatalf("expected empty list for unknown agent filter, got %s (%v)", raw, err)
	}
}

func TestDeploymentsHandlerRollback(t *testing.T) {
	env := newVersionsHandlerEnv(t)

	// v2 published; deployment A promoted to healthy in production.
	v2, err := env.versionsSvc.CreateVersionCtx(context.Background(), env.orgID, env.agentID, "user-1")
	if err != nil {
		t.Fatalf("seed v2 failed: %v", err)
	}
	if _, err := env.versionsSvc.PublishVersionCtx(context.Background(), env.orgID, env.agentID, v2.Version, "user-1"); err != nil {
		t.Fatalf("publish v2 failed: %v", err)
	}
	rr, body := env.do(t, http.MethodPost, "/deployments/create", env.ownerToken,
		`{"agent_id":"`+env.agentID+`","version":2,"environment":"production"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create deployment A: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	depA, _ := body["deployment"].(map[string]any)
	idA, _ := depA["id"].(string)
	for i := 0; i < 3; i++ {
		if rr, body = env.do(t, http.MethodPost, "/deployments/"+idA+"/promote", env.ownerToken, ""); rr.Code != http.StatusOK {
			t.Fatalf("promote A step %d: got %d body=%s", i+1, rr.Code, body)
		}
	}

	// v3 published; deployment B promoted to healthy (demotes A).
	v3, err := env.versionsSvc.CreateVersionCtx(context.Background(), env.orgID, env.agentID, "user-1")
	if err != nil {
		t.Fatalf("seed v3 failed: %v", err)
	}
	if _, err := env.versionsSvc.PublishVersionCtx(context.Background(), env.orgID, env.agentID, v3.Version, "user-1"); err != nil {
		t.Fatalf("publish v3 failed: %v", err)
	}
	rr, body = env.do(t, http.MethodPost, "/deployments/create", env.ownerToken,
		`{"agent_id":"`+env.agentID+`","version":3,"environment":"production"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create deployment B: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	depB, _ := body["deployment"].(map[string]any)
	idB, _ := depB["id"].(string)
	for i := 0; i < 3; i++ {
		if rr, body = env.do(t, http.MethodPost, "/deployments/"+idB+"/promote", env.ownerToken, ""); rr.Code != http.StatusOK {
			t.Fatalf("promote B step %d: got %d body=%s", i+1, rr.Code, body)
		}
	}

	// Rollback B: environment re-points to the previous healthy version (2).
	rr, body = env.do(t, http.MethodPost, "/deployments/"+idB+"/rollback", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("rollback B: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if v, _ := body["rolled_back_to_version"].(float64); v != 2 {
		t.Fatalf("rollback should re-point to version 2, got %v", body)
	}
	rollback, _ := body["deployment"].(map[string]any)
	if rollback["version"].(float64) != 2 || rollback["status"] != "healthy" || rollback["environment"] != "production" {
		t.Fatalf("rollback should create a healthy deployment of v2, got %v", rollback)
	}

	// Exactly one healthy deployment per agent+environment after the rollback.
	list, _ := env.depSvc.ListDeploymentsCtx(context.Background(), env.orgID, env.agentID)
	healthy := 0
	for _, d := range list {
		if d.Environment == "production" && d.Status == deployments.StatusHealthy {
			healthy++
		}
	}
	if healthy != 1 {
		t.Fatalf("expected exactly 1 healthy production deployment after rollback, got %d", healthy)
	}

	// Rollback a fresh requested deployment in an environment with no previous
	// healthy row -> 409 NO_PREVIOUS_HEALTHY.
	rr, _ = env.do(t, http.MethodPost, "/deployments/create", env.ownerToken,
		`{"agent_id":"`+env.agentID+`","version":3,"environment":"development"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create dev deployment: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	_, rollbackListBody := env.do(t, http.MethodGet, "/deployments?agent_id="+env.agentID, env.ownerToken, "")
	allRaw, _ := json.Marshal(rollbackListBody["deployments"])
	var allViews []map[string]any
	if err := json.Unmarshal(allRaw, &allViews); err != nil {
		t.Fatalf("deployments payload is not an array: %v (%s)", err, allRaw)
	}
	var devID string
	for _, view := range allViews {
		if environment, _ := view["environment"].(string); environment == "development" {
			devID, _ = view["id"].(string)
		}
	}
	if devID == "" {
		t.Fatalf("expected a development deployment in %s", allRaw)
	}
	rr, body = env.do(t, http.MethodPost, "/deployments/"+devID+"/rollback", env.ownerToken, "")
	if rr.Code != http.StatusConflict || errCode(t, body) != "NO_PREVIOUS_HEALTHY" {
		t.Fatalf("rollback without previous healthy: expected 409 NO_PREVIOUS_HEALTHY, got %d %v", rr.Code, body)
	}
}

func TestDeploymentsHandlerCreateValidation(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	// Draft version is not deployable.
	if _, err := env.versionsSvc.CreateVersionCtx(context.Background(), env.orgID, env.agentID, "user-1"); err != nil {
		t.Fatalf("seed draft failed: %v", err)
	}
	cases := []struct {
		name, body, code string
		status           int
	}{
		{"unknown environment", `{"agent_id":"` + env.agentID + `","version":2,"environment":"canary"}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"missing environment", `{"agent_id":"` + env.agentID + `","version":2}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"missing agent_id", `{"version":2,"environment":"production"}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"zero version", `{"agent_id":"` + env.agentID + `","version":0,"environment":"production"}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"draft version", `{"agent_id":"` + env.agentID + `","version":2,"environment":"production"}`, "VERSION_NOT_PUBLISHED", http.StatusUnprocessableEntity},
		{"unknown agent", `{"agent_id":"agent-missing","version":2,"environment":"production"}`, "VERSION_NOT_PUBLISHED", http.StatusUnprocessableEntity},
		{"malformed body", `{"agent_id":`, "INVALID_REQUEST", http.StatusBadRequest},
	}
	for _, tc := range cases {
		rr, body := env.do(t, http.MethodPost, "/deployments/create", env.ownerToken, tc.body)
		if rr.Code != tc.status || errCode(t, body) != tc.code {
			t.Fatalf("%s: expected %d %s, got %d %v", tc.name, tc.status, tc.code, rr.Code, body)
		}
	}

	// Cross-tenant GET on a deployment surfaces as 404.
	rr, body := env.do(t, http.MethodGet, "/deployments/dep-of-other-org", env.otherToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "DEPLOYMENT_NOT_FOUND" {
		t.Fatalf("cross-tenant get: expected 404 DEPLOYMENT_NOT_FOUND, got %d %v", rr.Code, body)
	}

	// Unknown deployment id -> 404 on promote.
	rr, body = env.do(t, http.MethodPost, "/deployments/dep-missing/promote", env.ownerToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "DEPLOYMENT_NOT_FOUND" {
		t.Fatalf("promote unknown id: expected 404 DEPLOYMENT_NOT_FOUND, got %d %v", rr.Code, body)
	}
}
