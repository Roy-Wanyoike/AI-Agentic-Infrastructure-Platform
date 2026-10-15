package main

// configgen_test.go — issue #129 HTTP surface, exercised through the REAL
// middleware chain (RequireAuthOrAPIKey → RequirePermission), mirroring
// cmd/api/synth_test.go. Covers: 401 without credentials, the configgen.use
// RBAC matrix (OWNER/ADMIN/MEMBER pass, VIEWER is 403), 400 malformed body,
// 422 validation errors (blank description, unknown type), agent and workflow
// draft generation with step progress ordering, the DRAFTS-ONLY contract
// (no server-side identity fields), the dual-mode LLM seam injection
// (model-backed draft + heuristic fallback when the model returns garbage),
// and one full-app E2E smoke proving the main.go registration line serves
// /config/generate under /api/v1.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/configgen"
	"agentos/internal/models"
)

func newConfigGenTestRouter(t *testing.T) (http.Handler, *auth.Service) {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	keysSvc := apikeys.NewService()
	svc := newConfigGenService(nil, false) // heuristic mode: deterministic
	apiMux := http.NewServeMux()
	registerConfigGenRoutes(apiMux, svc, authSvc, keysSvc)
	return http.StripPrefix("/api/v1", apiMux), authSvc
}

func configGenTokenFor(t *testing.T, authSvc *auth.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&auth.User{ID: "user-" + email, Organization: orgID, Email: email, Role: role})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

func configGenReq(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
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

func configGenBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, rr.Body.String())
	}
	return out
}

func configGenErrorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	body := configGenBody(t, rr)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope: %s", rr.Body.String())
	}
	code, _ := errObj["code"].(string)
	return code
}

// --- 401 + RBAC matrix ---------------------------------------------------------

func TestConfigGenAuthRequired(t *testing.T) {
	h, _ := newConfigGenTestRouter(t)
	rr := configGenReq(t, h, http.MethodPost, "/api/v1/config/generate", "",
		`{"type":"agent","description":"triage support tickets"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials = %d, want 401 — body=%s", rr.Code, rr.Body.String())
	}
}

func TestConfigGenRBACMatrix(t *testing.T) {
	h, authSvc := newConfigGenTestRouter(t)
	body := `{"type":"agent","description":"triage support tickets"}`
	for _, tc := range []struct {
		role string
		want int
	}{
		{"OWNER", http.StatusOK},
		{"ADMIN", http.StatusOK},
		{"MEMBER", http.StatusOK},
		{"VIEWER", http.StatusForbidden},
	} {
		t.Run(tc.role, func(t *testing.T) {
			token := configGenTokenFor(t, authSvc, strings.ToLower(tc.role), "org-cg", tc.role)
			rr := configGenReq(t, h, http.MethodPost, "/api/v1/config/generate", token, body)
			if rr.Code != tc.want {
				t.Fatalf("%s = %d, want %d — body=%s", tc.role, rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// --- 400/422 validation ---------------------------------------------------------

func TestConfigGenMalformedBodyIs400(t *testing.T) {
	h, authSvc := newConfigGenTestRouter(t)
	token := configGenTokenFor(t, authSvc, "owner", "org-cg", "OWNER")
	rr := configGenReq(t, h, http.MethodPost, "/api/v1/config/generate", token, `{not json`)
	if rr.Code != http.StatusBadRequest || configGenErrorCode(t, rr) != "INVALID_REQUEST" {
		t.Fatalf("malformed body = %d (%s), want 400 INVALID_REQUEST", rr.Code, rr.Body.String())
	}
}

func TestConfigGenValidationErrors(t *testing.T) {
	h, authSvc := newConfigGenTestRouter(t)
	token := configGenTokenFor(t, authSvc, "owner", "org-cg", "OWNER")
	tests := []struct {
		name string
		body string
	}{
		{"blank description", `{"type":"agent","description":"   "}`},
		{"missing description", `{"type":"agent"}`},
		{"missing type", `{"description":"triage support tickets"}`},
		{"unknown type", `{"type":"pipeline","description":"triage support tickets"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := configGenReq(t, h, http.MethodPost, "/api/v1/config/generate", token, tc.body)
			if rr.Code != http.StatusUnprocessableEntity || configGenErrorCode(t, rr) != "VALIDATION_ERROR" {
				t.Fatalf("%s = %d (%s), want 422 VALIDATION_ERROR", tc.name, rr.Code, rr.Body.String())
			}
		})
	}
}

// --- agent draft generation ------------------------------------------------------

func TestConfigGenAgentDraft(t *testing.T) {
	h, authSvc := newConfigGenTestRouter(t)
	token := configGenTokenFor(t, authSvc, "owner", "org-cg", "OWNER")
	rr := configGenReq(t, h, http.MethodPost, "/api/v1/config/generate", token,
		`{"type":"agent","description":"An agent that triages our support inbox: categorize tickets and draft replies","context":{"tone":"friendly"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("generate = %d — body=%s", rr.Code, rr.Body.String())
	}
	resp := configGenBody(t, rr)
	if resp["type"] != "agent" {
		t.Errorf("type = %v, want agent", resp["type"])
	}
	if valid, _ := resp["valid"].(bool); !valid {
		t.Errorf("valid = %v, want true — issues=%v", resp["valid"], resp["issues"])
	}
	draft, ok := resp["agent_draft"].(map[string]any)
	if !ok {
		t.Fatalf("missing agent_draft: %s", rr.Body.String())
	}
	if draft["model"] != "gpt-4o-mini" {
		t.Errorf("model = %v", draft["model"])
	}
	if intent, _ := draft["intent"].(string); intent != "support_inbox" {
		t.Errorf("intent = %v, want support_inbox", intent)
	}
	tools, ok := draft["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing: %s", rr.Body.String())
	}
	tool, _ := tools[0].(map[string]any)
	if tool["tool"] != "json_transform" {
		t.Errorf("first tool = %v, want json_transform", tool["tool"])
	}
	if _, ok := tool["reason"].(string); !ok {
		t.Error("tool binding missing its documented reason")
	}
	runtimeDraft, ok := draft["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("missing runtime: %s", rr.Body.String())
	}
	if maxSteps, _ := runtimeDraft["max_steps"].(float64); maxSteps != 10 {
		t.Errorf("max_steps = %v, want 10", runtimeDraft["max_steps"])
	}
	// Step progress: all 5 steps reported, in order.
	if got := fmt.Sprint(assertConfigGenProgress(t, resp)); got != "[1 2 3 4 5]" {
		t.Errorf("progress = %v", got)
	}
	// Context echo: the caller context is recorded as an assumption.
	assumptions, _ := draft["assumptions"].([]any)
	foundContext := false
	for _, a := range assumptions {
		if s, _ := a.(string); strings.Contains(s, "tone") {
			foundContext = true
		}
	}
	if !foundContext {
		t.Errorf("caller context not echoed into assumptions: %v", assumptions)
	}
	// DRAFTS-ONLY contract: no server-side identity fields exist on a draft.
	for _, forbidden := range []string{"id", "status", "created_at", "organization_id"} {
		if _, present := draft[forbidden]; present {
			t.Errorf("draft carries server-side field %q — drafts only!", forbidden)
		}
	}
}

func assertConfigGenProgress(t *testing.T, resp map[string]any) []any {
	t.Helper()
	progress, ok := resp["progress"].([]any)
	if !ok || len(progress) != 5 {
		t.Fatalf("progress = %v, want 5 steps", resp["progress"])
	}
	steps := make([]any, 0, len(progress))
	for i, raw := range progress {
		step, _ := raw.(map[string]any)
		n, _ := step["step"].(float64)
		total, _ := step["total"].(float64)
		if int(n) != i+1 || int(total) != 5 {
			t.Fatalf("progress[%d] = %v, want step %d of 5", i, step, i+1)
		}
		steps = append(steps, int(n))
	}
	return steps
}

// --- workflow draft generation ----------------------------------------------------

func TestConfigGenWorkflowDraft(t *testing.T) {
	h, authSvc := newConfigGenTestRouter(t)
	token := configGenTokenFor(t, authSvc, "owner", "org-cg", "OWNER")
	rr := configGenReq(t, h, http.MethodPost, "/api/v1/config/generate", token,
		`{"type":"workflow","description":"research the docs then publish the digest to the webhook after review"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("generate = %d — body=%s", rr.Code, rr.Body.String())
	}
	resp := configGenBody(t, rr)
	if resp["type"] != "workflow" || resp["valid"] != true {
		t.Fatalf("bad response: %s", rr.Body.String())
	}
	draft, ok := resp["workflow_draft"].(map[string]any)
	if !ok {
		t.Fatalf("missing workflow_draft: %s", rr.Body.String())
	}
	dsl, ok := draft["dsl"].(map[string]any)
	if !ok {
		t.Fatalf("missing dsl: %s", rr.Body.String())
	}
	nodes, _ := dsl["nodes"].([]any)
	if len(nodes) != 4 { // agent → approval → webhook → end
		t.Fatalf("nodes = %d, want 4 — body=%s", len(nodes), rr.Body.String())
	}
	edges, _ := dsl["edges"].([]any)
	if len(edges) != 3 {
		t.Fatalf("edges = %d, want 3", len(edges))
	}
	if got := fmt.Sprint(assertConfigGenProgress(t, resp)); got != "[1 2 3 4 5]" {
		t.Errorf("progress = %v", got)
	}
}

// --- dual-mode LLM seam injection ---------------------------------------------------

func TestConfigGenLLMSeamModelBackedDraft(t *testing.T) {
	// models.NewStaticProvider with a pinned payload is the deterministic
	// model stand-in: the draft must carry the model-authored fields.
	provider := models.NewStaticProvider("static", `{
		"name": "Refund Bot",
		"model": "gpt-4o",
		"system_instructions": "Handle refunds politely.",
		"tools": [{"tool": "calculator", "reason": "refund math"}]
	}`)
	svc := newConfigGenService(provider, true)
	authSvc := auth.NewService("test-secret")
	apiMux := http.NewServeMux()
	registerConfigGenRoutes(apiMux, svc, authSvc, apikeys.NewService())
	h := http.StripPrefix("/api/v1", apiMux)
	token := configGenTokenFor(t, authSvc, "owner", "org-cg", "OWNER")

	rr := configGenReq(t, h, http.MethodPost, "/api/v1/config/generate", token,
		`{"type":"agent","description":"process refund requests"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("generate = %d — body=%s", rr.Code, rr.Body.String())
	}
	draft := configGenBody(t, rr)["agent_draft"].(map[string]any)
	if draft["name"] != "Refund Bot" || draft["model"] != "gpt-4o" {
		t.Errorf("model-authored draft fields lost: %v", draft)
	}
	if draft["system_instructions"] != "Handle refunds politely." {
		t.Errorf("system instructions = %v", draft["system_instructions"])
	}
	if tools, _ := draft["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v, want [calculator]", tools)
	}
}

func TestConfigGenLLMSeamFallsBackToHeuristics(t *testing.T) {
	// StaticProvider echo mode returns "echo: <prompt>" — unparseable JSON —
	// so the generator must still produce the heuristic draft.
	svc := newConfigGenService(models.NewStaticProvider("echo", ""), true)
	gen := configgen.NewAgentConfigGenerator(svc.catalog, svc.llm)
	draft, err := gen.Generate(context.Background(), "categorize support tickets and draft replies")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if draft.Intent != "support_inbox" || draft.Model != "gpt-4o-mini" {
		t.Errorf("heuristic fallback not applied: %+v", draft)
	}
}

// --- main.go wiring (full-app E2E smoke through newApp(nil db)) ----------------------

func TestConfigGenWiringE2E(t *testing.T) {
	env := newE2EEnv(t)
	env.registerAndLogin(t)

	// The register line in main.go exposes /config/generate under the real
	// mux with the real auth middleware (heuristic mode: no OPENAI_API_KEY).
	rr, resp := env.do(t, http.MethodPost, "/api/v1/config/generate",
		`{"type":"workflow","description":"publish the weekly report after approval"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("e2e generate = %d — body=%s", rr.Code, rr.Body.String())
	}
	if resp["type"] != "workflow" || resp["valid"] != true {
		t.Fatalf("e2e response invalid: %s", rr.Body.String())
	}
}
