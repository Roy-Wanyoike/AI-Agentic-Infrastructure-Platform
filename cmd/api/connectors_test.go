package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
	"agentos/internal/connectors"
	"agentos/internal/secrets"
)

// connectors_test.go exercises the issue #30 HTTP surface through the REAL
// middleware chain (RequireAuthOrAPIKey -> RequirePermission), pinning the
// permission matrix, tenant isolation, envelopes and the no-secret-values
// guarantee.

// Compile-time structural-adapter proof: the platform secrets service
// satisfies connectors.SecretResolver WITHOUT any wrapper (its
// Resolve(ctx, orgID, name) method shape matches the interface), which is why
// the orchestrator wiring can pass a.secretsSvc directly.
var _ connectors.SecretResolver = (*secrets.Service)(nil)

func newConnectorsTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *apikeys.Service, *audit.Service, *connectors.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	svc := connectors.NewService()
	auditSvc := audit.NewService()
	apiMux := http.NewServeMux()
	registerConnectorsRoutes(apiMux, svc, authSvc, keysSvc, auditSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, keysSvc, auditSvc, svc
}

func connectorsTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
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

func doConnectorsReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func decodeConnectorsBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

func connectorCreateBody(name string) string {
	return fmt.Sprintf(`{"name":%q,"type":"http","base_url":"https://api.example.com/v1","auth_style":"bearer","secret_ref":"CRM_KEY","headers":{"X-Tenant":"acme"}}`, name)
}

func createConnectorViaHTTP(t *testing.T, h http.Handler, token, name string) *httptest.ResponseRecorder {
	t.Helper()
	return doConnectorsReq(t, h, http.MethodPost, "/api/v1/connectors", token, connectorCreateBody(name))
}

func TestConnectorsAuthRequired(t *testing.T) {
	h, _, _, _, _ := newConnectorsTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/connectors"},
		{http.MethodPost, "/api/v1/connectors"},
		{http.MethodGet, "/api/v1/connectors/some-id"},
		{http.MethodDelete, "/api/v1/connectors/some-id"},
		{http.MethodPost, "/api/v1/connectors/some-id/test"},
	} {
		rr := doConnectorsReq(t, h, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestConnectorsRoleMatrix(t *testing.T) {
	h, authSvc, _, _, svc := newConnectorsTestRouter(t)
	ctx := context.Background()
	const org = "org-m"

	// Seed independent per-role targets directly in the service so every HTTP
	// matrix row starts from identical state (delete targets are consumed by
	// the successful OWNER/ADMIN deletes and must not be reused elsewhere).
	seed := map[string]*connectors.Connector{}
	probes := map[string]*connectors.Connector{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		del, err := svc.Create(ctx, org, connectors.CreateInput{
			Name: "del-" + strings.ToLower(role), Type: connectors.TypeHTTP,
			BaseURL: "https://api.example.com",
		}, "seed")
		if err != nil {
			t.Fatalf("seed del-%s failed: %v", role, err)
		}
		seed[role] = del
		probe, err := svc.Create(ctx, org, connectors.CreateInput{
			Name: "probe-" + strings.ToLower(role), Type: connectors.TypeHTTP,
			BaseURL: "https://api.example.com",
		}, "seed")
		if err != nil {
			t.Fatalf("seed probe-%s failed: %v", role, err)
		}
		probes[role] = probe
	}
	getTarget, err := svc.Create(ctx, org, connectors.CreateInput{
		Name: "get-target", Type: connectors.TypeHTTP,
		BaseURL: "https://api.example.com",
	}, "seed")
	if err != nil {
		t.Fatalf("seed get-target failed: %v", err)
	}

	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = connectorsTokenFor(t, authSvc, strings.ToLower(role)+"@con.test", org, role)
	}

	// create/delete/test: connectors.write -> OWNER/ADMIN only
	for role, want := range map[string]int{"OWNER": 201, "ADMIN": 201, "MEMBER": 403, "VIEWER": 403} {
		rr := createConnectorViaHTTP(t, h, tokens[role], "created-by-"+strings.ToLower(role))
		if rr.Code != want {
			t.Errorf("create as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
	}
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 200, "MEMBER": 403, "VIEWER": 403} {
		rr := doConnectorsReq(t, h, http.MethodDelete, "/api/v1/connectors/"+seed[role].ID, tokens[role], "")
		if rr.Code != want {
			t.Errorf("delete as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
		if want == http.StatusOK {
			if _, err := svc.Get(ctx, org, seed[role].ID); err == nil {
				t.Errorf("delete as %s reported success but connector still exists", role)
			}
		} else if _, err := svc.Get(ctx, org, seed[role].ID); err != nil {
			t.Errorf("denied delete as %s must not delete the connector", role)
		}
	}
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 200, "MEMBER": 403, "VIEWER": 403} {
		rr := doConnectorsReq(t, h, http.MethodPost, "/api/v1/connectors/"+probes[role].ID+"/test", tokens[role], "")
		if rr.Code != want {
			t.Errorf("test as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
	}

	// list/get: connectors.read -> MEMBER+ (VIEWER excluded)
	for role, want := range map[string]int{"OWNER": 200, "ADMIN": 200, "MEMBER": 200, "VIEWER": 403} {
		rr := doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors", tokens[role], "")
		if rr.Code != want {
			t.Errorf("list as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
		rr = doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors/"+getTarget.ID, tokens[role], "")
		if rr.Code != want {
			t.Errorf("get as %s: expected %d, got %d body=%s", role, want, rr.Code, rr.Body.String())
		}
	}
}

func TestConnectorsCreateEnvelopeAndNoSecretValues(t *testing.T) {
	h, authSvc, _, auditSvc, svc := newConnectorsTestRouter(t)
	owner := connectorsTokenFor(t, authSvc, "owner@con.test", "org-a", "OWNER")

	rr := createConnectorViaHTTP(t, h, owner, "crm-prod")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeConnectorsBody(t, rr)
	conn, ok := body["connector"].(map[string]any)
	if !ok {
		t.Fatalf("expected {\"connector\":{...}} envelope, got %#v", body)
	}
	if conn["name"] != "crm-prod" || conn["type"] != "http" || conn["status"] != "active" {
		t.Fatalf("unexpected connector fields: %#v", conn)
	}
	if conn["secret_ref"] != "CRM_KEY" {
		t.Fatalf("secret_ref name must be echoed: %#v", conn["secret_ref"])
	}
	if conn["created_by"] != "user-owner@con.test" {
		t.Fatalf("created_by must come from claims: %#v", conn["created_by"])
	}
	cfg, ok := conn["config"].(map[string]any)
	if !ok || cfg["auth_style"] != "bearer" {
		t.Fatalf("config must round-trip auth_style: %#v", cfg)
	}
	if strings.Contains(rr.Body.String(), "Bearer ") || strings.Contains(rr.Body.String(), "tok_") {
		t.Fatal("connector responses must never contain secret material")
	}
	if _, hasValue := conn["value"]; hasValue {
		t.Fatal("connector projection must not carry a value field")
	}
	if conn["id"] == "" || conn["id"] == nil {
		t.Fatal("connector id missing")
	}

	// The service stored the name reference only.
	ctx := context.Background()
	list, _ := svc.List(ctx, "org-a")
	if len(list) != 1 || list[0].SecretRef != "CRM_KEY" {
		t.Fatalf("service must persist the ref name only: %+v", list)
	}

	// Duplicate name -> 409.
	rr = createConnectorViaHTTP(t, h, owner, "crm-prod")
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d", rr.Code)
	}
	if code := decodeConnectorsBody(t, rr)["error"].(map[string]any)["code"]; code != "CONNECTOR_ALREADY_EXISTS" {
		t.Fatalf("duplicate error code: %#v", code)
	}

	// Validation error -> 422.
	rr = doConnectorsReq(t, h, http.MethodPost, "/api/v1/connectors", owner,
		`{"name":"bad","type":"ftp","base_url":"https://api.example.com"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid type: expected 422, got %d", rr.Code)
	}
	// Malformed JSON -> 400.
	rr = doConnectorsReq(t, h, http.MethodPost, "/api/v1/connectors", owner, `{not-json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d", rr.Code)
	}

	// Audit trail: connector.created entry, no secret material.
	entries, _ := auditSvc.ListCtx(ctx, "org-a")
	found := false
	for _, e := range entries {
		if e.Action == "connector.created" && strings.HasPrefix(e.Resource, "connectors/") {
			found = true
		}
	}
	if !found {
		t.Fatal("connector.created audit entry missing")
	}
}

func TestConnectorsListGetDeleteEnvelopes(t *testing.T) {
	h, authSvc, _, _, _ := newConnectorsTestRouter(t)
	owner := connectorsTokenFor(t, authSvc, "owner@con.test", "org-a", "OWNER")

	rr := doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("empty list: expected 200, got %d", rr.Code)
	}
	if items, ok := decodeConnectorsBody(t, rr)["connectors"].([]any); !ok || len(items) != 0 {
		t.Fatal("list must be an array (possibly empty)")
	}

	rr = createConnectorViaHTTP(t, h, owner, "crm-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rr.Code)
	}
	id := decodeConnectorsBody(t, rr)["connector"].(map[string]any)["id"].(string)

	rr = doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors", owner, "")
	items, _ := decodeConnectorsBody(t, rr)["connectors"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 connector in list, got %d", len(items))
	}

	rr = doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors/"+id, owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", rr.Code)
	}
	if got := decodeConnectorsBody(t, rr)["connector"].(map[string]any)["id"]; got != id {
		t.Fatalf("get returned wrong connector: %v", got)
	}

	// Unknown id -> 404 with contract code.
	rr = doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors/nope", owner, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown get: expected 404, got %d", rr.Code)
	}
	if code := decodeConnectorsBody(t, rr)["error"].(map[string]any)["code"]; code != "CONNECTOR_NOT_FOUND" {
		t.Fatalf("not-found error code: %#v", code)
	}

	rr = doConnectorsReq(t, h, http.MethodDelete, "/api/v1/connectors/"+id, owner, "")
	if rr.Code != http.StatusOK || decodeConnectorsBody(t, rr)["deleted"] != true {
		t.Fatalf("delete envelope: %d %s", rr.Code, rr.Body.String())
	}
	rr = doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors/"+id, owner, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("post-delete get: expected 404, got %d", rr.Code)
	}

	// Method guard: PUT /connectors/{id} is not a registered route.
	rr = doConnectorsReq(t, h, http.MethodPut, "/api/v1/connectors/"+id, owner, "{}")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT must be 405, got %d", rr.Code)
	}
}

func TestConnectorsTenantIsolation(t *testing.T) {
	h, authSvc, _, _, svc := newConnectorsTestRouter(t)
	ctx := context.Background()
	ownerB := connectorsTokenFor(t, authSvc, "b@con.test", "org-b", "OWNER")

	created, err := svc.Create(ctx, "org-a", connectors.CreateInput{
		Name: "secret-crm", Type: connectors.TypeHTTP, BaseURL: "https://api.example.com",
	}, "seed")
	if err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	// org B cannot see, delete, or probe org A's connector (404, no leak).
	rr := doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors/"+created.ID, ownerB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get: expected 404, got %d", rr.Code)
	}
	rr = doConnectorsReq(t, h, http.MethodDelete, "/api/v1/connectors/"+created.ID, ownerB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete: expected 404, got %d", rr.Code)
	}
	rr = doConnectorsReq(t, h, http.MethodPost, "/api/v1/connectors/"+created.ID+"/test", ownerB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant test: expected 404, got %d", rr.Code)
	}
	if _, err := svc.Get(ctx, "org-a", created.ID); err != nil {
		t.Fatal("cross-tenant attempts must not delete the connector")
	}

	// Lists are scoped: org B sees nothing of org A.
	rr = doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors", ownerB, "")
	if items, _ := decodeConnectorsBody(t, rr)["connectors"].([]any); len(items) != 0 {
		t.Fatalf("org-b list leaked org-a connectors: %d", len(items))
	}

	// Same name in different orgs is allowed (uniqueness is per-org).
	rr = doConnectorsReq(t, h, http.MethodPost, "/api/v1/connectors", ownerB,
		`{"name":"secret-crm","type":"webhook","base_url":"https://hooks.example.com"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("same name other org: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestConnectorsTestEndpointLiveProbe(t *testing.T) {
	h, authSvc, _, auditSvc, svc := newConnectorsTestRouter(t)
	// The service-level probe semantics are pinned in internal/connectors;
	// here we prove the HTTP wiring end to end (auth -> probe -> recording).
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	owner := connectorsTokenFor(t, authSvc, "owner@con.test", "org-a", "OWNER")
	body := fmt.Sprintf(`{"name":"crm","type":"http","base_url":%q,"auth_style":"bearer","secret_ref":"CRM_KEY"}`, srv.URL)
	rr := doConnectorsReq(t, h, http.MethodPost, "/api/v1/connectors", owner, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	id := decodeConnectorsBody(t, rr)["connector"].(map[string]any)["id"].(string)

	// Wire the structural adapter (secrets service) as the resolver, mirroring
	// the orchestrator wiring line for main.go.
	secretsSvc := secrets.NewService()
	if _, err := secretsSvc.Create(context.Background(), "org-a", "CRM_KEY", "tok_abc", "seed"); err != nil {
		t.Fatalf("secret seed failed: %v", err)
	}
	svc.SetSecretResolver(secretsSvc)

	rr = doConnectorsReq(t, h, http.MethodPost, "/api/v1/connectors/"+id+"/test", owner, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	test, ok := decodeConnectorsBody(t, rr)["test"].(map[string]any)
	if !ok {
		t.Fatalf("expected {\"test\":{...}} envelope, got %s", rr.Body.String())
	}
	if test["status"] != "ok" || test["connector_id"] != id {
		t.Fatalf("unexpected probe result: %#v", test)
	}
	if gotAuth != "Bearer tok_abc" {
		t.Fatalf("probe must apply the bearer style through the adapter, got %q", gotAuth)
	}
	if strings.Contains(rr.Body.String(), "tok_abc") {
		t.Fatal("probe response must never contain secret values")
	}

	// last_check status is reflected by GET.
	rr = doConnectorsReq(t, h, http.MethodGet, "/api/v1/connectors/"+id, owner, "")
	conn := decodeConnectorsBody(t, rr)["connector"].(map[string]any)
	if conn["last_check_status"] != "ok" {
		t.Fatalf("last_check_status must be recorded: %#v", conn["last_check_status"])
	}
	if conn["last_check_at"] == nil {
		t.Fatal("last_check_at must be recorded")
	}

	// Audit: connector.tested entry with outcome metadata only.
	entries, _ := auditSvc.ListCtx(context.Background(), "org-a")
	found := false
	for _, e := range entries {
		if e.Action == "connector.tested" {
			found = true
			if strings.Contains(fmt.Sprintf("%v", e.Metadata), "tok_abc") {
				t.Fatal("audit metadata must not carry secret values")
			}
		}
	}
	if !found {
		t.Fatal("connector.tested audit entry missing")
	}
}

func TestConnectorsAPIKeyAuthAsOwner(t *testing.T) {
	h, _, keysSvc, _, _ := newConnectorsTestRouter(t)
	key, err := keysSvc.Create("org-key", "key-user", "ci-key")
	if err != nil {
		t.Fatalf("api key create failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connectors", nil)
	req.Header.Set("X-API-Key", key.Value)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("X-API-Key must authenticate as OWNER, got %d body=%s", rr.Code, rr.Body.String())
	}
}
