package main

// compliance_test.go — HTTP contract tests for the compliance evidence pack
// (wave-9 issue #138): RBAC matrix (audit.read = OWNER/ADMIN, the same
// evidence-level grant as the audit chain surface), the no-leak 404 for
// unknown AND foreign agents, format/window parameter validation, the
// deterministic NDJSON evidence bundle over HTTP, and the /v1 + /api/v1
// dual mount. The compliance service is constructed over STUB reader
// interfaces — no real audit/deployment/evaluation services are involved.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/compliance"
)

// complianceAgentStub is the minimal AgentsReader double: agents are keyed
// by (org, id) so foreign-org lookups surface agents.ErrAgentNotFound.
type complianceAgentStub struct {
	byOrgAgent map[string]map[string]*agents.Agent
}

func (s *complianceAgentStub) GetAgentCtx(_ context.Context, orgID, id string) (*agents.Agent, error) {
	if agent, ok := s.byOrgAgent[orgID][id]; ok {
		return agent, nil
	}
	return nil, agents.ErrAgentNotFound
}

func (s *complianceAgentStub) ListAgentsCtx(_ context.Context, orgID string) ([]*agents.Agent, error) {
	out := make([]*agents.Agent, 0, len(s.byOrgAgent[orgID]))
	for _, a := range s.byOrgAgent[orgID] {
		out = append(out, a)
	}
	return out, nil
}

// complianceAuditStub is the minimal AuditTrailReader double.
type complianceAuditStub struct {
	headHash string
	length   int
}

func (s *complianceAuditStub) ChainHead(_ context.Context, orgID string) (string, int, error) {
	return s.headHash, s.length, nil
}

func (s *complianceAuditStub) ListCtx(_ context.Context, orgID string) ([]*audit.Entry, error) {
	return nil, nil
}

// complianceEnv is the HTTP test environment.
type complianceEnv struct {
	mux         *http.ServeMux
	svc         *compliance.Service
	orgID       string
	ownerToken  string
	adminToken  string
	memberToken string
	viewerToken string
	otherToken  string
}

func newComplianceEnv(t *testing.T) *complianceEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	token := func(id, email, role string) string {
		t.Helper()
		generated, err := authSvc.GenerateToken(&auth.User{
			ID: id, Organization: owner.Organization, Email: email, Role: role,
		})
		if err != nil {
			t.Fatalf("GenerateToken(%s) returned error: %v", role, err)
		}
		return generated
	}
	_, foreign, err := authSvc.Register("OtherCo", "owner@other.test", "secret123")
	if err != nil {
		t.Fatalf("Register(foreign) returned error: %v", err)
	}
	otherToken, err := authSvc.GenerateToken(foreign)
	if err != nil {
		t.Fatalf("GenerateToken(foreign) returned error: %v", err)
	}

	orgA, orgB := owner.Organization, foreign.Organization
	agentsStub := &complianceAgentStub{byOrgAgent: map[string]map[string]*agents.Agent{
		orgA: {"agent-1": {ID: "agent-1", Name: "support-bot", OrganizationID: orgA, Status: "active"}},
		orgB: {"agent-9": {ID: "agent-9", Name: "other-bot", OrganizationID: orgB}},
	}}
	svc := compliance.NewService(compliance.Deps{
		Agents: agentsStub,
		Audit:  &complianceAuditStub{headHash: "cafe42", length: 3},
	})

	mux := http.NewServeMux()
	registerComplianceRoutes(mux, svc, authSvc, apiKeysSvc)

	return &complianceEnv{
		mux:         mux,
		svc:         svc,
		orgID:       orgA,
		ownerToken:  token("user-owner", "owner@acme.test", "OWNER"),
		adminToken:  token("user-admin", "admin@acme.test", "ADMIN"),
		memberToken: token("user-member", "member@acme.test", "MEMBER"),
		viewerToken: token("user-viewer", "viewer@acme.test", "VIEWER"),
		otherToken:  otherToken,
	}
}

func (e *complianceEnv) do(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// TestComplianceRBACMatrix — audit.read is OWNER/ADMIN only on every
// compliance endpoint; MEMBER/VIEWER are 403 and anonymous is 401.
func TestComplianceRBACMatrix(t *testing.T) {
	env := newComplianceEnv(t)
	paths := []string{
		"/compliance/controls",
		"/compliance/agents/agent-1/technical-doc",
		"/compliance/evidence?from=2025-01-01&to=2025-01-31",
	}
	for _, path := range paths {
		if rr := env.do(t, path, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous = %d, want 401", path, rr.Code)
		}
		if rr := env.do(t, path, env.ownerToken); rr.Code != http.StatusOK {
			t.Errorf("%s OWNER = %d, want 200 (body: %s)", path, rr.Code, rr.Body.String())
		}
		if rr := env.do(t, path, env.adminToken); rr.Code != http.StatusOK {
			t.Errorf("%s ADMIN = %d, want 200", path, rr.Code)
		}
		for name, tok := range map[string]string{"MEMBER": env.memberToken, "VIEWER": env.viewerToken} {
			if rr := env.do(t, path, tok); rr.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403", path, name, rr.Code)
			}
		}
	}
}

// TestComplianceControlsShapeOverHTTP — the report renders the catalog with
// live evidence and org scope from the claims.
func TestComplianceControlsShapeOverHTTP(t *testing.T) {
	env := newComplianceEnv(t)
	rr := env.do(t, "/compliance/controls", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("controls = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`"organization_id"`, `"compliance-catalog/v1"`,
		"identity_access", "logging_audit", "residency",
		"EUAI_ACT_ART_12", "audit.ChainHead",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("controls body missing %q", want)
		}
	}
}

// TestComplianceTechDocUnknownAndForeignAgent — unknown and foreign agents
// collapse into one 404 code (no existence leak).
func TestComplianceTechDocUnknownAndForeignAgent(t *testing.T) {
	env := newComplianceEnv(t)
	for name, path := range map[string]string{
		"unknown agent": "/compliance/agents/agent-404/technical-doc",
		"foreign agent": "/compliance/agents/agent-9/technical-doc",
	} {
		rr := env.do(t, path, env.ownerToken)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", name, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "COMPLIANCE_AGENT_NOT_FOUND") {
			t.Errorf("%s body = %s, want COMPLIANCE_AGENT_NOT_FOUND envelope", name, rr.Body.String())
		}
	}
	// The foreign token sees ITS OWN agent (org scoping works both ways).
	rr := env.do(t, "/compliance/agents/agent-9/technical-doc", env.otherToken)
	if rr.Code != http.StatusOK {
		t.Errorf("foreign owner on own agent = %d, want 200", rr.Code)
	}
}

// TestComplianceTechDocFormatParam — json default, md content type, invalid
// format rejected.
func TestComplianceTechDocFormatParam(t *testing.T) {
	env := newComplianceEnv(t)
	base := "/compliance/agents/agent-1/technical-doc"

	if rr := env.do(t, base, env.ownerToken); rr.Code != http.StatusOK ||
		rr.Header().Get("Content-Type") != "application/json" {
		t.Errorf("default format = %d %s, want 200 application/json", rr.Code, rr.Header().Get("Content-Type"))
	}
	if !strings.Contains(env.do(t, base, env.ownerToken).Body.String(), `"agent":{`) {
		t.Error("json doc must start with the agent section")
	}

	rr := env.do(t, base+"?format=md", env.ownerToken)
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("md format = %d %s, want 200 text/markdown", rr.Code, rr.Header().Get("Content-Type"))
	}
	body := rr.Body.String()
	if !strings.HasPrefix(body, "# Technical Documentation — support-bot") {
		t.Errorf("md header = %.60q", body)
	}
	if !strings.Contains(body, "## 7. Audit Posture") {
		t.Error("md doc missing the final section")
	}

	if rr := env.do(t, base+"?format=xml", env.ownerToken); rr.Code != http.StatusBadRequest {
		t.Errorf("xml format = %d, want 400", rr.Code)
	}
}

// TestComplianceEvidenceWindowParams — window validation + deterministic
// NDJSON bytes over HTTP.
func TestComplianceEvidenceWindowParams(t *testing.T) {
	env := newComplianceEnv(t)
	base := "/compliance/evidence"

	if rr := env.do(t, base, env.ownerToken); rr.Code != http.StatusBadRequest {
		t.Errorf("missing window = %d, want 400", rr.Code)
	}
	if rr := env.do(t, base+"?from=nope&to=2025-01-01", env.ownerToken); rr.Code != http.StatusBadRequest {
		t.Errorf("bad window = %d, want 400", rr.Code)
	}
	if rr := env.do(t, base+"?from=2025-02-01&to=2025-01-01", env.ownerToken); rr.Code != http.StatusBadRequest {
		t.Errorf("inverted window = %d, want 400", rr.Code)
	}

	url := base + "?from=2025-01-01&to=2025-01-31"
	rr := env.do(t, url, env.ownerToken)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("evidence = %d %s, want 200 application/x-ndjson", rr.Code, rr.Header().Get("Content-Type"))
	}
	first := strings.SplitN(rr.Body.String(), "\n", 2)[0]
	if !strings.Contains(first, `"type":"compliance_evidence"`) || !strings.Contains(first, `"org":"`+env.orgID+`"`) {
		t.Errorf("header line = %s", first)
	}
	if !strings.Contains(rr.Body.String(), `"type":"evidence_end","generated_for_window":true}`) {
		t.Error("bundle must end with the evidence_end footer line")
	}
	again := env.do(t, url, env.ownerToken)
	if again.Body.String() != rr.Body.String() {
		t.Error("evidence bundle over HTTP is not byte-deterministic for the fixed window")
	}
}

// TestComplianceDualMount — every endpoint is served under BOTH /v1 and
// /api/v1 (the shared StripPrefix mounts in main.go).
func TestComplianceDualMount(t *testing.T) {
	env := newComplianceEnv(t)
	outer := http.NewServeMux()
	outer.Handle("/v1/", http.StripPrefix("/v1", env.mux))
	outer.Handle("/api/v1/", http.StripPrefix("/api/v1", env.mux))

	paths := []string{
		"/v1/compliance/controls", "/api/v1/compliance/controls",
		"/v1/compliance/agents/agent-1/technical-doc", "/api/v1/compliance/agents/agent-1/technical-doc",
		"/v1/compliance/evidence?from=2025-01-01&to=2025-01-31", "/api/v1/compliance/evidence?from=2025-01-01&to=2025-01-31",
	}
	for _, path := range paths {
		req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+env.ownerToken)
		rr := httptest.NewRecorder()
		outer.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200 under the dual mount", path, rr.Code)
		}
	}
}
