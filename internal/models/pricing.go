package models

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// ModelPricing is the USD price of one model per million tokens. Values are
// list prices at the time of writing; operators override them (or add their
// own models) via the AGENTOS_PRICING_JSON environment variable without
// recompiling.
type ModelPricing struct {
	// Model is the model id exactly as the provider reports it (e.g. "gpt-4o",
	// "llama-3.1-8b-instruct"). Matched case-insensitively; a "vendor/model"
	// id (OpenRouter style, e.g. "openai/gpt-4o") also matches its bare suffix.
	Model            string  `json:"model"`
	InputPerMTokens  float64 `json:"input_per_m_tokens"`
	OutputPerMTokens float64 `json:"output_per_m_tokens"`
}

// PricingEnvVar names the environment variable holding the JSON pricing
// override: a JSON array of ModelPricing objects, e.g.
//
//	AGENTOS_PRICING_JSON='[{"model":"gpt-4o","input_per_m_tokens":2.5,"output_per_m_tokens":10}]'
//
// Override entries replace built-in entries with the same (case-insensitive)
// model id and add new ones. Invalid JSON is ignored (the built-in table stays
// effective) — pricing must never break a run; validate overrides at startup
// with PricingFromJSON when strictness is wanted.
const PricingEnvVar = "AGENTOS_PRICING_JSON"

// builtinPricing is the small built-in price table for common
// OpenAI-compatible models (USD per 1M tokens).
var builtinPricing = []ModelPricing{
	{Model: "gpt-4o", InputPerMTokens: 2.50, OutputPerMTokens: 10.00},
	{Model: "gpt-4o-mini", InputPerMTokens: 0.15, OutputPerMTokens: 0.60},
	{Model: "gpt-4.1", InputPerMTokens: 2.00, OutputPerMTokens: 8.00},
	{Model: "gpt-4.1-mini", InputPerMTokens: 0.40, OutputPerMTokens: 1.60},
	{Model: "gpt-4.1-nano", InputPerMTokens: 0.10, OutputPerMTokens: 0.40},
	{Model: "o3-mini", InputPerMTokens: 1.10, OutputPerMTokens: 4.40},
	{Model: "llama-3.1-8b-instruct", InputPerMTokens: 0.18, OutputPerMTokens: 0.18},
	{Model: "llama-3.1-70b-instruct", InputPerMTokens: 0.88, OutputPerMTokens: 0.88},
	{Model: "mistral-small", InputPerMTokens: 0.20, OutputPerMTokens: 0.60},
	{Model: "mistral-small-latest", InputPerMTokens: 0.20, OutputPerMTokens: 0.60},
}

const (
	// tokensPerMillion is the pricing unit denominator (prices are per 1M tokens).
	tokensPerMillion = 1_000_000.0
	// centsPerUSD converts USD to cents (costs are accounted in cents).
	centsPerUSD = 100.0
)

// pricingState caches the resolved pricing table: an explicit SetPricing
// override wins; otherwise the AGENTOS_PRICING_JSON override (re-parsed when
// the raw env value changes) is merged over the built-in table.
type pricingState struct {
	mu       sync.RWMutex
	override map[string]ModelPricing
	envRaw   string
	envTable map[string]ModelPricing
}

var pricing pricingState

// PricingFromJSON parses a AGENTOS_PRICING_JSON payload: a JSON array of
// ModelPricing objects. Entries without a model id are rejected; duplicate
// model ids keep the last occurrence. Useful for startup validation of the
// env override.
func PricingFromJSON(raw string) ([]ModelPricing, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("models: empty %s payload", PricingEnvVar)
	}
	var entries []ModelPricing
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("models: invalid %s payload: %v", PricingEnvVar, err)
	}
	for i, e := range entries {
		if strings.TrimSpace(e.Model) == "" {
			return nil, fmt.Errorf("models: %s entry %d is missing \"model\"", PricingEnvVar, i)
		}
	}
	return entries, nil
}

// SetPricing installs an explicit pricing override (e.g. from a config file).
// nil resets to the built-in table + AGENTOS_PRICING_JSON env override.
func SetPricing(entries []ModelPricing) {
	pricing.mu.Lock()
	defer pricing.mu.Unlock()
	if entries == nil {
		pricing.override = nil
		return
	}
	pricing.override = buildPricingTable(entries)
}

// LookupPricing returns the effective price for one model. The second return
// is false for models without a pricing entry (ComputeCostCents prices them
// at 0 cents — unknown models never fail a run; see package docs).
func LookupPricing(model string) (ModelPricing, bool) {
	table := effectivePricingTable()
	return lookupInTable(table, model)
}

// ComputeCostCents prices one completion in cents:
//
//	cents = (promptTokens*input + completionTokens*output) / 1_000_000 * 100
//
// with input/output taken from the effective pricing table (built-in defaults
// merged with the AGENTOS_PRICING_JSON override). Contract: unknown models,
// empty model ids and non-positive token counts yield 0 — pricing is
// best-effort metering and NEVER returns an error or fails a caller.
func ComputeCostCents(model string, promptTokens, completionTokens int) float64 {
	if promptTokens <= 0 && completionTokens <= 0 {
		return 0
	}
	entry, ok := LookupPricing(model)
	if !ok {
		return 0
	}
	if promptTokens < 0 {
		promptTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
	}
	usd := (float64(promptTokens)*entry.InputPerMTokens + float64(completionTokens)*entry.OutputPerMTokens) / tokensPerMillion
	return usd * centsPerUSD
}

// effectivePricingTable resolves the table in force: SetPricing override >
// AGENTOS_PRICING_JSON override (merged over built-ins, re-parsed when the raw
// env changes) > built-in defaults.
func effectivePricingTable() map[string]ModelPricing {
	pricing.mu.RLock()
	override, envRaw, envTable := pricing.override, pricing.envRaw, pricing.envTable
	pricing.mu.RUnlock()
	if override != nil {
		return override
	}
	raw := os.Getenv(PricingEnvVar)
	if raw == envRaw && envTable != nil {
		return envTable
	}
	table := buildPricingTable(builtinPricing)
	if strings.TrimSpace(raw) != "" {
		// Invalid JSON is ignored on purpose (documented): the built-in table
		// stays effective and runs are never failed for a pricing problem.
		if entries, err := PricingFromJSON(raw); err == nil {
			for id, e := range buildPricingTable(entries) {
				table[id] = e
			}
		}
	}
	pricing.mu.Lock()
	pricing.envRaw, pricing.envTable = raw, table
	pricing.mu.Unlock()
	return table
}

// buildPricingTable indexes entries by lowercased model id (last wins).
func buildPricingTable(entries []ModelPricing) map[string]ModelPricing {
	table := make(map[string]ModelPricing, len(entries))
	for _, e := range entries {
		id := strings.ToLower(strings.TrimSpace(e.Model))
		if id == "" {
			continue
		}
		table[id] = e
	}
	return table
}

// lookupInTable matches a model id case-insensitively; a "vendor/model" id
// (e.g. "openai/gpt-4o") also matches by its bare suffix so OpenRouter-style
// ids resolve against the built-in OpenAI entries.
func lookupInTable(table map[string]ModelPricing, model string) (ModelPricing, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return ModelPricing{}, false
	}
	if e, ok := table[id]; ok {
		return e, true
	}
	if idx := strings.LastIndex(id, "/"); idx >= 0 && idx+1 < len(id) {
		if e, ok := table[id[idx+1:]]; ok {
			return e, true
		}
	}
	return ModelPricing{}, false
}
