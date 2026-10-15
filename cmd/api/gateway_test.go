package main

// gateway_test.go — wave-9 issue #139 HTTP surface, exercised through the
// REAL middleware chain (RequireAuthOrAPIKey -> RequirePermission): the
// plaintext-shown-once contract, the allow-list nil/[]/list JSON round-trip,
// the OWNER/ADMIN (users.manage) role matrix, cross-tenant 404s, the spend
// endpoint (month validation, remaining math, unlimited null), denial error
// shapes (402/403) and the lifecycle audit rows.

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
	authpkg "agentos/internal/auth"
	"agentos/internal/models"
)

// --- fixtures ----------------------------------------------------------------

func newGatewayTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *models.GatewayService, *audit.Service) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	auditSvc := audit.NewService()
	gw := models.NewGatewayService()
	gw.SetAuditor(auditSvc)

	apiMux := http.NewServeMux()
	registerGatewayRoutes(apiMux, gw, authSvc, keysSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, gw, auditSvc
}

func gwTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
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

func doGwReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func gwDecode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

// gwCreateKey POSTs one key and fails the test on any non-201.
func gwCreateKey(t *testing.T, h http.Handler, token, body string) map[string]any {
	t.Helper()
	rr := doGwReq(t, h, http.MethodPost, "/api/v1/gateway/keys", token, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	return gwDecode(t, rr)
}

// --- auth + RBAC --------------------------------------------------------------

func TestGatewayAuthRequired(t *testing.T) {
	h, _, _, _ := newGatewayTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/gateway/keys"},
		{http.MethodGet, "/api/v1/gateway/keys"},
		{http.MethodGet, "/api/v1/gateway/keys/some-id"},
		{http.MethodPatch, "/api/v1/gateway/keys/some-id"},
		{http.MethodDelete, "/api/v1/gateway/keys/some-id"},
		{http.MethodGet, "/api/v1/gateway/keys/some-id/spend"},
	} {
		rr := doGwReq(t, h, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestGatewayRoleMatrix(t *testing.T) {
	h, authSvc, _, _ := newGatewayTestRouter(t)
	const org = "org-gw"
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = gwTokenFor(t, authSvc, strings.ToLower(role)+"@x.io", org, role)
	}

	// Writes AND reads sit on the users.manage tier (OWNER/ADMIN): virtual
	// keys decide what models a credential may touch and how much it may
	// spend — account administration, the budgets-write tier.
	created := gwCreateKey(t, h, tokens["OWNER"], `{"label":"owner key"}`)
	id := created["key"].(map[string]any)["id"].(string)
	if rr := doGwReq(t, h, http.MethodPost, "/api/v1/gateway/keys", tokens["ADMIN"], `{"label":"admin key"}`); rr.Code != http.StatusCreated {
		t.Fatalf("ADMIN create: expected 201, got %d", rr.Code)
	}
	if rr := doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys", tokens["ADMIN"], ""); rr.Code != http.StatusOK {
		t.Fatalf("ADMIN list: expected 200, got %d", rr.Code)
	}
	for _, role := range []string{"MEMBER", "VIEWER"} {
		if rr := doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys", tokens[role], ""); rr.Code != http.StatusForbidden {
			t.Errorf("%s list: expected 403, got %d", role, rr.Code)
		}
		if rr := doGwReq(t, h, http.MethodPost, "/api/v1/gateway/keys", tokens[role], `{"label":"nope"}`); rr.Code != http.StatusForbidden {
			t.Errorf("%s create: expected 403, got %d", role, rr.Code)
		}
		if rr := doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys/"+id+"/spend", tokens[role], ""); rr.Code != http.StatusForbidden {
			t.Errorf("%s spend: expected 403, got %d", role, rr.Code)
		}
	}
}

// --- plaintext-shown-once + view hygiene --------------------------------------

func TestGatewayCreateShowsPlaintextOnce(t *testing.T) {
	h, authSvc, gw, _ := newGatewayTestRouter(t)
	token := gwTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	created := gwCreateKey(t, h, token, `{"label":"ops","models":["gpt-4o"],"monthly_limit_cents":500}`)
	plaintext, _ := created["plaintext"].(string)
	if !strings.HasPrefix(plaintext, "vk_") {
		t.Fatalf("plaintext must start with vk_: %q", plaintext)
	}
	view := created["key"].(map[string]any)
	if view["models"] == nil || len(view["models"].([]any)) != 1 {
		t.Fatalf("listed allow-list must round-trip: %v", view["models"])
	}
	if view["status"] != "active" || view["monthly_limit_cents"].(float64) != 500 {
		t.Fatalf("key view wrong: %v", view)
	}

	// The key view (and every later read) never carries the plaintext or a
	// hash — and the service layer keeps it that way.
	rr := doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys", token, "")
	list := gwDecode(t, rr)
	keys := list["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), plaintext) || strings.Contains(string(raw), "key_hash") {
		t.Fatalf("list response leaks credential material: %s", raw)
	}
	// Resolution still works through the auth path (worker side).
	if _, err := gw.ResolveByPlaintext(context.Background(), plaintext); err != nil {
		t.Fatalf("plaintext must resolve: %v", err)
	}
}

// --- CRUD + validation ---------------------------------------------------------

func TestGatewayCRUDAndValidation(t *testing.T) {
	h, authSvc, _, _ := newGatewayTestRouter(t)
	token := gwTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	// Validation errors -> 400.
	for _, body := range []string{
		`{}`,
		`{"label":"  "}`,
		`{"label":"x","monthly_limit_cents":-5}`,
		`{"label":"x","models":"gpt-4o"}`,
		`{"label":"x","models":["  "]}`,
	} {
		if rr := doGwReq(t, h, http.MethodPost, "/api/v1/gateway/keys", token, body); rr.Code != http.StatusBadRequest {
			t.Errorf("create %s: expected 400, got %d body=%s", body, rr.Code, rr.Body.String())
		}
	}

	created := gwCreateKey(t, h, token, `{"label":"ops","models":["gpt-4o"],"monthly_limit_cents":500}`)
	id := created["key"].(map[string]any)["id"].(string)

	// GET item + GET unknown.
	if rr := doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys/"+id, token, ""); rr.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", rr.Code)
	}
	if rr := doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys/missing", token, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("get unknown: expected 404, got %d", rr.Code)
	}

	// PATCH allow-list states: [] -> NONE, null -> ALL (nil), list -> listed.
	rr := doGwReq(t, h, http.MethodPatch, "/api/v1/gateway/keys/"+id, token, `{"models":[]}`)
	patched := gwDecode(t, rr)["key"].(map[string]any)
	if models := patched["models"].([]any); models == nil || len(models) != 0 {
		t.Fatalf("[] must decode as enabled-and-empty, got %#v", patched["models"])
	}
	rr = doGwReq(t, h, http.MethodPatch, "/api/v1/gateway/keys/"+id, token, `{"models":null,"monthly_limit_cents":0}`)
	patched = gwDecode(t, rr)["key"].(map[string]any)
	if _, has := patched["models"]; !has || patched["models"] != nil {
		t.Fatalf("null must decode as allow-list DISABLED (null), got %#v", patched["models"])
	}
	rr = doGwReq(t, h, http.MethodPatch, "/api/v1/gateway/keys/"+id, token, `{"label":"renamed","models":["o3-mini"],"monthly_limit_cents":42}`)
	patched = gwDecode(t, rr)["key"].(map[string]any)
	if patched["label"] != "renamed" || patched["monthly_limit_cents"].(float64) != 42 {
		t.Fatalf("patch fields not applied: %v", patched)
	}
	if models := patched["models"].([]any); len(models) != 1 || models[0] != "o3-mini" {
		t.Fatalf("listed patch not applied: %v", patched["models"])
	}
	if rr := doGwReq(t, h, http.MethodPatch, "/api/v1/gateway/keys/"+id, token, `{"label":""}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty label patch: expected 400, got %d", rr.Code)
	}
	if rr := doGwReq(t, h, http.MethodPatch, "/api/v1/gateway/keys/missing", token, `{"label":"x"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("patch unknown: expected 404, got %d", rr.Code)
	}

	// Expiry set + clear via PATCH.
	rr = doGwReq(t, h, http.MethodPatch, "/api/v1/gateway/keys/"+id, token, `{"expires_at":"2030-01-01T00:00:00Z"}`)
	if _, has := gwDecode(t, rr)["key"].(map[string]any)["expires_at"]; !has {
		t.Fatalf("expires_at must render when set")
	}
	rr = doGwReq(t, h, http.MethodPatch, "/api/v1/gateway/keys/"+id, token, `{"expires_at":null}`)
	if _, has := gwDecode(t, rr)["key"].(map[string]any)["expires_at"]; has {
		t.Fatalf("expires_at null must clear the expiry")
	}

	// DELETE is idempotent on the key it revoked; unknown stays 404.
	if rr := doGwReq(t, h, http.MethodDelete, "/api/v1/gateway/keys/"+id, token, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", rr.Code)
	}
	if rr := doGwReq(t, h, http.MethodDelete, "/api/v1/gateway/keys/"+id, token, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("re-delete must be idempotent, got %d", rr.Code)
	}
	if rr := doGwReq(t, h, http.MethodDelete, "/api/v1/gateway/keys/missing", token, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: expected 404, got %d", rr.Code)
	}
}

func TestGatewayCrossTenant404(t *testing.T) {
	h, authSvc, _, _ := newGatewayTestRouter(t)
	orgA := gwTokenFor(t, authSvc, "a@x.io", "org-a", "OWNER")
	orgB := gwTokenFor(t, authSvc, "b@x.io", "org-b", "OWNER")

	created := gwCreateKey(t, h, orgA, `{"label":"a-only"}`)
	id := created["key"].(map[string]any)["id"].(string)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/gateway/keys/" + id},
		{http.MethodPatch, "/api/v1/gateway/keys/" + id},
		{http.MethodDelete, "/api/v1/gateway/keys/" + id},
		{http.MethodGet, "/api/v1/gateway/keys/" + id + "/spend"},
	} {
		rr := doGwReq(t, h, tc.method, tc.path, orgB, `{"label":"x"}`)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s cross-org: expected 404 (no existence leak), got %d", tc.method, rr.Code)
		}
	}
	// Org B's list never includes org A's key.
	rr := doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys", orgB, "")
	if keys := gwDecode(t, rr)["keys"].([]any); len(keys) != 0 {
		t.Fatalf("org-b list must be empty, got %v", keys)
	}
}

// --- spend endpoint ------------------------------------------------------------

func TestGatewaySpendEndpoint(t *testing.T) {
	h, authSvc, gw, _ := newGatewayTestRouter(t)
	token := gwTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	created := gwCreateKey(t, h, token, `{"label":"capped","monthly_limit_cents":1000}`)
	id := created["key"].(map[string]any)["id"].(string)
	now := time.Now().UTC()
	if err := gw.RecordSpend(context.Background(), "org-a", id, "gpt-4o", 250.5, now); err != nil {
		t.Fatalf("seed spend: %v", err)
	}

	// Default month = current UTC month.
	rr := doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys/"+id+"/spend", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("spend: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	out := gwDecode(t, rr)
	if out["used_cents"].(float64) != 250.5 || out["limit_cents"].(float64) != 1000 {
		t.Fatalf("spend numbers wrong: %v", out)
	}
	if out["remaining_cents"].(float64) != 749.5 {
		t.Fatalf("remaining wrong: %v", out["remaining_cents"])
	}
	if out["month"].(string) != now.Format("2006-01") {
		t.Fatalf("month bucket wrong: %v", out["month"])
	}

	// Explicit month query: an empty bucket, then an invalid one.
	rr = doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys/"+id+"/spend?month=2024-01", token, "")
	out = gwDecode(t, rr)
	if out["used_cents"].(float64) != 0 || out["month"] != "2024-01" {
		t.Fatalf("explicit month wrong: %v", out)
	}
	if rr = doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys/"+id+"/spend?month=2024-13", token, ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid month: expected 400, got %d", rr.Code)
	}
	if rr = doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys/"+id+"/spend?month=nope", token, ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("garbage month: expected 400, got %d", rr.Code)
	}
	if rr = doGwReq(t, h, http.MethodPost, "/api/v1/gateway/keys/"+id+"/spend", token, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("spend is read-only: expected 405, got %d", rr.Code)
	}

	// Unlimited keys expose a null remaining (limit sentinel 0).
	unlimited := gwCreateKey(t, h, token, `{"label":"unlimited"}`)
	uid := unlimited["key"].(map[string]any)["id"].(string)
	rr = doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys/"+uid+"/spend", token, "")
	out = gwDecode(t, rr)
	if out["limit_cents"].(float64) != 0 || out["remaining_cents"] != nil {
		t.Fatalf("unlimited must render null remaining: %v", out)
	}
	// Unknown key: 404 before the ledger is read.
	if rr = doGwReq(t, h, http.MethodGet, "/api/v1/gateway/keys/missing/spend", token, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown spend: expected 404, got %d", rr.Code)
	}
}

// --- denial error shapes --------------------------------------------------------

func TestGatewayDenialErrorShapes(t *testing.T) {
	rr := httptest.NewRecorder()
	if mapGatewayDenial(rr, &models.SpendLimitError{KeyID: "k1", LimitCents: 1000, UsedCents: 900, ProjectedCents: 1000.5}) != true {
		t.Fatal("spend denial must map")
	}
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("spend denial status: expected 402, got %d", rr.Code)
	}
	body := gwDecode(t, rr)["error"].(map[string]any)
	if body["code"] != "spend_limit_exceeded" || body["key_id"] != "k1" || body["limit_cents"].(float64) != 1000 {
		t.Fatalf("spend denial shape wrong: %v", body)
	}

	rr = httptest.NewRecorder()
	if !mapGatewayDenial(rr, &models.ModelNotAllowedError{KeyID: "k1", Model: "gpt-4o"}) {
		t.Fatal("model denial must map")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("model denial status: expected 403, got %d", rr.Code)
	}
	body = gwDecode(t, rr)["error"].(map[string]any)
	if body["code"] != "model_not_allowed" || body["model"] != "gpt-4o" {
		t.Fatalf("model denial shape wrong: %v", body)
	}

	rr = httptest.NewRecorder()
	if mapGatewayDenial(rr, errors.New("boom")) {
		t.Fatal("non-denial errors must not map")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("unmapped error must leave the response untouched, got %d", rr.Code)
	}
}

// --- lifecycle audit over the wire ----------------------------------------------

func TestGatewayLifecycleAudited(t *testing.T) {
	h, authSvc, _, auditSvc := newGatewayTestRouter(t)
	token := gwTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	created := gwCreateKey(t, h, token, `{"label":"audited","models":["gpt-4o"],"monthly_limit_cents":10}`)
	id := created["key"].(map[string]any)["id"].(string)
	if rr := doGwReq(t, h, http.MethodDelete, "/api/v1/gateway/keys/"+id, token, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("revoke: expected 204, got %d", rr.Code)
	}

	entries, err := auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	createdSeen, revokedSeen := false, false
	for _, e := range entries {
		switch e.Action {
		case models.AuditGatewayKeyCreated:
			createdSeen = true
			if e.Metadata["key_id"] != id || e.Metadata["prefix"] == nil {
				t.Fatalf("create audit metadata wrong: %v", e.Metadata)
			}
		case models.AuditGatewayKeyRevoked:
			revokedSeen = true
			if e.Metadata["key_id"] != id {
				t.Fatalf("revoke audit metadata wrong: %v", e.Metadata)
			}
		}
	}
	if !createdSeen || !revokedSeen {
		t.Fatalf("lifecycle audit rows missing (created=%v revoked=%v)", createdSeen, revokedSeen)
	}
}
