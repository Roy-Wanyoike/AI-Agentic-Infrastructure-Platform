package models

// capabilities.go — issue #112: model capability registry.
//
// The registry maps model ids to the capabilities the ROUTER needs to make
// placement decisions: context window, tool/vision support, a coarse quality
// tier, a latency estimate and per-1k-token prices. It is deliberately
// separate from the wire-format capability table in resilience.go
// (DetectCapabilities, issue #128): this file answers "which model CAN serve
// this request well", resilience.go answers "how do I call this model".
//
// Figures are plausible public list figures compiled from vendor model docs
// and pricing pages (OpenAI, Anthropic, Google, DeepSeek, Together) at the
// time of writing: context windows in tokens, latency as a rough median
// end-to-end time for a typical ~1k-token completion, prices in USD per 1k
// input/output tokens. They are approximations for routing/ranking, not
// billing data — actual billing goes through pricing.go. Operators override
// or extend per tenant via the AGENTOS_MODELS_JSON env var (same shape,
// entries replace built-ins by model id) or programmatically via
// CapabilityRegistry.Register.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// CapabilitiesEnvVar names the environment variable holding tenant model
// capability overrides: a JSON array of ModelCapabilities objects. Override
// entries replace built-in entries with the same (case-insensitive) model id
// and add new ones; invalid JSON is ignored (the built-in table stays
// effective — capability data must never break a run; validate overrides at
// startup with CapabilitiesFromJSON when strictness is wanted).
const CapabilitiesEnvVar = "AGENTOS_MODELS_JSON"

// QualityTier is the coarse capability band of a model, used by the
// task_complexity routing policy to match request complexity to model
// quality.
type QualityTier string

const (
	// TierBasic models handle short, low-stakes prompts.
	TierBasic QualityTier = "basic"
	// TierStandard models handle everyday assistant work.
	TierStandard QualityTier = "standard"
	// TierPremium models handle nuanced multi-step work.
	TierPremium QualityTier = "premium"
	// TierFrontier models handle the hardest reasoning/expert requests.
	TierFrontier QualityTier = "frontier"
)

// tierRank orders quality tiers from weakest (0) to strongest (3); unknown
// tiers rank below basic so they are only chosen when nothing better exists.
var tierRank = map[QualityTier]int{
	TierBasic:    0,
	TierStandard: 1,
	TierPremium:  2,
	TierFrontier: 3,
}

// ModelCapabilities describes one model for routing purposes.
type ModelCapabilities struct {
	// Model is the canonical model id (matched case-insensitively; a
	// "vendor/model" id also matches by its bare suffix, consistent with
	// LookupPricing).
	Model string `json:"model"`
	// ContextTokens is the model's context window (input+output); 0 means
	// unknown (no context-fit filtering is applied).
	ContextTokens int `json:"context_tokens"`
	// ToolSupport reports whether the model can execute tool/function calls.
	ToolSupport bool `json:"tool_support"`
	// VisionSupport reports whether the model accepts image input.
	VisionSupport bool `json:"vision_support"`
	// QualityTier is the coarse capability band (basic..frontier).
	QualityTier QualityTier `json:"quality_tier"`
	// LatencyEstimateMS is the rough median end-to-end latency for a typical
	// ~1k-token completion (routing heuristic, not an SLA).
	LatencyEstimateMS int `json:"latency_estimate_ms"`
	// InputCostPer1K is the list price in USD per 1k input tokens.
	InputCostPer1K float64 `json:"input_cost_per_1k"`
	// OutputCostPer1K is the list price in USD per 1k output tokens.
	OutputCostPer1K float64 `json:"output_cost_per_1k"`
}

// Candidate is one routable model instance: a model id, the provider that
// serves it and its registry capabilities. Provider may be nil for
// registry-level resolution (CapabilityRegistry.SelectCandidates); the
// router requires non-nil providers to actually serve requests.
type Candidate struct {
	Model    string
	Provider Provider
	Caps     ModelCapabilities
}

// builtinCapabilities is the built-in registry covering common models.
// See the file-header comment for the data-source rationale.
var builtinCapabilities = []ModelCapabilities{
	// OpenAI (platform.openai.com/docs/models, pricing page).
	{Model: "gpt-4o", ContextTokens: 128000, ToolSupport: true, VisionSupport: true, QualityTier: TierPremium, LatencyEstimateMS: 3000, InputCostPer1K: 0.0025, OutputCostPer1K: 0.010},
	{Model: "gpt-4o-mini", ContextTokens: 128000, ToolSupport: true, VisionSupport: true, QualityTier: TierStandard, LatencyEstimateMS: 1200, InputCostPer1K: 0.00015, OutputCostPer1K: 0.0006},
	{Model: "gpt-4.1", ContextTokens: 1000000, ToolSupport: true, VisionSupport: true, QualityTier: TierPremium, LatencyEstimateMS: 3200, InputCostPer1K: 0.002, OutputCostPer1K: 0.008},
	{Model: "gpt-4.1-mini", ContextTokens: 1000000, ToolSupport: true, VisionSupport: true, QualityTier: TierStandard, LatencyEstimateMS: 1000, InputCostPer1K: 0.0004, OutputCostPer1K: 0.0016},
	{Model: "gpt-4.1-nano", ContextTokens: 1000000, ToolSupport: true, VisionSupport: true, QualityTier: TierBasic, LatencyEstimateMS: 700, InputCostPer1K: 0.0001, OutputCostPer1K: 0.0004},
	{Model: "gpt-5", ContextTokens: 400000, ToolSupport: true, VisionSupport: true, QualityTier: TierFrontier, LatencyEstimateMS: 6000, InputCostPer1K: 0.00125, OutputCostPer1K: 0.010},
	{Model: "gpt-5-mini", ContextTokens: 400000, ToolSupport: true, VisionSupport: true, QualityTier: TierPremium, LatencyEstimateMS: 3500, InputCostPer1K: 0.00025, OutputCostPer1K: 0.002},
	{Model: "gpt-5-nano", ContextTokens: 400000, ToolSupport: true, VisionSupport: true, QualityTier: TierStandard, LatencyEstimateMS: 2000, InputCostPer1K: 0.00005, OutputCostPer1K: 0.0004},
	{Model: "o1", ContextTokens: 200000, ToolSupport: false, VisionSupport: true, QualityTier: TierFrontier, LatencyEstimateMS: 15000, InputCostPer1K: 0.015, OutputCostPer1K: 0.060},
	{Model: "o1-mini", ContextTokens: 128000, ToolSupport: false, VisionSupport: false, QualityTier: TierPremium, LatencyEstimateMS: 9000, InputCostPer1K: 0.0011, OutputCostPer1K: 0.0044},
	{Model: "o3", ContextTokens: 200000, ToolSupport: true, VisionSupport: true, QualityTier: TierFrontier, LatencyEstimateMS: 12000, InputCostPer1K: 0.002, OutputCostPer1K: 0.008},
	{Model: "o3-mini", ContextTokens: 200000, ToolSupport: true, VisionSupport: false, QualityTier: TierPremium, LatencyEstimateMS: 8000, InputCostPer1K: 0.0011, OutputCostPer1K: 0.0044},
	{Model: "o4-mini", ContextTokens: 200000, ToolSupport: true, VisionSupport: true, QualityTier: TierPremium, LatencyEstimateMS: 7500, InputCostPer1K: 0.0011, OutputCostPer1K: 0.0044},
	// Anthropic (docs.anthropic.com, pricing page).
	{Model: "claude-3-5-sonnet-latest", ContextTokens: 200000, ToolSupport: true, VisionSupport: true, QualityTier: TierPremium, LatencyEstimateMS: 4000, InputCostPer1K: 0.003, OutputCostPer1K: 0.015},
	{Model: "claude-sonnet-4", ContextTokens: 200000, ToolSupport: true, VisionSupport: true, QualityTier: TierPremium, LatencyEstimateMS: 4200, InputCostPer1K: 0.003, OutputCostPer1K: 0.015},
	{Model: "claude-3-5-haiku-latest", ContextTokens: 200000, ToolSupport: true, VisionSupport: true, QualityTier: TierStandard, LatencyEstimateMS: 1500, InputCostPer1K: 0.0008, OutputCostPer1K: 0.004},
	{Model: "claude-3-opus-latest", ContextTokens: 200000, ToolSupport: true, VisionSupport: true, QualityTier: TierFrontier, LatencyEstimateMS: 8000, InputCostPer1K: 0.015, OutputCostPer1K: 0.075},
	{Model: "claude-opus-4", ContextTokens: 200000, ToolSupport: true, VisionSupport: true, QualityTier: TierFrontier, LatencyEstimateMS: 9000, InputCostPer1K: 0.015, OutputCostPer1K: 0.075},
	{Model: "claude-3-haiku-20240307", ContextTokens: 200000, ToolSupport: true, VisionSupport: true, QualityTier: TierBasic, LatencyEstimateMS: 1200, InputCostPer1K: 0.00025, OutputCostPer1K: 0.00125},
	// Google (ai.google.dev models & pricing).
	{Model: "gemini-1.5-pro", ContextTokens: 2000000, ToolSupport: true, VisionSupport: true, QualityTier: TierPremium, LatencyEstimateMS: 4000, InputCostPer1K: 0.00125, OutputCostPer1K: 0.005},
	{Model: "gemini-1.5-flash", ContextTokens: 1000000, ToolSupport: true, VisionSupport: true, QualityTier: TierStandard, LatencyEstimateMS: 900, InputCostPer1K: 0.000075, OutputCostPer1K: 0.0003},
	{Model: "gemini-2.0-flash", ContextTokens: 1000000, ToolSupport: true, VisionSupport: true, QualityTier: TierStandard, LatencyEstimateMS: 800, InputCostPer1K: 0.0001, OutputCostPer1K: 0.0004},
	{Model: "gemini-2.5-pro", ContextTokens: 1000000, ToolSupport: true, VisionSupport: true, QualityTier: TierFrontier, LatencyEstimateMS: 7000, InputCostPer1K: 0.00125, OutputCostPer1K: 0.010},
	{Model: "gemini-2.5-flash", ContextTokens: 1000000, ToolSupport: true, VisionSupport: true, QualityTier: TierPremium, LatencyEstimateMS: 1500, InputCostPer1K: 0.0003, OutputCostPer1K: 0.0025},
	// DeepSeek (api-docs.deepseek.com).
	{Model: "deepseek-chat", ContextTokens: 64000, ToolSupport: true, VisionSupport: false, QualityTier: TierStandard, LatencyEstimateMS: 2000, InputCostPer1K: 0.00014, OutputCostPer1K: 0.00028},
	{Model: "deepseek-reasoner", ContextTokens: 64000, ToolSupport: false, VisionSupport: false, QualityTier: TierPremium, LatencyEstimateMS: 12000, InputCostPer1K: 0.00055, OutputCostPer1K: 0.00219},
	// Meta Llama (self-host / Together pricing as a public reference).
	{Model: "llama-3.1-8b-instruct", ContextTokens: 128000, ToolSupport: true, VisionSupport: false, QualityTier: TierBasic, LatencyEstimateMS: 600, InputCostPer1K: 0.00018, OutputCostPer1K: 0.00018},
	{Model: "llama-3.1-70b-instruct", ContextTokens: 128000, ToolSupport: true, VisionSupport: false, QualityTier: TierStandard, LatencyEstimateMS: 1800, InputCostPer1K: 0.00088, OutputCostPer1K: 0.00088},
	{Model: "llama-3.1-405b-instruct", ContextTokens: 128000, ToolSupport: true, VisionSupport: false, QualityTier: TierPremium, LatencyEstimateMS: 4000, InputCostPer1K: 0.00268, OutputCostPer1K: 0.00268},
	// Mistral (kept consistent with builtinPricing in pricing.go).
	{Model: "mistral-small", ContextTokens: 128000, ToolSupport: true, VisionSupport: false, QualityTier: TierBasic, LatencyEstimateMS: 900, InputCostPer1K: 0.0002, OutputCostPer1K: 0.0006},
	{Model: "mistral-small-latest", ContextTokens: 128000, ToolSupport: true, VisionSupport: false, QualityTier: TierBasic, LatencyEstimateMS: 900, InputCostPer1K: 0.0002, OutputCostPer1K: 0.0006},
}

// CapabilityRegistry maps model ids to ModelCapabilities. It is safe for
// concurrent use. The zero registry is empty — start from
// NewCapabilityRegistry (built-ins) or DefaultCapabilityRegistry (built-ins +
// AGENTOS_MODELS_JSON overrides).
type CapabilityRegistry struct {
	mu   sync.RWMutex
	byID map[string]ModelCapabilities
}

// NewCapabilityRegistry returns a registry preloaded with the built-in model
// table.
func NewCapabilityRegistry() *CapabilityRegistry {
	r := &CapabilityRegistry{byID: make(map[string]ModelCapabilities, len(builtinCapabilities))}
	for _, c := range builtinCapabilities {
		r.byID[strings.ToLower(c.Model)] = c
	}
	return r
}

// DefaultCapabilityRegistry returns the built-in registry with tenant
// overrides from AGENTOS_MODELS_JSON merged over it. Invalid override JSON is
// ignored (built-ins stay effective), mirroring the AGENTOS_PRICING_JSON
// contract.
func DefaultCapabilityRegistry() *CapabilityRegistry {
	r := NewCapabilityRegistry()
	if raw := strings.TrimSpace(envOr(CapabilitiesEnvVar, "")); raw != "" {
		if entries, err := CapabilitiesFromJSON(raw); err == nil {
			for _, e := range entries {
				r.Register(e)
			}
		}
	}
	return r
}

// envOr returns the environment value or the fallback when blank.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// Register adds or replaces one model entry (tenant override). Entries with
// a blank model id are ignored. Override entries MERGE field-by-field over
// the existing entry (built-in or earlier override): zero-valued numeric
// fields and an empty tier keep the existing value, and boolean capabilities
// merge with OR — an override may only WIDEN capability, never retract it
// (register a differently-named model id when a variant needs narrower
// capabilities). This lets a tenant adjust one attribute (e.g. a smaller
// context window) without re-stating the whole entry.
func (r *CapabilityRegistry) Register(caps ModelCapabilities) {
	id := strings.ToLower(strings.TrimSpace(caps.Model))
	if id == "" {
		return
	}
	caps.Model = strings.TrimSpace(caps.Model)
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.byID[id]; ok {
		caps = mergeCapabilities(prev, caps)
	}
	r.byID[id] = caps
}

// mergeCapabilities fills the zero-valued fields of override from prev (see
// Register for the merge contract).
func mergeCapabilities(prev, override ModelCapabilities) ModelCapabilities {
	if override.ContextTokens == 0 {
		override.ContextTokens = prev.ContextTokens
	}
	if override.LatencyEstimateMS == 0 {
		override.LatencyEstimateMS = prev.LatencyEstimateMS
	}
	if override.QualityTier == "" {
		override.QualityTier = prev.QualityTier
	}
	if override.InputCostPer1K == 0 {
		override.InputCostPer1K = prev.InputCostPer1K
	}
	if override.OutputCostPer1K == 0 {
		override.OutputCostPer1K = prev.OutputCostPer1K
	}
	override.ToolSupport = override.ToolSupport || prev.ToolSupport
	override.VisionSupport = override.VisionSupport || prev.VisionSupport
	return override
}

// Lookup resolves one model id case-insensitively; a "vendor/model" id also
// matches by its bare suffix (OpenRouter-style ids resolve against built-in
// entries), consistent with LookupPricing. The second return is false when
// the registry has no entry.
func (r *CapabilityRegistry) Lookup(model string) (ModelCapabilities, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return ModelCapabilities{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.byID[id]; ok {
		return c, true
	}
	if idx := strings.LastIndex(id, "/"); idx >= 0 && idx+1 < len(id) {
		if c, ok := r.byID[id[idx+1:]]; ok {
			return c, true
		}
	}
	return ModelCapabilities{}, false
}

// Models returns the registered model ids sorted ascending (deterministic).
func (r *CapabilityRegistry) Models() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	// sort for determinism
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// SelectCandidates resolves model ids against the registry and returns the
// candidates capable of serving req, in input order. Filtering rules (all
// deterministic):
//
//   - unknown models are DROPPED (fail-closed): a router must not send a
//     request to a model whose capabilities it cannot verify — register
//     custom models via Register or AGENTOS_MODELS_JSON;
//   - context fit: estimated prompt tokens + requested MaxTokens must fit
//     the context window (models with ContextTokens==0 are not filtered —
//     unknown window);
//   - tools: a request with attached tools requires ToolSupport;
//   - vision: a request flagged NeedsVision requires VisionSupport.
func (r *CapabilityRegistry) SelectCandidates(models []string, req CompletionRequest) []Candidate {
	out := make([]Candidate, 0, len(models))
	for _, id := range models {
		caps, ok := r.Lookup(id)
		if !ok || !capsFitRequest(caps, req) {
			continue
		}
		out = append(out, Candidate{Model: caps.Model, Caps: caps})
	}
	return out
}

// capsFitRequest reports whether one capability entry satisfies the request's
// capability constraints (context fit, tools, vision). See
// CapabilityRegistry.SelectCandidates for the documented rules.
func capsFitRequest(caps ModelCapabilities, req CompletionRequest) bool {
	if req.NeedsVision && !caps.VisionSupport {
		return false
	}
	if len(req.Tools) > 0 && !caps.ToolSupport {
		return false
	}
	if caps.ContextTokens > 0 {
		need := estimateTokens(req.Prompt) + estimateTokens(req.System) + req.MaxTokens
		for _, m := range req.Messages {
			need += estimateTokens(m.Content)
		}
		if need > caps.ContextTokens {
			return false
		}
	}
	return true
}

// CapabilitiesFromJSON parses an AGENTOS_MODELS_JSON payload: a JSON array of
// ModelCapabilities objects. Entries without a model id are rejected;
// duplicate model ids keep the last occurrence. Useful for startup
// validation of the env override.
func CapabilitiesFromJSON(raw string) ([]ModelCapabilities, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("models: empty %s payload", CapabilitiesEnvVar)
	}
	var entries []ModelCapabilities
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("models: invalid %s payload: %v", CapabilitiesEnvVar, err)
	}
	for i, e := range entries {
		if strings.TrimSpace(e.Model) == "" {
			return nil, fmt.Errorf("models: %s entry %d is missing \"model\"", CapabilitiesEnvVar, i)
		}
	}
	return entries, nil
}
