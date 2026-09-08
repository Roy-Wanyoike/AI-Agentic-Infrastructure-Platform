package main

// Issue #79 handler tests — DELETE /scim/tokens/{id}: the full middleware
// chain (RequireAuthOrAPIKey -> RequirePermission(users.manage) = exactly
// OWNER/ADMIN), org-scoped revocation (cross-org ids are 404 without an
// existence leak), the end-to-end "a revoked credential authenticates as
// nothing at all" contract, the idempotency behavior of the existing
// scim.Service.RevokeToken (in-memory store re-confirms as 204) and the
// audit row (no credential material, failed revocations write nothing).
// Reuses the scimHandlerEnv harness from scim_test.go — one shared identity
// table between authSvc and the SCIM service.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/scim"
)

// revokeTokenID resolves the stored id of a minted secret through
// scim.Service.Authenticate (the mint response carries the same id).
func revokeTokenID(t *testing.T, env *scimHandlerEnv, secret string) string {
	t.Helper()
	token, err := env.svc.Authenticate(t.Context(), secret)
	if err != nil {
		t.Fatalf("Authenticate(minted secret): %v", err)
	}
	return token.ID
}

// TestSCIMTokenRevokeLifecycle walks mint -> DELETE (204, empty body) ->
// the secret fails Authenticate AND the protocol surface -> the in-memory
// store re-confirms a repeated revoke as 204 (existing RevokeToken
// semantics) -> unknown ids are a structured 404.
func TestSCIMTokenRevokeLifecycle(t *testing.T) {
	env := newScimHandlerEnv(t)
	secret := env.mintToken(t)
	id := revokeTokenID(t, env, secret)

	rr := env.do(t, http.MethodDelete, "/scim/tokens/"+id, env.ownerToken, "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE /scim/tokens/{id}: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("204 must carry no body, got %q", rr.Body.String())
	}

	// The revoked secret authenticates as nothing at all — on the service
	// and on the bearer-plane surface.
	if _, err := env.svc.Authenticate(t.Context(), secret); !errors.Is(err, scim.ErrTokenInvalid) {
		t.Fatalf("revoked secret must fail Authenticate with ErrTokenInvalid, got %v", err)
	}
	if rr := env.do(t, http.MethodGet, "/scim/v2/Users", secret, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked secret on protocol surface: expected 401, got %d", rr.Code)
	}

	// Re-revoking the same id stays 204: the in-memory store pins the
	// idempotent RevokeToken behavior. (The Postgres store would 404 an
	// already-revoked id — its org-guarded UPDATE only matches live rows;
	// documented in the handler and the fragment, never un-revokes.)
	if rr := env.do(t, http.MethodDelete, "/scim/tokens/"+id, env.ownerToken, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("re-revoke: expected idempotent 204, got %d", rr.Code)
	}

	// Unknown ids are 404 with the shared structured error envelope.
	rr = env.do(t, http.MethodDelete, "/scim/tokens/does-not-exist", env.ownerToken, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id: expected 404, got %d", rr.Code)
	}
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("404 body is not JSON: %s", rr.Body.String())
	}
	if errBody.Error.Code != "SCIM_TOKEN_NOT_FOUND" {
		t.Fatalf("expected SCIM_TOKEN_NOT_FOUND envelope, got %s", rr.Body.String())
	}
}

// TestSCIMTokenRevokeCrossOrgIsolated pins the org-scoped guard: an OWNER of
// another organization gets 404 (same as unknown ids — no existence leak)
// and the foreign revoke attempt leaves the credential fully live.
func TestSCIMTokenRevokeCrossOrgIsolated(t *testing.T) {
	env := newScimHandlerEnv(t)
	secret := env.mintToken(t)
	id := revokeTokenID(t, env, secret)

	foreign, err := env.authSvc.GenerateToken(&auth.User{
		ID: "owner-2", Organization: "org-2", Email: "owner@other.test", Role: "OWNER", Active: true,
	})
	if err != nil {
		t.Fatalf("GenerateToken(org-2 owner): %v", err)
	}

	if rr := env.do(t, http.MethodDelete, "/scim/tokens/"+id, foreign, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org revoke: expected 404, got %d", rr.Code)
	}
	if _, err := env.svc.Authenticate(t.Context(), secret); err != nil {
		t.Fatalf("cross-org attempt must not revoke: %v", err)
	}
	if rr := env.do(t, http.MethodGet, "/scim/v2/Users", secret, ""); rr.Code != http.StatusOK {
		t.Fatalf("cross-org attempt must leave the credential usable: %d", rr.Code)
	}
}

// TestSCIMTokenRevokeRBAC pins the issue #79 matrix on the revoke route:
// unauthenticated 401; MEMBER/VIEWER 403 with no mutation; ADMIN is granted
// (users.manage = exactly OWNER/ADMIN — minting stays OWNER-only) and the
// ADMIN revocation actually retires the credential. DELETE /scim/tokens
// (no id) stays a 405 like every other method/path mismatch.
func TestSCIMTokenRevokeRBAC(t *testing.T) {
	env := newScimHandlerEnv(t)
	secret := env.mintToken(t)
	id := revokeTokenID(t, env, secret)

	if rr := env.do(t, http.MethodDelete, "/scim/tokens/"+id, "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated revoke: expected 401, got %d", rr.Code)
	}
	for _, role := range []string{"MEMBER", "VIEWER"} {
		rr := env.do(t, http.MethodDelete, "/scim/tokens/"+id, env.otherTokens[role], "")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s revoke: expected 403, got %d", role, rr.Code)
		}
	}
	if _, err := env.svc.Authenticate(t.Context(), secret); err != nil {
		t.Fatalf("403 attempts must not revoke: %v", err)
	}

	if rr := env.do(t, http.MethodDelete, "/scim/tokens", env.ownerToken, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /scim/tokens (no id): expected 405, got %d", rr.Code)
	}

	if rr := env.do(t, http.MethodDelete, "/scim/tokens/"+id, env.otherTokens["ADMIN"], ""); rr.Code != http.StatusNoContent {
		t.Fatalf("ADMIN revoke: expected 204 (users.manage), got %d", rr.Code)
	}
	if _, err := env.svc.Authenticate(t.Context(), secret); err == nil {
		t.Fatal("ADMIN revocation must retire the credential")
	}
}

// TestSCIMTokenRevokeAudited registers a dedicated mux with an audit service
// (leaving the shared harness untouched): a successful DELETE writes exactly
// one scim_token.revoked row — actor, org and resource pinned, metadata
// without credential material — while failed (404/403) revocations write
// nothing.
func TestSCIMTokenRevokeAudited(t *testing.T) {
	env := newScimHandlerEnv(t)
	auditSvc := audit.NewService()
	audited := http.NewServeMux()
	registerScimRoutes(audited, env.svc, env.authSvc, apikeys.NewService(), auditSvc)

	do := func(method, path, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(""))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		audited.ServeHTTP(rr, req)
		return rr
	}

	secret := env.mintToken(t)
	id := revokeTokenID(t, env, secret)

	// Failures first: unknown id (404) and forbidden role (403) write nothing.
	if rr := do(http.MethodDelete, "/scim/tokens/does-not-exist", env.ownerToken); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id: expected 404, got %d", rr.Code)
	}
	if rr := do(http.MethodDelete, "/scim/tokens/"+id, env.otherTokens["MEMBER"]); rr.Code != http.StatusForbidden {
		t.Fatalf("MEMBER revoke: expected 403, got %d", rr.Code)
	}
	entries, err := auditSvc.ListCtx(t.Context(), env.orgID)
	if err != nil {
		t.Fatalf("ListCtx: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed revocations must write no audit rows, got %d", len(entries))
	}

	// The successful OWNER revoke writes exactly one row.
	if rr := do(http.MethodDelete, "/scim/tokens/"+id, env.ownerToken); rr.Code != http.StatusNoContent {
		t.Fatalf("OWNER revoke: expected 204, got %d", rr.Code)
	}
	entries, err = auditSvc.ListCtx(t.Context(), env.orgID)
	if err != nil {
		t.Fatalf("ListCtx: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 audit row, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Action != "scim_token.revoked" || entry.Actor != "owner-1" || entry.OrganizationID != env.orgID {
		t.Fatalf("unexpected audit row: action=%q actor=%q org=%q", entry.Action, entry.Actor, entry.OrganizationID)
	}
	if entry.Resource != "scim/tokens/"+id {
		t.Fatalf("audit resource must name the revoked token, got %q", entry.Resource)
	}
	if entry.Metadata != nil {
		t.Fatalf("audit metadata must carry no credential material, got %v", entry.Metadata)
	}
	if raw, merr := json.Marshal(entry); merr != nil {
		t.Fatalf("marshal audit entry: %v", merr)
	} else if strings.Contains(string(raw), secret) {
		t.Fatalf("audit trail leaked secret material: %s", raw)
	}
}
