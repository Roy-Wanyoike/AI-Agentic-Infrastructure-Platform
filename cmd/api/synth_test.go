package main

// synth_test.go — issue #147 HTTP surface, exercised through the REAL
// middleware chain (RequireAuthOrAPIKey -> RequirePermission), mirroring
// cmd/api/residency_test.go. Covers: 401 without credentials, the
// synth.read/synth.write RBAC matrix, the handler-level OWNER/ADMIN gate on
// PRODUCTION test enablement, the challenge/confirm verification flow with
// the one-time token rendered exactly once, cross-org 404 isolation, the
// ScopeGuard seam (service.AssertExecutionAllowed) reached through HTTP
// resources, persona validation, and one full-app E2E smoke proving the
// main.go registration line serves /synth/* under /api/v1.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	authpkg "agentos/internal/auth"
	"agentos/internal/synth"
)

func newSynthTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *synth.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	svc := synth.NewService()
	apiMux := http.NewServeMux()
	registerSynthRoutes(apiMux, svc, authSvc, keysSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, svc
}

func synthTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&authpkg.User{ID: "user-" + email, Organization: orgID, Email: email, Role: role})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

func synthReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func synthBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, rr.Body.String())
	}
	return out
}

func synthErrorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	body := synthBody(t, rr)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope: %s", rr.Body.String())
	}
	code, _ := errObj["code"].(string)
	return code
}

// seedSynthProject creates a project through the API and returns its id.
func seedSynthProject(t *testing.T, h http.Handler, token string) string {
	t.Helper()
	rr := synthReq(t, h, http.MethodPost, "/api/v1/synth/projects", token, `{"name":"checkout"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create project = %d, want 201 — body=%s", rr.Code, rr.Body.String())
	}
	project := synthBody(t, rr)["project"].(map[string]any)
	id, _ := project["id"].(string)
	if id == "" {
		t.Fatalf("missing project id: %s", rr.Body.String())
	}
	return id
}

// seedSynthTarget creates a target through the API and returns its id.
func seedSynthTarget(t *testing.T, h http.Handler, token, projectID, baseURL string) string {
	t.Helper()
	payload := fmt.Sprintf(`{"project_id":%q,"name":"app","base_url":%q}`, projectID, baseURL)
	rr := synthReq(t, h, http.MethodPost, "/api/v1/synth/targets", token, payload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create target = %d, want 201 — body=%s", rr.Code, rr.Body.String())
	}
	target := synthBody(t, rr)["target"].(map[string]any)
	id, _ := target["id"].(string)
	if id == "" {
		t.Fatalf("missing target id: %s", rr.Body.String())
	}
	if verified, _ := target["verified"].(bool); verified {
		t.Fatalf("fresh target must be unverified")
	}
	return id
}

// --- 401 + RBAC matrix --------------------------------------------------------

func TestSynthAuthRequired(t *testing.T) {
	h, _, _ := newSynthTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/synth/projects"},
		{http.MethodGet, "/api/v1/synth/projects"},
		{http.MethodPost, "/api/v1/synth/targets"},
		{http.MethodPost, "/api/v1/synth/targets/x/verify"},
		{http.MethodPost, "/api/v1/synth/targets/x/environments"},
		{http.MethodGet, "/api/v1/synth/environments/x"},
		{http.MethodPut, "/api/v1/synth/environments/x"},
		{http.MethodDelete, "/api/v1/synth/environments/x"},
		{http.MethodPost, "/api/v1/synth/personas"},
	} {
		if rr := synthReq(t, h, tc.method, tc.path, "", "{}"); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d", tc.method, tc.path, rr.Code)
		}
	}
}

func TestSynthRBACMatrix(t *testing.T) {
	h, authSvc, _ := newSynthTestRouter(t)
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = synthTokenFor(t, authSvc, strings.ToLower(role)+"@x.io", "org-a", role)
	}

	// synth.read: EVERY role (VIEWER included — the posture is dashboards').
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		for _, path := range []string{"/api/v1/synth/projects", "/api/v1/synth/targets", "/api/v1/synth/personas"} {
			if rr := synthReq(t, h, http.MethodGet, path, tokens[role], ""); rr.Code != http.StatusOK {
				t.Errorf("GET %s as %s: want 200, got %d", path, role, rr.Code)
			}
		}
	}

	// synth.write: OWNER/ADMIN/MEMBER.
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER"} {
		rr := synthReq(t, h, http.MethodPost, "/api/v1/synth/projects", tokens[role], `{"name":"p-`+role+`"}`)
		if rr.Code != http.StatusCreated {
			t.Errorf("POST projects as %s: want 201, got %d", role, rr.Code)
		}
	}
	// VIEWER never writes (collection POST + id-addressed PUT/DELETE).
	if rr := synthReq(t, h, http.MethodPost, "/api/v1/synth/projects", tokens["VIEWER"], `{"name":"x"}`); rr.Code != http.StatusForbidden {
		t.Errorf("POST projects as VIEWER: want 403, got %d", rr.Code)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if rr := synthReq(t, h, method, "/api/v1/synth/projects/p1", tokens["VIEWER"], `{"name":"x"}`); rr.Code != http.StatusForbidden {
			t.Errorf("%s projects as VIEWER: want 403, got %d", method, rr.Code)
		}
	}
}

// --- projects ------------------------------------------------------------------

func TestSynthProjectLifecycleHTTP(t *testing.T) {
	h, authSvc, _ := newSynthTestRouter(t)
	token := synthTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	// Validation: missing name -> 400 VALIDATION_ERROR.
	rr := synthReq(t, h, http.MethodPost, "/api/v1/synth/projects", token, `{"name":"  "}`)
	if rr.Code != http.StatusBadRequest || synthErrorCode(t, rr) != "VALIDATION_ERROR" {
		t.Fatalf("empty name: %d %s", rr.Code, rr.Body.String())
	}

	id := seedSynthProject(t, h, token)

	// GET reflects the stored project.
	rr = synthReq(t, h, http.MethodGet, "/api/v1/synth/projects/"+id, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d", rr.Code)
	}
	project := synthBody(t, rr)["project"].(map[string]any)
	if project["name"] != "checkout" || project["org_id"] != "org-a" {
		t.Fatalf("project wrong: %v", project)
	}

	// PUT renames.
	rr = synthReq(t, h, http.MethodPut, "/api/v1/synth/projects/"+id, token, `{"name":"checkout-v2"}`)
	if rr.Code != http.StatusOK || synthBody(t, rr)["project"].(map[string]any)["name"] != "checkout-v2" {
		t.Fatalf("rename: %d %s", rr.Code, rr.Body.String())
	}

	// DELETE then GET -> 404.
	if rr = synthReq(t, h, http.MethodDelete, "/api/v1/synth/projects/"+id, token, ""); rr.Code != http.StatusOK {
		t.Fatalf("delete: %d", rr.Code)
	}
	rr = synthReq(t, h, http.MethodGet, "/api/v1/synth/projects/"+id, token, "")
	if rr.Code != http.StatusNotFound || synthErrorCode(t, rr) != "NOT_FOUND" {
		t.Fatalf("get after delete: %d %s", rr.Code, rr.Body.String())
	}

	// Unknown id -> 404 (same shape as cross-org).
	rr = synthReq(t, h, http.MethodGet, "/api/v1/synth/projects/does-not-exist", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", rr.Code)
	}
}

// --- targets + verification flow --------------------------------------------------

func TestSynthTargetVerificationFlowHTTP(t *testing.T) {
	h, authSvc, _ := newSynthTestRouter(t)
	token := synthTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")
	projectID := seedSynthProject(t, h, token)

	// Invalid base_url -> 400.
	rr := synthReq(t, h, http.MethodPost, "/api/v1/synth/targets", token,
		fmt.Sprintf(`{"project_id":%q,"name":"app","base_url":"javascript:alert(1)"}`, projectID))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad base_url: %d %s", rr.Code, rr.Body.String())
	}

	targetID := seedSynthTarget(t, h, token, projectID, "https://app.example.com")

	// Challenge issue: the token appears EXACTLY ONCE, on this response.
	rr = synthReq(t, h, http.MethodPost, "/api/v1/synth/targets/"+targetID+"/verify", token, `{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("challenge issue: %d %s", rr.Code, rr.Body.String())
	}
	body := synthBody(t, rr)
	if body["status"] != "challenge_issued" {
		t.Fatalf("status = %v, want challenge_issued", body["status"])
	}
	verification := body["verification"].(map[string]any)
	challenge, _ := verification["token"].(string)
	if challenge == "" {
		t.Fatalf("challenge token missing: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"verify_token"`) {
		t.Fatalf("verify_token leaked inside the target object")
	}

	// Subsequent GET renders the target WITHOUT any token material.
	rr = synthReq(t, h, http.MethodGet, "/api/v1/synth/targets/"+targetID, token, "")
	if strings.Contains(rr.Body.String(), "verify_token") || strings.Contains(rr.Body.String(), challenge) {
		t.Fatalf("challenge material leaked via GET: %s", rr.Body.String())
	}

	// Wrong token confirm -> 403 VERIFICATION_FAILED, still unverified.
	rr = synthReq(t, h, http.MethodPost, "/api/v1/synth/targets/"+targetID+"/verify", token, `{"token":"nope"}`)
	if rr.Code != http.StatusForbidden || synthErrorCode(t, rr) != "VERIFICATION_FAILED" {
		t.Fatalf("wrong token: %d %s", rr.Code, rr.Body.String())
	}

	// Correct token confirm -> verified.
	rr = synthReq(t, h, http.MethodPost, "/api/v1/synth/targets/"+targetID+"/verify", token,
		fmt.Sprintf(`{"token":%q}`, challenge))
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	body = synthBody(t, rr)
	if body["status"] != "verified" || body["target"].(map[string]any)["verified"] != true {
		t.Fatalf("confirm body wrong: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), challenge) {
		t.Fatalf("one-time token echoed after consumption")
	}

	// Idempotent confirm after verification (any token) stays verified.
	rr = synthReq(t, h, http.MethodPost, "/api/v1/synth/targets/"+targetID+"/verify", token, `{"token":"whatever"}`)
	if rr.Code != http.StatusOK || synthBody(t, rr)["status"] != "verified" {
		t.Fatalf("idempotent confirm: %d %s", rr.Code, rr.Body.String())
	}

	// Re-issue on a verified target is a no-op that never un-verifies and
	// never mints a fresh token.
	rr = synthReq(t, h, http.MethodPost, "/api/v1/synth/targets/"+targetID+"/verify", token, `{"action":"issue"}`)
	if rr.Code != http.StatusOK || synthBody(t, rr)["status"] != "verified" {
		t.Fatalf("re-issue on verified: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"token"`) {
		t.Fatalf("fresh token minted for a verified target: %s", rr.Body.String())
	}
}

// --- environments + production enablement gate -------------------------------------

func TestSynthProductionEnablementGate(t *testing.T) {
	h, authSvc, _ := newSynthTestRouter(t)
	owner := synthTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")
	admin := synthTokenFor(t, authSvc, "admin@x.io", "org-a", "ADMIN")
	member := synthTokenFor(t, authSvc, "member@x.io", "org-a", "MEMBER")
	projectID := seedSynthProject(t, h, owner)
	targetID := seedSynthTarget(t, h, owner, projectID, "https://app.example.com")

	envPayload := `{"kind":"production","allowed_domains":["app.example.com"]}`
	envPath := "/api/v1/synth/targets/" + targetID + "/environments"

	// MEMBER holds synth.write but CANNOT enable production tests.
	if rr := synthReq(t, h, http.MethodPost, envPath, member, envPayload); rr.Code != http.StatusForbidden || synthErrorCode(t, rr) != "FORBIDDEN" {
		t.Fatalf("member production enablement: %d %s", rr.Code, rr.Body.String())
	}

	// OWNER/ADMIN can — production defaults to production_read_only.
	for _, token := range []string{owner, admin} {
		rr := synthReq(t, h, http.MethodPost, envPath, token, envPayload)
		if rr.Code != http.StatusCreated {
			t.Fatalf("production create: %d %s", rr.Code, rr.Body.String())
		}
		env := synthBody(t, rr)["environment"].(map[string]any)
		if env["mode"] != "production_read_only" || env["kind"] != "production" {
			t.Fatalf("production env wrong: %v", env)
		}
	}

	// Validation still applies for OWNER: production WITHOUT an allowlist
	// is a 400, rate caps on non-pre_production are a 400.
	if rr := synthReq(t, h, http.MethodPost, envPath, owner, `{"kind":"production"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("production without allowlist: %d %s", rr.Code, rr.Body.String())
	}
	if rr := synthReq(t, h, http.MethodPost, envPath, owner, `{"kind":"production","mode":"pre_production","allowed_domains":["app.example.com"]}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("production pre_production: %d %s", rr.Code, rr.Body.String())
	}

	// MEMBER CAN build non-production scope freely (dev defaults to
	// pre_production).
	rr := synthReq(t, h, http.MethodPost, envPath, member, `{"kind":"dev","rate_cap_per_min":60}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("member dev env: %d %s", rr.Code, rr.Body.String())
	}
	if env := synthBody(t, rr)["environment"].(map[string]any); env["mode"] != "pre_production" {
		t.Fatalf("dev env mode = %v, want pre_production", env["mode"])
	}

	// PUT /synth/environments/{id} enforces the same gate: a MEMBER cannot
	// escalate an existing env to production.
	list, err := listSynthEnvironmentIDs(t, h, owner, targetID)
	if err != nil {
		t.Fatalf("list envs: %v", err)
	}
	devEnvID := list[len(list)-1] // the member's dev env
	if rr := synthReq(t, h, http.MethodPut, "/api/v1/synth/environments/"+devEnvID, member, envPayload); rr.Code != http.StatusForbidden {
		t.Fatalf("member escalate to production: %d %s", rr.Code, rr.Body.String())
	}
	// ADMIN can.
	if rr := synthReq(t, h, http.MethodPut, "/api/v1/synth/environments/"+devEnvID, admin, envPayload); rr.Code != http.StatusOK {
		t.Fatalf("admin escalate: %d %s", rr.Code, rr.Body.String())
	}
}

// listSynthEnvironmentIDs walks GET /synth/targets/{id}/environments.
func listSynthEnvironmentIDs(t *testing.T, h http.Handler, token, targetID string) ([]string, error) {
	t.Helper()
	rr := synthReq(t, h, http.MethodGet, "/api/v1/synth/targets/"+targetID+"/environments", token, "")
	if rr.Code != http.StatusOK {
		return nil, fmt.Errorf("list envs = %d: %s", rr.Code, rr.Body.String())
	}
	envs, _ := synthBody(t, rr)["environments"].([]any)
	ids := make([]string, 0, len(envs))
	for _, e := range envs {
		if id, ok := e.(map[string]any)["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func TestSynthEnvironmentLifecycleHTTP(t *testing.T) {
	h, authSvc, _ := newSynthTestRouter(t)
	token := synthTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")
	projectID := seedSynthProject(t, h, token)
	targetID := seedSynthTarget(t, h, token, projectID, "https://app.example.com")
	envPath := "/api/v1/synth/targets/" + targetID + "/environments"

	// Unknown target anchor -> 404 (cross-org convention).
	if rr := synthReq(t, h, http.MethodPost, "/api/v1/synth/targets/nope/environments", token, `{"kind":"dev"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown target: %d", rr.Code)
	}

	rr := synthReq(t, h, http.MethodPost, envPath, token,
		`{"kind":"staging","allowed_domains":["app.example.com","cdn.example.com"],"budget_per_run":42,
                  "quiet_windows":[{"weekdays":[0,6],"start":"00:00","end":"23:59","location":"UTC","reason":"weekend"}]}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create env: %d %s", rr.Code, rr.Body.String())
	}
	env := synthBody(t, rr)["environment"].(map[string]any)
	if env["mode"] != "pre_production" || env["budget_per_run"] != float64(42) {
		t.Fatalf("env wrong: %v", env)
	}
	if domains, _ := env["allowed_domains"].([]any); len(domains) != 2 || domains[1] != "cdn.example.com" {
		t.Fatalf("domains wrong: %v", env["allowed_domains"])
	}
	if quiet, _ := env["quiet_windows"].([]any); len(quiet) != 1 {
		t.Fatalf("quiet windows wrong: %v", env["quiet_windows"])
	}
	envID, _ := env["id"].(string)

	// Bad window -> 400.
	if rr = synthReq(t, h, http.MethodPut, "/api/v1/synth/environments/"+envID, token,
		`{"kind":"staging","quiet_windows":[{"weekdays":[9],"start":"00:00","end":"23:59","location":"UTC"}]}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad window: %d %s", rr.Code, rr.Body.String())
	}

	// GET + DELETE.
	if rr = synthReq(t, h, http.MethodGet, "/api/v1/synth/environments/"+envID, token, ""); rr.Code != http.StatusOK {
		t.Fatalf("get env: %d", rr.Code)
	}
	if rr = synthReq(t, h, http.MethodDelete, "/api/v1/synth/environments/"+envID, token, ""); rr.Code != http.StatusOK {
		t.Fatalf("delete env: %d", rr.Code)
	}
	if rr = synthReq(t, h, http.MethodGet, "/api/v1/synth/environments/"+envID, token, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", rr.Code)
	}
}

// --- personas -------------------------------------------------------------------

func TestSynthPersonaLifecycleHTTP(t *testing.T) {
	h, authSvc, _ := newSynthTestRouter(t)
	token := synthTokenFor(t, authSvc, "member@x.io", "org-a", "MEMBER")

	// Unknown built-in key -> 400.
	rr := synthReq(t, h, http.MethodPost, "/api/v1/synth/personas", token, `{"name":"Ghost","built_in":"not_a_builtin"}`)
	if rr.Code != http.StatusBadRequest || synthErrorCode(t, rr) != "VALIDATION_ERROR" {
		t.Fatalf("invalid built-in: %d %s", rr.Code, rr.Body.String())
	}

	rr = synthReq(t, h, http.MethodPost, "/api/v1/synth/personas", token,
		`{"name":"Mobile Shopper","built_in":"mobile_user","role_hints":["shopper"],"constraints":{"network":"slow3g"}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create persona: %d %s", rr.Code, rr.Body.String())
	}
	persona := synthBody(t, rr)["persona"].(map[string]any)
	if persona["version"] != float64(1) || persona["built_in"] != "mobile_user" {
		t.Fatalf("persona wrong: %v", persona)
	}
	id, _ := persona["id"].(string)

	// Update bumps the version.
	rr = synthReq(t, h, http.MethodPut, "/api/v1/synth/personas/"+id, token, `{"name":"Mobile Shopper v2"}`)
	if rr.Code != http.StatusOK || synthBody(t, rr)["persona"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("update persona: %d %s", rr.Code, rr.Body.String())
	}

	if rr = synthReq(t, h, http.MethodGet, "/api/v1/synth/personas", token, ""); rr.Code != http.StatusOK {
		t.Fatalf("list personas: %d", rr.Code)
	}
	if rr = synthReq(t, h, http.MethodDelete, "/api/v1/synth/personas/"+id, token, ""); rr.Code != http.StatusOK {
		t.Fatalf("delete persona: %d", rr.Code)
	}
}

// --- cross-org isolation ---------------------------------------------------------

func TestSynthCrossOrgIsolation(t *testing.T) {
	h, authSvc, _ := newSynthTestRouter(t)
	ownerA := synthTokenFor(t, authSvc, "a@x.io", "org-a", "OWNER")
	ownerB := synthTokenFor(t, authSvc, "b@x.io", "org-b", "OWNER")
	projectID := seedSynthProject(t, h, ownerA)
	targetID := seedSynthTarget(t, h, ownerA, projectID, "https://app.example.com")

	// Every foreign-org addressing is the SAME 404 as an unknown id.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/synth/projects/" + projectID},
		{http.MethodPut, "/api/v1/synth/projects/" + projectID},
		{http.MethodDelete, "/api/v1/synth/projects/" + projectID},
		{http.MethodGet, "/api/v1/synth/targets/" + targetID},
		{http.MethodPost, "/api/v1/synth/targets/" + targetID + "/verify"},
		{http.MethodPost, "/api/v1/synth/targets/" + targetID + "/environments"},
		{http.MethodDelete, "/api/v1/synth/targets/" + targetID},
	} {
		rr := synthReq(t, h, tc.method, tc.path, ownerB, `{"name":"x","kind":"dev"}`)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s %s as org-b: want 404, got %d", tc.method, tc.path, rr.Code)
		}
	}

	// org-b's lists never contain org-a records.
	rr := synthReq(t, h, http.MethodGet, "/api/v1/synth/projects", ownerB, "")
	if projects, _ := synthBody(t, rr)["projects"].([]any); len(projects) != 0 {
		t.Fatalf("org-b sees %d foreign projects", len(projects))
	}
}

// --- ScopeGuard seam (the worker tracks' contract, reached via HTTP) ---------------

func TestSynthScopeGuardSeam(t *testing.T) {
	h, authSvc, svc := newSynthTestRouter(t)
	token := synthTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")
	ctx := t.Context()
	projectID := seedSynthProject(t, h, token)
	targetID := seedSynthTarget(t, h, token, projectID, "https://app.example.com")

	// Env created through the HTTP surface while the target is UNVERIFIED.
	rr := synthReq(t, h, http.MethodPost, "/api/v1/synth/targets/"+targetID+"/environments", token,
		`{"kind":"production","allowed_domains":["app.example.com"]}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("env: %d %s", rr.Code, rr.Body.String())
	}
	envID, _ := synthBody(t, rr)["environment"].(map[string]any)["id"].(string)

	// Unverified target denies ALL (reads and writes alike) — the typed
	// error comes from types.go.
	d, err := svc.AssertExecutionAllowed(ctx, "org-a", envID, "https://app.example.com/prices", false)
	if !errors.Is(err, synth.ErrTargetUnverified) || d.Allowed {
		t.Fatalf("unverified read verdict=%+v err=%v", d, err)
	}
	if _, err := svc.AssertExecutionAllowed(ctx, "org-a", envID, "https://app.example.com/prices", true); !errors.Is(err, synth.ErrTargetUnverified) {
		t.Fatalf("unverified write err=%v", err)
	}

	// Verify through the challenge/confirm HTTP flow, then the guard opens.
	rr = synthReq(t, h, http.MethodPost, "/api/v1/synth/targets/"+targetID+"/verify", token, `{}`)
	challenge := synthBody(t, rr)["verification"].(map[string]any)["token"].(string)
	rr = synthReq(t, h, http.MethodPost, "/api/v1/synth/targets/"+targetID+"/verify", token,
		fmt.Sprintf(`{"token":%q}`, challenge))
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}

	// production_read_only: reads allowed (allowlisted), writes denied with
	// the typed ErrReadOnlyMode.
	if _, err := svc.AssertExecutionAllowed(ctx, "org-a", envID, "https://app.example.com/prices", true); !errors.Is(err, synth.ErrReadOnlyMode) {
		t.Fatalf("ro write err=%v", err)
	}
	if d, err := svc.AssertExecutionAllowed(ctx, "org-a", envID, "https://app.example.com/prices", false); err != nil || !d.Allowed {
		t.Fatalf("ro read verdict=%+v err=%v", d, err)
	}

	// Outside the allowlist -> typed scope violation.
	if _, err := svc.AssertExecutionAllowed(ctx, "org-a", envID, "https://evil.example.net/", false); !errors.Is(err, synth.ErrScopeViolation) {
		t.Fatalf("scope violation err=%v", err)
	}

	// Cross-org env -> 404 semantics.
	if _, err := svc.AssertExecutionAllowed(ctx, "org-b", envID, "https://app.example.com/", false); !errors.Is(err, synth.ErrNotFound) {
		t.Fatalf("cross-org err=%v", err)
	}
}

// --- main.go wiring (full-app E2E smoke through newApp(nil db)) ----------------------

func TestSynthWiringE2E(t *testing.T) {
	env := newE2EEnv(t)
	env.registerAndLogin(t)

	// The register line in main.go exposes /synth/* under the real mux with
	// the real auth middleware.
	rr, resp := env.do(t, http.MethodPost, "/api/v1/synth/projects", `{"name":"e2e-checkout"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("e2e create project = %d — body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := resp["project"].(map[string]any); !ok {
		t.Fatalf("e2e response missing project: %s", rr.Body.String())
	}
	// Empty registry renders as an empty list (never null).
	if rr, _ := env.do(t, http.MethodGet, "/api/v1/synth/personas", ""); rr.Code != http.StatusOK {
		t.Fatalf("e2e list personas = %d", rr.Code)
	}
}
