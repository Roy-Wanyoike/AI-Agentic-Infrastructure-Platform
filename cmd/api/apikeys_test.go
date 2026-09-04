package main

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
	"agentos/internal/secrets"
)

// newAPIKeysTestRouter builds a mux with only the api-keys routes mounted,
// exactly the way main.go mounts them (StripPrefix under /api/v1), plus the
// secrets routes as an EXISTING authenticated endpoint so the tests can prove
// a minted key actually authenticates. The apikeys service runs in in-memory
// mode (no database).
func newAPIKeysTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *apikeys.Service, *audit.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	auditSvc := audit.NewService()
	apiMux := http.NewServeMux()
	registerAPIKeysRoutes(apiMux, keysSvc, authSvc, keysSvc, auditSvc)
	registerSecretsRoutes(apiMux, secrets.NewService(), authSvc, keysSvc, auditSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, keysSvc, auditSvc
}

// newAPIKeysTestRouterNoAudit is the nil-audit variant: mint/revoke must keep
// working when the audit service is not wired (best-effort audit).
func newAPIKeysTestRouterNoAudit(t *testing.T) (http.Handler, *authpkg.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	apiMux := http.NewServeMux()
	registerAPIKeysRoutes(apiMux, keysSvc, authSvc, keysSvc, nil)
	return http.StripPrefix("/api/v1", apiMux), authSvc
}

// apiKeysTokenFor issues a bearer token for a user in the given org with the
// given role (claims carry the role; RequirePermission falls back to it when
// the user is not registered in memory).
func apiKeysTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
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

func doAPIKeysReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func doAPIKeysKeyReq(t *testing.T, h http.Handler, method, path, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-API-Key", apiKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeAPIKeysBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

// mintKeyViaHTTP creates a key through the HTTP surface and returns the id
// plus the one-time plaintext value.
func mintKeyViaHTTP(t *testing.T, h http.Handler, token, name string) (string, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatalf("marshal mint body: %v", err)
	}
	rr := doAPIKeysReq(t, h, http.MethodPost, "/api/v1/api-keys", token, string(body))
	if rr.Code != http.StatusCreated {
		t.Fatalf("mint: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	out := decodeAPIKeysBody(t, rr)
	value, _ := out["value"].(string)
	if !strings.HasPrefix(value, "ak_") {
		t.Fatalf("minted value must start with ak_, got %q", value)
	}
	meta, _ := out["api_key"].(map[string]any)
	id, _ := meta["id"].(string)
	if id == "" {
		t.Fatalf("mint response missing api_key.id: %s", rr.Body.String())
	}
	return id, value
}

func TestAPIKeysAuthRequired(t *testing.T) {
	h, _, _, _ := newAPIKeysTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/api-keys"},
		{http.MethodPost, "/api/v1/api-keys"},
		{http.MethodDelete, "/api/v1/api-keys/some-id"},
	} {
		rr := doAPIKeysReq(t, h, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestAPIKeysCreateReturnsValueExactlyOnce(t *testing.T) {
	h, authSvc, _, _ := newAPIKeysTestRouter(t)
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	rr := doAPIKeysReq(t, h, http.MethodPost, "/api/v1/api-keys", owner, `{"name":"ci-deploy"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("mint: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeAPIKeysBody(t, rr)
	value, _ := body["value"].(string)
	if !strings.HasPrefix(value, "ak_") || len(value) != len("ak_")+64 {
		t.Fatalf("minted value must be ak_ + 64 hex chars, got %q", value)
	}
	if got := strings.Count(rr.Body.String(), value); got != 1 {
		t.Fatalf("plaintext value must appear EXACTLY once in the mint body, got %d: %s", got, rr.Body.String())
	}
	meta, ok := body["api_key"].(map[string]any)
	if !ok {
		t.Fatalf("expected {\"api_key\":{...}} envelope, got %#v", body)
	}
	if meta["name"] != "ci-deploy" || meta["created_by"] != "user-owner@keys.test" {
		t.Fatalf("unexpected metadata: %#v", meta)
	}
	if meta["revoked"] != false {
		t.Fatalf("fresh key must not be revoked, got %#v", meta["revoked"])
	}
	if p, _ := meta["prefix"].(string); !strings.HasPrefix(p, "ak_") || p == value {
		t.Fatalf("prefix must be a display prefix distinct from the value, got %q", p)
	}
	if _, hasHash := meta["hash"]; hasHash {
		t.Fatal("metadata must never carry the key hash")
	}
	if _, hasValue := meta["value"]; hasValue {
		t.Fatal("metadata must never carry the plaintext value")
	}

	// List: metadata only — the plaintext and the hash are structurally absent.
	rr = doAPIKeysReq(t, h, http.MethodGet, "/api/v1/api-keys", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body = decodeAPIKeysBody(t, rr)
	items, ok := body["api_keys"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected {\"api_keys\":[...]}, got %#v", body)
	}
	item := items[0].(map[string]any)
	if item["id"] != meta["id"] || item["name"] != "ci-deploy" {
		t.Fatalf("unexpected list item: %#v", item)
	}
	if _, hasValue := item["value"]; hasValue {
		t.Fatal("list items must be metadata only (no value field)")
	}
	if _, hasHash := item["hash"]; hasHash {
		t.Fatal("list items must be metadata only (no hash field)")
	}
	if strings.Contains(rr.Body.String(), value) {
		t.Fatal("list response leaks the plaintext value")
	}
}

func TestAPIKeysRoleMatrix(t *testing.T) {
	h, authSvc, svc, _ := newAPIKeysTestRouter(t)
	ctx := context.Background()
	const org = "org-m"

	// Seed per-role revoke targets directly in the service so every HTTP call
	// below starts from identical state.
	seeded := map[string]string{}
	for _, name := range []string{"del-owner", "del-admin", "del-member", "del-viewer"} {
		key, err := svc.CreateCtx(ctx, org, "seed", name)
		if err != nil {
			t.Fatalf("seed %s failed: %v", name, err)
		}
		seeded[name] = key.ID
	}

	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = apiKeysTokenFor(t, authSvc, strings.ToLower(role)+"@keys.test", org, role)
	}

	// mint: agents.write -> OWNER/ADMIN only; MEMBER cannot create.
	for role, want := range map[string]int{"OWNER": 201, "ADMIN": 201, "MEMBER": 403, "VIEWER": 403} {
		rr := doAPIKeysReq(t, h, http.MethodPost, "/api/v1/api-keys", tokens[role],
			`{"name":"created-by-`+strings.ToLower(role)+`"}`)
		if rr.Code != want {
			t.Errorf("mint as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
		if want == http.StatusForbidden && strings.Contains(rr.Body.String(), "ak_") {
			t.Errorf("denied %s mint must not leak any key material", role)
		}
	}
	// list: runs.execute (MEMBER+) -> everyone but VIEWER
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 200, "MEMBER": 200, "VIEWER": 403} {
		rr := doAPIKeysReq(t, h, http.MethodGet, "/api/v1/api-keys", tokens[role], "")
		if rr.Code != want {
			t.Errorf("list as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
	}
	// revoke: agents.write -> OWNER/ADMIN only; denied roles must not revoke.
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 200, "MEMBER": 403, "VIEWER": 403} {
		id := seeded["del-"+strings.ToLower(role)]
		rr := doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/"+id, tokens[role], "")
		if rr.Code != want {
			t.Errorf("revoke as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
		if want != 200 {
			list, err := svc.ListKeysCtx(ctx, org)
			if err != nil {
				t.Fatalf("list seeded keys: %v", err)
			}
			for _, k := range list {
				if k.ID == id && k.Revoked {
					t.Errorf("denied %s revoke must not revoke %s", role, id)
				}
			}
		}
	}
}

// TestAPIKeysMintedKeyAuthenticatesThenRevoked proves the minted key WORKS
// (against an existing authenticated endpoint), and that revocation cuts it
// off immediately.
func TestAPIKeysMintedKeyAuthenticatesThenRevoked(t *testing.T) {
	h, authSvc, _, _ := newAPIKeysTestRouter(t)
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	id, value := mintKeyViaHTTP(t, h, owner, "lifecycle")

	// The freshly minted key authenticates against an existing authenticated
	// endpoint (secrets list) via header AND query param.
	if rr := doAPIKeysKeyReq(t, h, http.MethodGet, "/api/v1/secrets", value); rr.Code != http.StatusOK {
		t.Fatalf("minted key must authenticate via X-API-Key, got %d body=%s", rr.Code, rr.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/secrets?api_key="+value, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("minted key must authenticate via api_key query param, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Revoke through the new management surface.
	rr = doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/"+id, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if body := decodeAPIKeysBody(t, rr); body["revoked"] != true {
		t.Fatalf("expected {\"revoked\":true}, got %#v", body)
	}

	// The revoked key is cut off immediately (401 on the auth middleware).
	if rr := doAPIKeysKeyReq(t, h, http.MethodGet, "/api/v1/secrets", value); rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key must stop authenticating, got %d body=%s", rr.Code, rr.Body.String())
	}

	// The list still shows the key, flagged revoked, without the plaintext.
	rr = doAPIKeysReq(t, h, http.MethodGet, "/api/v1/api-keys", owner, "")
	body := decodeAPIKeysBody(t, rr)
	items, _ := body["api_keys"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected the revoked key to remain listed, got %#v", body)
	}
	item := items[0].(map[string]any)
	if item["id"] != id || item["revoked"] != true {
		t.Fatalf("revoked key metadata mismatch: %#v", item)
	}
	if strings.Contains(rr.Body.String(), value) {
		t.Fatal("list leaks the revoked key plaintext")
	}
}

func TestAPIKeysRevocationSemantics(t *testing.T) {
	h, authSvc, _, _ := newAPIKeysTestRouter(t)
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	// Unknown id -> 404 with the structured code.
	rr := doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/never-existed", owner, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown revoke: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if body := decodeAPIKeysBody(t, rr); body["error"].(map[string]any)["code"] != "API_KEY_NOT_FOUND" {
		t.Fatalf("expected API_KEY_NOT_FOUND envelope, got %#v", body)
	}

	// Double revoke: idempotent per service semantics (already-revoked -> 200).
	id, _ := mintKeyViaHTTP(t, h, owner, "twice")
	for i := 0; i < 2; i++ {
		rr = doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/"+id, owner, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("revoke #%d: expected 200 (idempotent), got %d body=%s", i+1, rr.Code, rr.Body.String())
		}
	}
}

func TestAPIKeysOrgIsolationOverHTTP(t *testing.T) {
	h, authSvc, _, _ := newAPIKeysTestRouter(t)
	ownerA := apiKeysTokenFor(t, authSvc, "a@keys.test", "org-a", "OWNER")
	ownerB := apiKeysTokenFor(t, authSvc, "b@keys.test", "org-b", "OWNER")

	idA, valueA := mintKeyViaHTTP(t, h, ownerA, "org-a-only")

	// org-b cannot see org-a's keys (empty list, no id/name leak)...
	rr := doAPIKeysReq(t, h, http.MethodGet, "/api/v1/api-keys", ownerB, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("org-b list: expected 200, got %d", rr.Code)
	}
	if items, _ := decodeAPIKeysBody(t, rr)["api_keys"].([]any); len(items) != 0 {
		t.Fatalf("org-b list must be empty, got %#v", items)
	}
	if strings.Contains(rr.Body.String(), idA) || strings.Contains(rr.Body.String(), valueA) {
		t.Fatal("org-b list must not surface org-a key material")
	}
	// ...cannot revoke org-a's key (404, no existence leak)...
	rr = doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/"+idA, ownerB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org revoke: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	// ...and org-a's key keeps authenticating.
	if rr := doAPIKeysKeyReq(t, h, http.MethodGet, "/api/v1/secrets", valueA); rr.Code != http.StatusOK {
		t.Fatalf("foreign revoke attempt must not disable the key, got %d", rr.Code)
	}
	// org-a can still revoke its own key afterwards.
	if rr := doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/"+idA, ownerA, ""); rr.Code != http.StatusOK {
		t.Fatalf("org-a revoke after foreign attempts: expected 200, got %d", rr.Code)
	}
}

func TestAPIKeysValidationErrors(t *testing.T) {
	h, authSvc, _, _ := newAPIKeysTestRouter(t)
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	// Malformed JSON -> 400 INVALID_REQUEST.
	rr := doAPIKeysReq(t, h, http.MethodPost, "/api/v1/api-keys", owner, `{not-json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if body := decodeAPIKeysBody(t, rr); body["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST envelope, got %#v", body)
	}

	// Missing/blank name -> 422 VALIDATION_ERROR (service message mirrored).
	for _, tc := range []struct{ name, body string }{
		{"missing name", `{}`},
		{"empty name", `{"name":""}`},
		{"blank name", `{"name":"   "}`},
	} {
		rr := doAPIKeysReq(t, h, http.MethodPost, "/api/v1/api-keys", owner, tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: expected 422, got %d body=%s", tc.name, rr.Code, rr.Body.String())
			continue
		}
		body := decodeAPIKeysBody(t, rr)
		if body["error"].(map[string]any)["code"] != "VALIDATION_ERROR" {
			t.Errorf("%s: expected VALIDATION_ERROR envelope, got %#v", tc.name, body)
		}
		if body["error"].(map[string]any)["message"] != "key name is required" {
			t.Errorf("%s: unexpected message: %#v", tc.name, body["error"])
		}
	}
}

func TestAPIKeysAuditLogged(t *testing.T) {
	h, authSvc, _, auditSvc := newAPIKeysTestRouter(t)
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	id, value := mintKeyViaHTTP(t, h, owner, "audited")
	if rr := doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/"+id, owner, ""); rr.Code != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d", rr.Code)
	}

	entries, err := auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit ListCtx failed: %v", err)
	}
	actions := map[string]*audit.Entry{}
	for _, e := range entries {
		actions[e.Action] = e
	}
	created, ok := actions["api_key.created"]
	if !ok {
		t.Fatalf("mint must write an api_key.created audit entry, got %v", actions)
	}
	revoked, ok := actions["api_key.revoked"]
	if !ok {
		t.Fatal("revoke must write an api_key.revoked audit entry")
	}
	if created.Resource != "api-keys/"+id || revoked.Resource != "api-keys/"+id {
		t.Fatalf("unexpected audit resources %q / %q", created.Resource, revoked.Resource)
	}
	for _, e := range []*audit.Entry{created, revoked} {
		if strings.Contains(serializeAPIKeyEntry(t, e), value) {
			t.Fatalf("audit entry %s leaks the plaintext value", e.Action)
		}
	}
}

func serializeAPIKeyEntry(t *testing.T, e *audit.Entry) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal audit entry: %v", err)
	}
	return string(b)
}

func TestAPIKeysWorkWithoutAuditService(t *testing.T) {
	h, authSvc := newAPIKeysTestRouterNoAudit(t)
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	id, value := mintKeyViaHTTP(t, h, owner, "no-audit")
	if !strings.HasPrefix(value, "ak_") {
		t.Fatal("mint with nil audit must still return the value")
	}
	if rr := doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/"+id, owner, ""); rr.Code != http.StatusOK {
		t.Fatalf("revoke with nil audit: expected 200, got %d", rr.Code)
	}
}

func TestAPIKeysMethodNotAllowed(t *testing.T) {
	h, authSvc, _, _ := newAPIKeysTestRouter(t)
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	// PUT on the collection is not registered.
	if rr := doAPIKeysReq(t, h, http.MethodPut, "/api/v1/api-keys", owner, `{"name":"n"}`); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api-keys: expected 405, got %d", rr.Code)
	}
	// GET on /api-keys/{id}: the path only supports DELETE (keys are
	// write-only credentials — there is no single-key read).
	if rr := doAPIKeysReq(t, h, http.MethodGet, "/api/v1/api-keys/some-id", owner, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api-keys/{id}: expected 405, got %d", rr.Code)
	}
}

// --- store-mode tests (sqlmock): the durable path mirrors the pg store
// semantics — INSERT binds only the SHA-256 hash, the UPDATE re-checks
// organization_id, and list projections never carry key_hash.

// newAPIKeysStoreTestEnv wires the api-keys routes against a sqlmock-backed
// apikeys service (the store is the source of truth, like the Postgres mode).
func newAPIKeysStoreTestEnv(t *testing.T) (http.Handler, *authpkg.Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewServiceWithStore(apikeys.NewPostgresStore(db))
	apiMux := http.NewServeMux()
	registerAPIKeysRoutes(apiMux, keysSvc, authSvc, keysSvc, nil)
	return http.StripPrefix("/api/v1", apiMux), authSvc, mock, func() { _ = db.Close() }
}

// apikeyCaptureArg records every bound string argument so tests can assert
// WHAT the store persisted (hash-at-rest, never plaintext).
type apikeyCaptureArg struct {
	captured []string
}

func (c *apikeyCaptureArg) Match(v driver.Value) bool {
	if s, ok := v.(string); ok {
		c.captured = append(c.captured, s)
	}
	return true
}

func apiKeySQLRows() []string {
	return []string{"id", "organization_id", "user_id", "name", "prefix", "key_hash", "created_at", "revoked_at", "last_used_at"}
}

func TestAPIKeysStoreModeCreateHashAtRest(t *testing.T) {
	h, authSvc, mock, closeDB := newAPIKeysStoreTestEnv(t)
	defer closeDB()
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	idCap := &apikeyCaptureArg{}
	orgCap := &apikeyCaptureArg{}
	userCap := &apikeyCaptureArg{}
	nameCap := &apikeyCaptureArg{}
	prefixCap := &apikeyCaptureArg{}
	hashCap := &apikeyCaptureArg{}

	// INSERT (id, organization_id, user_id, name, prefix, key_hash, created_at)
	mock.ExpectExec("INSERT INTO api_keys").
		WithArgs(idCap, orgCap, userCap, nameCap, prefixCap, hashCap, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rr := doAPIKeysReq(t, h, http.MethodPost, "/api/v1/api-keys", owner, `{"name":"ci"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("mint: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	value, _ := decodeAPIKeysBody(t, rr)["value"].(string)
	if value == "" {
		t.Fatalf("mint must return the plaintext value, got %s", rr.Body.String())
	}
	sum := sha256.Sum256([]byte(value))
	wantHash := hex.EncodeToString(sum[:])

	for _, c := range []*apikeyCaptureArg{idCap, orgCap, userCap, nameCap, prefixCap, hashCap} {
		if strings.Contains(strings.Join(c.captured, "|"), value) {
			t.Fatal("the store must never bind the plaintext value")
		}
	}
	if len(hashCap.captured) != 1 || hashCap.captured[0] != wantHash {
		t.Fatalf("INSERT must bind the SHA-256 hex of the value, got %v (want %s)", hashCap.captured, wantHash)
	}
	foundOrg, foundName, foundPrefix := false, false, false
	for _, s := range orgCap.captured {
		if s == "org-a" {
			foundOrg = true
		}
	}
	for _, s := range nameCap.captured {
		if s == "ci" {
			foundName = true
		}
	}
	for _, s := range prefixCap.captured {
		if strings.HasPrefix(s, "ak_") {
			foundPrefix = true
		}
	}
	if !foundOrg || !foundName || !foundPrefix {
		t.Fatalf("INSERT must bind org/user/name/prefix (org=%v name=%v prefix=%v)", foundOrg, foundName, foundPrefix)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestAPIKeysStoreModeListNeverLeaksHash(t *testing.T) {
	h, authSvc, mock, closeDB := newAPIKeysStoreTestEnv(t)
	defer closeDB()
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	const storedHash = "cafebabe0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	// SELECT ... FROM api_keys WHERE organization_id = $1 ORDER BY created_at DESC
	mock.ExpectQuery("FROM api_keys WHERE organization_id").
		WithArgs("org-a").
		WillReturnRows(sqlmock.NewRows(apiKeySQLRows()).
			AddRow("key-row-1", "org-a", "user-owner@keys.test", "ci", "ak_1a2b3c4d", storedHash,
				time.Now().UTC().Add(-time.Hour), nil, nil).
			AddRow("key-row-2", "org-a", "user-owner@keys.test", "old", "ak_9e8d7c6b", storedHash,
				time.Now().UTC().Add(2*time.Hour), time.Now().UTC(), nil))

	rr := doAPIKeysReq(t, h, http.MethodGet, "/api/v1/api-keys", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeAPIKeysBody(t, rr)
	items, ok := body["api_keys"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected two metadata rows, got %s", rr.Body.String())
	}
	first := items[0].(map[string]any)
	if first["id"] != "key-row-2" {
		t.Fatalf("list must be newest first, got %#v", first)
	}
	if first["revoked"] != true || items[1].(map[string]any)["revoked"] != false {
		t.Fatalf("revoked flag mismatch: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), storedHash) {
		t.Fatal("list response leaks the stored key hash")
	}
	if _, has := first["key_hash"]; has {
		t.Fatal("list items must not expose key_hash")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestAPIKeysStoreModeRevokeSemantics(t *testing.T) {
	h, authSvc, mock, closeDB := newAPIKeysStoreTestEnv(t)
	defer closeDB()
	owner := apiKeysTokenFor(t, authSvc, "owner@keys.test", "org-a", "OWNER")

	// Revoke #1: ownership pre-check (org-scoped SELECT) then the guarded
	// UPDATE re-checking organization_id.
	mock.ExpectQuery("FROM api_keys WHERE organization_id").
		WithArgs("org-a").
		WillReturnRows(sqlmock.NewRows(apiKeySQLRows()).
			AddRow("key-row-1", "org-a", "user-owner@keys.test", "ci", "ak_1a2b3c4d", "aa", time.Now().UTC(), nil, nil))
	mock.ExpectExec("UPDATE api_keys SET revoked_at").
		WithArgs(sqlmock.AnyArg(), "key-row-1", "org-a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if rr := doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/key-row-1", owner, ""); rr.Code != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Revoke #2 (idempotent): the durable UPDATE matches the already-revoked
	// row again (no revoked_at filter) -> still 200, never 404.
	mock.ExpectQuery("FROM api_keys WHERE organization_id").
		WithArgs("org-a").
		WillReturnRows(sqlmock.NewRows(apiKeySQLRows()).
			AddRow("key-row-1", "org-a", "user-owner@keys.test", "ci", "ak_1a2b3c4d", "aa", time.Now().UTC(), time.Now().UTC(), nil))
	mock.ExpectExec("UPDATE api_keys SET revoked_at").
		WithArgs(sqlmock.AnyArg(), "key-row-1", "org-a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if rr := doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/key-row-1", owner, ""); rr.Code != http.StatusOK {
		t.Fatalf("double revoke: expected 200 (idempotent), got %d body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestAPIKeysStoreModeCrossOrgRevoke404NoUpdate pins the tenant guard on the
// durable path: the ownership pre-check 404s BEFORE any UPDATE is attempted —
// the UPDATE expectation below is deliberately absent, so a stray UPDATE would
// fail the request (500) and the test.
func TestAPIKeysStoreModeCrossOrgRevoke404NoUpdate(t *testing.T) {
	h, authSvc, mock, closeDB := newAPIKeysStoreTestEnv(t)
	defer closeDB()
	ownerB := apiKeysTokenFor(t, authSvc, "b@keys.test", "org-b", "OWNER")

	// The org-scoped listing for org-b returns nothing: org-a's key is invisible.
	mock.ExpectQuery("FROM api_keys WHERE organization_id").
		WithArgs("org-b").
		WillReturnRows(sqlmock.NewRows(apiKeySQLRows()))

	rr := doAPIKeysReq(t, h, http.MethodDelete, "/api/v1/api-keys/key-row-org-a", ownerB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org revoke: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if body := decodeAPIKeysBody(t, rr); body["error"].(map[string]any)["code"] != "API_KEY_NOT_FOUND" {
		t.Fatalf("expected API_KEY_NOT_FOUND envelope, got %#v", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations (an UPDATE must never run for a foreign key): %v", err)
	}
}
