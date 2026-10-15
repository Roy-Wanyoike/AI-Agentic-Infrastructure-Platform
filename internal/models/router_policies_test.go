package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// scriptedRouterProvider is a deterministic candidate provider: it records
// its call count and optionally fails its first failN calls with a transient
// error (or always, when failAll).
type scriptedRouterProvider struct {
	name  string
	calls atomic.Int32
	failN int32
	authF bool // fail with ErrProviderAuth instead of a transient error
}

func (p *scriptedRouterProvider) Name() string { return p.name }

func (p *scriptedRouterProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	n := p.calls.Add(1)
	if p.authF {
		return nil, fmt.Errorf("%w (http 401): bad key", ErrProviderAuth)
	}
	if n <= p.failN {
		return nil, fmt.Errorf("%w (http 503): %s down", ErrProviderUnavailable, p.name)
	}
	return &CompletionResponse{Text: "from " + p.name, Model: req.Model}, nil
}

// newScriptedCandidates builds Candidates for the given test-registry models,
// each backed by its own scripted provider named after the model.
func newScriptedCandidates(r *CapabilityRegistry, models ...string) []Candidate {
	out := make([]Candidate, 0, len(models))
	for _, m := range models {
		caps, ok := r.Lookup(m)
		if !ok {
			panic("test registry missing " + m)
		}
		out = append(out, Candidate{Model: m, Provider: &scriptedRouterProvider{name: "prov:" + m}, Caps: caps})
	}
	return out
}

func TestParseRoutingPolicy(t *testing.T) {
	valid := map[string]RoutingPolicy{
		"cost_optimize":      PolicyCostOptimize,
		" LATENCY_OPTIMIZE ": PolicyLatencyOptimize,
		"Task_Complexity":    PolicyTaskComplexity,
		"round_robin":        PolicyRoundRobin,
		"weighted":           PolicyWeighted,
	}
	for in, want := range valid {
		got, err := ParseRoutingPolicy(in)
		if err != nil || got != want {
			t.Fatalf("ParseRoutingPolicy(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "cheapest", "speed"} {
		if _, err := ParseRoutingPolicy(bad); err == nil {
			t.Fatalf("ParseRoutingPolicy(%q) must reject unknown policies", bad)
		}
	}
}

func TestRoutingConfigParsers(t *testing.T) {
	cfg, err := RoutingConfigFromJSON(`{"policy":"weighted","candidates":["test-small","test-big@100"]}`)
	if err != nil {
		t.Fatalf("valid routing config must parse: %v", err)
	}
	if cfg.Policy != PolicyWeighted {
		t.Fatalf("policy = %q", cfg.Policy)
	}
	if len(cfg.Candidates) != 2 || cfg.Candidates[0] != "test-small" || cfg.Candidates[1] != "test-big" {
		t.Fatalf("candidates = %v (weight suffix must be stripped)", cfg.Candidates)
	}
	if cfg.Weight("test-big") != 100 || cfg.Weight("test-small") != 1 || cfg.Weight("unknown") != 1 {
		t.Fatalf("weights parsed incorrectly: big=%d small=%d unknown=%d",
			cfg.Weight("test-big"), cfg.Weight("test-small"), cfg.Weight("unknown"))
	}

	// Map form (the runtime_config runtime path).
	m, err := RoutingConfigFromMap(map[string]any{"policy": "cost_optimize", "candidates": []any{"a@2"}})
	if err != nil || m.Policy != PolicyCostOptimize || m.Weight("a") != 2 {
		t.Fatalf("map form must parse: %v %v", m, err)
	}

	for name, raw := range map[string]map[string]any{
		"missing policy": {"candidates": []any{"a"}},
		"unknown policy": {"policy": "fastest"},
		"bad weight":     {"policy": "weighted", "candidates": []any{"a@-1"}},
		"bad weight fmt": {"policy": "weighted", "candidates": []any{"a@many"}},
		"non-string":     {"policy": "weighted", "candidates": []any{42}},
		"non-array":      {"policy": "weighted", "candidates": "a"},
		"blank entry":    {"policy": "weighted", "candidates": []any{"  "}},
	} {
		if _, err := RoutingConfigFromMap(raw); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	if _, err := RoutingConfigFromMap(nil); err == nil {
		t.Fatal("nil routing config must be rejected")
	}
}

func TestClassifyComplexityDeterministic(t *testing.T) {
	long := strings.Repeat("word ", 500)  // 2500 chars
	huge := strings.Repeat("word ", 1800) // 9000 chars
	cases := []struct {
		name string
		req  CompletionRequest
		want Complexity
	}{
		{"empty", CompletionRequest{}, ComplexitySimple},
		{"short", CompletionRequest{Prompt: "hi"}, ComplexitySimple},
		{"short with tools", CompletionRequest{Prompt: "hi", Tools: []string{"calc"}}, ComplexityStandard},                                    // 0 + tools 1
		{"medium plain", CompletionRequest{Prompt: strings.Repeat("a", 300)}, ComplexityStandard},                                             // length 1
		{"long plain", CompletionRequest{Prompt: long}, ComplexityStandard},                                                                   // length 2 -> still Standard
		{"huge plain", CompletionRequest{Prompt: huge}, ComplexityComplex},                                                                    // length 3 -> Complex
		{"reasoning indicators capped", CompletionRequest{Prompt: "analyze why, compare and derive and prove and debug"}, ComplexityStandard}, // indicators capped at +2
		{"long + indicators", CompletionRequest{Prompt: long + " analyze step by step"}, ComplexityComplex},                                   // 2 + capped 2 = 4
		{"expert combo", CompletionRequest{Prompt: huge, Tools: []string{"t"}, MaxTokens: 4096}, ComplexityExpert},                            // 3 + 1 + 1 = 5
	}

	for _, tc := range cases {
		got := ClassifyComplexity(tc.req)
		if got != tc.want {
			t.Fatalf("%s: ClassifyComplexity = %s, want %s", tc.name, got, tc.want)
		}
		if again := ClassifyComplexity(tc.req); again != got {
			t.Fatalf("%s: classification must be deterministic (%s vs %s)", tc.name, got, again)
		}
	}
}

func TestComplexityTierMapping(t *testing.T) {
	cases := map[Complexity]QualityTier{
		ComplexitySimple:   TierBasic,
		ComplexityStandard: TierStandard,
		ComplexityComplex:  TierPremium,
		ComplexityExpert:   TierFrontier,
		Complexity("???"):  TierStandard, // unknown -> conservative standard
	}
	for c, want := range cases {
		if got := c.Tier(); got != want {
			t.Fatalf("Tier(%s) = %s, want %s", c, got, want)
		}
	}
}

func TestNewPolicyProviderValidation(t *testing.T) {
	r := newTestRegistry()
	if _, err := NewPolicyProvider(nil, PolicyCostOptimize, newScriptedCandidates(r, "test-mid")); err == nil {
		t.Fatal("nil registry must be rejected")
	}
	if _, err := NewPolicyProvider(r, "fastest", newScriptedCandidates(r, "test-mid")); err == nil {
		t.Fatal("unknown policy must be rejected")
	}
	if _, err := NewPolicyProvider(r, PolicyCostOptimize, nil); !errors.Is(err, ErrNoProvider) {
		t.Fatal("empty candidates must yield ErrNoProvider")
	}
	bad := []Candidate{{Model: "test-mid"}}
	if _, err := NewPolicyProvider(r, PolicyCostOptimize, bad); err == nil {
		t.Fatal("candidate without a provider must be rejected")
	}
	unknown := []Candidate{{Model: "mystery", Provider: &scriptedRouterProvider{name: "mystery"}}}
	if _, err := NewPolicyProvider(r, PolicyCostOptimize, unknown); err == nil {
		t.Fatal("unregistered model without explicit caps must be rejected (fail-closed)")
	}
	// An explicit Caps entry bypasses the registry check (tenant override).
	over := []Candidate{{Model: "mystery", Provider: &scriptedRouterProvider{name: "mystery"},
		Caps: ModelCapabilities{Model: "mystery", ContextTokens: 100, QualityTier: TierBasic, LatencyEstimateMS: 10}}}
	if _, err := NewPolicyProvider(r, PolicyCostOptimize, over); err != nil {
		t.Fatalf("explicit caps must be honored: %v", err)
	}
}

func TestCostOptimizeSelectsCheapestCapable(t *testing.T) {
	r := newTestRegistry()
	cands := newScriptedCandidates(r, "test-small", "test-mid", "test-big")
	router, err := NewPolicyProvider(r, PolicyCostOptimize, cands)
	if err != nil {
		t.Fatalf("router construction: %v", err)
	}

	// Plain request: the small (cheapest) model is capable -> it serves.
	resp, err := router.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "from prov:test-small" {
		t.Fatalf("cost_optimize must pick the cheapest capable model, got %q", resp.Text)
	}
	if resp.Metadata[MetaRoutingPolicy] != string(PolicyCostOptimize) ||
		resp.Metadata[MetaRoutingModel] != "test-small" ||
		resp.Metadata[MetaRoutingProvider] != "prov:test-small" {
		t.Fatalf("routing metadata missing/incorrect: %+v", resp.Metadata)
	}

	// Tool request: test-small lacks tool support -> cheapest CAPABLE is test-mid.
	resp, err = router.Complete(context.Background(), CompletionRequest{Prompt: "hi", Tools: []string{"calc"}})
	if err != nil {
		t.Fatalf("Complete with tools: %v", err)
	}
	if resp.Metadata[MetaRoutingModel] != "test-mid" {
		t.Fatalf("cost_optimize must pick the cheapest TOOL-CAPABLE model, got %s", resp.Metadata[MetaRoutingModel])
	}
	if resp.Metadata[MetaRoutingComplexity] == "" {
		t.Fatal("complexity must be reported in metadata")
	}
}

func TestLatencyOptimizeSelectsLowestLatency(t *testing.T) {
	r := newTestRegistry()
	router, err := NewPolicyProvider(r, PolicyLatencyOptimize, newScriptedCandidates(r, "test-mid", "test-small", "test-big"))
	if err != nil {
		t.Fatalf("router construction: %v", err)
	}
	resp, err := router.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Metadata[MetaRoutingModel] != "test-small" { // 100ms < 500ms < 2000ms
		t.Fatalf("latency_optimize must pick the lowest-latency candidate, got %s", resp.Metadata[MetaRoutingModel])
	}
	dec, err := router.Decide(CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !strings.Contains(dec.Reason, "latency") {
		t.Fatalf("decision reason should mention latency, got %q", dec.Reason)
	}
}

func TestTaskComplexityRoutesToTier(t *testing.T) {
	r := newTestRegistry()
	router, err := NewPolicyProvider(r, PolicyTaskComplexity, newScriptedCandidates(r, "test-small", "test-mid", "test-big"))
	if err != nil {
		t.Fatalf("router construction: %v", err)
	}

	// Simple request -> basic tier (test-small).
	resp, err := router.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Metadata[MetaRoutingModel] != "test-small" || resp.Metadata[MetaRoutingComplexity] != string(ComplexitySimple) {
		t.Fatalf("simple request must route to the basic tier, got %+v", resp.Metadata)
	}

	// Expert request (huge + tools + big budget) -> frontier tier (test-big).
	expert := CompletionRequest{Prompt: strings.Repeat("analyze ", 1200), Tools: []string{"t"}, MaxTokens: 4096}
	resp, err = router.Complete(context.Background(), expert)
	if err != nil {
		t.Fatalf("Complete expert: %v", err)
	}
	if resp.Metadata[MetaRoutingComplexity] != string(ComplexityExpert) {
		t.Fatalf("expected expert classification, got %s", resp.Metadata[MetaRoutingComplexity])
	}
	if resp.Metadata[MetaRoutingModel] != "test-big" {
		t.Fatalf("expert request must route to the frontier tier, got %s", resp.Metadata[MetaRoutingModel])
	}

	// Complex request (long, no tools) -> closest tier is standard (test-mid;
	// no premium candidate exists, the tier distance tie is broken by latency).
	complexReq := CompletionRequest{Prompt: strings.Repeat("a", 2500)}
	resp, err = router.Complete(context.Background(), complexReq)
	if err != nil {
		t.Fatalf("Complete complex: %v", err)
	}
	if resp.Metadata[MetaRoutingModel] != "test-mid" {
		t.Fatalf("complex request must route to the closest available tier, got %s", resp.Metadata[MetaRoutingModel])
	}
}

func TestRoundRobinDeterministic(t *testing.T) {
	r := newTestRegistry()
	router, err := NewPolicyProvider(r, PolicyRoundRobin, newScriptedCandidates(r, "test-small", "test-mid", "test-big"))
	if err != nil {
		t.Fatalf("router construction: %v", err)
	}
	req := CompletionRequest{Prompt: "hi"}

	var firstRun []string
	for i := 0; i < 4; i++ {
		resp, err := router.Complete(context.Background(), req)
		if err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
		firstRun = append(firstRun, resp.Metadata[MetaRoutingModel])
	}
	want := []string{"test-small", "test-mid", "test-big", "test-small"} // input order rotation
	for i := range want {
		if firstRun[i] != want[i] {
			t.Fatalf("round_robin rotation broken: got %v, want %v", firstRun, want)
		}
	}

	// A fresh router repeats the identical sequence (deterministic under
	// fixed inputs).
	router2, _ := NewPolicyProvider(r, PolicyRoundRobin, newScriptedCandidates(r, "test-small", "test-mid", "test-big"))
	for i := 0; i < 4; i++ {
		resp, _ := router2.Complete(context.Background(), req)
		if resp.Metadata[MetaRoutingModel] != want[i] {
			t.Fatalf("fresh router must repeat the sequence: got %s, want %s", resp.Metadata[MetaRoutingModel], want[i])
		}
	}

	// Decide never advances the rotation.
	d1, _ := router.Decide(req)
	d2, _ := router.Decide(req)
	if d1.SelectedModel != d2.SelectedModel {
		t.Fatalf("Decide must not advance the rotation: %s vs %s", d1.SelectedModel, d2.SelectedModel)
	}
}

func TestWeightedDeterministic(t *testing.T) {
	r := newTestRegistry()
	req := CompletionRequest{Prompt: "deterministic weighted seed"}

	// Same request -> same selection, on the same router and across routers.
	router, err := NewPolicyProvider(r, PolicyWeighted, newScriptedCandidates(r, "test-small", "test-mid", "test-big"))
	if err != nil {
		t.Fatalf("router construction: %v", err)
	}
	r1, _ := router.Complete(context.Background(), req)
	r2, _ := router.Complete(context.Background(), req)
	if r1.Metadata[MetaRoutingModel] != r2.Metadata[MetaRoutingModel] {
		t.Fatalf("weighted must be deterministic per request: %s vs %s",
			r1.Metadata[MetaRoutingModel], r2.Metadata[MetaRoutingModel])
	}
	router2, _ := NewPolicyProvider(r, PolicyWeighted, newScriptedCandidates(r, "test-small", "test-mid", "test-big"))
	r3, _ := router2.Complete(context.Background(), req)
	if r3.Metadata[MetaRoutingModel] != r1.Metadata[MetaRoutingModel] {
		t.Fatalf("weighted must be stable across router instances: %s vs %s",
			r3.Metadata[MetaRoutingModel], r1.Metadata[MetaRoutingModel])
	}

	// A hugely dominant weight always wins regardless of the hash draw.
	cfg, err := RoutingConfigFromJSON(`{"policy":"weighted","candidates":["test-small@1","test-big@1000000000"]}`)
	if err != nil {
		t.Fatalf("routing config: %v", err)
	}
	dominant := req
	dominant.Routing = &cfg
	for i := 0; i < 5; i++ {
		resp, err := router.Complete(context.Background(), dominant)
		if err != nil {
			t.Fatalf("weighted dominant: %v", err)
		}
		if resp.Metadata[MetaRoutingModel] != "test-big" {
			t.Fatalf("weight 1e9 must always dominate, got %s (iter %d)", resp.Metadata[MetaRoutingModel], i)
		}
	}
}

func TestRouterHonorsPerRequestRoutingOverride(t *testing.T) {
	r := newTestRegistry()
	router, err := NewPolicyProvider(r, PolicyCostOptimize, newScriptedCandidates(r, "test-small", "test-mid", "test-big"))
	if err != nil {
		t.Fatalf("router construction: %v", err)
	}

	// Default policy is cost; the per-request override switches to latency.
	req := CompletionRequest{Prompt: "hi", Routing: &RoutingConfig{Policy: PolicyLatencyOptimize}}
	resp, err := router.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Metadata[MetaRoutingPolicy] != string(PolicyLatencyOptimize) ||
		resp.Metadata[MetaRoutingModel] != "test-small" {
		t.Fatalf("per-request policy override not honored: %+v", resp.Metadata)
	}

	// Candidate restriction: only test-big is allowed despite the cost policy.
	restricted := CompletionRequest{Prompt: "hi", Routing: &RoutingConfig{
		Policy:     PolicyCostOptimize,
		Candidates: []string{"test-big"},
	}}
	resp, err = router.Complete(context.Background(), restricted)
	if err != nil {
		t.Fatalf("Complete restricted: %v", err)
	}
	if resp.Metadata[MetaRoutingModel] != "test-big" || resp.Metadata[MetaRoutingFallback] == "true" {
		t.Fatalf("candidate restriction must force test-big: %+v", resp.Metadata)
	}

	// An invalid per-request policy errors (deterministic, surfaced).
	bad := CompletionRequest{Prompt: "hi", Routing: &RoutingConfig{Policy: "turbo"}}
	if _, err := router.Complete(context.Background(), bad); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid per-request policy must fail with ErrInvalidRequest, got %v", err)
	}
}

func TestRouterDecisionProjection(t *testing.T) {
	r := newTestRegistry()
	cands := newScriptedCandidates(r, "test-small", "test-mid", "test-big")
	router, _ := NewPolicyProvider(r, PolicyCostOptimize, cands)

	dec, err := router.Decide(CompletionRequest{Prompt: "hi", Tools: []string{"calc"}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.SelectedModel != "test-mid" {
		t.Fatalf("decision must project the cost-capable pick, got %s", dec.SelectedModel)
	}
	if len(dec.CandidateOrder) != 2 || dec.CandidateOrder[0] != "test-mid" || dec.CandidateOrder[1] != "test-big" {
		t.Fatalf("candidate order must list the capable candidates in attempt order: %v", dec.CandidateOrder)
	}
	if len(dec.Skipped) != 1 || dec.Skipped[0].Reason != "tools_unsupported" {
		t.Fatalf("capability skip must be reported: %+v", dec.Skipped)
	}
	// Decide is read-only: no provider was called, and a repeated call
	// returns the identical projection.
	for _, c := range cands {
		if got := c.Provider.(*scriptedRouterProvider).calls.Load(); got != 0 {
			t.Fatalf("Decide must not call providers, %s saw %d calls", c.Model, got)
		}
	}
	dec2, _ := router.Decide(CompletionRequest{Prompt: "hi", Tools: []string{"calc"}})
	if dec2.SelectedModel != dec.SelectedModel || len(dec2.CandidateOrder) != len(dec.CandidateOrder) {
		t.Fatal("Decide must be stable for identical input")
	}
}

func TestRouterNoCapableCandidate(t *testing.T) {
	r := newTestRegistry()
	router, _ := NewPolicyProvider(r, PolicyCostOptimize, newScriptedCandidates(r, "test-small", "test-mid"))
	_, err := router.Complete(context.Background(), CompletionRequest{Prompt: "hi", NeedsVision: true})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("no vision-capable candidate must yield ErrNoProvider, got %v", err)
	}
}

func TestRouterFailsOverOnTransientOnly(t *testing.T) {
	r := newTestRegistry()
	cands := newScriptedCandidates(r, "test-small", "test-mid")
	small := cands[0].Provider.(*scriptedRouterProvider)
	small.failN = 1 // first call fails transiently, then succeeds
	router, _ := NewPolicyProvider(r, PolicyCostOptimize, cands)

	resp, err := router.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Metadata[MetaRoutingModel] != "test-mid" {
		t.Fatalf("the healthy fallback must serve, got %s", resp.Metadata[MetaRoutingModel])
	}
	if resp.Metadata[MetaRoutingFallback] != "true" {
		t.Fatalf("fallback serving must be flagged in metadata: %+v", resp.Metadata)
	}
	if small.calls.Load() != 1 {
		t.Fatalf("primary must be attempted exactly once, saw %d", small.calls.Load())
	}

	// Auth failures never fail over: the error surfaces immediately.
	auth := newScriptedCandidates(r, "test-small", "test-mid")
	auth[0].Provider.(*scriptedRouterProvider).authF = true
	router2, _ := NewPolicyProvider(r, PolicyCostOptimize, auth)
	_, err = router2.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if !errors.Is(err, ErrProviderAuth) {
		t.Fatalf("auth failure must surface immediately, got %v", err)
	}
	if got := auth[1].Provider.(*scriptedRouterProvider).calls.Load(); got != 0 {
		t.Fatalf("auth failure must not fail over, fallback saw %d calls", got)
	}
}

func TestRouterSkipsCircuitOpenProvider(t *testing.T) {
	r := newTestRegistry()
	cands := newScriptedCandidates(r, "test-small", "test-mid")
	small := cands[0].Provider.(*scriptedRouterProvider)
	small.failN = 100 // effectively always failing (transient)
	router, _ := NewPolicyProvider(r, PolicyCostOptimize, cands)

	// Default threshold is 5: the first five requests each attempt (and
	// fail on) test-small before test-mid serves.
	for i := 0; i < 5; i++ {
		resp, err := router.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if resp.Metadata[MetaRoutingFallback] != "true" {
			t.Fatalf("request %d must be served by the fallback", i)
		}
	}
	br, ok := router.Breaker("prov:test-small")
	if !ok {
		t.Fatal("the router must expose a breaker per provider")
	}
	if br.State() != BreakerOpen {
		t.Fatalf("5 consecutive failures must open the breaker, got %s", br.State())
	}
	if br.Snapshot().Failures != 5 {
		t.Fatalf("breaker failures = %d, want 5", br.Snapshot().Failures)
	}

	// The sixth request SKIPS the circuit-open provider entirely.
	resp, err := router.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("request 6: %v", err)
	}
	if resp.Metadata[MetaRoutingModel] != "test-mid" {
		t.Fatalf("fallback must still serve, got %s", resp.Metadata[MetaRoutingModel])
	}
	if got := small.calls.Load(); got != 5 {
		t.Fatalf("circuit-open provider must be skipped: saw %d calls, want 5", got)
	}

	// Decide lists the circuit-open candidate.
	dec, _ := router.Decide(CompletionRequest{Prompt: "hi"})
	var openSkip bool
	for _, s := range dec.Skipped {
		if s.Reason == "circuit_open" && s.Model == "test-small" {
			openSkip = true
		}
	}
	if !openSkip {
		t.Fatalf("circuit_open skip must be reported on the decision: %+v", dec.Skipped)
	}
}

func TestRouterAllCandidatesCircuitOpen(t *testing.T) {
	r := newTestRegistry()
	cands := newScriptedCandidates(r, "test-small", "test-mid")
	cands[0].Provider.(*scriptedRouterProvider).failN = 100
	cands[1].Provider.(*scriptedRouterProvider).failN = 100
	router, _ := NewPolicyProvider(r, PolicyCostOptimize, cands)

	// Drive both breakers open: request 1 fails over small->mid (1 failure
	// each); requests 2-6 keep failing. After 5 consecutive failures each
	// breaker is open and the router reports ErrNoProvider.
	var lastErr error
	for i := 0; i < 6; i++ {
		_, lastErr = router.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	}
	if !errors.Is(lastErr, ErrNoProvider) {
		t.Fatalf("all circuit-open candidates must yield ErrNoProvider, got %v", lastErr)
	}
}

// ---- env wiring tests (AGENTOS_ROUTING_POLICY at the ProviderFromEnv seam) ----

func TestProviderFromEnvRoutingEnabled(t *testing.T) {
	stub, _ := newUsageStub(t, 200, stubCompletion)
	t.Setenv(ProviderAPIKeyEnvVar, "k")
	t.Setenv(ProviderBaseURLEnvVar, stub.URL)
	t.Setenv(ProviderModelEnvVar, "gpt-4o-mini")
	t.Setenv(ProviderFallbackAPIKeyEnvVar, "")
	t.Setenv(RoutingPolicyEnvVar, "cost_optimize")
	t.Setenv(RoutingCandidatesEnvVar, "gpt-4o-mini, gpt-4o")

	logr, logs := captureLogger()
	provider, ok := ProviderFromEnv(logr)
	if !ok {
		t.Fatal("provider mode expected")
	}
	router, isRouter := provider.(*PolicyProvider)
	if !isRouter {
		t.Fatalf("AGENTOS_ROUTING_POLICY must wrap the provider in a PolicyProvider, got %T", provider)
	}
	if !strings.Contains(router.Name(), "cost_optimize") || !strings.Contains(router.Name(), "gpt-4o-mini") {
		t.Fatalf("router name should carry policy + candidates, got %q", router.Name())
	}
	resp, err := provider.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete through the routed provider: %v", err)
	}
	if resp.Metadata[MetaRoutingModel] != "gpt-4o-mini" {
		t.Fatalf("cost_optimize over {gpt-4o-mini,gpt-4o} must pick gpt-4o-mini, got %s", resp.Metadata[MetaRoutingModel])
	}
	if out := logs.String(); !strings.Contains(out, "model routing enabled") {
		t.Fatalf("routing enablement should be logged, got %q", out)
	}
}

func TestProviderFromEnvRoutingCandidatesDefaultToModel(t *testing.T) {
	stub, _ := newUsageStub(t, 200, stubCompletion)
	t.Setenv(ProviderAPIKeyEnvVar, "k")
	t.Setenv(ProviderBaseURLEnvVar, stub.URL)
	t.Setenv(ProviderModelEnvVar, "gpt-4o")
	t.Setenv(RoutingPolicyEnvVar, "latency_optimize")
	t.Setenv(RoutingCandidatesEnvVar, "") // unset -> the configured model

	provider, ok := ProviderFromEnv(discardLogger())
	if !ok {
		t.Fatal("provider mode expected")
	}
	router, isRouter := provider.(*PolicyProvider)
	if !isRouter {
		t.Fatalf("expected a PolicyProvider, got %T", provider)
	}
	if !strings.Contains(router.Name(), "gpt-4o)") || strings.Contains(router.Name(), "gpt-4o|") {
		t.Fatalf("candidates must default to the configured model only, got %q", router.Name())
	}
}

func TestProviderFromEnvInvalidRoutingPolicyKeepsChain(t *testing.T) {
	stub, _ := newUsageStub(t, 200, stubCompletion)
	t.Setenv(ProviderAPIKeyEnvVar, "k")
	t.Setenv(ProviderBaseURLEnvVar, stub.URL)
	t.Setenv(ProviderModelEnvVar, "gpt-4o-mini")
	t.Setenv(ProviderFallbackAPIKeyEnvVar, "")
	t.Setenv(RoutingPolicyEnvVar, "fastest-but-wrong")

	logr, logs := captureLogger()
	provider, ok := ProviderFromEnv(logr)
	if !ok {
		t.Fatal("provider mode expected")
	}
	if _, isRouter := provider.(*PolicyProvider); isRouter {
		t.Fatal("an invalid policy must keep the unwrapped provider")
	}
	if _, isOpenAI := provider.(*OpenAIProvider); !isOpenAI {
		t.Fatalf("expected the plain OpenAI provider after the warning, got %T", provider)
	}
	if out := logs.String(); !strings.Contains(out, "model routing disabled") {
		t.Fatalf("routing misconfiguration must warn, got %q", out)
	}
}

func TestProviderFromEnvUnknownRoutingCandidateSkipped(t *testing.T) {
	stub, _ := newUsageStub(t, 200, stubCompletion)
	t.Setenv(ProviderAPIKeyEnvVar, "k")
	t.Setenv(ProviderBaseURLEnvVar, stub.URL)
	t.Setenv(ProviderModelEnvVar, "")
	t.Setenv(RoutingPolicyEnvVar, "round_robin")
	t.Setenv(RoutingCandidatesEnvVar, "not-a-model,gpt-4o-mini")

	logr, logs := captureLogger()
	provider, ok := ProviderFromEnv(logr)
	if !ok {
		t.Fatal("provider mode expected")
	}
	router, isRouter := provider.(*PolicyProvider)
	if !isRouter {
		t.Fatalf("one valid candidate must still enable routing, got %T", provider)
	}
	if strings.Contains(router.Name(), "not-a-model") {
		t.Fatalf("unknown candidate must be skipped, got %q", router.Name())
	}
	resp, err := provider.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Metadata[MetaRoutingModel] != "gpt-4o-mini" {
		t.Fatalf("the registered candidate must serve: %+v", resp.Metadata)
	}
	if out := logs.String(); !strings.Contains(out, "not-a-model") {
		t.Fatalf("the skipped candidate should be warned about, got %q", out)
	}
}
