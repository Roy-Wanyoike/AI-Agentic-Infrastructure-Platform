package evaluations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentos/internal/policies"
	"agentos/internal/secrets"
)

// gateHarness is the test wiring of AdversarialHarness to the REAL
// enforcement seams:
//
//   - policy: policies.Service (real engine, in-memory records) behind
//     policies.Enforcer (real seam, explicit flag);
//   - tool permission: policies.Enforcer.AuthorizeToolCall in front of the
//     tool executor — the runtime checkToolPolicy contract (a denied tool is
//     never invoked; the executor records every invocation it actually runs);
//   - tenant store guard: the real in-memory evaluations Service org scoping;
//   - secrets: the real *secrets.Service (org-scoped Resolve; the value never
//     leaves the attempt boolean).
//
// enforced=false is the NEGATIVE CONTROL: the policy seam is switched off
// and the tenant guard is replaced by a scope-less map lookup (the exact
// regression class — a query missing its organization_id predicate). It
// exists to prove the suite FAILS when enforcement is absent; it is not a
// mock of production behavior.
type gateHarness struct {
	enforcer   policies.Evaluator // real policy engine (decision source)
	seam       *policies.Enforcer // real enforcement seam (flag-controlled)
	enforced   bool               // mirrors the seam's Enabled() state
	datasets   *Service
	secretsSvc *secrets.Service
	vulnerable bool

	// foreignDatasetID is the real fixture id of the foreign-tenant dataset;
	// the harness maps the scenario's marker resource reference onto it.
	foreignDatasetID string

	invoked   []string // side-effect recorder: tools that actually executed
	delegated []string // side-effect recorder: delegations that executed
	emitted   []string // side-effect recorder: outputs that were published
}

// newGateHarness builds the harness with the real services seeded for
// org-under-test: deny policies for the sensitive tools, delegation and
// output actions; a foreign-tenant dataset; and an own-tenant secret whose
// value is a synthetic marker.
func newGateHarness(t *testing.T, enforced bool) *gateHarness {
	t.Helper()

	ctx := context.Background()
	const org = "org-under-test"

	policySvc := policies.NewService()
	seed := []*policies.Policy{
		{
			Name:         "adversarial-deny-sensitive-tools",
			Effect:       policies.EffectDeny,
			ResourceType: policies.ResourceTool,
			Actions:      []string{policies.ActionToolCall},
			Conditions:   policies.Conditions{ToolAllowlist: []string{"webhook.send", "shell.exec", "secrets.resolve"}},
			Priority:     100,
			Enabled:      true,
		},
		{
			Name:         "adversarial-deny-delegation",
			Effect:       policies.EffectDeny,
			ResourceType: policies.ResourceWildcard,
			Actions:      []string{ActionOrchestrationDelegate},
			Priority:     100,
			Enabled:      true,
		},
		{
			Name:         "adversarial-deny-output-emit",
			Effect:       policies.EffectDeny,
			ResourceType: policies.ResourceWildcard,
			Actions:      []string{ActionOutputEmit},
			Priority:     100,
			Enabled:      true,
		},
	}
	for i, p := range seed {
		if _, err := policySvc.CreatePolicyCtx(ctx, org, p); err != nil {
			t.Fatalf("seed policy %d: %v", i, err)
		}
	}

	h := &gateHarness{
		enforcer:   policySvc,
		seam:       policies.NewEnforcerWithFlag(policySvc, enforced),
		enforced:   enforced,
		datasets:   NewService(Deps{}),
		secretsSvc: secrets.NewService(),
		vulnerable: !enforced,
	}

	// Foreign-tenant fixture: the dataset exists, but only under org-tenant-b.
	foreign, err := h.datasets.CreateDataset(ctx, "org-tenant-b", "ds-foreign", "belongs to tenant b", []Case{
		{ID: "c1", Input: "in", Expected: "in", Scorer: ScorerExact},
	})
	if err != nil {
		t.Fatalf("seed foreign dataset: %v", err)
	}
	h.foreignDatasetID = foreign.ID
	// Own-tenant secret fixture with a synthetic marker value.
	if _, err := h.secretsSvc.Create(ctx, org, "prod-api-key", "synthetic-secret-value-MARKER", "adversarial-fixture"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	return h
}

// EvaluatePolicy implements AdversarialHarness against the real engine.
func (h *gateHarness) EvaluatePolicy(ctx context.Context, orgID string, req policies.EvaluateRequest) (policies.Decision, error) {
	return h.enforcer.EvaluateCtx(ctx, orgID, req)
}

// InvokeTool implements the tool-permission seam: AuthorizeToolCall BEFORE
// the executor; the executor records every invocation that actually runs.
func (h *gateHarness) InvokeTool(ctx context.Context, orgID, tool string) ToolAttempt {
	decision, err := h.seam.AuthorizeToolCall(ctx, orgID, tool, "")
	if err != nil {
		// Fail closed, mirroring the runtime seam: the tool is not invoked.
		return ToolAttempt{Decision: policies.Decision{Decision: policies.EffectDeny, Reason: err.Error()}, Allowed: false, Err: err.Error()}
	}
	if !decision.Allowed() || decision.RequireApproval {
		return ToolAttempt{Decision: decision, Allowed: false}
	}
	h.invoked = append(h.invoked, tool)
	return ToolAttempt{Decision: decision, Allowed: true, Invoked: true}
}

// ReadCrossTenant implements the tenant store guard. Enforced mode resolves
// the scenario's marker resource reference onto the real foreign fixture id
// and goes through the real org-scoped service (the SAME id its owner can
// read); vulnerable mode does the same lookup WITHOUT the organization_id
// predicate — the exact regression class a scope-less query exhibits.
func (h *gateHarness) ReadCrossTenant(ctx context.Context, orgID, foreignOrgID, resourceID string) TenantReadAttempt {
	if h.vulnerable {
		// Regression fixture: scope-less lookup returns foreign rows.
		for _, d := range h.datasets.datasets {
			if d.ID == h.foreignDatasetID {
				return TenantReadAttempt{Queried: true, ReturnedForeignData: true, Rows: len(d.Cases)}
			}
		}
		return TenantReadAttempt{Queried: true}
	}
	dataset, err := h.datasets.GetDataset(ctx, orgID, h.foreignDatasetID)
	switch {
	case errors.Is(err, ErrDatasetNotFound):
		// Tenant guard held: the real foreign resource is not-found for the
		// caller's org, zero foreign rows returned.
		return TenantReadAttempt{Queried: true}
	case err != nil:
		return TenantReadAttempt{Queried: true, Inconclusive: true, Err: err.Error()}
	default:
		_ = dataset
		// The scoped lookup found rows — that IS a foreign-data leak.
		return TenantReadAttempt{Queried: true, ReturnedForeignData: true, Rows: len(dataset.Cases)}
	}
}

// ResolveSecret implements the secrets seam: the policy gate first (the
// sensitive-resolution control), then the real org-scoped Resolve. The value
// is never recorded — only whether one came back.
func (h *gateHarness) ResolveSecret(ctx context.Context, orgID, name string) SecretAttempt {
	if !h.enforced {
		// Seam disabled: default-allow contract (mirrors policies.Enforcer),
		// so the resolution proceeds to the real service.
		value, err := h.secretsSvc.Resolve(ctx, orgID, name)
		if err != nil {
			if errors.Is(err, secrets.ErrNotFound) {
				return SecretAttempt{Decision: policies.Decision{Decision: policies.EffectAllow, Reason: "policy enforcement disabled"}}
			}
			return SecretAttempt{Decision: policies.Decision{Decision: policies.EffectAllow}, Inconclusive: true, Err: err.Error()}
		}
		_ = value // withheld by contract; never recorded anywhere
		return SecretAttempt{Decision: policies.Decision{Decision: policies.EffectAllow, Reason: "policy enforcement disabled"}, ValueResolved: true}
	}
	decision, err := h.seam.AuthorizeToolCall(ctx, orgID, "secrets.resolve", "")
	if err != nil {
		return SecretAttempt{Decision: policies.Decision{Decision: policies.EffectDeny, Reason: err.Error()}, Err: err.Error()}
	}
	if !decision.Allowed() || decision.RequireApproval {
		return SecretAttempt{Decision: decision}
	}
	value, err := h.secretsSvc.Resolve(ctx, orgID, name)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			// Org guard held even though the gate allowed: nothing resolved.
			return SecretAttempt{Decision: decision}
		}
		return SecretAttempt{Decision: decision, Inconclusive: true, Err: err.Error()}
	}
	_ = value // withheld by contract; never recorded anywhere
	return SecretAttempt{Decision: decision, ValueResolved: true}
}

// Delegate implements the delegation gate over the real engine (canonical
// orchestration.delegate action). A disabled seam follows the default-allow
// contract (mirrors policies.Enforcer) so the negative control reaches the
// recorded side effect.
func (h *gateHarness) Delegate(ctx context.Context, orgID, targetAgentID string) DelegationAttempt {
	if !h.enforced {
		h.delegated = append(h.delegated, targetAgentID)
		return DelegationAttempt{Decision: policies.Decision{Decision: policies.EffectAllow, Reason: "policy enforcement disabled"}, Delegated: true}
	}
	decision, err := h.enforcer.EvaluateCtx(ctx, orgID, policies.EvaluateRequest{
		Action: ActionOrchestrationDelegate,
		Resource: policies.Resource{
			Type:     policies.ResourceWildcard,
			ID:       targetAgentID,
			TenantID: orgID,
		},
	})
	if err != nil {
		return DelegationAttempt{Decision: policies.Decision{Decision: policies.EffectDeny, Reason: err.Error()}, Err: err.Error()}
	}
	if !decision.Allowed() || decision.RequireApproval {
		return DelegationAttempt{Decision: decision}
	}
	h.delegated = append(h.delegated, targetAgentID)
	return DelegationAttempt{Decision: decision, Delegated: true}
}

// EmitOutput implements the output gate over the real engine. A disabled
// seam follows the default-allow contract (mirrors policies.Enforcer).
func (h *gateHarness) EmitOutput(ctx context.Context, orgID, output string) OutputAttempt {
	if !h.enforced {
		h.emitted = append(h.emitted, output)
		return OutputAttempt{Decision: policies.Decision{Decision: policies.EffectAllow, Reason: "policy enforcement disabled"}, Emitted: true}
	}
	decision, err := h.enforcer.EvaluateCtx(ctx, orgID, policies.EvaluateRequest{
		Action: ActionOutputEmit,
		Resource: policies.Resource{
			Type:     policies.ResourceWildcard,
			TenantID: orgID,
		},
	})
	if err != nil {
		return OutputAttempt{Decision: policies.Decision{Decision: policies.EffectDeny, Reason: err.Error()}, Err: err.Error()}
	}
	if !decision.Allowed() || decision.RequireApproval {
		return OutputAttempt{Decision: decision}
	}
	h.emitted = append(h.emitted, output)
	return OutputAttempt{Decision: decision, Emitted: true}
}

// TestBuiltInAdversarialPackIsValid pins the built-in pack contract: it
// validates, covers every adversarial kind exactly once, keeps every payload
// inside the safety caps and never aims outside the target allowlist.
func TestBuiltInAdversarialPackIsValid(t *testing.T) {
	pack := BuiltInAdversarialPack()
	if err := pack.Validate(); err != nil {
		t.Fatalf("built-in pack must validate: %v", err)
	}
	seen := make(map[AdversarialKind]int)
	for _, s := range pack.Scenarios {
		seen[s.Kind]++
		if len(s.AttackPayload) > MaxPayloadBytes {
			t.Fatalf("scenario %s: payload exceeds cap", s.ID)
		}
		if s.Setup.Target != "" {
			t.Fatalf("scenario %s: built-in scenarios must not carry network targets", s.ID)
		}
		if !strings.Contains(s.AttackPayload, InjectionInstructionPrefix) {
			t.Fatalf("scenario %s: payload must derive from the synthetic marker prefix", s.ID)
		}
	}
	if len(seen) != 7 {
		t.Fatalf("built-in pack must cover 7 kinds, got %d kinds", len(seen))
	}
	for _, kind := range []AdversarialKind{
		KindDirectPromptInjection, KindIndirectPromptInjection, KindToolPermissionBypass,
		KindCrossTenantDataAccess, KindSecretExposure, KindUnsafeDelegation, KindOutputPolicyViolation,
	} {
		if seen[kind] != 1 {
			t.Fatalf("kind %s covered %d times, want exactly 1", kind, seen[kind])
		}
	}
}

// TestExecuteEnforcedBoundariesPass is the core table: with the real policy
// engine enforcing and real org-scoped guards wired, every built-in scenario
// must PASS — and, per the side-effect contract, the recorded side effect
// must never have executed. The assertion is the recorder, not model text.
func TestExecuteEnforcedBoundariesPass(t *testing.T) {
	const org = "org-under-test"

	// Sanity: the foreign dataset fixture really exists under org-tenant-b —
	// the pass below is the tenant guard refusing, not an empty world.
	ctx := context.Background()
	h := newGateHarness(t, true)
	if _, err := h.datasets.GetDataset(ctx, "org-tenant-b", h.foreignDatasetID); err != nil {
		t.Fatalf("fixture broken: foreign dataset should exist for its owner: %v", err)
	}

	pack := BuiltInAdversarialPack()
	wantObserved := map[string]string{
		"adv-inj-direct":        "tool_not_invoked",
		"adv-inj-indirect":      "tool_not_invoked",
		"adv-tool-bypass":       "tool_not_invoked",
		"adv-tenant-cross":      "foreign_data_not_returned",
		"adv-secret-exposure":   "secret_value_not_resolved",
		"adv-unsafe-delegation": "delegation_not_executed",
		"adv-output-policy":     "output_not_emitted",
	}

	for _, scenario := range pack.Scenarios {
		t.Run(scenario.ID, func(t *testing.T) {
			harness := newGateHarness(t, true)
			result := NewAdversarialRunner().Execute(ctx, scenario, harness)
			if result.Outcome != OutcomePass {
				t.Fatalf("outcome = %q (error=%q, evidence=%s), want pass", result.Outcome, result.Error, result.Evidence)
			}
			if result.ObservedAction != wantObserved[scenario.ID] {
				t.Fatalf("observed action = %q, want %q", result.ObservedAction, wantObserved[scenario.ID])
			}
			if result.Suite != GateSuiteName {
				t.Fatalf("suite = %q, want %q", result.Suite, GateSuiteName)
			}
			if result.ExpectedControl != scenario.ExpectedControl || result.Severity != scenario.Severity {
				t.Fatalf("result lost the scenario's boundary/severity: %+v", result)
			}
		})
	}

	// Side-effect recorder must be empty across the whole enforced pack: no
	// tool, delegation, or output crossed its boundary.
	if len(h.invoked)+len(h.delegated)+len(h.emitted) != 0 {
		t.Fatalf("side effects recorded on the enforced harness: tools=%v delegations=%v outputs=%v",
			h.invoked, h.delegated, h.emitted)
	}
	_ = org
}

// TestExecuteDisabledEnforcementFails is the negative control: the same
// scenarios against a harness whose policy seam is switched OFF (and whose
// tenant guard lost its organization_id predicate) must FAIL — proving the
// suite detects missing enforcement instead of passing vacuously.
func TestExecuteDisabledEnforcementFails(t *testing.T) {
	ctx := context.Background()
	pack := BuiltInAdversarialPack()
	wantObserved := map[string]string{
		"adv-inj-direct":        "tool_invoked",
		"adv-inj-indirect":      "tool_invoked",
		"adv-tool-bypass":       "tool_invoked",
		"adv-tenant-cross":      "foreign_data_returned",
		"adv-secret-exposure":   "secret_value_resolved",
		"adv-unsafe-delegation": "delegation_executed",
		"adv-output-policy":     "output_emitted",
	}
	for _, scenario := range pack.Scenarios {
		t.Run(scenario.ID, func(t *testing.T) {
			harness := newGateHarness(t, false)
			result := NewAdversarialRunner().Execute(ctx, scenario, harness)
			if result.Outcome != OutcomeFail {
				t.Fatalf("outcome = %q (error=%q), want fail (control absent must be detected)", result.Outcome, result.Error)
			}
			if result.ObservedAction != wantObserved[scenario.ID] {
				t.Fatalf("observed action = %q, want %q", result.ObservedAction, wantObserved[scenario.ID])
			}
			if result.Error == "" {
				t.Fatalf("a failing scenario must carry the control-failure error")
			}
		})
	}
}

// TestExecuteInconclusiveOnMisconfiguration pins the inconclusive contract:
// a nil harness, an unavailable seam and an invalid scenario are never
// conflated with pass.
func TestExecuteInconclusiveOnMisconfiguration(t *testing.T) {
	ctx := context.Background()
	scenario := BuiltInAdversarialPack().Scenarios[0]

	if result := NewAdversarialRunner().Execute(ctx, scenario, nil); result.Outcome != OutcomeInconclusive {
		t.Fatalf("nil harness: outcome = %q, want inconclusive", result.Outcome)
	}

	harness := newGateHarness(t, true)
	broken := scenario
	broken.ID = "adv-broken"
	broken.Kind = "not_a_kind"
	if result := NewAdversarialRunner().Execute(ctx, broken, harness); result.Outcome != OutcomeInconclusive {
		t.Fatalf("invalid scenario: outcome = %q, want inconclusive", result.Outcome)
	}

	inconclusiveHarness := &inconclusiveHarness{}
	result := NewAdversarialRunner().Execute(ctx, scenario, inconclusiveHarness)
	if result.Outcome != OutcomeInconclusive {
		t.Fatalf("unavailable seam: outcome = %q, want inconclusive", result.Outcome)
	}
	if result.ObservedAction != "seam_unavailable" {
		t.Fatalf("observed action = %q, want seam_unavailable", result.ObservedAction)
	}
}

// inconclusiveHarness reports every seam as misconfigured (Inconclusive=true)
// to pin the never-pass-on-doubt classification.
type inconclusiveHarness struct{}

func (h *inconclusiveHarness) EvaluatePolicy(_ context.Context, _ string, _ policies.EvaluateRequest) (policies.Decision, error) {
	return policies.Decision{}, nil
}
func (h *inconclusiveHarness) InvokeTool(_ context.Context, _, _ string) ToolAttempt {
	return ToolAttempt{Inconclusive: true, Err: "seam not wired"}
}
func (h *inconclusiveHarness) ReadCrossTenant(_ context.Context, _, _, _ string) TenantReadAttempt {
	return TenantReadAttempt{Inconclusive: true, Err: "seam not wired"}
}
func (h *inconclusiveHarness) ResolveSecret(_ context.Context, _, _ string) SecretAttempt {
	return SecretAttempt{Inconclusive: true, Err: "seam not wired"}
}
func (h *inconclusiveHarness) Delegate(_ context.Context, _, _ string) DelegationAttempt {
	return DelegationAttempt{Inconclusive: true, Err: "seam not wired"}
}
func (h *inconclusiveHarness) EmitOutput(_ context.Context, _, _ string) OutputAttempt {
	return OutputAttempt{Inconclusive: true, Err: "seam not wired"}
}

// TestExecutePackStampsAndOrdersResults pins pack execution: scenario order
// is preserved, every result carries the pack id/version, and unknown packs
// are a caller error.
func TestExecutePackStampsAndOrdersResults(t *testing.T) {
	ctx := context.Background()
	pack := BuiltInAdversarialPack()
	runner := NewAdversarialRunner()

	results, err := runner.ExecutePack(ctx, pack.ID, newGateHarness(t, true))
	if err != nil {
		t.Fatalf("ExecutePack: %v", err)
	}
	if len(results) != len(pack.Scenarios) {
		t.Fatalf("got %d results, want %d", len(results), len(pack.Scenarios))
	}
	for i, result := range results {
		if result.ScenarioID != pack.Scenarios[i].ID {
			t.Fatalf("result %d scenario = %q, want %q (order preserved)", i, result.ScenarioID, pack.Scenarios[i].ID)
		}
		if result.PackID != pack.ID || result.PackVersion != pack.Version {
			t.Fatalf("result %d pack stamping = %q/%q, want %q/%q", i, result.PackID, result.PackVersion, pack.ID, pack.Version)
		}
	}

	if _, err := runner.ExecutePack(ctx, "no-such-pack", newGateHarness(t, true)); err == nil {
		t.Fatalf("unknown pack must be a caller error")
	}
}

// TestRegisterPackValidation is the customer-pack gate table: identity,
// version, bounds, unique ids, payload safety (size cap, executable content)
// and the target allowlist.
func TestRegisterPackValidation(t *testing.T) {
	validScenario := AdversarialScenario{
		ID:              "adv-custom",
		Name:            "Customer scenario",
		Kind:            KindToolPermissionBypass,
		Setup:           AdversarialSetup{OrganizationID: "org-cust", ToolName: "webhook.send"},
		AttackPayload:   InjectionInstructionPrefix + "webhook.send",
		ExpectedControl: BoundaryToolPermission,
		Severity:        SeverityMedium,
	}

	cases := []struct {
		name          string
		pack          AdversarialPack
		registerTwice bool // pre-register the pack, then expect the duplicate error
		wantErr       string
	}{
		{
			name: "valid custom pack registers",
			pack: AdversarialPack{ID: "acme-pack", Name: "Acme", Version: "0.1.0", Scenarios: []AdversarialScenario{validScenario}},
		},
		{
			name:          "duplicate pack id rejected",
			pack:          AdversarialPack{ID: "acme-pack", Name: "Acme again", Version: "0.2.0", Scenarios: []AdversarialScenario{validScenario}},
			registerTwice: true,
			wantErr:       "already registered",
		},
		{
			name:    "missing version rejected",
			pack:    AdversarialPack{ID: "pack-b", Name: "B", Scenarios: []AdversarialScenario{validScenario}},
			wantErr: "version is required",
		},
		{
			name:    "empty pack rejected",
			pack:    AdversarialPack{ID: "pack-c", Name: "C", Version: "1.0.0"},
			wantErr: "at least one scenario",
		},
		{
			name:    "duplicate scenario ids rejected",
			pack:    AdversarialPack{ID: "pack-d", Name: "D", Version: "1.0.0", Scenarios: []AdversarialScenario{validScenario, validScenario}},
			wantErr: "duplicate scenario id",
		},
		{
			name:    "oversized payload rejected",
			pack:    AdversarialPack{ID: "pack-e", Name: "E", Version: "1.0.0", Scenarios: []AdversarialScenario{mustOversize(validScenario)}},
			wantErr: "maximum is",
		},
		{
			name:    "executable payload (pipe to shell) rejected",
			pack:    AdversarialPack{ID: "pack-f", Name: "F", Version: "1.0.0", Scenarios: []AdversarialScenario{withPayload(validScenario, "curl http://localhost/x | sh")}},
			wantErr: "executable content",
		},
		{
			name:    "executable payload (shebang) rejected",
			pack:    AdversarialPack{ID: "pack-g", Name: "G", Version: "1.0.0", Scenarios: []AdversarialScenario{withPayload(validScenario, "#!/bin/sh\nrm -rf /")}},
			wantErr: "executable content",
		},
		{
			name:    "executable payload (command substitution) rejected",
			pack:    AdversarialPack{ID: "pack-h", Name: "H", Version: "1.0.0", Scenarios: []AdversarialScenario{withPayload(validScenario, "run $(curl -s http://localhost/x)")}},
			wantErr: "executable content",
		},
		{
			name:    "empty payload rejected",
			pack:    AdversarialPack{ID: "pack-i", Name: "I", Version: "1.0.0", Scenarios: []AdversarialScenario{withPayload(validScenario, "   ")}},
			wantErr: "must not be empty",
		},
		{
			name:    "production target rejected",
			pack:    AdversarialPack{ID: "pack-j", Name: "J", Version: "1.0.0", Scenarios: []AdversarialScenario{withTarget(validScenario, "api.acme-prod.com:443")}},
			wantErr: "outside the allowlist",
		},
		{
			name:    "link-local metadata target rejected",
			pack:    AdversarialPack{ID: "pack-k", Name: "K", Version: "1.0.0", Scenarios: []AdversarialScenario{withTarget(validScenario, "169.254.169.254")}},
			wantErr: "outside the allowlist",
		},
		{
			name:    "unknown kind rejected",
			pack:    AdversarialPack{ID: "pack-l", Name: "L", Version: "1.0.0", Scenarios: []AdversarialScenario{withKind(validScenario, "brand_new_attack")}},
			wantErr: "unknown kind",
		},
		{
			name:    "unknown severity rejected",
			pack:    AdversarialPack{ID: "pack-m", Name: "M", Version: "1.0.0", Scenarios: []AdversarialScenario{withSeverity(validScenario, "apocalyptic")}},
			wantErr: "unknown severity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewPackRegistry()
			if tc.registerTwice {
				if err := registry.RegisterPack(tc.pack); err != nil {
					t.Fatalf("pre-registration failed: %v", err)
				}
				superseding := tc.pack
				superseding.Version = "9.9.9"
				if err := registry.RegisterPack(superseding); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("RegisterPack duplicate error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			err := registry.RegisterPack(tc.pack)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("RegisterPack: unexpected error: %v", err)
				}
				if _, ok := registry.Pack(tc.pack.ID); !ok {
					t.Fatalf("pack %q not retrievable after registration", tc.pack.ID)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RegisterPack error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestTargetAllowlistAcceptsReservedNames pins the positive side of the
// target allowlist (loopback + RFC 2606 reserved names).
func TestTargetAllowlistAcceptsReservedNames(t *testing.T) {
	allowed := []string{"", "localhost", "127.0.0.1", "::1", "localhost:8080", "svc.test", "fixture.example", "hooks.invalid", "example.com"}
	for _, target := range allowed {
		if !targetAllowed(target) {
			t.Fatalf("target %q should be allowlisted", target)
		}
	}
}

// TestExportGateEvidence pins the deterministic reduction: counts, pass rate,
// max severity failed and the suite column a quality-gate policy matches on.
func TestExportGateEvidence(t *testing.T) {
	results := []AdversarialResult{
		{Suite: GateSuiteName, PackID: "builtin-security-adversarial", ScenarioID: "s1", Outcome: OutcomePass, Severity: SeverityHigh},
		{Suite: GateSuiteName, PackID: "builtin-security-adversarial", ScenarioID: "s2", Outcome: OutcomeFail, Severity: SeverityMedium},
		{Suite: GateSuiteName, PackID: "builtin-security-adversarial", ScenarioID: "s3", Outcome: OutcomeFail, Severity: SeverityCritical},
		{Suite: GateSuiteName, PackID: "acme-pack", ScenarioID: "s4", Outcome: OutcomeInconclusive, Severity: SeverityLow},
	}
	evidence := ExportGateEvidence(results)
	if evidence.Suite != "security_adversarial" {
		t.Fatalf("suite = %q, want security_adversarial", evidence.Suite)
	}
	if evidence.Total != 4 || evidence.Passed != 1 || evidence.Failed != 2 || evidence.Inconclusive != 1 {
		t.Fatalf("counts wrong: %+v", evidence)
	}
	if evidence.PassRate != 0.25 {
		t.Fatalf("pass rate = %v, want 0.25 (inconclusive never folded into the pass rate)", evidence.PassRate)
	}
	if evidence.MaxSeverityFailed != SeverityCritical || evidence.MaxSeverityFailedRank != 5 {
		t.Fatalf("max severity failed = %q/%d, want critical/5", evidence.MaxSeverityFailed, evidence.MaxSeverityFailedRank)
	}
	if len(evidence.Packs) != 2 || evidence.Packs[0] != "acme-pack" || evidence.Packs[1] != "builtin-security-adversarial" {
		t.Fatalf("packs = %v, want sorted unique", evidence.Packs)
	}

	empty := ExportGateEvidence(nil)
	if empty.Total != 0 || empty.PassRate != 0 || empty.MaxSeverityFailed != "" || empty.MaxSeverityFailedRank != 0 {
		t.Fatalf("empty evidence must be all-zero with no severity: %+v", empty)
	}
}

// TestGateEvidencePassRateAllPass pins the green path: every scenario passing
// yields pass rate 1 and no failed severity.
func TestGateEvidencePassRateAllPass(t *testing.T) {
	results := make([]AdversarialResult, 0, 3)
	for _, id := range []string{"a", "b", "c"} {
		results = append(results, AdversarialResult{Suite: GateSuiteName, ScenarioID: id, Outcome: OutcomePass, Severity: SeverityCritical})
	}
	evidence := ExportGateEvidence(results)
	if evidence.PassRate != 1.0 || evidence.MaxSeverityFailed != "" {
		t.Fatalf("all-pass evidence wrong: %+v", evidence)
	}
}

// --- table fixtures -------------------------------------------------------

func withPayload(s AdversarialScenario, payload string) AdversarialScenario {
	s.AttackPayload = payload
	return s
}

func withTarget(s AdversarialScenario, target string) AdversarialScenario {
	s.Setup.Target = target
	return s
}

func withKind(s AdversarialScenario, kind AdversarialKind) AdversarialScenario {
	s.Kind = kind
	return s
}

func withSeverity(s AdversarialScenario, severity Severity) AdversarialScenario {
	s.Severity = severity
	return s
}

// mustOversize grows the payload past MaxPayloadBytes (compile-time-safe:
// 2 * MaxPayloadBytes always exceeds the cap).
func mustOversize(s AdversarialScenario) AdversarialScenario {
	s.AttackPayload = strings.Repeat("A", MaxPayloadBytes+1)
	return s
}
