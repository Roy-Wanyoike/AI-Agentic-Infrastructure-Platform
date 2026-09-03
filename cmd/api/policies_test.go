package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/policies"
)

// newPoliciesTestServer wires the policy routes with in-memory services and
// returns the mux plus tokens for an OWNER and a VIEWER of the same org.
func newPoliciesTestServer(t *testing.T) (*http.ServeMux, string, string) {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()
	org, owner, err := authSvc.RegisterCtx(t.Context(), "org-gov", "owner@gov.example", "pw")
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	ownerToken, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("owner token failed: %v", err)
	}
	// A viewer token for a user that is not registered in memory: middleware
	// falls back to the role claim for permission checks.
	viewerToken, err := authSvc.GenerateToken(&auth.User{
		ID: "viewer-1", Organization: org.ID, Email: "viewer@gov.example", Role: "VIEWER",
	})
	if err != nil {
		t.Fatalf("viewer token failed: %v", err)
	}

	mux := http.NewServeMux()
	registerPoliciesRoutes(mux, policies.NewService(), authSvc, apiKeysSvc)
	return mux, ownerToken, viewerToken
}

func polRequest(t *testing.T, mux *http.ServeMux, method, path, token, body string) *httptest.ResponseRecorder {
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
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestPoliciesRoutesRequireAuth(t *testing.T) {
	mux, _, _ := newPoliciesTestServer(t)

	rec := polRequest(t, mux, http.MethodGet, "/policies", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request must be 401, got %d", rec.Code)
	}

	rec = polRequest(t, mux, http.MethodPost, "/policies/evaluate", "", `{"action":"tools.call"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated evaluate must be 401, got %d", rec.Code)
	}
}

func TestPoliciesRoutesRBAC(t *testing.T) {
	mux, ownerToken, viewerToken := newPoliciesTestServer(t)

	body := `{"name":"Deny prod tools","effect":"deny","resource_type":"tool",
		"actions":["tools.call"],"priority":100,"enabled":true,
		"conditions":{"environments":["production"]}}`

	// VIEWER may read but not write.
	if rec := polRequest(t, mux, http.MethodGet, "/policies", viewerToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer read must be 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := polRequest(t, mux, http.MethodPost, "/policies/create", viewerToken, body); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer write must be 403, got %d body=%s", rec.Code, rec.Body.String())
	}

	// OWNER may write.
	rec := polRequest(t, mux, http.MethodPost, "/policies/create", ownerToken, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("owner create must be 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Policy map[string]any `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response must be JSON: %v", err)
	}
	policyID, _ := created.Policy["id"].(string)
	if policyID == "" {
		t.Fatalf("missing policy id in response: %s", rec.Body.String())
	}
	if created.Policy["effect"] != "deny" || created.Policy["resource_type"] != "tool" {
		t.Fatalf("unexpected policy payload: %#v", created.Policy)
	}
	if _, hasOrg := created.Policy["organization_id"]; hasOrg {
		t.Fatal("organization_id must not leak in policy JSON")
	}

	// List contains exactly the one policy.
	rec = polRequest(t, mux, http.MethodGet, "/policies", ownerToken, "")
	var list struct {
		Policies []map[string]any `json:"policies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list response must be JSON: %v", err)
	}
	if len(list.Policies) != 1 || list.Policies[0]["id"] != policyID {
		t.Fatalf("unexpected list: %#v", list.Policies)
	}
}

func TestPoliciesEvaluateContractShape(t *testing.T) {
	mux, ownerToken, viewerToken := newPoliciesTestServer(t)

	body := `{"name":"Block expensive prod tools","effect":"deny","resource_type":"tool",
		"actions":["tools.call"],"priority":100,"enabled":true,
		"conditions":{"tool_allowlist":["httpx"],"environments":["production"],"max_cost_cents":500}}`
	if rec := polRequest(t, mux, http.MethodPost, "/policies/create", ownerToken, body); rec.Code != http.StatusCreated {
		t.Fatalf("setup create failed: %d %s", rec.Code, rec.Body.String())
	}

	evaluate := func(token, payload string) map[string]any {
		t.Helper()
		rec := polRequest(t, mux, http.MethodPost, "/policies/evaluate", token, payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("evaluate must be 200, got %d body=%s", rec.Code, rec.Body.String())
		}
		var decision map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &decision); err != nil {
			t.Fatalf("evaluate response must be JSON: %v", err)
		}
		return decision
	}

	// Highest-priority deny wins for the listed tool in production.
	got := evaluate(ownerToken, `{
		"subject":{"user_id":"u1","role":"MEMBER"},
		"action":"tools.call",
		"resource":{"type":"tool","id":"httpx","tenant_id":"tenant-a"},
		"context":{"estimated_cost_cents":900,"environment":"production"}}`)
	if got["decision"] != "deny" {
		t.Fatalf("expected deny, got %#v", got)
	}
	if _, ok := got["matched_policy_id"].(string); !ok {
		t.Fatalf("matched_policy_id must be present: %#v", got)
	}
	if reason, _ := got["reason"].(string); reason == "" {
		t.Fatal("reason must be non-empty")
	}

	// Unrelated request: default allow with empty matched id.
	got = evaluate(viewerToken, `{
		"subject":{"user_id":"u2","role":"VIEWER"},
		"action":"tools.call",
		"resource":{"type":"tool","id":"search"},
		"context":{"estimated_cost_cents":10,"environment":"development"}}`)
	if got["decision"] != "allow" {
		t.Fatalf("expected default allow, got %#v", got)
	}
	if id, _ := got["matched_policy_id"].(string); id != "" {
		t.Fatalf("default allow must have empty matched_policy_id, got %q", id)
	}

	// Missing action -> 422.
	rec := polRequest(t, mux, http.MethodPost, "/policies/evaluate", ownerToken, `{"resource":{"type":"tool"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing action must be 422, got %d", rec.Code)
	}
}

func TestPoliciesDetailCRUDAndValidation(t *testing.T) {
	mux, ownerToken, _ := newPoliciesTestServer(t)

	createBody := `{"name":"Allow search","effect":"allow","resource_type":"tool",
		"actions":["tools.call"],"priority":10,"enabled":true,
		"conditions":{"tool_allowlist":["search"]}}`
	rec := polRequest(t, mux, http.MethodPost, "/policies/create", ownerToken, createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Policy struct {
			ID string `json:"id"`
		} `json:"policy"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created.Policy.ID

	// PUT full update.
	updateBody := `{"name":"Allow search v2","effect":"deny","resource_type":"tool",
		"actions":["tools.call"],"priority":20,"enabled":true,"conditions":{}}`
	rec = polRequest(t, mux, http.MethodPut, "/policies/"+id, ownerToken, updateBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Policy struct {
			Name   string `json:"name"`
			Effect string `json:"effect"`
		} `json:"policy"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Policy.Name != "Allow search v2" || updated.Policy.Effect != "deny" {
		t.Fatalf("update not applied: %#v", updated.Policy)
	}

	// GET single.
	rec = polRequest(t, mux, http.MethodGet, "/policies/"+id, ownerToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get failed: %d", rec.Code)
	}

	// Validation error -> 422 structured envelope.
	rec = polRequest(t, mux, http.MethodPost, "/policies/create", ownerToken,
		`{"name":"Bad","effect":"maybe","resource_type":"tool"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid policy must be 422, got %d", rec.Code)
	}
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Error.Code != "invalid_policy" {
		t.Fatalf("expected invalid_policy code, got %#v", errBody)
	}

	// DELETE.
	rec = polRequest(t, mux, http.MethodDelete, "/policies/"+id, ownerToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete failed: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"deleted":true`) {
		t.Fatalf("delete must return {deleted:true}, got %s", rec.Body.String())
	}

	// Unknown id -> 404.
	rec = polRequest(t, mux, http.MethodGet, "/policies/does-not-exist", ownerToken, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown policy must be 404, got %d", rec.Code)
	}
}
