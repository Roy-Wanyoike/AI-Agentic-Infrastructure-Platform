package models

// router_policies.go — issue #112: advanced model routing policies.
//
// PolicyProvider wraps a set of Candidate backends and implements the plain
// Provider interface, so it drops into every consumer that takes a
// models.Provider (the runtime runner, the evaluation runner, tests). For
// each request it:
//
//  1. resolves the effective routing policy — the per-agent override from
//     CompletionRequest.Routing (parsed from the agent runtime_config
//     `routing` object) when present, otherwise the router's default;
//  2. restricts the candidate pool to the per-agent candidate list (when
//     the override carries one) and drops candidates whose capabilities do
//     not fit the request (context fit, tools, vision — see
//     CapabilityRegistry.SelectCandidates);
//  3. orders the pool with the policy (cost / latency / complexity /
//     round-robin / weighted — all deterministic);
//  4. executes the order with per-provider circuit breakers, skipping
//     circuit-open providers, failing over on transient errors only
//     (mirroring FailoverProvider semantics) and annotating the response
//     Metadata with the routing decision.
//
// All orderings are deterministic functions of (request, candidate set,
// state the router owns), so tests are stable without seeds or sleeps.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// RoutingPolicy selects how the PolicyProvider orders its candidates.
type RoutingPolicy string

const (
	// PolicyCostOptimize prefers the cheapest capable model per request.
	PolicyCostOptimize RoutingPolicy = "cost_optimize"
	// PolicyLatencyOptimize prefers the model with the lowest estimated
	// latency.
	PolicyLatencyOptimize RoutingPolicy = "latency_optimize"
	// PolicyTaskComplexity classifies the request (ClassifyComplexity) and
	// matches it to the closest capability tier.
	PolicyTaskComplexity RoutingPolicy = "task_complexity"
	// PolicyRoundRobin rotates deterministically across the candidates.
	PolicyRoundRobin RoutingPolicy = "round_robin"
	// PolicyWeighted picks via a seeded hash of the request content and the
	// configured candidate weights (deterministic per request content).
	PolicyWeighted RoutingPolicy = "weighted"
)

// Response metadata keys written by PolicyProvider (observability surface;
// see CompletionResponse.Metadata).
const (
	MetaRoutingPolicy     = "routing_policy"
	MetaRoutingModel      = "routing_model"
	MetaRoutingProvider   = "routing_provider"
	MetaRoutingComplexity = "routing_complexity"
	MetaRoutingFallback   = "routing_fallback" // "true" when a non-first candidate served the request
)

// RoutingConfig is the per-agent routing configuration carried in the agent
// runtime_config under the `routing` key:
//
//	{"routing": {"policy": "cost_optimize", "candidates": ["gpt-4o-mini", "gpt-4o@3"]}}
//
// Candidates are model ids (resolved against the CapabilityRegistry); an
// optional "@<weight>" suffix sets the relative weight used by the weighted
// policy (default 1; must be a positive integer). The runtime copies this
// object into CompletionRequest.Routing; a nil Routing on a request means
// "use the router's default policy and candidate set".
type RoutingConfig struct {
	// Policy is the routing policy name (required, see ParseRoutingPolicy).
	Policy RoutingPolicy `json:"policy"`
	// Candidates optionally restricts/overrides the router's candidate set
	// for requests carrying this config, in the given order.
	Candidates []string `json:"candidates,omitempty"`

	// weights is parsed from the "@<weight>" suffixes of Candidates by
	// RoutingConfigFromMap/RoutingConfigFromJSON. A RoutingConfig built by
	// plain JSON unmarshalling has no weights; Weight then returns 1 for
	// every model.
	weights map[string]int
}

// Weight returns the configured relative weight for one model id (default 1
// for unconfigured or non-positive entries).
func (c RoutingConfig) Weight(model string) int {
	if c.weights == nil {
		return 1
	}
	if w, ok := c.weights[strings.ToLower(strings.TrimSpace(model))]; ok && w > 0 {
		return w
	}
	return 1
}

// ParseRoutingPolicy parses a policy name (trimmed, lowercased). Unknown or
// empty names are rejected with a listed error.
func ParseRoutingPolicy(s string) (RoutingPolicy, error) {
	switch p := RoutingPolicy(strings.ToLower(strings.TrimSpace(s))); p {
	case PolicyCostOptimize, PolicyLatencyOptimize, PolicyTaskComplexity, PolicyRoundRobin, PolicyWeighted:
		return p, nil
	case "":
		return "", fmt.Errorf("%w: empty routing policy (want one of %s, %s, %s, %s, %s)",
			ErrInvalidRequest, PolicyCostOptimize, PolicyLatencyOptimize, PolicyTaskComplexity, PolicyRoundRobin, PolicyWeighted)
	default:
		return "", fmt.Errorf("%w: unknown routing policy %q (want one of %s, %s, %s, %s, %s)",
			ErrInvalidRequest, s, PolicyCostOptimize, PolicyLatencyOptimize, PolicyTaskComplexity, PolicyRoundRobin, PolicyWeighted)
	}
}

// RoutingConfigFromMap parses the `routing` object of an agent runtime_config
// (values as produced by encoding/json into map[string]any). Policy is
// required and must be known; candidates, when present, must be an array of
// strings carrying optional "@<weight>" suffixes (positive integers).
func RoutingConfigFromMap(m map[string]any) (RoutingConfig, error) {
	if m == nil {
		return RoutingConfig{}, fmt.Errorf("%w: routing config is required", ErrInvalidRequest)
	}
	rawPolicy, _ := m["policy"].(string)
	policy, err := ParseRoutingPolicy(rawPolicy)
	if err != nil {
		return RoutingConfig{}, err
	}
	cfg := RoutingConfig{Policy: policy}
	rawCands, ok := m["candidates"]
	if !ok || rawCands == nil {
		return cfg, nil
	}
	var names []string
	switch cands := rawCands.(type) {
	case []any:
		names = make([]string, 0, len(cands))
		for i, c := range cands {
			s, ok := c.(string)
			if !ok {
				return RoutingConfig{}, fmt.Errorf("%w: routing candidate %d is not a string", ErrInvalidRequest, i)
			}
			names = append(names, s)
		}
	case []string:
		names = cands
	default:
		return RoutingConfig{}, fmt.Errorf("%w: routing candidates must be an array of strings", ErrInvalidRequest)
	}
	if err := cfg.setCandidates(names); err != nil {
		return RoutingConfig{}, err
	}
	return cfg, nil
}

// RoutingConfigFromJSON parses a JSON-encoded routing object (the
// `{"policy": ..., "candidates": [...]}` shape).
func RoutingConfigFromJSON(raw string) (RoutingConfig, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return RoutingConfig{}, fmt.Errorf("%w: invalid routing config JSON: %v", ErrInvalidRequest, err)
	}
	return RoutingConfigFromMap(m)
}

// setCandidates parses candidate entries and their optional "@<weight>"
// suffixes; the receiver must have a valid Policy already.
func (c *RoutingConfig) setCandidates(names []string) error {
	c.Candidates = make([]string, 0, len(names))
	for _, n := range names {
		entry := strings.TrimSpace(n)
		if entry == "" {
			return fmt.Errorf("%w: routing candidate entries must not be blank", ErrInvalidRequest)
		}
		weight := 1
		if idx := strings.LastIndex(entry, "@"); idx > 0 {
			w, err := strconv.Atoi(strings.TrimSpace(entry[idx+1:]))
			if err != nil || w <= 0 {
				return fmt.Errorf("%w: routing candidate %q has an invalid @weight suffix", ErrInvalidRequest, entry)
			}
			weight = w
			entry = strings.TrimSpace(entry[:idx])
		}
		if c.weights == nil {
			c.weights = make(map[string]int)
		}
		c.weights[strings.ToLower(entry)] = weight
		c.Candidates = append(c.Candidates, entry)
	}
	return nil
}

// Complexity classifies how demanding a request is for the
// task_complexity routing policy.
type Complexity string

const (
	// ComplexitySimple — short, single-turn, no reasoning demand.
	ComplexitySimple Complexity = "simple"
	// ComplexityStandard — everyday assistant work.
	ComplexityStandard Complexity = "standard"
	// ComplexityComplex — long or multi-step reasoning work.
	ComplexityComplex Complexity = "complex"
	// ComplexityExpert — the hardest analytical/expert requests.
	ComplexityExpert Complexity = "expert"
)

// complexityReasoningIndicators are the documented reasoning indicators of
// the classification heuristic (matched case-insensitively as substrings;
// each DISTINCT indicator present adds +1, capped at +2).
var complexityReasoningIndicators = []string{
	"step by step", "step-by-step", "reason", "analyze", "derive",
	"prove", "root cause", "debug", "architecture", "trade-off",
	"optimize", "compare", "why",
}

// ClassifyComplexity maps a completion request onto a complexity class with
// a deterministic, additive heuristic over three signals:
//
//  1. prompt size — total characters across Prompt/System/Messages:
//     <200 → +0, 200..1999 → +1, 2000..7999 → +2, >=8000 → +3;
//  2. reasoning indicators — the documented phrases above, capped at +2;
//  3. request shape — attached tools add +1 (agentic loop); MaxTokens >= 4096
//     adds +1 (long-form generation).
//
// Score → class: 0 → Simple, 1–2 → Standard, 3–4 → Complex, >=5 → Expert.
// The function is pure: identical input always yields the identical class.
func ClassifyComplexity(req CompletionRequest) Complexity {
	chars := len(req.Prompt) + len(req.System)
	for _, m := range req.Messages {
		chars += len(m.Content)
	}
	score := 0
	switch {
	case chars >= 8000:
		score += 3
	case chars >= 2000:
		score += 2
	case chars >= 200:
		score += 1
	}
	haystack := strings.ToLower(req.Prompt + "\n" + req.System)
	for _, m := range req.Messages {
		haystack += "\n" + strings.ToLower(m.Content)
	}
	indicators := 0
	for _, ind := range complexityReasoningIndicators {
		if strings.Contains(haystack, ind) {
			indicators++
		}
	}
	if indicators > 2 {
		indicators = 2
	}
	score += indicators
	if len(req.Tools) > 0 {
		score++
	}
	if req.MaxTokens >= 4096 {
		score++
	}
	switch {
	case score >= 5:
		return ComplexityExpert
	case score >= 3:
		return ComplexityComplex
	case score >= 1:
		return ComplexityStandard
	default:
		return ComplexitySimple
	}
}

// Tier maps a complexity class onto the capability tier that should serve it
// (Simple→basic, Standard→standard, Complex→premium, Expert→frontier).
// Unknown classes map to standard (a conservative default).
func (c Complexity) Tier() QualityTier {
	switch c {
	case ComplexitySimple:
		return TierBasic
	case ComplexityComplex:
		return TierPremium
	case ComplexityExpert:
		return TierFrontier
	default:
		return TierStandard
	}
}

// SkippedCandidate records why a candidate was excluded from (or jumped over
// during) routing.
type SkippedCandidate struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Reason   string `json:"reason"`
}

// RouterDecision is the observability projection of one routing choice:
// which policy ran, how the request was classified, which model was picked
// and why, in which order the candidates would be attempted and which were
// skipped. Decide never calls providers or mutates breaker state.
type RouterDecision struct {
	Policy         RoutingPolicy      `json:"policy"`
	Complexity     Complexity         `json:"complexity"`
	SelectedModel  string             `json:"selected_model,omitempty"`
	ProviderName   string             `json:"provider_name,omitempty"`
	Reason         string             `json:"reason,omitempty"`
	CandidateOrder []string           `json:"candidate_order,omitempty"`
	Skipped        []SkippedCandidate `json:"skipped,omitempty"`
}

// RouterOption configures a PolicyProvider at construction.
type RouterOption func(*routerOptions)

type routerOptions struct {
	clock Clock
}

// WithRouterClock injects the clock used by the router's circuit breakers
// (tests inject a fake clock for deterministic transitions). Nil keeps the
// wall clock.
func WithRouterClock(c Clock) RouterOption {
	return func(o *routerOptions) {
		if c != nil {
			o.clock = c
		}
	}
}

// PolicyProvider implements Provider by routing each request across its
// candidate set with the configured policy (see the file header). It is safe
// for concurrent use.
type PolicyProvider struct {
	registry      *CapabilityRegistry
	defaultPolicy RoutingPolicy
	candidates    []Candidate
	breakers      map[string]*CircuitBreaker
	clock         Clock

	rrCounter atomic.Uint64 // round-robin rotation counter
}

// NewPolicyProvider builds a router over candidates. The registry resolves
// missing capability entries; candidates whose model is unknown to the
// registry (and that carry no explicit Caps) are rejected — routing must not
// send requests to models whose capabilities it cannot verify (register
// custom models via CapabilityRegistry.Register or AGENTOS_MODELS_JSON).
func NewPolicyProvider(registry *CapabilityRegistry, policy RoutingPolicy, candidates []Candidate, opts ...RouterOption) (*PolicyProvider, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: capability registry is required", ErrInvalidRequest)
	}
	if _, err := ParseRoutingPolicy(string(policy)); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNoProvider
	}
	ropts := routerOptions{}
	for _, o := range opts {
		if o != nil {
			o(&ropts)
		}
	}
	if ropts.clock == nil {
		ropts.clock = realClock{}
	}
	resolved := make([]Candidate, 0, len(candidates))
	breakers := make(map[string]*CircuitBreaker, len(candidates))
	for i, c := range candidates {
		if c.Provider == nil {
			return nil, fmt.Errorf("%w: candidate %d (%s) has no provider", ErrInvalidRequest, i, c.Model)
		}
		if strings.TrimSpace(c.Model) == "" && strings.TrimSpace(c.Caps.Model) == "" {
			return nil, fmt.Errorf("%w: candidate %d has no model id", ErrInvalidRequest, i)
		}
		caps := c.Caps
		if strings.TrimSpace(caps.Model) == "" {
			// Adopt the registry entry; unknown models are a construction
			// error (fail-closed capability verification).
			lookup, ok := registry.Lookup(c.Model)
			if !ok {
				return nil, fmt.Errorf("%w: candidate model %q is not in the capability registry (register it via CapabilityRegistry.Register or %s)",
					ErrInvalidRequest, c.Model, CapabilitiesEnvVar)
			}
			caps = lookup
		}
		if strings.TrimSpace(c.Model) == "" {
			c.Model = caps.Model
		}
		name := c.Provider.Name()
		if _, exists := breakers[name]; !exists {
			breakers[name] = NewCircuitBreaker(CircuitBreakerConfig{Clock: ropts.clock})
		}
		resolved = append(resolved, c)
	}
	return &PolicyProvider{
		registry:      registry,
		defaultPolicy: policy,
		candidates:    resolved,
		breakers:      breakers,
		clock:         ropts.clock,
	}, nil
}

// Name identifies the router for logs and step records:
// "router:<policy>(model1|model2|...)".
func (p *PolicyProvider) Name() string {
	if p == nil {
		return ""
	}
	ids := make([]string, 0, len(p.candidates))
	for _, c := range p.candidates {
		ids = append(ids, c.Model)
	}
	return fmt.Sprintf("router:%s(%s)", p.defaultPolicy, strings.Join(ids, "|"))
}

// Breaker exposes the circuit breaker guarding one provider name (used by
// operators/tests to inspect or prime breaker state). The second return is
// false when no candidate is served by that provider name.
func (p *PolicyProvider) Breaker(providerName string) (*CircuitBreaker, bool) {
	b, ok := p.breakers[providerName]
	return b, ok
}

// Decide projects the routing decision for req WITHOUT calling providers or
// mutating breaker/rotation state (read-only; the round-robin projection
// shows the rotation as of the last Complete). Candidates skipped for
// capability or circuit-open reasons are listed on the decision.
func (p *PolicyProvider) Decide(req CompletionRequest) (*RouterDecision, error) {
	ordered, decision, err := p.plan(req, false)
	if err != nil {
		return nil, err
	}
	decision.CandidateOrder = modelIDs(ordered)
	// Annotate circuit-open candidates from breaker state (no mutation).
	for _, c := range ordered {
		if b, ok := p.breakers[c.Provider.Name()]; ok && b.Snapshot().State == BreakerOpen {
			decision.Skipped = append(decision.Skipped, SkippedCandidate{
				Model: c.Model, Provider: c.Provider.Name(), Reason: "circuit_open",
			})
		}
	}
	if len(ordered) > 0 {
		decision.SelectedModel = ordered[0].Model
		decision.ProviderName = ordered[0].Provider.Name()
	}
	return decision, nil
}

// Complete implements Provider: plan the order, then execute it across the
// candidates, skipping circuit-open providers and failing over on transient
// errors only. The served response carries the routing decision in Metadata.
func (p *PolicyProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ordered, decision, err := p.plan(req, true)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for i, c := range ordered {
		breaker := p.breakers[c.Provider.Name()]
		if breaker != nil && !breaker.Allow() {
			decision.Skipped = append(decision.Skipped, SkippedCandidate{
				Model: c.Model, Provider: c.Provider.Name(), Reason: "circuit_open",
			})
			continue
		}
		resp, err := c.Provider.Complete(ctx, req)
		if err == nil {
			if breaker != nil {
				breaker.RecordSuccess()
			}
			p.annotate(resp, decision, c, i > 0)
			return resp, nil
		}
		if breaker != nil && IsTransient(err) {
			breaker.RecordFailure()
		}
		if !IsTransient(err) {
			// Non-transient failures (auth, invalid request, cancellation)
			// surface immediately — the same contract as FailoverProvider.
			return nil, err
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	// Every candidate was skipped (circuit-open) or the pool was empty.
	return nil, fmt.Errorf("%w: no routable candidate for policy %s", ErrNoProvider, decision.Policy)
}

// annotate writes the routing decision onto the response metadata.
func (p *PolicyProvider) annotate(resp *CompletionResponse, decision *RouterDecision, served Candidate, fallback bool) {
	if resp == nil {
		return
	}
	metaSet(resp, MetaRoutingPolicy, string(decision.Policy))
	metaSet(resp, MetaRoutingModel, served.Model)
	metaSet(resp, MetaRoutingProvider, served.Provider.Name())
	metaSet(resp, MetaRoutingComplexity, string(decision.Complexity))
	if fallback {
		metaSet(resp, MetaRoutingFallback, "true")
	}
}

// plan resolves the effective policy + candidate pool and returns the
// attempt order plus the decision projection. advanceRR selects whether the
// round-robin counter advances (Complete) or is only peeked (Decide).
func (p *PolicyProvider) plan(req CompletionRequest, advanceRR bool) ([]Candidate, *RouterDecision, error) {
	policy := p.defaultPolicy
	if req.Routing != nil && req.Routing.Policy != "" {
		parsed, err := ParseRoutingPolicy(string(req.Routing.Policy))
		if err != nil {
			return nil, nil, err
		}
		policy = parsed
	}

	pool := p.candidates
	var skipped []SkippedCandidate
	if req.Routing != nil && len(req.Routing.Candidates) > 0 {
		pool, skipped = restrictCandidates(pool, req.Routing)
	}

	fit := make([]Candidate, 0, len(pool))
	for _, c := range pool {
		if capsFitRequest(c.Caps, req) {
			fit = append(fit, c)
		} else {
			reason := "capability_mismatch"
			if req.NeedsVision && !c.Caps.VisionSupport {
				reason = "vision_unsupported"
			} else if len(req.Tools) > 0 && !c.Caps.ToolSupport {
				reason = "tools_unsupported"
			} else if c.Caps.ContextTokens > 0 {
				reason = "context_overflow"
			}
			skipped = append(skipped, SkippedCandidate{Model: c.Model, Provider: c.Provider.Name(), Reason: reason})
		}
	}
	if len(fit) == 0 {
		return nil, &RouterDecision{Policy: policy, Skipped: skipped}, fmt.Errorf("%w: no capable candidate for policy %s", ErrNoProvider, policy)
	}

	complexity := ClassifyComplexity(req)
	ordered, reason := p.order(policy, fit, req, complexity, advanceRR)
	return ordered, &RouterDecision{
		Policy:     policy,
		Complexity: complexity,
		Reason:     reason,
		Skipped:    skipped,
	}, nil
}

// restrictCandidates narrows the pool to the per-agent candidate list,
// preserving the CONFIG order (the agent's stated preference order).
func restrictCandidates(pool []Candidate, cfg *RoutingConfig) ([]Candidate, []SkippedCandidate) {
	byID := make(map[string]Candidate, len(pool))
	for _, c := range pool {
		byID[strings.ToLower(c.Model)] = c
	}
	out := make([]Candidate, 0, len(cfg.Candidates))
	var skipped []SkippedCandidate
	seen := make(map[string]bool, len(cfg.Candidates))
	for _, want := range cfg.Candidates {
		id := strings.ToLower(strings.TrimSpace(want))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if c, ok := byID[id]; ok {
			out = append(out, c)
		} else {
			skipped = append(skipped, SkippedCandidate{Model: id, Reason: "not_configured"})
		}
	}
	return out, skipped
}

// order applies the policy ordering. The returned reason documents why the
// first candidate was picked (surfaced on the RouterDecision).
func (p *PolicyProvider) order(policy RoutingPolicy, pool []Candidate, req CompletionRequest, complexity Complexity, advanceRR bool) ([]Candidate, string) {
	switch policy {
	case PolicyCostOptimize:
		out := cloneCandidates(pool)
		best := out[0]
		bestCost := estimateRequestCost(best.Caps, req)
		for _, c := range out[1:] {
			if cost := estimateRequestCost(c.Caps, req); cost < bestCost {
				best, bestCost = c, cost
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			ci, cj := estimateRequestCost(out[i].Caps, req), estimateRequestCost(out[j].Caps, req)
			if ci != cj {
				return ci < cj
			}
			return out[i].Model < out[j].Model
		})
		return out, fmt.Sprintf("cheapest capable (est $%.6f/request)", bestCost)
	case PolicyLatencyOptimize:
		out := cloneCandidates(pool)
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Caps.LatencyEstimateMS != out[j].Caps.LatencyEstimateMS {
				return out[i].Caps.LatencyEstimateMS < out[j].Caps.LatencyEstimateMS
			}
			return out[i].Model < out[j].Model
		})
		return out, fmt.Sprintf("lowest estimated latency (%dms)", out[0].Caps.LatencyEstimateMS)
	case PolicyTaskComplexity:
		want := complexity.Tier()
		out := cloneCandidates(pool)
		sort.SliceStable(out, func(i, j int) bool {
			di := tierDistance(out[i].Caps.QualityTier, want)
			dj := tierDistance(out[j].Caps.QualityTier, want)
			if di != dj {
				return di < dj
			}
			if out[i].Caps.LatencyEstimateMS != out[j].Caps.LatencyEstimateMS {
				return out[i].Caps.LatencyEstimateMS < out[j].Caps.LatencyEstimateMS
			}
			return out[i].Model < out[j].Model
		})
		return out, fmt.Sprintf("closest capability tier to %s (want %s)", complexity, want)
	case PolicyRoundRobin:
		out := cloneCandidates(pool)
		var start uint64
		if advanceRR {
			start = p.rrCounter.Add(1) - 1
		} else {
			start = p.rrCounter.Load()
		}
		offset := int(start % uint64(len(out)))
		rotated := make([]Candidate, 0, len(out))
		rotated = append(rotated, out[offset:]...)
		rotated = append(rotated, out[:offset]...)
		return rotated, fmt.Sprintf("round-robin rotation (turn %d)", start)
	case PolicyWeighted:
		out := cloneCandidates(pool)
		type scored struct {
			cand  Candidate
			score float64
		}
		seed := requestSeed(req)
		ss := make([]scored, 0, len(out))
		for _, c := range out {
			w := 1
			if req.Routing != nil {
				w = req.Routing.Weight(c.Model)
			}
			ss = append(ss, scored{cand: c, score: weightedRandomKey(seed, c.Model, w)})
		}
		sort.SliceStable(ss, func(i, j int) bool {
			if ss[i].score != ss[j].score {
				return ss[i].score > ss[j].score
			}
			return ss[i].cand.Model < ss[j].cand.Model
		})
		out = out[:0]
		for _, s := range ss {
			out = append(out, s.cand)
		}
		return out, fmt.Sprintf("weighted random-key selection (seed %d)", seed)
	default:
		// Unreachable (policy validated at construction), kept total for
		// safety: the pool order is already deterministic.
		return cloneCandidates(pool), "pool order"
	}
}

// cloneCandidates copies the pool so ordering never mutates the router's
// candidate slice.
func cloneCandidates(in []Candidate) []Candidate {
	out := make([]Candidate, len(in))
	copy(out, in)
	return out
}

// modelIDs extracts the candidate model ids in order.
func modelIDs(cands []Candidate) []string {
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Model)
	}
	return ids
}

// tierDistance ranks a candidate's tier against the wanted tier (0 = exact
// match); unknown tiers rank below basic.
func tierDistance(have, want QualityTier) int {
	hr, ok := tierRank[have]
	if !ok {
		hr = -1
	}
	wr := tierRank[want]
	if hr < 0 {
		return wr + 1 // unknown tier: distance grows with how much was wanted
	}
	d := hr - wr
	if d < 0 {
		d = -d
	}
	return d
}

// defaultOutputTokensEstimate is the output volume assumed by the cost
// estimate when a request carries no explicit MaxTokens.
const defaultOutputTokensEstimate = 256

// estimateRequestCost estimates the USD cost of serving req on one model:
// estimated input tokens at the input price plus the requested output
// budget (default 256 tokens) at the output price.
func estimateRequestCost(caps ModelCapabilities, req CompletionRequest) float64 {
	in := estimateTokens(req.Prompt) + estimateTokens(req.System)
	for _, m := range req.Messages {
		in += estimateTokens(m.Content)
	}
	out := req.MaxTokens
	if out <= 0 {
		out = defaultOutputTokensEstimate
	}
	return float64(in)*caps.InputCostPer1K/1000 + float64(out)*caps.OutputCostPer1K/1000
}

// requestSeed derives the deterministic weighted-selection seed from the
// request content (FNV-1a over prompt, system, messages, model and output
// budget, separated by NUL bytes).
func requestSeed(req CompletionRequest) uint64 {
	h := fnv.New64a()
	parts := [][]byte{[]byte(req.Prompt), []byte(req.System), []byte(req.Model), []byte(strconv.Itoa(req.MaxTokens))}
	for _, p := range parts {
		_, _ = h.Write(p)
		_, _ = h.Write([]byte{0})
	}
	for _, m := range req.Messages {
		_, _ = h.Write([]byte(m.Role))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(m.Content))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// weightedRandomKey implements one step of the deterministic weighted
// selection (weighted-random-key / Efraimidis–Spirakis with the randomness
// replaced by a content-seeded hash):
//
//	key(m)   = FNV-1a64(seed || '|' || model) / 2^64   ∈ [0,1)
//	score(m) = key(m) ^ (1/weight)                     (weight > 0)
//
// Higher score wins. Because the seed depends only on request content, the
// same request always selects the same model; across varying requests the
// selection frequency approaches the weight distribution.
func weightedRandomKey(seed uint64, model string, weight int) float64 {
	if weight <= 0 {
		weight = 1
	}
	h := fnv.New64a()
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(seed >> (8 * (7 - i)))
	}
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(model))
	key := float64(h.Sum64()) / float64(math.MaxUint64)
	return math.Pow(key, 1.0/float64(weight))
}

// metaSet lazily creates the response metadata map and sets one key.
func metaSet(resp *CompletionResponse, key, value string) {
	if resp == nil {
		return
	}
	if resp.Metadata == nil {
		resp.Metadata = make(map[string]string, 4)
	}
	resp.Metadata[key] = value
}

// compile-time check
var _ Provider = (*PolicyProvider)(nil)
