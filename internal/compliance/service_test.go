package compliance

// service_test.go — control-mapping tests: catalog shape, live evidence
// injection through the stub interfaces, degradation on collector errors,
// and byte-determinism of the rendered report.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/deployments"
	"agentos/internal/evaluations"
	"agentos/internal/policies"
)

func testClock() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }

// fullDeps builds a fully-wired Deps with deterministic stub data.
func fullDeps() (Deps, *stubAudit) {
	aud := &stubAudit{
		headHash: "aa55",
		length:   7,
		entries: []*audit.Entry{
			{ID: "e1", Action: "agent.created", OrganizationID: "org-1", CreatedAt: testClock().Add(-24 * time.Hour)},
			{ID: "e2", Action: "run.executed", OrganizationID: "org-1", CreatedAt: testClock().Add(-40 * 24 * time.Hour)},
		},
	}
	dep := &stubDeployments{rows: []*deployments.Deployment{
		{ID: "d1", AgentID: "agent-1", Environment: "production", Status: deployments.StatusHealthy,
			Version: 3, CanaryVersion: 4, CanaryWeight: 10,
			Promotion: &deployments.CanaryPromotion{Policy: deployments.AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 5}}},
		{ID: "d2", AgentID: "agent-1", Environment: "development", Status: deployments.StatusFailed, Version: 1},
	}}
	return Deps{
		Audit:       aud,
		Agents:      &stubAgents{agent: &agents.Agent{ID: "agent-1", Name: "support-bot", OrganizationID: "org-1", Status: "active"}, list: []*agents.Agent{{ID: "agent-1"}}},
		Versions:    &stubVersions{rows: []*agents.ConfigVersion{{Version: 1, Status: "archived"}, {Version: 2, Status: "published"}}},
		Deployments: dep,
		Evaluations: &stubEvals{datasets: []*evaluations.Dataset{}},
		Policies:    &stubPolicies{rows: []*policies.Policy{{ID: "p1", Effect: policies.EffectDeny, Enabled: true}, {ID: "p2", Effect: policies.EffectAllow, Enabled: false}}},
		OrgAPIKeys:  &stubOrgKeys{n: 3},
		AgentKeys:   &stubAgentKeys{byAgent: map[string][]*apikeys.AgentKey{}},
		Approvals:   &stubApprovals{rows: []*approvals.Approval{{Status: approvals.StatusPending}, {Status: approvals.StatusApproved}, {Status: approvals.StatusApproved}, {Status: approvals.StatusCancelled}}},
		MCPRegistry: &stubMCP{n: 2},
		Now:         fixedClock(testClock()),
	}, aud
}

// TestControlsCatalogShape pins the domain taxonomy and per-control fields.
func TestControlsCatalogShape(t *testing.T) {
	svc := NewService(Deps{Now: fixedClock(testClock())})
	report, err := svc.Controls(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("Controls returned error: %v", err)
	}
	wantDomains := []string{"identity_access", "tenant_isolation_secrets", "logging_audit", "human_oversight", "risk_policy", "lifecycle_change_mgmt", "supply_chain", "residency"}
	if len(report.Domains) != len(wantDomains) {
		t.Fatalf("domains = %d, want %d", len(report.Domains), len(wantDomains))
	}
	for i, want := range wantDomains {
		if report.Domains[i].ID != want {
			t.Errorf("domains[%d].id = %q, want %q (fixed catalog order)", i, report.Domains[i].ID, want)
		}
		if len(report.Domains[i].Controls) == 0 {
			t.Errorf("domain %q has no controls", want)
		}
		for _, c := range report.Domains[i].Controls {
			if len(c.SatisfiedBy) == 0 {
				t.Errorf("control %q has empty satisfied_by", c.ID)
			}
			if len(c.Frameworks) == 0 {
				t.Errorf("control %q has empty frameworks", c.ID)
			}
			for _, tag := range c.Frameworks {
				switch {
				case strings.HasPrefix(tag, "SOC2_TSC_"),
					strings.HasPrefix(tag, "ISO42001_A_"), strings.HasPrefix(tag, "ISO42001_A."),
					strings.HasPrefix(tag, "EUAI_ACT_ART_"),
					strings.HasPrefix(tag, "NIST_AIRMF_"):
				default:
					t.Errorf("control %q framework tag %q does not match any framework prefix", c.ID, tag)
				}
			}
			if c.Evidence == nil {
				t.Errorf("control %q evidence is nil, want non-nil slice", c.ID)
			}
		}
	}
	if len(report.Frameworks) != 4 {
		t.Errorf("frameworks = %v, want 4 entries", report.Frameworks)
	}
	if report.CatalogVersion != CatalogVersion {
		t.Errorf("catalog_version = %q, want %q", report.CatalogVersion, CatalogVersion)
	}
}

// TestControlsLiveEvidenceInjection verifies that live evidence pointers are
// measured at request time from the stubbed interfaces.
func TestControlsLiveEvidenceInjection(t *testing.T) {
	deps, _ := fullDeps()
	svc := NewService(deps)
	report, err := svc.Controls(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("Controls returned error: %v", err)
	}
	byID := map[string]ControlReport{}
	for _, d := range report.Domains {
		for _, c := range d.Controls {
			byID[c.ID] = c
		}
	}

	// Chain anchor is measured from the audit stub.
	ev := findEvidence(t, byID["logging_audit.tamper_evident_chain"].Evidence, "audit.ChainHead", EvidenceKindLive)
	if got := ev.Value.(map[string]any); got["head_hash"] != "aa55" || got["length"] != 7 {
		t.Errorf("chain evidence = %v, want head_hash aa55 length 7", got)
	}
	// Policy inventory counts.
	ev = findEvidence(t, byID["risk_policy.engine"].Evidence, "policies.ListPoliciesCtx", EvidenceKindLive)
	if got := ev.Value.(policySummary); got.Total != 2 || got.Enabled != 1 || got.Allow != 1 || got.Deny != 1 {
		t.Errorf("policy summary = %+v, want total 2 enabled 1 allow 1 deny 1", got)
	}
	// Oversight counts.
	ev = findEvidence(t, byID["human_oversight.approvals"].Evidence, "approvals.List", EvidenceKindLive)
	if got := ev.Value.(approvalSummary); got.Pending != 1 || got.Approved != 2 || got.Cancelled != 1 || got.Total != 4 {
		t.Errorf("approval summary = %+v", got)
	}
	// Supply chain count.
	ev = findEvidence(t, byID["supply_chain.mcp_trust"].Evidence, "mcp.Registry.ListServers", EvidenceKindLive)
	if got := ev.Value.(map[string]any); got["registered_servers"] != 2 {
		t.Errorf("mcp evidence = %v, want registered_servers 2", got)
	}
	// RBAC evidence measured from the apikeys stub.
	ev = findEvidence(t, byID["identity_access.rbac"].Evidence, "apikeys.ListKeysCtx", EvidenceKindLive)
	if got := ev.Value.(map[string]any); got["org_api_keys"] != 3 {
		t.Errorf("api keys evidence = %v, want org_api_keys 3", got)
	}
	// Canary state measured from the deployments stub.
	ev = findEvidence(t, byID["lifecycle_change_mgmt.eval_gated_canary"].Evidence, "deployments.ListDeploymentsCtx", EvidenceKindLive)
	if got := ev.Value.(deploymentSummary); got.Total != 2 || got.Healthy != 1 || got.WithCanary != 1 || got.WithPolicy != 1 {
		t.Errorf("deployment summary = %+v", got)
	}
	// SSO/SCIM stay static pointers (counts not exported read-only).
	for _, ev := range byID["identity_access.sso_scim"].Evidence {
		if ev.Kind != EvidenceKindStaticPointer {
			t.Errorf("sso_scim evidence %s: kind = %s, want static_pointer", ev.Source, ev.Kind)
		}
	}
}

// TestControlsDegradeOnCollectorError — a failing backend degrades that one
// control to an explicit static pointer; the report still renders.
func TestControlsDegradeOnCollectorError(t *testing.T) {
	svc := NewService(Deps{
		Audit:    &stubAudit{headErr: errBoom},
		Policies: &stubPolicies{err: errBoom},
		Now:      fixedClock(testClock()),
	})
	report, err := svc.Controls(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("Controls returned error despite degraded collector: %v", err)
	}
	var chainEvidence []Evidence
	for _, d := range report.Domains {
		for _, c := range d.Controls {
			if c.ID == "logging_audit.tamper_evident_chain" {
				chainEvidence = c.Evidence
			}
		}
	}
	ev := findEvidence(t, chainEvidence, "audit.ChainHead", EvidenceKindStaticPointer)
	if ev.Note != "live evidence unavailable" {
		t.Errorf("degraded evidence note = %q", ev.Note)
	}
}

// TestControlsJSONDeterministic — same data + fixed clock => identical bytes.
func TestControlsJSONDeterministic(t *testing.T) {
	deps, _ := fullDeps()
	svc := NewService(deps)
	a, err := json.Marshal(mustControls(t, svc))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := json.Marshal(mustControls(t, svc))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("Controls JSON is not byte-deterministic for unchanged data")
	}
}

// TestControlsRequiresOrg guards the tenant scope.
func TestControlsRequiresOrg(t *testing.T) {
	svc := NewService(Deps{})
	if _, err := svc.Controls(context.Background(), ""); err != ErrOrgRequired {
		t.Errorf("Controls(\"\") = %v, want ErrOrgRequired", err)
	}
}

// TestNilDepsRenderDegradedArtifacts — zero deps never panic; artifacts
// degrade to header/footer-only bundles and static pointers.
func TestNilDepsRenderDegradedArtifacts(t *testing.T) {
	svc := NewService(Deps{Now: fixedClock(testClock())})
	report, err := svc.Controls(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("Controls with zero deps: %v", err)
	}
	if len(report.Domains) == 0 {
		t.Fatal("zero-deps report has no domains")
	}
	// TechnicalDoc without the agents surface is unknown-by-construction.
	if _, err := svc.TechnicalDoc(context.Background(), "org-1", "agent-1"); err != ErrAgentNotFound {
		t.Errorf("TechnicalDoc with nil agents dep = %v, want ErrAgentNotFound", err)
	}
	// Evidence bundle renders header + access + footer only.
	bundle, err := svc.EvidenceBundle(context.Background(), "org-1", testClock().Add(-time.Hour), testClock())
	if err != nil {
		t.Fatalf("EvidenceBundle with zero deps: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(bundle, "\n"), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("zero-deps bundle lines = %d, want 3 (header, access, footer)", len(lines))
	}
	if !bytes.Contains(bundle, []byte(`"access_lifecycle_summary"`)) {
		t.Error("access line should be present (fields optional)")
	}
	if bytes.Contains(bundle, []byte("audit_chain_head")) || bytes.Contains(bundle, []byte("policy_summary")) {
		t.Error("unwired sections must be omitted from the bundle")
	}
}

// --- helpers ---

func mustControls(t *testing.T, svc *Service) *ControlsReport {
	t.Helper()
	report, err := svc.Controls(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("Controls: %v", err)
	}
	return report
}

func findEvidence(t *testing.T, list []Evidence, source, kind string) Evidence {
	t.Helper()
	for _, ev := range list {
		if ev.Source == source {
			if ev.Kind != kind {
				t.Fatalf("evidence %s kind = %s, want %s", source, ev.Kind, kind)
			}
			return ev
		}
	}
	t.Fatalf("evidence with source %q not found in %v", source, list)
	return Evidence{}
}
