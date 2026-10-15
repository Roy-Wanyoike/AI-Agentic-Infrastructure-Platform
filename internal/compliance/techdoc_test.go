package compliance

// techdoc_test.go — per-agent technical documentation tests: JSON shape +
// determinism, Markdown section order, the 404 contract for unknown/foreign
// agents, tool-surface derivation and the 30-day audit window.

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

// techDeps builds a fully-wired Deps for the technical-doc tests.
func techDeps() (Deps, *stubAudit) {
	now := testClock()
	aud := &stubAudit{
		headHash: "chain-head-42",
		length:   12,
		entries: []*audit.Entry{
			{ID: "a", OrganizationID: "org-1", CreatedAt: now.Add(-2 * 24 * time.Hour)},   // in 30d window
			{ID: "b", OrganizationID: "org-1", CreatedAt: now.Add(-29 * 24 * time.Hour)},  // in
			{ID: "c", OrganizationID: "org-1", CreatedAt: now.Add(-31 * 24 * time.Hour)},  // out
			{ID: "d", OrganizationID: "org-1", CreatedAt: now.Add(-400 * 24 * time.Hour)}, // out
		},
	}
	return Deps{
		Audit: aud,
		Agents: &stubAgents{agent: &agents.Agent{
			ID: "agent-1", Name: "support-bot", Description: "answers tickets",
			Model: "gpt-4o-mini", Status: "active", OrganizationID: "org-1",
			CreatedAt: now.Add(-90 * 24 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour),
		}, list: []*agents.Agent{{ID: "agent-1"}, {ID: "agent-2"}}},
		Versions: &stubVersions{rows: []*agents.ConfigVersion{
			{Version: 2, Status: "published", CreatedAt: now.Add(-48 * time.Hour), PublishedAt: ptrTime(now.Add(-47 * time.Hour)), PublishedBy: "alice"},
			{Version: 1, Status: "archived", CreatedAt: now.Add(-90 * 24 * time.Hour)},
		}},
		Deployments: &stubDeployments{rows: []*deployments.Deployment{
			{ID: "dep-b", AgentID: "agent-1", Environment: "production", Status: deployments.StatusHealthy, Version: 2},
			{ID: "dep-a", AgentID: "agent-1", Environment: "production", Status: deployments.StatusFailed, Version: 1},
			{ID: "dep-c", AgentID: "agent-1", Environment: "staging", Status: deployments.StatusHealthy,
				Version: 2, CanaryVersion: 3, CanaryWeight: 25,
				Promotion: &deployments.CanaryPromotion{
					Policy:      deployments.AgentPromotionPolicy{MinPassRate: 0.9, MinCanaryRuns: 3, MaxP95LatencyMs: 2500, MaxCostPerRunCents: 40},
					WindowStart: now.Add(-72 * time.Hour),
					Decision: &deployments.CanaryDecision{
						Action: deployments.CanaryDecisionRollback, Reason: "pass_rate 0.62 < 0.90 → rollback",
						DecidedAt: now.Add(-time.Hour), RunsCounted: 3, PassRate: 0.6152,
					},
				}},
		}},
		Evaluations: &stubEvals{
			datasets: []*evaluations.Dataset{{ID: "ds1", CaseCount: 10}, {ID: "ds2", CaseCount: 6}},
			samples: []evaluations.RunSample{
				{RunID: "r1", Cases: 4, Passed: 2, CostCents: 1.5, CreatedAt: now.Add(-26 * time.Hour)},
				{RunID: "r2", Cases: 4, Passed: 4, CostCents: 2.5, CreatedAt: now.Add(-2 * time.Hour)},
			},
		},
		Policies: &stubPolicies{rows: []*policies.Policy{
			{ID: "pa", Name: "agent guard", Effect: policies.EffectDeny, ResourceType: policies.ResourceAgent, Actions: []string{"tool.call"}, Enabled: true, Priority: 5, Conditions: policies.Conditions{RequireApproval: true}},
			{ID: "pw", Name: "wild", Effect: policies.EffectAllow, ResourceType: policies.ResourceWildcard, Actions: []string{"*"}, Enabled: true, Priority: 1},
			{ID: "pt", Name: "tool rule", Effect: policies.EffectDeny, ResourceType: policies.ResourceTool, Actions: []string{"tool.call"}, Enabled: true},
		}},
		OrgAPIKeys: &stubOrgKeys{n: 1},
		AgentKeys: &stubAgentKeys{byAgent: map[string][]*apikeys.AgentKey{
			"agent-1": {
				{ID: "k1", Name: "billing", ToolAllowList: []string{"refund"}},
				{ID: "k2", Name: "crm", ToolAllowList: []string{"crm.lookup", "refund"}},
				{ID: "k3", Name: "revoked", ToolAllowList: []string{"admin.console"}, RevokedAt: ptrTime(now)},
			},
		}},
		Approvals:   &stubApprovals{rows: []*approvals.Approval{{Status: approvals.StatusPending}}},
		MCPRegistry: &stubMCP{n: 1},
		Now:         fixedClock(now),
	}, aud
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestTechnicalDocShape verifies the structured document end to end.
func TestTechnicalDocShape(t *testing.T) {
	deps, _ := techDeps()
	svc := NewService(deps)
	doc, err := svc.TechnicalDoc(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("TechnicalDoc: %v", err)
	}
	if doc.Agent.Name != "support-bot" || doc.Agent.Model != "gpt-4o-mini" || doc.Agent.Status != "active" {
		t.Errorf("agent identity = %+v", doc.Agent)
	}
	if doc.OrganizationID != "org-1" || doc.GeneratedAt != testClock().Format(time.RFC3339) {
		t.Errorf("header = org %q generated %q", doc.OrganizationID, doc.GeneratedAt)
	}
	// Versions ascending.
	if len(doc.Versions) != 2 || doc.Versions[0].Version != 1 || doc.Versions[1].Version != 2 {
		t.Fatalf("versions = %+v, want ascending [1, 2]", doc.Versions)
	}
	if doc.Versions[1].PublishedAt == "" || doc.Versions[1].PublishedBy != "alice" {
		t.Errorf("published version render = %+v", doc.Versions[1])
	}
	// Eval aggregates: 8 cases, 6 passed => 0.75; last run at r2's creation.
	if doc.Evaluations.Datasets != 2 || doc.Evaluations.DatasetCases != 16 {
		t.Errorf("eval datasets = %+v", doc.Evaluations)
	}
	if doc.Evaluations.CompletedRuns != 2 || doc.Evaluations.PassRate != 0.75 {
		t.Errorf("eval runs = %+v, want 2 runs at pass rate 0.75", doc.Evaluations)
	}
	if doc.Evaluations.LastRunAt != testClock().Add(-2*time.Hour).UTC().Format(time.RFC3339) {
		t.Errorf("last run = %q", doc.Evaluations.LastRunAt)
	}
	// Deployments sorted by environment then id ("production" < "staging");
	// canary + policy + decision on the staging row.
	if len(doc.Deployments) != 3 || doc.Deployments[2].Environment != "staging" {
		t.Fatalf("deployments order = %+v", doc.Deployments)
	}
	c := doc.Deployments[2]
	if c.Canary == nil || c.Canary.CanaryVersion != 3 || c.Canary.Weight != 25 || !c.Canary.Active {
		t.Errorf("canary = %+v", c.Canary)
	}
	if c.Policy == nil || c.Policy.MinPassRate != 0.9 || c.Policy.MinCanaryRuns != 3 || c.Policy.MaxP95LatencyMs != 2500 {
		t.Errorf("policy = %+v", c.Policy)
	}
	if c.Decision == nil || c.Decision.Action != "rollback" || c.Decision.PassRate != 0.6152 {
		t.Errorf("decision = %+v", c.Decision)
	}
	// Tool surface: union of the two active allow-lists, sorted; revoked key
	// excluded from the count and its tool absent.
	ts := doc.ToolSurface
	if ts.Mode != "allowlist" || ts.AgentKeys != 2 {
		t.Fatalf("tool surface = %+v", ts)
	}
	if strings.Join(ts.Tools, ",") != "crm.lookup,refund" {
		t.Errorf("tools = %v, want sorted union [crm.lookup refund]", ts.Tools)
	}
	// Policy bindings: only agent + wildcard rules.
	if len(doc.PolicyBindings) != 2 {
		t.Fatalf("policy bindings = %+v, want agent + wildcard only", doc.PolicyBindings)
	}
	if doc.PolicyBindings[0].ID != "pa" || !doc.PolicyBindings[0].RequireApproval {
		t.Errorf("agent policy binding = %+v", doc.PolicyBindings[0])
	}
	// Audit posture: head + 30-day window count (2 of 4 entries).
	if doc.AuditPosture.ChainHead != "chain-head-42" || doc.AuditPosture.ChainLength != 12 {
		t.Errorf("audit posture = %+v", doc.AuditPosture)
	}
	if doc.AuditPosture.EntriesLast30Days != 2 {
		t.Errorf("entries_last_30_days = %d, want 2", doc.AuditPosture.EntriesLast30Days)
	}
}

// TestTechnicalDocForeignAgent404 — unknown and foreign collapse into one
// sentinel (no existence leak); unrelated backend errors propagate.
func TestTechnicalDocForeignAgent404(t *testing.T) {
	deps, _ := techDeps()
	deps.Agents = &stubAgents{getErr: agents.ErrAgentNotFound}
	svc := NewService(deps)
	if _, err := svc.TechnicalDoc(context.Background(), "org-1", "foreign-agent"); err != ErrAgentNotFound {
		t.Errorf("foreign agent = %v, want ErrAgentNotFound", err)
	}

	deps.Agents = &stubAgents{getErr: errBoom}
	svc = NewService(deps)
	if _, err := svc.TechnicalDoc(context.Background(), "org-1", "agent-1"); err == nil || err == ErrAgentNotFound {
		t.Errorf("backend error = %v, want propagated non-sentinel error", err)
	}
}

// TestTechnicalDocJSONDeterministic — same data + fixed clock => identical
// bytes, with the stable top-level field order starting at "agent".
func TestTechnicalDocJSONDeterministic(t *testing.T) {
	deps, _ := techDeps()
	svc := NewService(deps)
	doc, err := svc.TechnicalDoc(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("TechnicalDoc: %v", err)
	}
	a, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc2, err := svc.TechnicalDoc(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("TechnicalDoc: %v", err)
	}
	b, err := json.Marshal(doc2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("TechnicalDoc JSON is not byte-deterministic for unchanged data")
	}
	if !strings.HasPrefix(string(a), `{"agent":{`) {
		t.Errorf("unexpected top-level field order: %.20s", a)
	}
}

// TestTechnicalDocMDSectionOrder pins the Markdown section order and the
// deterministic rendering.
func TestTechnicalDocMDSectionOrder(t *testing.T) {
	deps, _ := techDeps()
	svc := NewService(deps)
	doc, err := svc.TechnicalDoc(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("TechnicalDoc: %v", err)
	}
	md := svc.RenderMarkdown(doc)
	md2 := svc.RenderMarkdown(doc)
	if !bytes.Equal(md, md2) {
		t.Error("Markdown rendering is not deterministic")
	}
	text := string(md)
	wantOrder := []string{
		"# Technical Documentation — support-bot",
		"## 1. Agent Identity",
		"## 2. Version History",
		"## 3. Evaluation Evidence",
		"## 4. Deployment & Canary Policy",
		"## 5. Tool Surface",
		"## 6. Policy Bindings",
		"## 7. Audit Posture",
	}
	last := -1
	for _, heading := range wantOrder {
		idx := strings.Index(text, heading)
		if idx < 0 {
			t.Fatalf("section %q missing from Markdown", heading)
		}
		if idx <= last {
			t.Errorf("section %q out of order (idx %d <= %d)", heading, idx, last)
		}
		last = idx
	}
	if !strings.Contains(text, "pass rate: 0.7500") {
		t.Error("Markdown evaluation section missing the pass rate")
	}
	if !strings.Contains(text, "Mode: allowlist") || !strings.Contains(text, "Tools: crm.lookup, refund") {
		t.Error("Markdown tool surface section wrong")
	}
}

// TestTechnicalDocToolSurfaceUnrestricted — nil allow-list on any active key
// widens the surface to unrestricted (auditor-visible), and key-less agents
// get the RBAC note.
func TestTechnicalDocToolSurfaceUnrestricted(t *testing.T) {
	now := testClock()
	base := func(keys []*apikeys.AgentKey) Deps {
		deps, _ := techDeps()
		deps.AgentKeys = &stubAgentKeys{byAgent: map[string][]*apikeys.AgentKey{"agent-1": keys}}
		deps.Now = fixedClock(now)
		return deps
	}

	// (a) an active key with a DISABLED allow-list (nil) => unrestricted.
	deps := base([]*apikeys.AgentKey{{ID: "k1", ToolAllowList: nil}, {ID: "k2", ToolAllowList: []string{"x"}}})
	doc, err := NewService(deps).TechnicalDoc(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("TechnicalDoc: %v", err)
	}
	if doc.ToolSurface.Mode != "unrestricted" || doc.ToolSurface.AgentKeys != 2 {
		t.Errorf("nil allow-list surface = %+v", doc.ToolSurface)
	}

	// (b) an EMPTY allow-list means NO tools.
	deps = base([]*apikeys.AgentKey{{ID: "k1", ToolAllowList: []string{}}})
	doc, err = NewService(deps).TechnicalDoc(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("TechnicalDoc: %v", err)
	}
	if doc.ToolSurface.Mode != "allowlist" || len(doc.ToolSurface.Tools) != 0 {
		t.Errorf("empty allow-list surface = %+v", doc.ToolSurface)
	}

	// (c) no keys at all => unrestricted + RBAC note.
	deps = base(nil)
	doc, err = NewService(deps).TechnicalDoc(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("TechnicalDoc: %v", err)
	}
	if doc.ToolSurface.Mode != "unrestricted" || doc.ToolSurface.AgentKeys != 0 || doc.ToolSurface.Note == "" {
		t.Errorf("key-less surface = %+v", doc.ToolSurface)
	}

	// (d) an EXPIRED key is not active.
	expired := now.Add(-time.Hour)
	deps = base([]*apikeys.AgentKey{{ID: "k1", ToolAllowList: []string{"x"}, ExpiresAt: &expired}})
	doc, err = NewService(deps).TechnicalDoc(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("TechnicalDoc: %v", err)
	}
	if doc.ToolSurface.AgentKeys != 0 || doc.ToolSurface.Mode != "unrestricted" {
		t.Errorf("expired-key surface = %+v", doc.ToolSurface)
	}
}

// TestTechnicalDocAuditWindowBoundary — an entry exactly at the 30-day
// cutoff start counts (inclusive lower bound), one nanosecond earlier does
// not.
func TestTechnicalDocAuditWindowBoundary(t *testing.T) {
	now := testClock()
	cutoff := now.Add(-30 * 24 * time.Hour)
	deps, _ := techDeps()
	deps.Audit = &stubAudit{headHash: "h", length: 2, entries: []*audit.Entry{
		{ID: "on-cutoff", OrganizationID: "org-1", CreatedAt: cutoff},
		{ID: "before-cutoff", OrganizationID: "org-1", CreatedAt: cutoff.Add(-time.Nanosecond)},
		{ID: "now", OrganizationID: "org-1", CreatedAt: now},
	}}
	doc, err := NewService(deps).TechnicalDoc(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("TechnicalDoc: %v", err)
	}
	if doc.AuditPosture.EntriesLast30Days != 2 {
		t.Errorf("entries_last_30_days = %d, want 2 (cutoff-inclusive, now-inclusive)", doc.AuditPosture.EntriesLast30Days)
	}
}
