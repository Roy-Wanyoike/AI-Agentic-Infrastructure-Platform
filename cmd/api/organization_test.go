package main

// Issue #52 handler tests — the organizations & membership management
// surface: the full middleware chain (RequireAuthOrAPIKey ->
// RequirePermission), the complete member lifecycle over HTTP (add -> role
// change -> remove -> login blocked -> re-add), the last-owner guard on both
// PATCH and DELETE, the RBAC matrix, cross-org isolation (404 without
// existence leak) and the audit trail. All in-memory, no database, mirroring
// the scim_test.go harness: ONE shared identity table between auth.Service
// and the members handlers so the deprovisioning flow can assert on the
// real password-login path.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/organizations"
)

// orgHandlerEnv wires the members routes over one shared identity table.
type orgHandlerEnv struct {
	mux        *http.ServeMux
	authSvc    *auth.Service
	identities *auth.MemoryStore
	orgsSvc    *organizations.Service
	auditSvc   *audit.Service
	orgID      string
	otherOrgID string
	ownerToken string
	tokens     map[string]string // role -> session token (org-1 users)
}

func newOrgHandlerEnv(t *testing.T) *orgHandlerEnv {
	t.Helper()
	return newOrgHandlerEnvWithAudit(t, audit.NewService())
}

func newOrgHandlerEnvWithAudit(t *testing.T, auditSvc *audit.Service) *orgHandlerEnv {
	t.Helper()
	identities := auth.NewMemoryStore()
	authSvc := auth.NewServiceWithStore("test-secret", identities)
	orgsSvc := organizations.NewService()

	ctx := context.Background()
	org, err := orgsSvc.CreateCtx(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateCtx(Acme): %v", err)
	}
	if err := identities.CreateOrganization(ctx, &auth.Organization{ID: org.ID, Name: "Acme"}); err != nil {
		t.Fatalf("CreateOrganization(identity table): %v", err)
	}
	other, err := orgsSvc.CreateCtx(ctx, "Beta")
	if err != nil {
		t.Fatalf("CreateCtx(Beta): %v", err)
	}
	if err := identities.CreateOrganization(ctx, &auth.Organization{ID: other.ID, Name: "Beta"}); err != nil {
		t.Fatalf("CreateOrganization(Beta): %v", err)
	}

	seedIdentity := func(id, email, role, password string, org string) {
		t.Helper()
		hash := ""
		if password != "" {
			hash, err = auth.HashPassword(password)
			if err != nil {
				t.Fatalf("HashPassword: %v", err)
			}
		}
		if err := identities.CreateUser(ctx, &auth.User{
			ID: id, Organization: org, Email: email, PasswordHash: hash, Role: role, Active: true,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateUser(%s): %v", email, err)
		}
	}
	tokenFor := func(id, email, role, org string) string {
		t.Helper()
		token, err := authSvc.GenerateToken(&auth.User{ID: id, Organization: org, Email: email, Role: role, Active: true})
		if err != nil {
			t.Fatalf("GenerateToken(%s): %v", role, err)
		}
		return token
	}

	seedIdentity("owner-1", "owner@acme.test", "OWNER", "owner-pass-1", org.ID)
	seedIdentity("admin-1", "admin@acme.test", "ADMIN", "admin-pass-1", org.ID)
	seedIdentity("member-1", "member@acme.test", "MEMBER", "member-pass-1", org.ID)
	seedIdentity("viewer-1", "viewer@acme.test", "VIEWER", "viewer-pass-1", org.ID)
	seedIdentity("beta-owner", "owner@beta.test", "OWNER", "beta-pass-1", other.ID)

	// The caller identities also carry membership rows so the directory is
	// coherent (registration itself does not create membership rows — the
	// members API manages them explicitly).
	if err := orgsSvc.AddMemberCtx(ctx, org.ID, "owner-1", "OWNER"); err != nil {
		t.Fatalf("seed owner membership: %v", err)
	}

	mux := http.NewServeMux()
	registerOrganizationRoutes(mux, orgsSvc, identities, authSvc, apikeys.NewService(), auditSvc)

	return &orgHandlerEnv{
		mux:        mux,
		authSvc:    authSvc,
		identities: identities,
		orgsSvc:    orgsSvc,
		auditSvc:   auditSvc,
		orgID:      org.ID,
		otherOrgID: other.ID,
		ownerToken: tokenFor("owner-1", "owner@acme.test", "OWNER", org.ID),
		tokens: map[string]string{
			"ADMIN":  tokenFor("admin-1", "admin@acme.test", "ADMIN", org.ID),
			"MEMBER": tokenFor("member-1", "member@acme.test", "MEMBER", org.ID),
			"VIEWER": tokenFor("viewer-1", "viewer@acme.test", "VIEWER", org.ID),
		},
	}
}

// tokenFor mints a session token for any identity row (cross-org tests use
// it for the Beta tenant).
func (e *orgHandlerEnv) tokenFor(t *testing.T, id, email, role, org string) string {
	t.Helper()
	token, err := e.authSvc.GenerateToken(&auth.User{ID: id, Organization: org, Email: email, Role: role, Active: true})
	if err != nil {
		t.Fatalf("GenerateToken(%s): %v", role, err)
	}
	return token
}

func (e *orgHandlerEnv) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
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

func decodeOrgBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response body is not JSON: %s", rr.Body.String())
	}
	return decoded
}

func orgErrCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeOrgBody(t, rr)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected structured error envelope, got %s", rr.Body.String())
	}
	code, _ := errObj["code"].(string)
	return code
}

// seedMember creates an org-1 identity (with a login password) plus the
// membership row through the REAL POST route, returning the user id.
func (e *orgHandlerEnv) seedMember(t *testing.T, id, email, role string) string {
	t.Helper()
	if err := e.identities.CreateUser(context.Background(), &auth.User{
		ID: id, Organization: e.orgID, Email: email, Role: role, Active: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	rr := e.do(t, http.MethodPost, "/organization/members", e.ownerToken, `{"email":"`+email+`","role":"`+role+`"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed member %s: expected 201, got %d: %s", email, rr.Code, rr.Body.String())
	}
	return id
}

// TestOrganizationInfoHandler pins GET /organization: claims-derived org +
// caller's role, visible to every role, 404 when the directory misses the
// tenant, 401 without credentials.
func TestOrganizationInfoHandler(t *testing.T) {
	env := newOrgHandlerEnv(t)

	for role, token := range map[string]string{
		"OWNER":  env.ownerToken,
		"ADMIN":  env.tokens["ADMIN"],
		"MEMBER": env.tokens["MEMBER"],
		"VIEWER": env.tokens["VIEWER"],
	} {
		rr := env.do(t, http.MethodGet, "/organization", token, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s GET /organization: expected 200, got %d: %s", role, rr.Code, rr.Body.String())
		}
		body := decodeOrgBody(t, rr)
		orgObj, ok := body["organization"].(map[string]any)
		if !ok {
			t.Fatalf("%s: missing organization object: %s", role, rr.Body.String())
		}
		if orgObj["id"] != env.orgID || orgObj["name"] != "Acme" {
			t.Fatalf("%s: wrong organization projection: %v", role, orgObj)
		}
		if orgObj["created_at"] == "" || orgObj["status"] != "ACTIVE" {
			t.Fatalf("%s: incomplete organization projection: %v", role, orgObj)
		}
		if body["role"] != role {
			t.Fatalf("%s: role echo mismatch, got %v", role, body["role"])
		}
	}

	// Unauthenticated callers get 401.
	if rr := env.do(t, http.MethodGet, "/organization", "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: expected 401, got %d", rr.Code)
	}

	// A token whose org the directory does not know is a 404 with the
	// structured code (no partial data is invented).
	strayToken, err := env.authSvc.GenerateToken(&auth.User{ID: "stray-1", Organization: "org-stray", Email: "stray@nowhere.test", Role: "OWNER", Active: true})
	if err != nil {
		t.Fatalf("GenerateToken(stray): %v", err)
	}
	rr := env.do(t, http.MethodGet, "/organization", strayToken, "")
	if rr.Code != http.StatusNotFound || orgErrCode(t, rr) != "ORGANIZATION_NOT_FOUND" {
		t.Fatalf("unknown org: expected 404 ORGANIZATION_NOT_FOUND, got %d %s", rr.Code, rr.Body.String())
	}
}

// TestOrganizationMembersLifecycleOverHTTP walks the whole team-management
// story over the HTTP surface: add -> list -> role change -> remove ->
// login blocked -> re-add -> login works again.
func TestOrganizationMembersLifecycleOverHTTP(t *testing.T) {
	env := newOrgHandlerEnv(t)
	ctx := context.Background()

	// The invite target is an EXISTING platform user (registered identity
	// with a password, no membership row yet).
	if err := env.identities.CreateUser(ctx, &auth.User{
		ID: "emp-1", Organization: env.orgID, Email: "emp@acme.test",
		PasswordHash: func() string {
			hash, err := auth.HashPassword("emp-pass-1")
			if err != nil {
				t.Fatalf("HashPassword: %v", err)
			}
			return hash
		}(), Role: "MEMBER", Active: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateUser(emp): %v", err)
	}

	// 1. ADD (owner invite) — 201 with the full member projection.
	rr := env.do(t, http.MethodPost, "/organization/members", env.ownerToken, `{"email":"emp@acme.test","role":"member"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("add member: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeOrgBody(t, rr)
	member, ok := body["member"].(map[string]any)
	if !ok {
		t.Fatalf("add member: missing member object: %s", rr.Body.String())
	}
	if member["id"] != "emp-1" || member["email"] != "emp@acme.test" || member["role"] != "MEMBER" {
		t.Fatalf("add member: wrong projection: %v", member)
	}
	if member["name"] != "emp" {
		t.Fatalf("add member: display fallback should be the email local part, got %v", member["name"])
	}
	if joined, _ := member["joined_at"].(string); joined == "" || strings.HasPrefix(joined, "0001-") {
		t.Fatalf("add member: joined_at must be the real row timestamp, got %v", member["joined_at"])
	}

	// 2. LIST — the member appears with role and joined_at.
	rr = env.do(t, http.MethodGet, "/organization/members", env.tokens["MEMBER"], "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list members: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	listBody := decodeOrgBody(t, rr)
	items, ok := listBody["members"].([]any)
	if !ok || len(items) != 2 { // owner-1 (seeded) + emp-1
		t.Fatalf("list members: expected 2 rows, got %s", rr.Body.String())
	}
	first, _ := items[0].(map[string]any)
	if first["id"] != "owner-1" || first["role"] != "OWNER" || first["joined_at"] == "" {
		t.Fatalf("list members: owner row wrong: %v", first)
	}

	// 3. ROLE CHANGE (owner) — member -> admin.
	rr = env.do(t, http.MethodPatch, "/organization/members/emp-1", env.ownerToken, `{"role":"admin"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch role: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	patched := decodeOrgBody(t, rr)["member"].(map[string]any)
	if patched["role"] != "ADMIN" {
		t.Fatalf("patch role: not persisted, got %v", patched["role"])
	}

	// 4. REMOVE — 200 and the listing no longer contains the member.
	rr = env.do(t, http.MethodDelete, "/organization/members/emp-1", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("remove member: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodGet, "/organization/members", env.ownerToken, "")
	items, _ = decodeOrgBody(t, rr)["members"].([]any)
	if len(items) != 1 {
		t.Fatalf("remove member: expected 1 row left, got %s", rr.Body.String())
	}

	// 5. REMOVAL BLOCKS LOGIN — the SCIM-deprovisioning contract: the
	// correct password now fails with auth.ErrAccountDisabled.
	if _, err := env.authSvc.LoginCtx(ctx, "emp@acme.test", "emp-pass-1"); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("removed member must fail login with ErrAccountDisabled, got %v", err)
	}

	// 6. RE-ADD — works (the membership row is gone) and re-activates the
	// login through the same org-guarded store path SCIM uses for
	// re-enabling.
	rr = env.do(t, http.MethodPost, "/organization/members", env.ownerToken, `{"email":"emp@acme.test","role":"ADMIN"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("re-add member: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := env.authSvc.LoginCtx(ctx, "emp@acme.test", "emp-pass-1"); err != nil {
		t.Fatalf("re-added member must log in again, got %v", err)
	}
}

// TestOrganizationMembersLastOwnerGuard pins the 409 LAST_OWNER contract on
// BOTH mutating paths: a single OWNER membership row can neither be demoted
// (PATCH) nor removed (DELETE), and the guard re-engages once the org is
// down to its final owner row.
func TestOrganizationMembersLastOwnerGuard(t *testing.T) {
	env := newOrgHandlerEnv(t)

	// env seeds owner-1 with the only OWNER membership row.
	adminID := env.seedMember(t, "admin-2", "admin2@acme.test", "ADMIN")

	// PATCH demote of the last owner: 409 LAST_OWNER, membership unchanged.
	rr := env.do(t, http.MethodPatch, "/organization/members/owner-1", env.ownerToken, `{"role":"MEMBER"}`)
	if rr.Code != http.StatusConflict || orgErrCode(t, rr) != "LAST_OWNER" {
		t.Fatalf("demote last owner: expected 409 LAST_OWNER, got %d %s", rr.Code, rr.Body.String())
	}
	// DELETE of the last owner: 409 LAST_OWNER as well.
	rr = env.do(t, http.MethodDelete, "/organization/members/owner-1", env.ownerToken, "")
	if rr.Code != http.StatusConflict || orgErrCode(t, rr) != "LAST_OWNER" {
		t.Fatalf("remove last owner: expected 409 LAST_OWNER, got %d %s", rr.Code, rr.Body.String())
	}
	// The rejected operations wrote nothing.
	for _, m := range env.orgsSvc.Members(env.orgID) {
		if m.UserID == "owner-1" && m.Role != "OWNER" {
			t.Fatalf("guard must reject before any write, owner-1 is now %q", m.Role)
		}
	}
	// Demoting the owner to OWNER (no-op normalization) stays allowed.
	if rr := env.do(t, http.MethodPatch, "/organization/members/owner-1", env.ownerToken, `{"role":"owner"}`); rr.Code != http.StatusOK {
		t.Fatalf("owner re-affirmation must pass, got %d: %s", rr.Code, rr.Body.String())
	}
	// Non-owner removal is never guarded.
	if rr := env.do(t, http.MethodDelete, "/organization/members/"+adminID, env.ownerToken, ""); rr.Code != http.StatusOK {
		t.Fatalf("remove non-owner: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// With a second owner the demotion succeeds and the guard then
	// re-engages for the remaining owner row.
	env.seedMember(t, "owner-2", "owner2@acme.test", "OWNER")
	if rr := env.do(t, http.MethodPatch, "/organization/members/owner-2", env.ownerToken, `{"role":"MEMBER"}`); rr.Code != http.StatusOK {
		t.Fatalf("demote with second owner: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// owner-1 is still OWNER: removing him now is legal but would leave
	// owner-2 (MEMBER) as the last row of a zero-owner org — the guard
	// counts OWNER rows, so removing the remaining OWNER row is 409.
	rr = env.do(t, http.MethodDelete, "/organization/members/owner-1", env.ownerToken, "")
	if rr.Code != http.StatusConflict || orgErrCode(t, rr) != "LAST_OWNER" {
		t.Fatalf("remove final owner row: expected 409 LAST_OWNER, got %d %s", rr.Code, rr.Body.String())
	}
}

// TestOrganizationMembersRBACMatrix pins the permission matrices:
//   - MEMBER cannot add/patch/remove (users.manage / organization.manage)
//   - ADMIN can add and remove but NOT patch roles (organization.manage is
//     exactly OWNER, like POST /scim/tokens)
//   - VIEWER can see the org but cannot list members (runs.execute is
//     MEMBER+) and cannot write
func TestOrganizationMembersRBACMatrix(t *testing.T) {
	env := newOrgHandlerEnv(t)
	memberID := env.seedMember(t, "emp-2", "emp2@acme.test", "MEMBER")

	// MEMBER: all three writes are forbidden, no mutation happens.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/organization/members", `{"email":"emp2@acme.test","role":"ADMIN"}`},
		{http.MethodPatch, "/organization/members/" + memberID, `{"role":"ADMIN"}`},
		{http.MethodDelete, "/organization/members/" + memberID, ""},
	} {
		rr := env.do(t, tc.method, tc.path, env.tokens["MEMBER"], tc.body)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("MEMBER %s %s: expected 403, got %d", tc.method, tc.path, rr.Code)
		}
	}
	if len(env.orgsSvc.Members(env.orgID)) != 2 {
		t.Fatal("forbidden writes must not mutate memberships")
	}

	// ADMIN: add + remove allowed, role patch is OWNER-only.
	if err := env.identities.CreateUser(context.Background(), &auth.User{
		ID: "emp-nh", Organization: env.orgID, Email: "newhire@acme.test", Role: "MEMBER", Active: true,
	}); err != nil {
		t.Fatalf("CreateUser(newhire): %v", err)
	}
	rr := env.do(t, http.MethodPost, "/organization/members", env.tokens["ADMIN"], `{"email":"newhire@acme.test","role":"VIEWER"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("ADMIN add: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if err := env.identities.CreateUser(context.Background(), &auth.User{
		ID: "emp-3", Organization: env.orgID, Email: "emp3@acme.test", Role: "MEMBER", Active: true,
	}); err != nil {
		t.Fatalf("CreateUser(emp3): %v", err)
	}
	rr = env.do(t, http.MethodPost, "/organization/members", env.tokens["ADMIN"], `{"email":"emp3@acme.test","role":"MEMBER"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("ADMIN add existing: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodPatch, "/organization/members/emp-3", env.tokens["ADMIN"], `{"role":"ADMIN"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("ADMIN patch role: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodDelete, "/organization/members/emp-3", env.tokens["ADMIN"], "")
	if rr.Code != http.StatusOK {
		t.Fatalf("ADMIN remove: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// VIEWER: can read the org, cannot list members, cannot write.
	if rr := env.do(t, http.MethodGet, "/organization", env.tokens["VIEWER"], ""); rr.Code != http.StatusOK {
		t.Fatalf("VIEWER GET /organization: expected 200, got %d", rr.Code)
	}
	if rr := env.do(t, http.MethodGet, "/organization/members", env.tokens["VIEWER"], ""); rr.Code != http.StatusForbidden {
		t.Fatalf("VIEWER list members: expected 403, got %d", rr.Code)
	}
	if rr := env.do(t, http.MethodPost, "/organization/members", env.tokens["VIEWER"], `{"email":"emp3@acme.test","role":"MEMBER"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("VIEWER add: expected 403, got %d", rr.Code)
	}

	// Unauthenticated: 401 across the surface.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/organization/members"},
		{http.MethodPost, "/organization/members"},
		{http.MethodPatch, "/organization/members/emp-2"},
		{http.MethodDelete, "/organization/members/emp-2"},
	} {
		if rr := env.do(t, tc.method, tc.path, "", ""); rr.Code != http.StatusUnauthorized {
			t.Fatalf("unauth %s %s: expected 401, got %d", tc.method, tc.path, rr.Code)
		}
	}
}

// TestOrganizationMembersCrossOrgIsolation proves the tenant guards: ids and
// memberships of another org are invisible (404 without existence leak) and
// a foreign member's ACCOUNT is never touched by this surface.
func TestOrganizationMembersCrossOrgIsolation(t *testing.T) {
	env := newOrgHandlerEnv(t)
	ctx := context.Background()

	// Beta has its own owner identity with a working login; the Beta token
	// is bound to the Beta tenant id (created in newOrgHandlerEnv).
	betaToken := env.tokenFor(t, "beta-owner", "owner@beta.test", "OWNER", env.otherOrgID)

	// An org-1 owner token cannot see, patch or remove Beta's owner id —
	// and Beta's login is untouched by any of it.
	rr := env.do(t, http.MethodPatch, "/organization/members/beta-owner", env.ownerToken, `{"role":"MEMBER"}`)
	if rr.Code != http.StatusNotFound || orgErrCode(t, rr) != "MEMBER_NOT_FOUND" {
		t.Fatalf("cross-org patch: expected 404 MEMBER_NOT_FOUND, got %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodDelete, "/organization/members/beta-owner", env.ownerToken, "")
	if rr.Code != http.StatusNotFound || orgErrCode(t, rr) != "MEMBER_NOT_FOUND" {
		t.Fatalf("cross-org delete: expected 404 MEMBER_NOT_FOUND, got %d %s", rr.Code, rr.Body.String())
	}
	if user, err := env.identities.GetUserByID(ctx, "beta-owner"); err != nil || !user.Active {
		t.Fatalf("cross-org delete must not deactivate the foreign account (user=%v err=%v)", user, err)
	}
	if _, err := env.authSvc.LoginCtx(ctx, "owner@beta.test", "beta-pass-1"); err != nil {
		t.Fatalf("foreign login must keep working, got %v", err)
	}

	// Beta's own view of the world: an empty member list (its owner has no
	// membership row), its own org info and no leakage of Acme rows.
	rr = env.do(t, http.MethodGet, "/organization/members", betaToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("beta list: expected 200, got %d", rr.Code)
	}
	if items, _ := decodeOrgBody(t, rr)["members"].([]any); len(items) != 0 {
		t.Fatalf("beta must see an empty member list, got %s", rr.Body.String())
	}
	rr = env.do(t, http.MethodGet, "/organization", betaToken, "")
	body := decodeOrgBody(t, rr)
	orgObj := body["organization"].(map[string]any)
	if orgObj["id"] == env.orgID {
		t.Fatalf("beta org info leaked Acme's tenant: %v", orgObj)
	}

	// An unknown user id is the same 404 as a foreign one (no existence
	// leak within the tenant either).
	rr = env.do(t, http.MethodDelete, "/organization/members/ghost", env.ownerToken, "")
	if rr.Code != http.StatusNotFound || orgErrCode(t, rr) != "MEMBER_NOT_FOUND" {
		t.Fatalf("unknown member: expected 404 MEMBER_NOT_FOUND, got %d %s", rr.Code, rr.Body.String())
	}
}

// TestOrganizationMembersAddValidation pins the add-member error contract:
// unknown email 404 (no unregistered invites), duplicate 409, blank email
// and invalid role 422, malformed JSON 400.
func TestOrganizationMembersAddValidation(t *testing.T) {
	env := newOrgHandlerEnv(t)

	// Unknown email: 404 USER_NOT_FOUND.
	rr := env.do(t, http.MethodPost, "/organization/members", env.ownerToken, `{"email":"nobody@acme.test","role":"MEMBER"}`)
	if rr.Code != http.StatusNotFound || orgErrCode(t, rr) != "USER_NOT_FOUND" {
		t.Fatalf("unknown email: expected 404 USER_NOT_FOUND, got %d %s", rr.Code, rr.Body.String())
	}

	// Duplicate membership: 409 ALREADY_MEMBER (owner-1 is seeded).
	rr = env.do(t, http.MethodPost, "/organization/members", env.ownerToken, `{"email":"owner@acme.test","role":"MEMBER"}`)
	if rr.Code != http.StatusConflict || orgErrCode(t, rr) != "ALREADY_MEMBER" {
		t.Fatalf("duplicate add: expected 409 ALREADY_MEMBER, got %d %s", rr.Code, rr.Body.String())
	}

	// Blank email and invalid role: 422 VALIDATION_ERROR.
	rr = env.do(t, http.MethodPost, "/organization/members", env.ownerToken, `{"email":"  ","role":"MEMBER"}`)
	if rr.Code != http.StatusUnprocessableEntity || orgErrCode(t, rr) != "VALIDATION_ERROR" {
		t.Fatalf("blank email: expected 422 VALIDATION_ERROR, got %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodPost, "/organization/members", env.ownerToken, `{"email":"new@acme.test","role":"SUPERADMIN"}`)
	if rr.Code != http.StatusUnprocessableEntity || orgErrCode(t, rr) != "VALIDATION_ERROR" {
		t.Fatalf("invalid role: expected 422 VALIDATION_ERROR, got %d %s", rr.Code, rr.Body.String())
	}

	// Malformed JSON: 400 INVALID_REQUEST (validated before any lookup).
	rr = env.do(t, http.MethodPost, "/organization/members", env.ownerToken, "{not json")
	if rr.Code != http.StatusBadRequest || orgErrCode(t, rr) != "INVALID_REQUEST" {
		t.Fatalf("malformed body: expected 400 INVALID_REQUEST, got %d %s", rr.Code, rr.Body.String())
	}

	// PATCH with an invalid role is 422 before any membership lookup.
	rr = env.do(t, http.MethodPatch, "/organization/members/owner-1", env.ownerToken, `{"role":""}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("patch blank role: expected 422, got %d %s", rr.Code, rr.Body.String())
	}
}

// TestOrganizationMembersAuditTrail verifies the audit rows for all three
// mutating operations: actor, action names, tenant scope, resource path and
// the role metadata (never any credential material).
func TestOrganizationMembersAuditTrail(t *testing.T) {
	env := newOrgHandlerEnv(t)
	memberID := env.seedMember(t, "emp-4", "emp4@acme.test", "MEMBER")

	if rr := env.do(t, http.MethodPatch, "/organization/members/"+memberID, env.ownerToken, `{"role":"ADMIN"}`); rr.Code != http.StatusOK {
		t.Fatalf("patch: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := env.do(t, http.MethodDelete, "/organization/members/"+memberID, env.ownerToken, ""); rr.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// A rejected operation must not write an audit row for the mutation
	// itself (only the three successful ones exist plus the seed add).
	if rr := env.do(t, http.MethodPatch, "/organization/members/owner-1", env.ownerToken, `{"role":"MEMBER"}`); rr.Code != http.StatusConflict {
		t.Fatalf("guard patch: expected 409, got %d", rr.Code)
	}

	entries := env.auditSvc.List()
	want := map[string]bool{
		"organization.member_added":        false,
		"organization.member_role_updated": false,
		"organization.member_removed":      false,
	}
	for _, entry := range entries {
		switch entry.Action {
		case "organization.member_added":
			want["organization.member_added"] = true
			if entry.Actor != "owner-1" || entry.OrganizationID != env.orgID {
				t.Fatalf("added audit row actor/org wrong: %+v", entry)
			}
			if entry.Resource != "organization/members/emp-4" {
				t.Fatalf("added audit row resource wrong: %+v", entry)
			}
			if entry.Metadata["email"] != "emp4@acme.test" || entry.Metadata["role"] != "MEMBER" {
				t.Fatalf("added audit row metadata wrong: %+v", entry.Metadata)
			}
		case "organization.member_role_updated":
			want["organization.member_role_updated"] = true
			if entry.Metadata["from_role"] != "MEMBER" || entry.Metadata["to_role"] != "ADMIN" {
				t.Fatalf("role-change audit metadata wrong: %+v", entry.Metadata)
			}
		case "organization.member_removed":
			want["organization.member_removed"] = true
			if entry.Metadata["role"] != "ADMIN" {
				t.Fatalf("removed audit metadata should carry the role at removal: %+v", entry.Metadata)
			}
		}
		if strings.Contains(entry.Resource, "password") {
			t.Fatalf("audit rows must never carry credential material: %+v", entry)
		}
	}
	for action, seen := range want {
		if !seen {
			t.Fatalf("missing audit action %s in %d entries", action, len(entries))
		}
	}
	if len(entries) != 3 {
		t.Fatalf("rejected operations must not audit, got %d entries: %+v", len(entries), entries)
	}
}

// TestOrganizationMembersWorkWithoutAuditService mirrors the api-keys
// nil-audit variant: the surface is fully usable without an audit service.
func TestOrganizationMembersWorkWithoutAuditService(t *testing.T) {
	env := newOrgHandlerEnvWithAudit(t, nil)

	rr := env.do(t, http.MethodPost, "/organization/members", env.ownerToken, `{"email":"x@acme.test","role":"MEMBER"}`)
	if rr.Code != http.StatusNotFound {
		// x@acme.test is not an existing identity — the nil audit service
		// must not change the 404 contract.
		t.Fatalf("unknown email with nil audit: expected 404, got %d %s", rr.Code, rr.Body.String())
	}
	env.seedMember(t, "emp-5", "emp5@acme.test", "MEMBER")
	if rr := env.do(t, http.MethodDelete, "/organization/members/emp-5", env.ownerToken, ""); rr.Code != http.StatusOK {
		t.Fatalf("delete with nil audit: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestOrganizationMembersMethodMatrix pins 405s the mux derives from method
// patterns (collection supports GET/POST only; the member path supports
// PATCH/DELETE only).
func TestOrganizationMembersMethodMatrix(t *testing.T) {
	env := newOrgHandlerEnv(t)

	if rr := env.do(t, http.MethodPut, "/organization/members", env.ownerToken, `{"email":"x@acme.test"}`); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT collection: expected 405, got %d", rr.Code)
	}
	if rr := env.do(t, http.MethodGet, "/organization/members/owner-1", env.ownerToken, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET single member: expected 405, got %d", rr.Code)
	}
	if rr := env.do(t, http.MethodPost, "/organization/members/owner-1", env.ownerToken, `{}`); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST single member: expected 405, got %d", rr.Code)
	}
	if rr := env.do(t, http.MethodDelete, "/organization", env.ownerToken, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /organization: expected 405, got %d", rr.Code)
	}
}
