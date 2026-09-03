package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
	"agentos/internal/secrets"
)

// newSecretsTestRouter builds a mux with only the secrets routes mounted,
// exactly the way main.go mounts them (StripPrefix under /api/v1), plus the
// auth services needed to obtain credentials. The service runs in in-memory
// mode (no AGENTOS_SECRETS_MASTER_KEY required); values are still sealed.
func newSecretsTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *apikeys.Service, *audit.Service, *secrets.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	svc := secrets.NewService()
	auditSvc := audit.NewService()
	apiMux := http.NewServeMux()
	registerSecretsRoutes(apiMux, svc, authSvc, keysSvc, auditSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, keysSvc, auditSvc, svc
}

// newSecretsTestRouterNoAudit is the nil-audit variant: reveal/create/delete
// must keep working when the audit service is not wired (best-effort audit).
func newSecretsTestRouterNoAudit(t *testing.T) (http.Handler, *authpkg.Service, *secrets.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	svc := secrets.NewService()
	apiMux := http.NewServeMux()
	registerSecretsRoutes(apiMux, svc, authSvc, apikeys.NewService(), nil)
	return http.StripPrefix("/api/v1", apiMux), authSvc, svc
}

// secretsTokenFor issues a bearer token for a user in the given org with the
// given role (claims carry the role; RequirePermission falls back to it when
// the user is not registered in memory).
func secretsTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
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

func doSecretsReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func decodeSecretsBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

func createSecretViaHTTP(t *testing.T, h http.Handler, token, org, name, value string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name, "value": value})
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	return doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets", token, string(body))
}

func TestSecretsAuthRequired(t *testing.T) {
	h, _, _, _, _ := newSecretsTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/secrets"},
		{http.MethodPost, "/api/v1/secrets"},
		{http.MethodDelete, "/api/v1/secrets/n"},
		{http.MethodPost, "/api/v1/secrets/n/reveal"},
	} {
		rr := doSecretsReq(t, h, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestSecretsCreateListMetadataOnly(t *testing.T) {
	h, authSvc, _, _, _ := newSecretsTestRouter(t)
	owner := secretsTokenFor(t, authSvc, "owner@sec.test", "org-a", "OWNER")
	const value = "sk-live-do-not-leak"

	rr := createSecretViaHTTP(t, h, owner, "org-a", "OPENAI_API_KEY", value)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeSecretsBody(t, rr)
	secret, ok := body["secret"].(map[string]any)
	if !ok {
		t.Fatalf("expected {\"secret\":{...}} envelope, got %#v", body)
	}
	if secret["name"] != "OPENAI_API_KEY" || secret["created_by"] != "user-owner@sec.test" {
		t.Fatalf("unexpected metadata: %#v", secret)
	}
	if _, hasValue := secret["value"]; hasValue {
		t.Fatal("create response must not echo the value")
	}
	if keyVersion, ok := secret["key_version"].(float64); !ok || keyVersion < 1 {
		t.Fatalf("metadata must carry key_version, got %#v", secret["key_version"])
	}
	if strings.Contains(rr.Body.String(), value) {
		t.Fatal("create response leaks the plaintext value")
	}

	rr = doSecretsReq(t, h, http.MethodGet, "/api/v1/secrets", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body = decodeSecretsBody(t, rr)
	items, ok := body["secrets"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected {\"secrets\":[...]}, got %#v", body)
	}
	item := items[0].(map[string]any)
	if item["name"] != "OPENAI_API_KEY" {
		t.Fatalf("unexpected list item: %#v", item)
	}
	if _, hasValue := item["value"]; hasValue {
		t.Fatal("list items must be metadata only (no value field)")
	}
	if strings.Contains(rr.Body.String(), value) {
		t.Fatal("list response leaks the plaintext value")
	}
}

func TestSecretsRoleMatrix(t *testing.T) {
	h, authSvc, _, _, svc := newSecretsTestRouter(t)
	ctx := context.Background()
	const org = "org-m"

	// Seed per-role delete targets and one reveal target directly in the
	// service so every HTTP call below starts from identical state.
	for _, name := range []string{"del-owner", "del-admin", "del-member", "del-viewer", "revealme"} {
		if _, err := svc.Create(ctx, org, name, "seed-value", "seed"); err != nil {
			t.Fatalf("seed %s failed: %v", name, err)
		}
	}

	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = secretsTokenFor(t, authSvc, strings.ToLower(role)+"@sec.test", org, role)
	}

	// create: agents.write -> OWNER/ADMIN only
	for role, want := range map[string]int{"OWNER": 201, "ADMIN": 201, "MEMBER": 403, "VIEWER": 403} {
		rr := createSecretViaHTTP(t, h, tokens[role], org, "created-by-"+strings.ToLower(role), "v")
		if rr.Code != want {
			t.Errorf("create as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
	}
	// list: runs.execute (MEMBER+) -> everyone but VIEWER
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 200, "MEMBER": 200, "VIEWER": 403} {
		rr := doSecretsReq(t, h, http.MethodGet, "/api/v1/secrets", tokens[role], "")
		if rr.Code != want {
			t.Errorf("list as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
	}
	// delete: agents.write -> OWNER/ADMIN only; denied roles must not delete
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 200, "MEMBER": 403, "VIEWER": 403} {
		name := "del-" + strings.ToLower(role)
		rr := doSecretsReq(t, h, http.MethodDelete, "/api/v1/secrets/"+name, tokens[role], "")
		if rr.Code != want {
			t.Errorf("delete as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
		if want != 200 {
			if _, err := svc.Get(ctx, org, name); err != nil {
				t.Errorf("denied %s delete must not remove %s: %v", role, name, err)
			}
		}
	}
	// reveal: organization.manage -> OWNER only
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 403, "MEMBER": 403, "VIEWER": 403} {
		rr := doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets/revealme/reveal", tokens[role], "")
		if rr.Code != want {
			t.Errorf("reveal as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
	}
}

func TestSecretsRevealValueOnceAndAuditLogged(t *testing.T) {
	h, authSvc, _, auditSvc, _ := newSecretsTestRouter(t)
	owner := secretsTokenFor(t, authSvc, "owner@sec.test", "org-a", "OWNER")
	const value = "sk-reveal-42-once"

	if rr := createSecretViaHTTP(t, h, owner, "org-a", "api_key", value); rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	rr := doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets/api_key/reveal", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("reveal: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeSecretsBody(t, rr)
	secret, ok := body["secret"].(map[string]any)
	if !ok {
		t.Fatalf("expected {\"secret\":{...}} envelope, got %#v", body)
	}
	if secret["value"] != value {
		t.Fatalf("reveal must return the plaintext value once, got %#v", secret["value"])
	}
	if got := strings.Count(rr.Body.String(), value); got != 1 {
		t.Fatalf("value must appear EXACTLY once in the reveal body, got %d: %s", got, rr.Body.String())
	}

	// Audit trail: reveal writes an entry; create does too; neither carries
	// the plaintext.
	entries, err := auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit ListCtx failed: %v", err)
	}
	actions := map[string]*audit.Entry{}
	for _, e := range entries {
		actions[e.Action] = e
	}
	revealed, ok := actions["secret.revealed"]
	if !ok {
		t.Fatalf("reveal must write a secret.revealed audit entry, got %v", actions)
	}
	if revealed.Resource != "secrets/api_key" {
		t.Fatalf("unexpected audit resource %q", revealed.Resource)
	}
	created, ok := actions["secret.created"]
	if !ok {
		t.Fatal("create must write a secret.created audit entry")
	}
	for _, e := range []*audit.Entry{revealed, created} {
		if strings.Contains(serializeEntry(t, e), value) {
			t.Fatalf("audit entry %s leaks the plaintext value", e.Action)
		}
	}
}

func serializeEntry(t *testing.T, e *audit.Entry) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal audit entry: %v", err)
	}
	return string(b)
}

func TestSecretsOrgIsolationOverHTTP(t *testing.T) {
	h, authSvc, _, _, _ := newSecretsTestRouter(t)
	ownerA := secretsTokenFor(t, authSvc, "a@sec.test", "org-a", "OWNER")
	ownerB := secretsTokenFor(t, authSvc, "b@sec.test", "org-b", "OWNER")

	if rr := createSecretViaHTTP(t, h, ownerA, "org-a", "shared", "org-a-value"); rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	// org-b cannot see, reveal, or delete org-a's secret (404, no leak).
	if rr := doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets/shared/reveal", ownerB, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org reveal: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if rr := doSecretsReq(t, h, http.MethodDelete, "/api/v1/secrets/shared", ownerB, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org delete: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr := doSecretsReq(t, h, http.MethodGet, "/api/v1/secrets", ownerB, "")
	body := decodeSecretsBody(t, rr)
	if items, _ := body["secrets"].([]any); len(items) != 0 {
		t.Fatalf("org-b list must be empty, got %#v", items)
	}
	if strings.Contains(rr.Body.String(), "shared") {
		t.Fatal("org-b list must not surface org-a secret names")
	}
	// org-a still resolves its own secret.
	if rr := doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets/shared/reveal", ownerA, ""); rr.Code != http.StatusOK {
		t.Fatalf("org-a reveal after foreign attempts: expected 200, got %d", rr.Code)
	}
}

func TestSecretsValidationErrors(t *testing.T) {
	h, authSvc, _, _, _ := newSecretsTestRouter(t)
	owner := secretsTokenFor(t, authSvc, "owner@sec.test", "org-a", "OWNER")

	// Malformed JSON -> 400 INVALID_REQUEST.
	rr := doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets", owner, `{not-json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if body := decodeSecretsBody(t, rr); body["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST envelope, got %#v", body)
	}

	cases := []struct{ name, body string }{
		{"bad name charset", `{"name":"bad name!","value":"v"}`},
		{"name starting with dot", `{"name":".hidden","value":"v"}`},
		{"missing name", `{"value":"v"}`},
		{"missing value", `{"name":"ok-name"}`},
	}
	for _, tc := range cases {
		rr := doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets", owner, tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: expected 422, got %d body=%s", tc.name, rr.Code, rr.Body.String())
			continue
		}
		if body := decodeSecretsBody(t, rr); body["error"].(map[string]any)["code"] != "VALIDATION_ERROR" {
			t.Errorf("%s: expected VALIDATION_ERROR envelope, got %#v", tc.name, body)
		}
	}

	// Duplicate (org, name) -> 409 SECRET_ALREADY_EXISTS.
	if rr := createSecretViaHTTP(t, h, owner, "org-a", "dup", "v1"); rr.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d", rr.Code)
	}
	rr = createSecretViaHTTP(t, h, owner, "org-a", "dup", "v2")
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if body := decodeSecretsBody(t, rr); body["error"].(map[string]any)["code"] != "SECRET_ALREADY_EXISTS" {
		t.Fatalf("expected SECRET_ALREADY_EXISTS envelope, got %#v", body)
	}
}

func TestSecretsSoftDeleteLifecycle(t *testing.T) {
	h, authSvc, _, _, _ := newSecretsTestRouter(t)
	owner := secretsTokenFor(t, authSvc, "owner@sec.test", "org-a", "OWNER")

	if rr := createSecretViaHTTP(t, h, owner, "org-a", "gone", "v"); rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rr.Code)
	}
	rr := doSecretsReq(t, h, http.MethodDelete, "/api/v1/secrets/gone", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if body := decodeSecretsBody(t, rr); body["deleted"] != true {
		t.Fatalf("expected {\"deleted\":true}, got %#v", body)
	}
	// Tombstoned: reveal/delete/list all behave as not-found/empty.
	if rr := doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets/gone/reveal", owner, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("deleted reveal: expected 404, got %d", rr.Code)
	}
	if rr := doSecretsReq(t, h, http.MethodDelete, "/api/v1/secrets/gone", owner, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("double delete: expected 404, got %d", rr.Code)
	}
	rr = doSecretsReq(t, h, http.MethodGet, "/api/v1/secrets", owner, "")
	if items, _ := decodeSecretsBody(t, rr)["secrets"].([]any); len(items) != 0 {
		t.Fatalf("deleted secret must leave the list, got %#v", items)
	}
	// Unknown names are 404 from the start.
	if rr := doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets/never-existed/reveal", owner, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown reveal: expected 404, got %d", rr.Code)
	}
	if rr := doSecretsReq(t, h, http.MethodDelete, "/api/v1/secrets/never-existed", owner, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown delete: expected 404, got %d", rr.Code)
	}
}

func TestSecretsAPIKeyAuth(t *testing.T) {
	h, _, keysSvc, _, _ := newSecretsTestRouter(t)
	key, err := keysSvc.Create("org-key", "key-user", "ci")
	if err != nil {
		t.Fatalf("api key create failed: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	req.Header.Set("X-API-Key", key.Value)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with X-API-Key, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSecretsWorkWithoutAuditService(t *testing.T) {
	h, authSvc, _ := newSecretsTestRouterNoAudit(t)
	owner := secretsTokenFor(t, authSvc, "owner@sec.test", "org-a", "OWNER")
	const value = "sk-no-audit"

	if rr := createSecretViaHTTP(t, h, owner, "org-a", "n", value); rr.Code != http.StatusCreated {
		t.Fatalf("create with nil audit: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr := doSecretsReq(t, h, http.MethodPost, "/api/v1/secrets/n/reveal", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("reveal with nil audit: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), value) {
		t.Fatal("reveal must still return the value")
	}
}

func TestSecretsMethodNotAllowed(t *testing.T) {
	h, authSvc, _, _, _ := newSecretsTestRouter(t)
	owner := secretsTokenFor(t, authSvc, "owner@sec.test", "org-a", "OWNER")

	// GET on /secrets/{name}: the path only supports DELETE (and the reveal
	// POST subpath), so the mux answers 405 Method Not Allowed.
	if rr := doSecretsReq(t, h, http.MethodGet, "/api/v1/secrets/n", owner, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /secrets/{name}: expected 405, got %d", rr.Code)
	}
	// PUT on the collection is not registered either.
	if rr := doSecretsReq(t, h, http.MethodPut, "/api/v1/secrets", owner, `{"name":"n","value":"v"}`); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /secrets: expected 405, got %d", rr.Code)
	}
}
