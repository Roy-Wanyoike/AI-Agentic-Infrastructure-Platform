package main

// qualitygates_test.go — issue #152 HTTP surface, exercised through the
// REAL middleware chain (RequireAuthOrAPIKey -> RequirePermission), the
// residency_test.go house pattern: the 401 matrix, the RBAC matrix
// (qualitygates.read ALL roles — viewers inspect release verdicts;
// qualitygates.write OWNER/ADMIN), the versioned policy lifecycle, the
// 422 validation matrix, the deterministic evaluate contract end-to-end
// (hold on missing evidence -> abort on hard security failure -> promote
// under an unexpired waiver), forced-actor exception approval with audit
// rows, org isolation over the wire, dual mount and nil guards.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	authpkg "agentos/internal/auth"
	"agentos/internal/qualitygates"
)

func newQualityGateTestRouter(t *testing.T) (http.Handler, *authpkg.Service, *audit.Service, *qualitygates.Service, *qualitygates.InMemoryEvidenceStore) {
	t.Helper()
	authSvc := authpkg.NewService("test-secret")
	keysSvc := apikeys.NewService()
	auditSvc := audit.NewService()
	evidence := qualitygates.NewInMemoryEvidenceStore()
	svc := qualitygates.NewService(qualitygates.NewInMemoryPolicyStore(), evidence)
	apiMux := http.NewServeMux()
	registerQualityGateRoutes(apiMux, svc, authSvc, keysSvc, auditSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc, auditSvc, svc, evidence
}

func qgTokenFor(t *testing.T, authSvc *authpkg.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&authpkg.User{ID: "user-" + email, Organization: orgID, Email: email, Role: role})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

func qgReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func qgBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, rr.Body.String())
	}
	return out
}

func qgErrorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	errObj, ok := qgBody(t, rr)["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got %s", rr.Body.String())
	}
	code, _ := errObj["code"].(string)
	return code
}

// validPolicyPayload is a well-formed policy body (evals + fail-closed
// security suite).
const validPolicyPayload = `{
  "name": "release-default",
  "active": true,
  "required_suites": [
    {"kind": "evaluations", "min_pass_rate": 0.8, "max_p95_ms": 5000, "max_age_ms": 86400000},
    {"kind": "security_adversarial", "max_severity_allowed": "high", "fail_closed": true}
  ]
}`

func TestQualityGatesAuthRequired(t *testing.T) {
	h, _, _, _, _ := newQualityGateTestRouter(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/quality-gates/policies", ""},
		{http.MethodPost, "/api/v1/quality-gates/policies", validPolicyPayload},
		{http.MethodGet, "/api/v1/quality-gates/policies/pol-1", ""},
		{http.MethodPut, "/api/v1/quality-gates/policies/pol-1", validPolicyPayload},
		{http.MethodDelete, "/api/v1/quality-gates/policies/pol-1", ""},
		{http.MethodPost, "/api/v1/quality-gates/evaluate", `{"policy_id":"pol-1"}`},
		{http.MethodPost, "/api/v1/quality-gates/exceptions", `{"policy_id":"pol-1","reason":"r","expires_at":"2030-01-01T00:00:00Z"}`},
	} {
		if rr := qgReq(t, h, tc.method, tc.path, "", tc.body); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d", tc.method, tc.path, rr.Code)
		}
	}
}

func TestQualityGatesRBACMatrix(t *testing.T) {
	h, authSvc, _, svc, _ := newQualityGateTestRouter(t)
	// Seed one policy so reads/evaluate have a real target (unknown ids
	// would 404 and still prove the permission check passed).
	seeded, err := svc.CreatePolicy(context.Background(), "org-a", "seed", qualitygates.GatePolicy{
		Name:   "seed",
		Active: true,
		RequiredSuites: []qualitygates.SuiteRequirement{
			{Kind: qualitygates.SuiteEvaluations, MinPassRate: 0.5},
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = qgTokenFor(t, authSvc, strings.ToLower(role)+"@x.io", "org-a", role)
	}

	// Reads + evaluate: qualitygates.read -> ALL roles (release verdicts
	// are org-wide facts; viewers inspect, never shape).
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		if rr := qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies", tokens[role], ""); rr.Code != http.StatusOK {
			t.Errorf("GET list as %s: want 200, got %d", role, rr.Code)
		}
		if rr := qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies/"+seeded.ID, tokens[role], ""); rr.Code != http.StatusOK {
			t.Errorf("GET policy as %s: want 200, got %d", role, rr.Code)
		}
		evalBody := `{"policy_id":"` + seeded.ID + `"}`
		if rr := qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/evaluate", tokens[role], evalBody); rr.Code != http.StatusOK {
			t.Errorf("evaluate as %s: want 200, got %d body=%s", role, rr.Code, rr.Body.String())
		}
	}

	// Writes: qualitygates.write -> OWNER/ADMIN only.
	writeCases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/quality-gates/policies", validPolicyPayload},
		{http.MethodPut, "/api/v1/quality-gates/policies/" + seeded.ID, validPolicyPayload},
		{http.MethodDelete, "/api/v1/quality-gates/policies/" + seeded.ID, ""},
		{http.MethodPost, "/api/v1/quality-gates/exceptions", `{"policy_id":"` + seeded.ID + `","reason":"r","expires_at":"2030-01-01T00:00:00Z"}`},
	}
	for _, role := range []string{"MEMBER", "VIEWER"} {
		for _, tc := range writeCases {
			if rr := qgReq(t, h, tc.method, tc.path, tokens[role], tc.body); rr.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s: want 403, got %d", tc.method, tc.path, role, rr.Code)
			}
		}
	}
	for _, role := range []string{"OWNER", "ADMIN"} {
		for _, tc := range writeCases {
			// The DELETE fires once per role, so the second call may 404
			// (already deleted) — both are non-403 outcomes. Creates need a
			// fresh name each round.
			body := strings.Replace(validPolicyPayload, "release-default", "policy-"+role, 1)
			if tc.method == http.MethodPost {
				tc.body = body
			}
			if rr := qgReq(t, h, tc.method, tc.path, tokens[role], tc.body); rr.Code == http.StatusForbidden {
				t.Errorf("%s %s as %s: want non-403, got 403 body=%s", tc.method, tc.path, role, rr.Body.String())
			}
		}
	}
}

func TestQualityGatesPolicyLifecycleVersioned(t *testing.T) {
	h, authSvc, _, _, _ := newQualityGateTestRouter(t)
	token := qgTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	// Create -> 201 version 1.
	rr := qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/policies", token, validPolicyPayload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	policy := qgBody(t, rr)["policy"].(map[string]any)
	id := policy["id"].(string)
	if policy["version"].(float64) != 1 || policy["active"] != true || policy["created_by"] != "user-owner@x.io" {
		t.Fatalf("created policy wrong: %v", policy)
	}
	if len(policy["required_suites"].([]any)) != 2 {
		t.Fatalf("required_suites wrong: %v", policy["required_suites"])
	}

	// List contains it; GET by id returns it.
	if rr = qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies", token, ""); rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	if rr = qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies/"+id, token, ""); rr.Code != http.StatusOK {
		t.Fatalf("get: %d", rr.Code)
	}

	// PUT appends version 2; ?version=1 still serves the immutable v1.
	rr = qgReq(t, h, http.MethodPut, "/api/v1/quality-gates/policies/"+id, token, validPolicyPayload)
	if rr.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := qgBody(t, rr)["policy"].(map[string]any)["version"].(float64); got != 2 {
		t.Fatalf("want version 2, got %v", got)
	}
	rr = qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies/"+id+"?version=1", token, "")
	if rr.Code != http.StatusOK || qgBody(t, rr)["policy"].(map[string]any)["version"].(float64) != 1 {
		t.Fatalf("versioned read: %d body=%s", rr.Code, rr.Body.String())
	}
	// Bad version query -> 422.
	if rr = qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies/"+id+"?version=zero", token, ""); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad version: want 422, got %d", rr.Code)
	}

	// DELETE removes every version.
	if rr = qgReq(t, h, http.MethodDelete, "/api/v1/quality-gates/policies/"+id, token, ""); rr.Code != http.StatusOK {
		t.Fatalf("delete: %d", rr.Code)
	}
	if rr = qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies/"+id, token, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("get after delete: want 404, got %d", rr.Code)
	}
	if rr = qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies/"+id, qgTokenFor(t, authSvc, "other@x.io", "org-b", "OWNER"), ""); rr.Code != http.StatusNotFound {
		t.Fatalf("foreign get after delete: want 404, got %d", rr.Code)
	}
}

func TestQualityGatesValidationMatrix(t *testing.T) {
	h, authSvc, _, svc, _ := newQualityGateTestRouter(t)
	owner := qgTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	// Unknown suite kind -> 422 VALIDATION_ERROR.
	rr := qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/policies", owner, `{"name":"x","required_suites":[{"kind":"chaos"}]}`)
	if rr.Code != http.StatusUnprocessableEntity || qgErrorCode(t, rr) != "VALIDATION_ERROR" {
		t.Fatalf("unknown kind: %d %s", rr.Code, rr.Body.String())
	}
	// Security suite without fail_closed -> 422 (structural enforcement).
	rr = qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/policies", owner, `{"name":"x","required_suites":[{"kind":"security_adversarial","max_severity_allowed":"high"}]}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("soft security suite must be rejected: %d %s", rr.Code, rr.Body.String())
	}
	// Empty name -> 422.
	rr = qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/policies", owner, `{"name":"  ","required_suites":[{"kind":"evaluations"}]}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty name: %d", rr.Code)
	}

	// Exceptions: time-bounded only — missing/past expiry -> 422.
	seeded, err := svc.CreatePolicy(context.Background(), "org-a", "seed", qualitygates.GatePolicy{
		Name: "seed", Active: true,
		RequiredSuites: []qualitygates.SuiteRequirement{{Kind: qualitygates.SuiteEvaluations}},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	exPath := "/api/v1/quality-gates/exceptions"
	for _, body := range []string{
		`{"policy_id":"` + seeded.ID + `","reason":"r"}`,                                                                    // no expiry
		`{"policy_id":"` + seeded.ID + `","reason":"r","expires_at":"not-a-time"}`,                                          // malformed
		`{"policy_id":"` + seeded.ID + `","reason":"r","expires_at":"2020-01-01T00:00:00Z"}`,                                // past
		`{"policy_id":"` + seeded.ID + `","reason":"r","suite":"security_adversarial","expires_at":"2030-01-01T00:00:00Z"}`, // unwaivable
	} {
		if rr = qgReq(t, h, http.MethodPost, exPath, owner, body); rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("exception validation %s: want 422, got %d body=%s", body, rr.Code, rr.Body.String())
		}
	}

	// Evaluate contract: missing policy_id -> 422; inactive -> 422
	// POLICY_INACTIVE; unknown policy -> 404.
	if rr = qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/evaluate", owner, `{}`); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing policy_id: want 422, got %d", rr.Code)
	}
	inactive, err := svc.CreatePolicy(context.Background(), "org-a", "seed", qualitygates.GatePolicy{
		Name: "off", RequiredSuites: []qualitygates.SuiteRequirement{{Kind: qualitygates.SuiteEvaluations}},
	})
	if err != nil {
		t.Fatalf("seed inactive: %v", err)
	}
	rr = qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/evaluate", owner, `{"policy_id":"`+inactive.ID+`"}`)
	if rr.Code != http.StatusUnprocessableEntity || qgErrorCode(t, rr) != "POLICY_INACTIVE" {
		t.Fatalf("inactive evaluate: %d %s", rr.Code, rr.Body.String())
	}
	rr = qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/evaluate", owner, `{"policy_id":"nope"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown policy evaluate: want 404, got %d", rr.Code)
	}
}

func TestQualityGatesEvaluateDeterministicContract(t *testing.T) {
	h, authSvc, _, _, evidence := newQualityGateTestRouter(t)
	owner := qgTokenFor(t, authSvc, "owner@x.io", "org-a", "OWNER")

	// Create the gate over HTTP.
	rr := qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/policies", owner, validPolicyPayload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rr.Code, rr.Body.String())
	}
	id := qgBody(t, rr)["policy"].(map[string]any)["id"].(string)

	putEvidence := func(t *testing.T, rec *qualitygates.EvidenceRecord) {
		t.Helper()
		rec.OrgID = "org-a"
		if err := evidence.PutEvidence(context.Background(), rec); err != nil {
			t.Fatalf("put evidence: %v", err)
		}
	}
	collected := time.Now().UTC().Add(-time.Hour)
	good := &qualitygates.EvidenceRecord{
		ID: "ev-e", Suite: qualitygates.SuiteEvaluations, CollectedAt: collected,
		Result: qualitygates.SuiteResult{Status: qualitygates.EvidenceCompleted, Cases: 100, Passed: 99, CoveragePct: 100, P95Ms: 900},
	}
	putEvidence(t, good)

	// 1) Evaluations pass but security evidence is missing -> HOLD +
	// blocked security check.
	evalPath := "/api/v1/quality-gates/evaluate"
	rr = qgReq(t, h, http.MethodPost, evalPath, owner, `{"policy_id":"`+id+`","evidence_refs":["ev-e"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("evaluate: %d body=%s", rr.Code, rr.Body.String())
	}
	decision := qgBody(t, rr)["decision"].(map[string]any)
	if decision["recommendation"] != "hold" {
		t.Fatalf("missing security evidence must hold: %v", decision)
	}

	// 2) A hard security failure -> ABORT regardless of aggregates.
	bad := &qualitygates.EvidenceRecord{
		ID: "ev-s", Suite: qualitygates.SuiteSecurityAdversarial, CollectedAt: collected,
		Result: qualitygates.SuiteResult{Status: qualitygates.EvidenceCompleted, Cases: 50, Passed: 50,
			Findings: []qualitygates.EvidenceFinding{{Severity: qualitygates.SevCritical, Title: "prompt injection"}}},
	}
	putEvidence(t, bad)
	rr = qgReq(t, h, http.MethodPost, evalPath, owner, `{"policy_id":"`+id+`","evidence_refs":["ev-e","ev-s"]}`)
	decision = qgBody(t, rr)["decision"].(map[string]any)
	if decision["recommendation"] != "abort" {
		t.Fatalf("hard security failure must abort: %v", decision)
	}

	// 3) Determinism over the wire: identical refs -> byte-identical
	// decision (modulo nothing — the service clock is wall time but the
	// decision JSON is stable within the call? No: evaluated_at uses the
	// service clock, so compare everything EXCEPT evaluated_at twice and
	// the full envelope across two immediate calls for the fields that
	// matter).
	a, _ := json.Marshal(decision)
	rr = qgReq(t, h, http.MethodPost, evalPath, owner, `{"policy_id":"`+id+`","evidence_refs":["ev-e","ev-s"]}`)
	b, _ := json.Marshal(qgBody(t, rr)["decision"])
	var da, db map[string]any
	_ = json.Unmarshal(a, &da)
	_ = json.Unmarshal(b, &db)
	delete(da, "evaluated_at")
	delete(db, "evaluated_at")
	a2, _ := json.Marshal(da)
	b2, _ := json.Marshal(db)
	if string(a2) != string(b2) {
		t.Fatalf("same inputs must decide identically:\n%s\n%s", a2, b2)
	}
	if decision["policy_evaluation_version"] == "" || decision["policy_id"] != id {
		t.Fatalf("decision must pin the policy: %v", decision)
	}
}

func TestQualityGatesExceptionApprovalWaivesAndAudits(t *testing.T) {
	h, authSvc, auditSvc, _, evidence := newQualityGateTestRouter(t)
	admin := qgTokenFor(t, authSvc, "admin@x.io", "org-a", "ADMIN")

	// Policy requiring a pass rate the evidence cannot meet, with a waiver
	// approved over HTTP.
	rr := qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/policies", admin, `{
		"name":"waived-gate",
		"required_suites":[{"kind":"evaluations","min_pass_rate":0.8}]
	}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rr.Code, rr.Body.String())
	}
	id := qgBody(t, rr)["policy"].(map[string]any)["id"].(string)

	if err := evidence.PutEvidence(context.Background(), &qualitygates.EvidenceRecord{
		ID: "ev-weak", OrgID: "org-a", Suite: qualitygates.SuiteEvaluations,
		CollectedAt: time.Now().UTC().Add(-time.Minute),
		Result:      qualitygates.SuiteResult{Status: qualitygates.EvidenceCompleted, Cases: 100, Passed: 61},
	}); err != nil {
		t.Fatalf("put evidence: %v", err)
	}

	// Without the waiver: rollback (proven regression).
	rr = qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/evaluate", admin, `{"policy_id":"`+id+`","evidence_refs":["ev-weak"]}`)
	if got := qgBody(t, rr)["decision"].(map[string]any)["recommendation"]; got != "rollback" {
		t.Fatalf("unwaived failure must rollback: %v", got)
	}

	// Approve the waiver; approved_by in the body is IGNORED — attribution
	// is forced to the authenticated actor.
	rr = qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/exceptions", admin, `{
		"policy_id":"`+id+`",
		"reason":"holiday freeze sign-off",
		"approved_by":"someone-else",
		"expires_at":"2030-01-01T00:00:00Z"
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d body=%s", rr.Code, rr.Body.String())
	}
	policy := qgBody(t, rr)["policy"].(map[string]any)
	if policy["version"].(float64) != 2 {
		t.Fatalf("approval must create version 2: %v", policy["version"])
	}
	exceptions := policy["exceptions"].([]any)
	if len(exceptions) != 1 {
		t.Fatalf("want one exception: %v", exceptions)
	}
	ex := exceptions[0].(map[string]any)
	if ex["approved_by"] != "user-admin@x.io" {
		t.Fatalf("approved_by must be the authenticated actor, got %v", ex["approved_by"])
	}

	// With the unexpired waiver: skipped check -> promote.
	rr = qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/evaluate", admin, `{"policy_id":"`+id+`","evidence_refs":["ev-weak"]}`)
	decision := qgBody(t, rr)["decision"].(map[string]any)
	if decision["recommendation"] != "promote" {
		t.Fatalf("unexpired waiver must promote: %v", decision)
	}
	checks := decision["check_results"].([]any)
	if checks[0].(map[string]any)["status"] != "skipped" {
		t.Fatalf("waived check must be skipped: %v", checks[0])
	}

	// Audit rows flowed through the real audit service.
	entries, err := auditSvc.ListCtx(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Action]++
	}
	for _, action := range []string{
		"qualitygates.policy_created", "qualitygates.exception_approved",
		"qualitygates.decision_recommended",
	} {
		if counts[action] == 0 {
			t.Fatalf("missing audit action %s in %v", action, counts)
		}
	}
}

func TestQualityGatesOrgIsolationOverHTTP(t *testing.T) {
	h, authSvc, _, _, _ := newQualityGateTestRouter(t)
	orgA := qgTokenFor(t, authSvc, "a@x.io", "org-a", "OWNER")
	orgB := qgTokenFor(t, authSvc, "b@x.io", "org-b", "OWNER")

	rr := qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/policies", orgA, validPolicyPayload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("org-a create: %d", rr.Code)
	}
	id := qgBody(t, rr)["policy"].(map[string]any)["id"].(string)

	// org-b sees an empty policy list and 404s on org-a's policy (never
	// 403: tenants cannot probe each other's release posture).
	rr = qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies", orgB, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("org-b list: %d", rr.Code)
	}
	if policies := qgBody(t, rr)["policies"].([]any); len(policies) != 0 {
		t.Fatalf("org-b must see no org-a policies: %v", policies)
	}
	if rr = qgReq(t, h, http.MethodGet, "/api/v1/quality-gates/policies/"+id, orgB, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("org-b foreign read: want 404, got %d", rr.Code)
	}
	if rr = qgReq(t, h, http.MethodPost, "/api/v1/quality-gates/evaluate", orgB, `{"policy_id":"`+id+`"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("org-b foreign evaluate: want 404, got %d", rr.Code)
	}
	if rr = qgReq(t, h, http.MethodDelete, "/api/v1/quality-gates/policies/"+id, orgB, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("org-b foreign delete: want 404, got %d", rr.Code)
	}
}

func TestQualityGatesDualMountAndNilGuards(t *testing.T) {
	// The surface is registered on the shared apiMux and therefore served
	// under BOTH versioned prefixes (/v1 and /api/v1).
	authSvc := authpkg.NewService("test-secret")
	svc := qualitygates.NewService(qualitygates.NewInMemoryPolicyStore(), qualitygates.NewInMemoryEvidenceStore())
	apiMux := http.NewServeMux()
	registerQualityGateRoutes(apiMux, svc, authSvc, apikeys.NewService(), audit.NewService())
	h := http.StripPrefix("/v1", apiMux)
	token := qgTokenFor(t, authSvc, "o@x.io", "org-a", "OWNER")
	if rr := qgReq(t, h, http.MethodGet, "/v1/quality-gates/policies", token, ""); rr.Code != http.StatusOK {
		t.Fatalf("/v1 mount: want 200, got %d", rr.Code)
	}

	// Nil guards: registering against a nil mux or nil service must not
	// panic (the nil-service wiring is a no-op by contract).
	registerQualityGateRoutes(nil, svc, authSvc, nil, nil)
	registerQualityGateRoutes(http.NewServeMux(), nil, authSvc, nil, nil)
}
