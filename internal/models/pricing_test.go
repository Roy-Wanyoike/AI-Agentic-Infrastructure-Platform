package models

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestComputeCostCentsBuiltinTable(t *testing.T) {
	t.Setenv(PricingEnvVar, "")
	SetPricing(nil)
	defer SetPricing(nil)

	cases := []struct {
		name             string
		model            string
		promptTokens     int
		completionTokens int
		want             float64
	}{
		{
			name:             "gpt-4o 1k in / 500 out",
			model:            "gpt-4o",
			promptTokens:     1000,
			completionTokens: 500,
			// (1000*2.50 + 500*10.00)/1e6*100 = 0.75 cents
			want: 0.75,
		},
		{
			name:             "gpt-4o-mini 1M in / 1M out",
			model:            "gpt-4o-mini",
			promptTokens:     1_000_000,
			completionTokens: 1_000_000,
			// (1M*0.15 + 1M*0.60)/1e6*100 = 75 cents
			want: 75.0,
		},
		{
			name:             "gpt-4.1 2k in only",
			model:            "gpt-4.1",
			promptTokens:     2000,
			completionTokens: 0,
			want:             0.4,
		},
		{
			name:             "o3-mini 10k out only",
			model:            "o3-mini",
			promptTokens:     0,
			completionTokens: 10_000,
			want:             4.4,
		},
		{
			name:             "llama-3.1-8b-instruct",
			model:            "llama-3.1-8b-instruct",
			promptTokens:     500_000,
			completionTokens: 500_000,
			want:             18.0,
		},
		{
			name:             "mistral-small",
			model:            "mistral-small",
			promptTokens:     1_000_000,
			completionTokens: 0,
			want:             20.0,
		},
		{
			name:             "case-insensitive match",
			model:            "GPT-4O-MINI",
			promptTokens:     1_000_000,
			completionTokens: 0,
			want:             15.0,
		},
		{
			name:             "vendor-prefixed id matches bare entry",
			model:            "openai/gpt-4o",
			promptTokens:     1_000_000,
			completionTokens: 0,
			want:             250.0,
		},
		{
			name:             "unknown model is 0 cents (documented contract)",
			model:            "totally-unknown-model",
			promptTokens:     1_000_000,
			completionTokens: 1_000_000,
			want:             0,
		},
		{
			name:             "empty model is 0 cents",
			model:            "",
			promptTokens:     1_000_000,
			completionTokens: 1_000_000,
			want:             0,
		},
		{
			name:             "zero tokens is 0 cents",
			model:            "gpt-4o",
			promptTokens:     0,
			completionTokens: 0,
			want:             0,
		},
		{
			name:             "negative tokens clamp to 0",
			model:            "gpt-4o",
			promptTokens:     -100,
			completionTokens: 500,
			// (500*10.00)/1e6*100 = 0.5 cents
			want: 0.5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeCostCents(tc.model, tc.promptTokens, tc.completionTokens)
			if !almostEqual(got, tc.want) {
				t.Fatalf("ComputeCostCents(%q, %d, %d) = %v, want %v", tc.model, tc.promptTokens, tc.completionTokens, got, tc.want)
			}
		})
	}
}

func TestLookupPricing(t *testing.T) {
	t.Setenv(PricingEnvVar, "")
	SetPricing(nil)
	defer SetPricing(nil)

	if entry, ok := LookupPricing("gpt-4o"); !ok || entry.InputPerMTokens != 2.50 || entry.OutputPerMTokens != 10.00 {
		t.Fatalf("LookupPricing(gpt-4o) = %+v, %v", entry, ok)
	}
	if _, ok := LookupPricing("gpt-4o-2024-08-06-dated-snapshot"); ok {
		t.Fatal("exact-match only: unknown snapshots must not resolve")
	}
}

func TestPricingFromJSON(t *testing.T) {
	valid := `[{"model":"custom-model","input_per_m_tokens":1.5,"output_per_m_tokens":3},{"model":"GPT-4O","input_per_m_tokens":9,"output_per_m_tokens":9}]`
	entries, err := PricingFromJSON(valid)
	if err != nil {
		t.Fatalf("PricingFromJSON(valid) returned error: %v", err)
	}
	if len(entries) != 2 || entries[0].Model != "custom-model" || entries[0].InputPerMTokens != 1.5 {
		t.Fatalf("unexpected entries: %+v", entries)
	}

	if _, err := PricingFromJSON("not json"); err == nil {
		t.Fatal("malformed JSON should return an error")
	}
	if _, err := PricingFromJSON(`[{"input_per_m_tokens":1}]`); err == nil {
		t.Fatal("entry without model id should return an error")
	}
	if _, err := PricingFromJSON(""); err == nil {
		t.Fatal("empty payload should return an error")
	}
}

func TestPricingEnvOverride(t *testing.T) {
	t.Cleanup(func() { SetPricing(nil) })

	// Override replaces a built-in entry and adds a new one.
	t.Setenv(PricingEnvVar, `[{"model":"gpt-4o","input_per_m_tokens":5,"output_per_m_tokens":20},{"model":"custom-model","input_per_m_tokens":1,"output_per_m_tokens":2}]`)
	if got := ComputeCostCents("gpt-4o", 1_000_000, 0); !almostEqual(got, 500) {
		t.Fatalf("override should replace the built-in gpt-4o price, got %v want 500", got)
	}
	if got := ComputeCostCents("custom-model", 1_000_000, 1_000_000); !almostEqual(got, 300) {
		t.Fatalf("override should add custom-model, got %v want 300", got)
	}

	// Invalid JSON silently keeps the built-in table (documented behavior).
	t.Setenv(PricingEnvVar, `{not an array`)
	if got := ComputeCostCents("gpt-4o", 1_000_000, 0); !almostEqual(got, 250) {
		t.Fatalf("invalid override JSON should fall back to built-ins, got %v want 250", got)
	}

	// Changing the env re-parses (cache keyed by the raw value).
	// 1M input tokens at 0.01 USD/1M = 0.01 USD = 1 cent (a re-parse: the
	// previous cache would have priced 250 cents).
	t.Setenv(PricingEnvVar, `[{"model":"gpt-4o","input_per_m_tokens":0.01,"output_per_m_tokens":0.02}]`)
	if got := ComputeCostCents("gpt-4o", 1_000_000, 0); !almostEqual(got, 1) {
		t.Fatalf("changed override should re-parse, got %v want 1", got)
	}
}

func TestSetPricingOverridesEnvAndResets(t *testing.T) {
	t.Cleanup(func() { SetPricing(nil) })

	SetPricing([]ModelPricing{{Model: "gpt-4o", InputPerMTokens: 7, OutputPerMTokens: 7}})
	if got := ComputeCostCents("gpt-4o", 1_000_000, 0); !almostEqual(got, 700) {
		t.Fatalf("SetPricing should win over built-ins/env, got %v want 700", got)
	}
	// Unknown-model rule still holds with an override table.
	if got := ComputeCostCents("gpt-4o-mini", 1_000, 0); got != 0 {
		t.Fatalf("models absent from the override table should price 0, got %v", got)
	}

	SetPricing(nil)
	if got := ComputeCostCents("gpt-4o", 1_000_000, 0); !almostEqual(got, 250) {
		t.Fatalf("SetPricing(nil) should restore the built-in table, got %v want 250", got)
	}
}

func TestComputeCostCentsNeverErrors(t *testing.T) {
	t.Setenv(PricingEnvVar, "")
	SetPricing(nil)
	defer SetPricing(nil)

	// The documented "never fail a run" rule: every input combination returns
	// a plain float64 (0 for unknown/empty models), never an error or panic.
	for _, model := range []string{"", " ", "unknown", "gpt-4o", "openai/gpt-4o-mini"} {
		for _, pt := range []int{-1000, 0, 1, 1_000_000} {
			for _, ct := range []int{-1000, 0, 1, 1_000_000} {
				cost := ComputeCostCents(model, pt, ct)
				if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
					t.Fatalf("ComputeCostCents(%q,%d,%d) = %v, want a finite non-negative value", model, pt, ct, cost)
				}
			}
		}
	}
}
