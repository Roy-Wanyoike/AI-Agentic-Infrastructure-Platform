package main

// residency_test.go — wave-9 issue #140 HTTP surface, exercised through the
// REAL middleware chain (RequireAuthOrAPIKey -> RequirePermission): the
// policy lifecycle (default unpinned -> PUT pin), validation codes, the
// strict-deny 403 {"error":{"code":"residency_violation"}} contract with
// HOST-ONLY evidence (query strings must never leak), posture reflection
// incl. last_violation, org isolation over the wire, the audit rows emitted
// through the service's AuditPublisher seam, and the registration nil-guards.
// Companion test of cmd/api/residency.go (mcp_trust_test.go precedent).

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
	"agentos/internal/tenant"
)

func newResidencyTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *audit.Service, *tenant.ResidencyService) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	auditSvc := audit.NewService()
	svc := tenant.NewResidencyService()
	apiMux := http.NewServeMux()
	registerResidencyRoutes(apiMux, svc, authSvc, keysSvc, auditSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, auditSvc, svc
}

func resTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&authpkg.User{ID: "user-" + email, Organization: orgID, Email: email, Role: role})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

func resReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func resBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, rr.Body.String())
	}
	return out
}

func TestResidencyAuthRequired(t *testing.T) {
	h, _, _, _ := newResidencyTestRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/residency/policy"},
		{http.MethodPut, "/api/v1/residency/policy"},
		{http.MethodGet, "/api/v1/residency/posture"},
		{http.MethodPost, "/api/v1/residency/check"},
	} {
		if rr := resReq(t, h, tc.method, tc.path, "", ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d", tc.method, tc.path, rr.Code)
		}
	}
}

func TestResidencyRBACMatrix(t *testing.T) {
	h, authSvc, _, _ := newResidencyTestRouter(t)
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = resTokenFor(t, authSvc, strings.ToLower(role)+"@x.io", "org-a", role)
	}
	// GET policy: MEMBER+ (connectors.read reuse — VIEWER excluded).
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER"} {
		if rr := resReq(t, h, http.MethodGet, "/api/v1/residency/policy", tokens[role], ""); rr.Code != http.StatusOK {
			t.Errorf("GET policy as %s: want 200, got %d", role, rr.Code)
		}
	}
	if rr := resReq(t, h, http.MethodGet, "/api/v1/residency/policy", tokens["VIEWER"], ""); rr.Code != http.StatusForbidden {
		t.Errorf("GET policy as VIEWER: want 403, got %d", rr.Code)
	}
	// PUT / posture / check: OWNER/ADMIN (users.manage reuse — org security
	// configuration, same tier as budget ceilings).
	for _, role := range []string{"MEMBER", "VIEWER"} {
		if rr := resReq(t, h, http.MethodPut, "/api/v1/residency/policy", tokens[role], `{"pinned_region":"eu-west"}`); rr.Code != http.StatusForbidden {
			t.Errorf("PUT policy as %s: want 403, got %d", role, rr.Code)
		}
		if rr := resReq(t, h, http.MethodGet, "/api/v1/residency/posture", tokens[role], ""); rr.Code != http.StatusForbidden {
			t.Errorf("posture as %s: want 403, got %d", role, rr.Code)
		}
		if rr := resReq(t, h, http.MethodPost, "/api/v1/residency/check", tokens[role], `{"url":"https://x.io"}`); rr.Code != http.StatusForbidden {
			t.Errorf("check as %s: want 403, got %d", role, rr.Code)
		}
	}
	for _, role := range []string{"OWNER", "ADMIN"} {
		if rr := resReq(t, h, http.MethodPut, "/api/v1/residency/policy", tokens[role], `{"pinned_region":"eu-west"}`); rr.Code != http.StatusOK {
			t.Errorf("PUT policy as %s: want 200, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}
}

func TestResidencyPolicyLifecycleAndValidation(t *testing.T) {
	h, authSvc, _, _ := newResidencyTestRouter(t)
	token := resTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")
	const policyPath = "/api/v1/residency/policy"

	// Default: the org has no stored policy -> 200 unpinned (never 404).
	rr := resReq(t, h, http.MethodGet, policyPath, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET default policy: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := resBody(t, rr)
	if body["mode"] != "unpinned" {
		t.Fatalf("default mode = %v, want unpinned", body["mode"])
	}
	policy := body["policy"].(map[string]any)
	if policy["pinned_region"] != "" || policy["strict_mode"] != false {
		t.Fatalf("default policy wrong: %v", policy)
	}

	// Validation: unknown region -> 400 VALIDATION_ERROR.
	rr = resReq(t, h, http.MethodPut, policyPath, token, `{"pinned_region":"mars-central"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid region: want 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := resBody(t, rr)["error"].(map[string]any)["code"]; got != "VALIDATION_ERROR" {
		t.Fatalf("error code = %v, want VALIDATION_ERROR", got)
	}

	// Validation: strict with nothing pinned is a misconfiguration -> 400.
	rr = resReq(t, h, http.MethodPut, policyPath, token, `{"strict_mode":true}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("strict-empty policy: want 400, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Valid PUT pins eu-central strict.
	rr = resReq(t, h, http.MethodPut, policyPath, token, `{"pinned_region":"eu-central","allowed_regions":["us-east"],"strict_mode":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT policy: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body = resBody(t, rr)
	if body["mode"] != "pinned" {
		t.Fatalf("PUT response mode = %v, want pinned", body["mode"])
	}
	policy = body["policy"].(map[string]any)
	if policy["pinned_region"] != "eu-central" || policy["strict_mode"] != true {
		t.Fatalf("stored policy wrong: %v", policy)
	}
	if got := fmtSlice(policy["allowed_regions"]); len(got) != 1 || got[0] != "us-east" {
		t.Fatalf("allowed_regions = %v, want [us-east]", policy["allowed_regions"])
	}

	// GET reflects the stored policy.
	rr = resReq(t, h, http.MethodGet, policyPath, token, "")
	if rr.Code != http.StatusOK || resBody(t, rr)["mode"] != "pinned" {
		t.Fatalf("GET after PUT: %d body=%s", rr.Code, rr.Body.String())
	}

	// Clearing the pin (empty non-strict policy) is legal and flips the mode
	// back to unpinned — the response mode is COMPUTED, never hardcoded.
	rr = resReq(t, h, http.MethodPut, policyPath, token, `{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("clearing PUT: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if body = resBody(t, rr); body["mode"] != "unpinned" {
		t.Fatalf("cleared policy mode = %v, want unpinned", body["mode"])
	}
}

func TestResidencyCheckContract(t *testing.T) {
	h, authSvc, _, _ := newResidencyTestRouter(t)
	token := resTokenFor(t, authSvc, "admin@x.io", "org-a", "ADMIN")

	// Missing url -> 400 VALIDATION_ERROR.
	if rr := resReq(t, h, http.MethodPost, "/api/v1/residency/check", token, `{"url":"  "}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty url: want 400, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Unpinned org: allowed, mode unpinned, region still classified.
	rr := resReq(t, h, http.MethodPost, "/api/v1/residency/check", token, `{"url":"https://dynamodb.us-east-1.amazonaws.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("unpinned check: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	verdict := resBody(t, rr)["verdict"].(map[string]any)
	if verdict["allowed"] != true || verdict["reason"] != "unpinned" || verdict["mode"] != "unpinned" || verdict["region"] != "us-east" {
		t.Fatalf("unpinned verdict wrong: %v", verdict)
	}

	// Pin eu-central strict; out-of-region deny -> the issue #140 403 shape.
	if rr := resReq(t, h, http.MethodPut, "/api/v1/residency/policy", token, `{"pinned_region":"eu-central","strict_mode":true}`); rr.Code != http.StatusOK {
		t.Fatalf("PUT policy: %d body=%s", rr.Code, rr.Body.String())
	}
	rr = resReq(t, h, http.MethodPost, "/api/v1/residency/check", token, `{"url":"https://dynamodb.us-east-1.amazonaws.com/p?token=supersecret"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("strict deny: want 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	errObj := resBody(t, rr)["error"].(map[string]any)
	if errObj["code"] != "residency_violation" {
		t.Fatalf("403 code = %v, want residency_violation", errObj["code"])
	}
	if errObj["url_host"] != "dynamodb.us-east-1.amazonaws.com" || errObj["region"] != "us-east" || errObj["reason"] != "out_of_region" || errObj["verdict"] != "denied" || errObj["strict_mode"] != true || errObj["mode"] != "pinned" {
		t.Fatalf("403 body wrong: %v", errObj)
	}
	// HOST-ONLY evidence: the query string must never surface anywhere.
	for key, val := range errObj {
		if s, ok := val.(string); ok && strings.Contains(s, "supersecret") {
			t.Fatalf("403 leaks query material in %q: %v", key, s)
		}
	}

	// In-region allow -> 200 with the full verdict shape.
	rr = resReq(t, h, http.MethodPost, "/api/v1/residency/check", token, `{"url":"https://s3.eu-central-1.amazonaws.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("in-region check: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	verdict = resBody(t, rr)["verdict"].(map[string]any)
	if verdict["allowed"] != true || verdict["reason"] != "in_region" || verdict["strict_mode"] != true || verdict["mode"] != "pinned" {
		t.Fatalf("in-region verdict wrong: %v", verdict)
	}
}

func TestResidencyPostureReflectsLastViolation(t *testing.T) {
	h, authSvc, _, _ := newResidencyTestRouter(t)
	token := resTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	// Fresh org: posture has NO fabricated violation evidence.
	rr := resReq(t, h, http.MethodGet, "/api/v1/residency/posture", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("posture: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	posture := resBody(t, rr)
	if posture["mode"] != "unpinned" || posture["last_violation"] != nil {
		t.Fatalf("fresh posture wrong: %v", posture)
	}

	// Observe-mode flag first (allowed, but still evidence), then strict
	// enforcement — the posture must reflect the LATEST evidence.
	if rr := resReq(t, h, http.MethodPut, "/api/v1/residency/policy", token, `{"pinned_region":"eu-central","strict_mode":false}`); rr.Code != http.StatusOK {
		t.Fatalf("PUT observe policy: %d", rr.Code)
	}
	if rr := resReq(t, h, http.MethodPost, "/api/v1/residency/check", token, `{"url":"https://sns.ap-south-1.amazonaws.com"}`); rr.Code != http.StatusOK {
		t.Fatalf("observe-mode flagged check: want 200, got %d", rr.Code)
	}
	if rr := resReq(t, h, http.MethodPut, "/api/v1/residency/policy", token, `{"pinned_region":"eu-central","strict_mode":true}`); rr.Code != http.StatusOK {
		t.Fatalf("PUT strict policy: %d", rr.Code)
	}
	if rr := resReq(t, h, http.MethodPost, "/api/v1/residency/check", token, `{"url":"https://dynamodb.us-east-2.amazonaws.com"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("strict deny: want 403, got %d", rr.Code)
	}

	rr = resReq(t, h, http.MethodGet, "/api/v1/residency/posture", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("posture after denies: %d", rr.Code)
	}
	posture = resBody(t, rr)
	if posture["mode"] != "pinned" || posture["pinned_region"] != "eu-central" || posture["strict_mode"] != true {
		t.Fatalf("posture policy fields wrong: %v", posture)
	}
	viol, ok := posture["last_violation"].(map[string]any)
	if !ok {
		t.Fatalf("last_violation must be present after a strict deny: %v", posture)
	}
	if viol["url_host"] != "dynamodb.us-east-2.amazonaws.com" || viol["region"] != "us-east" || viol["reason"] != "out_of_region" {
		t.Fatalf("last_violation wrong: %v", viol)
	}
	if at, _ := viol["at"].(string); at == "" {
		t.Fatalf("last_violation timestamp missing: %v", viol)
	}
}

func TestResidencyOrgIsolationOverHTTP(t *testing.T) {
	h, authSvc, _, _ := newResidencyTestRouter(t)
	orgA := resTokenFor(t, authSvc, "a@x.io", "org-a", "OWNER")
	orgB := resTokenFor(t, authSvc, "b@x.io", "org-b", "OWNER")

	// org-a pins strict and generates its own violation evidence.
	if rr := resReq(t, h, http.MethodPut, "/api/v1/residency/policy", orgA, `{"pinned_region":"eu-central","strict_mode":true}`); rr.Code != http.StatusOK {
		t.Fatalf("org-a PUT: %d", rr.Code)
	}
	if rr := resReq(t, h, http.MethodPost, "/api/v1/residency/check", orgA, `{"url":"https://s3.us-west-2.amazonaws.com"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("org-a strict deny: want 403, got %d", rr.Code)
	}

	// org-b sees none of it: unpinned policy, no evidence, unpinned verdicts.
	rr := resReq(t, h, http.MethodGet, "/api/v1/residency/policy", orgB, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("org-b GET policy: %d", rr.Code)
	}
	if body := resBody(t, rr); body["mode"] != "unpinned" {
		t.Fatalf("org-b policy leak: %v", body)
	}
	rr = resReq(t, h, http.MethodGet, "/api/v1/residency/posture", orgB, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("org-b posture: %d", rr.Code)
	}
	if body := resBody(t, rr); body["last_violation"] != nil {
		t.Fatalf("org-a evidence leaked into org-b posture: %v", body["last_violation"])
	}
	rr = resReq(t, h, http.MethodPost, "/api/v1/residency/check", orgB, `{"url":"https://s3.us-east-1.amazonaws.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("org-b check: want 200, got %d", rr.Code)
	}
	if verdict := resBody(t, rr)["verdict"].(map[string]any); verdict["reason"] != "unpinned" {
		t.Fatalf("org-b verdict must be unpinned: %v", verdict)
	}
}

func TestResidencyAuditEventsOverHTTP(t *testing.T) {
	h, authSvc, auditSvc, _ := newResidencyTestRouter(t)
	token := resTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	if rr := resReq(t, h, http.MethodPut, "/api/v1/residency/policy", token, `{"pinned_region":"eu-central","strict_mode":true}`); rr.Code != http.StatusOK {
		t.Fatalf("PUT policy: %d", rr.Code)
	}
	if rr := resReq(t, h, http.MethodPost, "/api/v1/residency/check", token, `{"url":"https://s3.eu-central-1.amazonaws.com"}`); rr.Code != http.StatusOK {
		t.Fatalf("allow check: %d", rr.Code)
	}
	if rr := resReq(t, h, http.MethodPost, "/api/v1/residency/check", token, `{"url":"https://s3.us-west-1.amazonaws.com/p?token=supersecret"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("deny check: want 403, got %d", rr.Code)
	}

	entries, err := auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Action]++
	}
	if counts["residency.policy_updated"] != 1 || counts["residency.check"] != 1 || counts["residency.violation"] != 1 {
		t.Fatalf("audit counts wrong: %v", counts)
	}
	// The deny row must carry HOST-ONLY evidence (no URL, no query string).
	for _, e := range entries {
		if e.Action != "residency.violation" {
			continue
		}
		if e.Metadata["url_host"] != "s3.us-west-1.amazonaws.com" {
			t.Fatalf("violation url_host = %v", e.Metadata["url_host"])
		}
		if e.Metadata["verdict"] != "denied" || e.Metadata["region"] != "us-west" {
			t.Fatalf("violation metadata wrong: %v", e.Metadata)
		}
		for key, val := range e.Metadata {
			if s, ok := val.(string); ok && (strings.Contains(s, "supersecret") || strings.Contains(s, "https://")) {
				t.Fatalf("audit metadata %q leaks URL material: %v", key, s)
			}
		}
	}
}

func TestResidencyDualMountAndNilGuards(t *testing.T) {
	// The surface is registered on the shared apiMux and therefore served
	// under BOTH versioned prefixes (/v1 and /api/v1).
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	svc := tenant.NewResidencyService()
	apiMux := http.NewServeMux()
	registerResidencyRoutes(apiMux, svc, authSvc, keysSvc, audit.NewService())
	h := http.StripPrefix("/v1", apiMux)
	token := resTokenFor(t, authSvc, "o@x.io", "org-a", "OWNER")
	if rr := resReq(t, h, http.MethodGet, "/v1/residency/policy", token, ""); rr.Code != http.StatusOK {
		t.Fatalf("/v1 mount: want 200, got %d", rr.Code)
	}

	// Nil guards: registering against a nil mux or nil service must not panic
	// (and the nil-service wiring must not panic either).
	registerResidencyRoutes(nil, svc, authSvc, keysSvc, nil)
	registerResidencyRoutes(http.NewServeMux(), nil, authSvc, keysSvc, nil)
	svcNil := (*tenant.ResidencyService)(nil)
	svcNil.SetAuditPublisher(nil) // nil-safe by contract
	svcNil.SetActorResolver(nil)
}

// fmtSlice converts a decoded JSON array value into []string for assertions.
func fmtSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(string))
	}
	return out
}
