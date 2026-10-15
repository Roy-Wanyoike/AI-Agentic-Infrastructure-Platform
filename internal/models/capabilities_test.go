package models

import (
	"strings"
	"testing"
)

func TestBuiltinRegistryCoversCommonModels(t *testing.T) {
	r := NewCapabilityRegistry()
	want := []string{
		"gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-5", "gpt-5-mini",
		"o1", "o1-mini", "o3", "o3-mini", "o4-mini",
		"claude-3-5-sonnet-latest", "claude-3-5-haiku-latest", "claude-3-opus-latest", "claude-opus-4",
		"gemini-1.5-pro", "gemini-1.5-flash", "gemini-2.0-flash", "gemini-2.5-pro",
		"deepseek-chat", "deepseek-reasoner",
		"llama-3.1-8b-instruct", "llama-3.1-70b-instruct", "llama-3.1-405b-instruct",
		"mistral-small",
	}
	for _, id := range want {
		caps, ok := r.Lookup(id)
		if !ok {
			t.Fatalf("built-in registry must cover %q", id)
		}
		if caps.ContextTokens <= 0 {
			t.Fatalf("%s: ContextTokens = %d, want positive", id, caps.ContextTokens)
		}
		if caps.LatencyEstimateMS <= 0 {
			t.Fatalf("%s: LatencyEstimateMS = %d, want positive", id, caps.LatencyEstimateMS)
		}
		if _, known := tierRank[caps.QualityTier]; !known {
			t.Fatalf("%s: unknown quality tier %q", id, caps.QualityTier)
		}
		if caps.InputCostPer1K < 0 || caps.OutputCostPer1K < 0 {
			t.Fatalf("%s: negative pricing (%v/%v)", id, caps.InputCostPer1K, caps.OutputCostPer1K)
		}
	}
	if got := len(r.Models()); got != len(builtinCapabilities) {
		t.Fatalf("registry has %d models, want %d", got, len(builtinCapabilities))
	}
}

func TestRegistryLookupIsCaseAndVendorInsensitive(t *testing.T) {
	r := NewCapabilityRegistry()
	cases := []string{"GPT-4O", " gpt-4o ", "openai/gpt-4o", "together/llama-3.1-8b-instruct"}
	for _, id := range cases {
		if _, ok := r.Lookup(id); !ok {
			t.Fatalf("Lookup(%q) must resolve via case/vendor normalization", id)
		}
	}
	if _, ok := r.Lookup("gpt-999"); ok {
		t.Fatal("unknown model must not resolve")
	}
	if _, ok := r.Lookup(""); ok {
		t.Fatal("empty model must not resolve")
	}
}

func TestRegistryRegisterOverridesBuiltin(t *testing.T) {
	r := NewCapabilityRegistry()
	before, _ := r.Lookup("gpt-4o")
	r.Register(ModelCapabilities{Model: "GPT-4O", ContextTokens: 8, QualityTier: TierBasic})
	after, ok := r.Lookup("gpt-4o")
	if !ok {
		t.Fatal("override must replace the built-in entry")
	}
	if after.ContextTokens != 8 || after.QualityTier != TierBasic {
		t.Fatalf("override not applied: %+v", after)
	}
	if after.InputCostPer1K != before.InputCostPer1K {
		t.Fatal("partial override must keep untouched fields of the replaced entry")
	}
	// Blank ids are ignored.
	r.Register(ModelCapabilities{Model: "   "})
	if got := len(r.Models()); got != len(builtinCapabilities) {
		t.Fatalf("blank registration must be ignored (have %d models)", got)
	}
}

func TestSelectCandidatesFiltersCapabilities(t *testing.T) {
	r := newTestRegistry()

	// Order preserved; everything passes for a plain small request.
	got := r.SelectCandidates([]string{"test-small", "test-mid", "test-big"},
		CompletionRequest{Prompt: "hi"})
	if len(got) != 3 || got[0].Model != "test-small" || got[2].Model != "test-big" {
		t.Fatalf("plain request must keep all candidates in order, got %+v", modelIDs(got))
	}

	// Tools filter out models without tool support.
	got = r.SelectCandidates([]string{"test-small", "test-mid"},
		CompletionRequest{Prompt: "hi", Tools: []string{"calculator"}})
	if len(got) != 1 || got[0].Model != "test-mid" {
		t.Fatalf("tool request must drop non-tool models, got %+v", modelIDs(got))
	}

	// Vision filter.
	got = r.SelectCandidates([]string{"test-small", "test-big"},
		CompletionRequest{Prompt: "describe", NeedsVision: true})
	if len(got) != 1 || got[0].Model != "test-big" {
		t.Fatalf("vision request must keep only vision models, got %+v", modelIDs(got))
	}

	// Context fit: 5000 chars ≈ 1250 estimated tokens exceed test-small's
	// 1000-token window but fit test-big.
	huge := strings.Repeat("x", 5000) // ~1250 tokens
	got = r.SelectCandidates([]string{"test-small", "test-big"},
		CompletionRequest{Prompt: huge})
	if len(got) != 1 || got[0].Model != "test-big" {
		t.Fatalf("oversized prompt must drop the small-window model, got %+v", modelIDs(got))
	}

	// Unknown models are dropped (fail-closed capability verification).
	got = r.SelectCandidates([]string{"not-a-model", "test-mid"}, CompletionRequest{Prompt: "hi"})
	if len(got) != 1 || got[0].Model != "test-mid" {
		t.Fatalf("unknown models must be dropped, got %+v", modelIDs(got))
	}
}

func TestCapabilitiesFromJSON(t *testing.T) {
	valid := `[{"model":"custom-model","context_tokens":4096,"tool_support":true,"quality_tier":"standard","latency_estimate_ms":900,"input_cost_per_1k":0.001,"output_cost_per_1k":0.002}]`
	entries, err := CapabilitiesFromJSON(valid)
	if err != nil || len(entries) != 1 || entries[0].Model != "custom-model" {
		t.Fatalf("valid payload must parse, got entries=%v err=%v", entries, err)
	}
	if _, err := CapabilitiesFromJSON(""); err == nil {
		t.Fatal("empty payload must error")
	}
	if _, err := CapabilitiesFromJSON("not json"); err == nil {
		t.Fatal("invalid JSON must error")
	}
	if _, err := CapabilitiesFromJSON(`[{"context_tokens":10}]`); err == nil {
		t.Fatal("entry without a model id must error")
	}
}

func TestDefaultCapabilityRegistryAppliesEnvOverrides(t *testing.T) {
	t.Setenv(CapabilitiesEnvVar, `[{"model":"gpt-4o","context_tokens":777},{"model":"tenant-model","context_tokens":4096}]`)
	r := DefaultCapabilityRegistry()
	if caps, _ := r.Lookup("gpt-4o"); caps.ContextTokens != 777 {
		t.Fatalf("env override must replace the built-in entry, got %+v", caps)
	}
	if _, ok := r.Lookup("tenant-model"); !ok {
		t.Fatal("env override must add new models")
	}
	if caps, _ := r.Lookup("gpt-4o-mini"); caps.ContextTokens == 777 {
		t.Fatal("other built-ins must be untouched")
	}

	// Invalid JSON keeps the built-in table effective (documented contract).
	t.Setenv(CapabilitiesEnvVar, "{not json")
	r2 := DefaultCapabilityRegistry()
	if caps, ok := r2.Lookup("gpt-4o"); !ok || caps.ContextTokens != builtinCapabilities[0].ContextTokens {
		t.Fatalf("invalid override JSON must keep built-ins, got %+v ok=%v", caps, ok)
	}
}

// newTestRegistry returns a small deterministic registry used by the router
// tests: test-small (basic, no tools), test-mid (standard, tools),
// test-big (frontier, tools+vision, huge window).
func newTestRegistry() *CapabilityRegistry {
	r := NewCapabilityRegistry()
	r.Register(ModelCapabilities{Model: "test-small", ContextTokens: 1000, QualityTier: TierBasic, LatencyEstimateMS: 100, InputCostPer1K: 0.0001, OutputCostPer1K: 0.0001})
	r.Register(ModelCapabilities{Model: "test-mid", ContextTokens: 10000, ToolSupport: true, QualityTier: TierStandard, LatencyEstimateMS: 500, InputCostPer1K: 0.001, OutputCostPer1K: 0.002})
	r.Register(ModelCapabilities{Model: "test-big", ContextTokens: 100000, ToolSupport: true, VisionSupport: true, QualityTier: TierFrontier, LatencyEstimateMS: 2000, InputCostPer1K: 0.01, OutputCostPer1K: 0.03})
	return r
}
