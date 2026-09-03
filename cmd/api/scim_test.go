package main

// Issue #29 handler tests — SCIM half: the full middleware chain
// (RequireAuthOrAPIKey -> RequirePermission(organization.manage) for token
// minting; scim.RequireSCIMToken for the protocol surface), the SCIM 2.0
// HTTP contract (envelopes, filter, PATCH deprovisioning blocking password
// login) and the bearer-token failure matrix. All in-memory, no database.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/scim"
)

// scimHandlerEnv wires both middleware chains over one shared identity
// table: the OWNER session token mints SCIM credentials, the scim_ secret
// drives the /scim/v2/Users surface, and authSvc shares identities so the
// deprovisioning flow can assert on the password-login path.
type scimHandlerEnv struct {
	mux         *http.ServeMux
	authSvc     *auth.Service
	identities  *auth.MemoryStore
	svc         *scim.Service
	orgID       string
	ownerToken  string
	otherTokens map[string]string
}

func newScimHandlerEnv(t *testing.T) *scimHandlerEnv {
	t.Helper()
	identities := auth.NewMemoryStore()
	authSvc := auth.NewServiceWithStore("test-secret", identities)
	apiKeysSvc := apikeys.NewService()
	svc := scim.NewService(identities)

	if err := identities.CreateOrganization(t.Context(), &auth.Organization{ID: "org-1", Name: "Acme"}); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if err := identities.CreateOrganization(t.Context(), &auth.Organization{ID: "org-2", Name: "Other"}); err != nil {
		t.Fatalf("CreateOrganization(org-2): %v", err)
	}
	ownerToken, err := authSvc.GenerateToken(&auth.User{
		ID: "owner-1", Organization: "org-1", Email: "owner@acme.test", Role: "OWNER", Active: true,
	})
	if err != nil {
		t.Fatalf("GenerateToken(owner): %v", err)
	}
	tokenFor := func(id, role string) string {
		t.Helper()
		generated, err := authSvc.GenerateToken(&auth.User{
			ID: id, Organization: "org-1", Email: id + "@acme.test", Role: role, Active: true,
		})
		if err != nil {
			t.Fatalf("GenerateToken(%s): %v", role, err)
		}
		return generated
	}

	mux := http.NewServeMux()
	registerScimRoutes(mux, svc, authSvc, apiKeysSvc)

	env := &scimHandlerEnv{mux: mux, authSvc: authSvc, identities: identities, svc: svc, orgID: "org-1", ownerToken: ownerToken}
	env.otherTokens = map[string]string{
		"ADMIN":  tokenFor("admin-1", "ADMIN"),
		"MEMBER": tokenFor("member-1", "MEMBER"),
		"VIEWER": tokenFor("viewer-1", "VIEWER"),
	}
	return env
}

// otherTokens holds the non-owner role tokens (ADMIN/MEMBER/VIEWER), filled
// in by newScimHandlerEnv.

func (e *scimHandlerEnv) do(t *testing.T, method, path, token string, body string) *httptest.ResponseRecorder {
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
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// mintToken drives the real OWNER-only route end-to-end and returns the
// plaintext scim_ secret.
func (e *scimHandlerEnv) mintToken(t *testing.T) string {
	t.Helper()
	rr := e.do(t, http.MethodPost, "/scim/tokens", e.ownerToken, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /scim/tokens: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var decoded struct {
		Token  map[string]any `json:"token"`
		Secret string         `json:"secret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("token body is not JSON: %s", rr.Body.String())
	}
	if !strings.HasPrefix(decoded.Secret, scim.TokenPrefix) {
		t.Fatalf("secret %q must carry the scim_ prefix", decoded.Secret)
	}
	if decoded.Token["id"] == nil || decoded.Token["organization_id"] != e.orgID {
		t.Fatalf("token metadata incomplete: %v", decoded.Token)
	}
	if _, hasHash := decoded.Token["token_hash"]; hasHash {
		t.Fatal("token metadata must not echo the stored hash")
	}
	if strings.Contains(rr.Body.String(), "token_hash") {
		t.Fatal("response must not contain the hash")
	}
	return decoded.Secret
}

// TestSCIMTokenRouteRBAC pins the OWNER-only matrix on the minting route.
func TestSCIMTokenRouteRBAC(t *testing.T) {
	env := newScimHandlerEnv(t)

	// Unauthenticated.
	if rr := env.do(t, http.MethodPost, "/scim/tokens", "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mint: expected 401, got %d", rr.Code)
	}
	// ADMIN/MEMBER/VIEWER are all locked out (organization.manage = OWNER).
	for role, token := range env.otherTokens {
		rr := env.do(t, http.MethodPost, "/scim/tokens", token, "")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s mint: expected 403, got %d", role, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "secret") {
			t.Fatalf("%s must never receive a secret", role)
		}
	}
}

// TestSCIMUsersFullLifecycleThroughHTTP walks create -> list/filter -> get ->
// put -> patch over the protocol surface with a minted bearer credential.
func TestSCIMUsersFullLifecycleThroughHTTP(t *testing.T) {
	env := newScimHandlerEnv(t)
	secret := env.mintToken(t)

	// The minted secret must authenticate the protocol surface.
	req := httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("minted secret rejected by protocol surface: %d %s", rr.Code, rr.Body.String())
	}

	// POST: 201 + Location + standard envelope.
	createBody := `{"schemas":["` + scim.SchemaUser + `"],"userName":"New.Hire@Acme.TEST","name":{"givenName":"New"},"displayName":"New Hire","password":"ignored"}`
	rr = env.do(t, http.MethodPost, "/scim/v2/Users", secret, createBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /scim/v2/Users: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if created["userName"] != "new.hire@acme.test" {
		t.Fatalf("userName must be stored lowercased, got %v", created["userName"])
	}
	if created["active"] != true {
		t.Fatalf("default active must be true, got %v", created["active"])
	}
	if got := rr.Header().Get("Location"); got == "" {
		t.Fatal("201 must carry a Location header")
	}
	if body := rr.Body.String(); strings.Contains(body, "password") || strings.Contains(body, "role") {
		t.Fatalf("response leaked credential/role material: %s", body)
	}
	userID := created["id"].(string)

	// Malformed JSON is a 400 SCIM error, never a panic.
	if rr := env.do(t, http.MethodPost, "/scim/v2/Users", secret, "{not json"); rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed create body: expected 400, got %d", rr.Code)
	}

	// GET list without filter: totalResults 1.
	rr = env.do(t, http.MethodGet, "/scim/v2/Users", secret, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET list: expected 200, got %d", rr.Code)
	}
	var list struct {
		Schemas      []string         `json:"schemas"`
		TotalResults int              `json:"totalResults"`
		Resources    []map[string]any `json:"Resources"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if list.TotalResults != 1 || len(list.Resources) != 1 || list.Schemas[0] != scim.SchemaListResponse {
		t.Fatalf("unexpected list envelope: %+v", list)
	}

	// GET list with the userName eq filter (case-insensitive; URL-escaped).
	filtered := "/scim/v2/Users?filter=" + url.QueryEscape(`userName eq "NEW.HIRE@ACME.TEST"`)
	rr = env.do(t, http.MethodGet, filtered, secret, "")
	var flist struct {
		TotalResults int              `json:"totalResults"`
		Resources    []map[string]any `json:"Resources"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &flist)
	if flist.TotalResults != 1 || flist.Resources[0]["id"] != userID {
		t.Fatalf("userName eq filter did not match exactly one user: %s", rr.Body.String())
	}

	// Unsupported filters are 400 and never degrade into a full listing.
	rr = env.do(t, http.MethodGet, "/scim/v2/Users?filter="+url.QueryEscape(`userName pr`), secret, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unsupported filter: expected 400, got %d", rr.Code)
	}
	var scimErr map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &scimErr)
	if scimErr["status"] != "400" || scimErr["detail"] == nil {
		t.Fatalf("expected SCIM error envelope, got %s", rr.Body.String())
	}

	// GET by id; a foreign tenant id is 404 (no existence leak).
	if rr = env.do(t, http.MethodGet, "/scim/v2/Users/"+userID, secret, ""); rr.Code != http.StatusOK {
		t.Fatalf("GET by id: expected 200, got %d", rr.Code)
	}
	if rr = env.do(t, http.MethodGet, "/scim/v2/Users/does-not-exist", secret, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("GET unknown id: expected 404, got %d", rr.Code)
	}

	// PUT with a different userName is immutable -> 400, user unchanged.
	rr = env.do(t, http.MethodPut, "/scim/v2/Users/"+userID, secret, `{"userName":"renamed@acme.test"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PUT rename: expected 400, got %d", rr.Code)
	}
	// PUT with the same userName (case differs) and active=false -> 200.
	rr = env.do(t, http.MethodPut, "/scim/v2/Users/"+userID, secret, `{"userName":"NEW.HIRE@acme.test","active":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT disable: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var replaced map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &replaced)
	if replaced["active"] != false {
		t.Fatalf("PUT must honor active=false, got %v", replaced["active"])
	}

	// PATCH replaces active again (pathless form) and lists reflect it.
	patch := `{"schemas":["` + scim.SchemaPatchOp + `"],"Operations":[{"op":"replace","value":{"active":true}}]}`
	rr = env.do(t, http.MethodPatch, "/scim/v2/Users/"+userID, secret, patch)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH enable: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// PATCH with an unsupported op is rejected before mutation.
	rr = env.do(t, http.MethodPatch, "/scim/v2/Users/"+userID, secret, `{"Operations":[{"op":"add","path":"active","value":true}]}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PATCH add: expected 400, got %d", rr.Code)
	}

	// The protocol surface rejects wrong-scheme credentials.
	rr = env.do(t, http.MethodGet, "/scim/v2/Users", "session-token-not-scim", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("session token on protocol surface: expected 401, got %d", rr.Code)
	}
}

// TestSCIMDeactivationBlocksPasswordLoginThroughHTTP is the end-to-end
// deprovisioning contract: PATCH replace active=false on the HTTP surface
// blocks auth.Service password login (shared identity table).
func TestSCIMDeactivationBlocksPasswordLoginThroughHTTP(t *testing.T) {
	env := newScimHandlerEnv(t)
	secret := env.mintToken(t)

	hash, err := auth.HashPassword("s3cretpass")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	local := &auth.User{
		ID: "local-1", Organization: env.orgID, Email: "emp@acme.test",
		PasswordHash: hash, Role: "MEMBER", Active: true,
	}
	if err := env.identities.CreateUser(t.Context(), local); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	patch := `{"Operations":[{"op":"replace","path":"active","value":false}]}`
	rr := env.do(t, http.MethodPatch, "/scim/v2/Users/local-1", secret, patch)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH disable: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var patched map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &patched)
	if patched["active"] != false {
		t.Fatalf("patched user must be inactive, got %v", patched["active"])
	}
	if _, err := env.authSvc.LoginCtx(t.Context(), "emp@acme.test", "s3cretpass"); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("disabled user must fail password login with ErrAccountDisabled, got %v", err)
	}

	// Re-enabling restores the login path.
	rr = env.do(t, http.MethodPatch, "/scim/v2/Users/local-1", secret, `{"Operations":[{"op":"replace","path":"active","value":true}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH enable: expected 200, got %d", rr.Code)
	}
	if _, err := env.authSvc.LoginCtx(t.Context(), "emp@acme.test", "s3cretpass"); err != nil {
		t.Fatalf("re-enabled user must password-login again, got %v", err)
	}
}

// TestSCIMBearerAuthFailureMatrix covers the RequireSCIMToken 401 paths and
// that a revoked credential stops authenticating.
func TestSCIMBearerAuthFailureMatrix(t *testing.T) {
	env := newScimHandlerEnv(t)
	secret := env.mintToken(t)

	for _, tc := range []struct {
		name       string
		authHeader string
	}{
		{"missing header", ""},
		{"bare scheme", "Bearer"},
		{"wrong scheme", "Basic " + secret},
		{"unknown secret", "Bearer " + scim.TokenPrefix + strings.Repeat("ab", 32)},
		{"empty bearer", "Bearer "},
	} {
		req := httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil)
		if tc.authHeader != "" {
			req.Header.Set("Authorization", tc.authHeader)
		}
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d", tc.name, rr.Code)
		}
		var envelope map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s: 401 body is not JSON: %s", tc.name, rr.Body.String())
		}
		if envelope["status"] != "401" {
			t.Fatalf("%s: expected SCIM error envelope, got %s", tc.name, rr.Body.String())
		}
	}

	// Revocation kills the credential on the protocol surface.
	token, err := env.svc.Authenticate(t.Context(), secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := env.svc.RevokeToken(t.Context(), token.OrgID, token.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	rr := env.do(t, http.MethodGet, "/scim/v2/Users", secret, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked secret: expected 401, got %d", rr.Code)
	}
}

// TestSCIMRoutesMethodMatrix pins 405s the mux derives from method patterns.
func TestSCIMRoutesMethodMatrix(t *testing.T) {
	env := newScimHandlerEnv(t)
	secret := env.mintToken(t)

	if rr := env.do(t, http.MethodGet, "/scim/tokens", secret, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /scim/tokens: expected 405, got %d", rr.Code)
	}
	if rr := env.do(t, http.MethodDelete, "/scim/v2/Users/whatever", secret, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /scim/v2/Users/{id}: expected 405, got %d", rr.Code)
	}
	if rr := env.do(t, http.MethodPost, "/scim/v2/Users/x/patch", secret, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown subpath: expected 404, got %d", rr.Code)
	}
}
