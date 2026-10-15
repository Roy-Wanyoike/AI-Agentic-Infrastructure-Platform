package compliance

// evidence_test.go — the JSONL evidence bundle tests: byte-determinism for a
// fixed window + unchanged data, window filtering, fixed line order, and the
// no-secrets hygiene contract.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/policies"
)

// bundleDeps builds wired stubs for the bundle tests.
func bundleDeps() (Deps, *stubAudit) {
	now := testClock()
	aud := &stubAudit{
		headHash: "cafe1234",
		length:   5,
		entries: []*audit.Entry{
			{ID: "in-1", OrganizationID: "org-1", CreatedAt: now.Add(-24 * time.Hour), Metadata: map[string]any{"secret_material": "DO-NOT-LEAK"}},
			{ID: "in-2", OrganizationID: "org-1", CreatedAt: now.Add(-48 * time.Hour)},
			{ID: "out-1", OrganizationID: "org-1", CreatedAt: now.Add(-120 * time.Hour)},
		},
	}
	return Deps{
		Audit: aud,
		Policies: &stubPolicies{rows: []*policies.Policy{
			{ID: "p1", Effect: policies.EffectDeny, Enabled: true},
			{ID: "p2", Effect: policies.EffectDeny, Enabled: false},
			{ID: "p3", Effect: policies.EffectAllow, Enabled: true},
		}},
		Approvals: &stubApprovals{rows: []*approvals.Approval{
			{ID: "ap1", Status: approvals.StatusApproved, CreatedAt: now.Add(-24 * time.Hour)}, // in window
			{ID: "ap2", Status: approvals.StatusPending, CreatedAt: now.Add(-36 * time.Hour)},  // in window
			{ID: "ap3", Status: approvals.StatusApproved, CreatedAt: now.Add(-96 * time.Hour)}, // out
		}},
		OrgAPIKeys: &stubOrgKeys{n: 4},
		Agents:     &stubAgents{list: []*agents.Agent{{ID: "agent-1"}, {ID: "agent-2"}}},
		AgentKeys: &stubAgentKeys{byAgent: map[string][]*apikeys.AgentKey{
			"agent-2": {{ID: "k1"}},
		}},
		Now: fixedClock(now),
	}, aud
}

// TestEvidenceBundleDeterministic — two exports over the same fixed window
// are byte-identical; the line order is fixed and the footer closes it.
func TestEvidenceBundleDeterministic(t *testing.T) {
	deps, _ := bundleDeps()
	svc := NewService(deps)
	from := testClock().Add(-72 * time.Hour)
	to := testClock()
	a, err := svc.EvidenceBundle(context.Background(), "org-1", from, to)
	if err != nil {
		t.Fatalf("EvidenceBundle: %v", err)
	}
	b, err := svc.EvidenceBundle(context.Background(), "org-1", from, to)
	if err != nil {
		t.Fatalf("EvidenceBundle: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("evidence bundle is not byte-deterministic for unchanged data + fixed window")
	}

	lines := bytes.Split(bytes.TrimRight(a, "\n"), []byte("\n"))
	if len(lines) != 6 {
		t.Fatalf("bundle lines = %d, want 6 (header, chain, policy, approvals, access, footer)", len(lines))
	}
	types := make([]string, 0, len(lines))
	for _, l := range lines {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(l, &head); err != nil {
			t.Fatalf("bundle line is not JSON: %s", l)
		}
		types = append(types, head.Type)
	}
	want := []string{"compliance_evidence", "audit_chain_head", "policy_summary", "approval_oversight_summary", "access_lifecycle_summary", "evidence_end"}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("line %d type = %s, want %s", i, types[i], want[i])
		}
	}
	// Header carries org + the window verbatim (normalized RFC3339Nano UTC).
	var header struct {
		Org  string `json:"org"`
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("header unmarshal: %v", err)
	}
	if header.Org != "org-1" || header.From != from.UTC().Format(time.RFC3339Nano) || header.To != to.UTC().Format(time.RFC3339Nano) {
		t.Errorf("header = %+v", header)
	}
}

// TestEvidenceBundleWindowFiltering — the window drives entries_in_window
// and the approval counts; the chain anchor is current state.
func TestEvidenceBundleWindowFiltering(t *testing.T) {
	deps, _ := bundleDeps()
	svc := NewService(deps)
	bundle, err := svc.EvidenceBundle(context.Background(), "org-1", testClock().Add(-72*time.Hour), testClock())
	if err != nil {
		t.Fatalf("EvidenceBundle: %v", err)
	}
	text := string(bundle)

	var chain struct {
		HeadHash        string `json:"head_hash"`
		ChainLength     int    `json:"chain_length"`
		EntriesInWindow int    `json:"entries_in_window"`
		ChainVersion    string `json:"chain_version"`
	}
	if err := json.Unmarshal([]byte(strings.Split(text, "\n")[1]), &chain); err != nil {
		t.Fatalf("chain line: %v", err)
	}
	if chain.HeadHash != "cafe1234" || chain.ChainLength != 5 {
		t.Errorf("chain anchor = %+v", chain)
	}
	if chain.EntriesInWindow != 2 {
		t.Errorf("entries_in_window = %d, want 2", chain.EntriesInWindow)
	}
	if chain.ChainVersion != auditChainVersionV1 {
		t.Errorf("chain_version = %q, want %q", chain.ChainVersion, auditChainVersionV1)
	}

	var policyLine struct {
		Total   int `json:"total"`
		Enabled int `json:"enabled"`
		Allow   int `json:"effect_allow"`
		Deny    int `json:"effect_deny"`
	}
	if err := json.Unmarshal([]byte(strings.Split(text, "\n")[2]), &policyLine); err != nil {
		t.Fatalf("policy line: %v", err)
	}
	if policyLine.Total != 3 || policyLine.Enabled != 2 || policyLine.Allow != 1 || policyLine.Deny != 2 {
		t.Errorf("policy summary = %+v", policyLine)
	}

	var approvalsLine struct {
		Pending  int `json:"pending"`
		Approved int `json:"approved"`
		Total    int `json:"total"`
	}
	if err := json.Unmarshal([]byte(strings.Split(text, "\n")[3]), &approvalsLine); err != nil {
		t.Fatalf("approval line: %v", err)
	}
	if approvalsLine.Pending != 1 || approvalsLine.Approved != 1 || approvalsLine.Total != 2 {
		t.Errorf("approval summary = %+v, want window-filtered pending 1 approved 1 total 2", approvalsLine)
	}

	var access struct {
		APIKeys   *int `json:"api_keys"`
		AgentKeys *int `json:"agent_keys"`
	}
	if err := json.Unmarshal([]byte(strings.Split(text, "\n")[4]), &access); err != nil {
		t.Fatalf("access line: %v", err)
	}
	if access.APIKeys == nil || *access.APIKeys != 4 {
		t.Errorf("access api_keys = %+v, want 4", access.APIKeys)
	}
	if access.AgentKeys == nil || *access.AgentKeys != 1 {
		t.Errorf("access agent_keys = %+v, want 1 (one key across the org agents)", access.AgentKeys)
	}
}

// TestEvidenceBundleNoSecretMaterial — metadata bodies and payload-ish
// fields never reach the bundle; only counts, hashes and bounds do.
func TestEvidenceBundleNoSecretMaterial(t *testing.T) {
	deps, _ := bundleDeps()
	svc := NewService(deps)
	bundle, err := svc.EvidenceBundle(context.Background(), "org-1", testClock().Add(-72*time.Hour), testClock())
	if err != nil {
		t.Fatalf("EvidenceBundle: %v", err)
	}
	for _, forbidden := range []string{"DO-NOT-LEAK", "secret_material", "metadata"} {
		if bytes.Contains(bundle, []byte(forbidden)) {
			t.Errorf("bundle leaks %q", forbidden)
		}
	}
}

// TestEvidenceBundleWindowValidation — service-level window guards.
func TestEvidenceBundleWindowValidation(t *testing.T) {
	svc := NewService(Deps{})
	ctx := context.Background()
	if _, err := svc.EvidenceBundle(ctx, "org-1", time.Time{}, testClock()); err != ErrWindowRequired {
		t.Errorf("zero from = %v, want ErrWindowRequired", err)
	}
	if _, err := svc.EvidenceBundle(ctx, "org-1", testClock().Add(time.Hour), testClock()); err != ErrWindowRange {
		t.Errorf("from after to = %v, want ErrWindowRange", err)
	}
	if _, err := svc.EvidenceBundle(ctx, "", testClock(), testClock()); err != ErrOrgRequired {
		t.Errorf("empty org = %v, want ErrOrgRequired", err)
	}
}

// TestParseWindow — RFC3339 verbatim, date-only boundaries resolve to the
// day span, invalid input and inverted ranges fail with sentinels.
func TestParseWindow(t *testing.T) {
	// RFC3339 both ends: honored verbatim (converted to UTC).
	from, to, err := ParseWindow("2025-01-01T10:30:00+02:00", "2025-01-31T23:59:59Z")
	if err != nil {
		t.Fatalf("ParseWindow RFC3339: %v", err)
	}
	if want := time.Date(2025, 1, 1, 8, 30, 0, 0, time.UTC); !from.Equal(want) {
		t.Errorf("from = %v, want %v", from, want)
	}
	if want := time.Date(2025, 1, 31, 23, 59, 59, 0, time.UTC); !to.Equal(want) {
		t.Errorf("to = %v, want %v", to, want)
	}

	// Date-only: from at midnight, to at the end of that day (inclusive).
	from, to, err = ParseWindow("2025-01-01", "2025-01-01")
	if err != nil {
		t.Fatalf("ParseWindow date-only: %v", err)
	}
	if !from.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date-only from = %v", from)
	}
	if !to.Equal(time.Date(2025, 1, 1, 23, 59, 59, 999999999, time.UTC)) {
		t.Errorf("date-only to = %v, want end of day", to)
	}

	if _, _, err := ParseWindow("", "2025-01-01"); err != ErrWindowRequired {
		t.Errorf("missing from = %v, want ErrWindowRequired", err)
	}
	if _, _, err := ParseWindow("2025-01-01", ""); err != ErrWindowRequired {
		t.Errorf("missing to = %v, want ErrWindowRequired", err)
	}
	if _, _, err := ParseWindow("not-a-date", "2025-01-01"); !errors.Is(err, ErrInvalidWindowInput) {
		t.Errorf("bad input = %v, want ErrInvalidWindowInput", err)
	}
	if _, _, err := ParseWindow("2025-02-01", "2025-01-01"); err != ErrWindowRange {
		t.Errorf("inverted range = %v, want ErrWindowRange", err)
	}
}
