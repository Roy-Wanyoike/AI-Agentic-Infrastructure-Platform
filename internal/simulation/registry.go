package simulation

// registry.go — environment + agent factories for issue #133.
//
// Custom worlds plug in WITHOUT touching the engine: register an
// EnvironmentFactory under a kind string and (optionally) an AgentFactory
// for the same kind. Built-ins ("social", "market", "network") are seeded
// by NewRegistry; custom registrations deliberately OVERRIDE same-named
// kinds (documented extension semantics — a deployment may swap the
// market world for its own without forking the engine).
//
// Agents: if no AgentFactory is registered for a kind, the registry falls
// back to genericAgent — a documented, vocabulary-agnostic policy that
// (a) reads its action weights from the persona's PlatformBehavior[kind],
// and (b) targets uniformly over EnvState.Facts["peers"] (a []string of
// candidate target ids the custom environment may expose). With no
// behavior and no peers it idles, which keeps minimal custom worlds
// runnable while never inventing hidden behavior.

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
)

// EnvironmentFactory constructs a fresh, unconfigured environment instance.
type EnvironmentFactory func() SimulationEnvironment

// AgentFactory adapts a persona into the platform's concrete SimAgent.
type AgentFactory func(p SimAgentPersona) SimAgent

// Registry resolves environment kinds to factories. Safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	envs   map[string]EnvironmentFactory
	agents map[string]AgentFactory
}

// NewRegistry returns a registry seeded with the built-in adapters.
func NewRegistry() *Registry {
	r := &Registry{
		envs:   make(map[string]EnvironmentFactory, 4),
		agents: make(map[string]AgentFactory, 4),
	}
	r.envs[KindSocial] = func() SimulationEnvironment { return NewSocialEnvironment() }
	r.agents[KindSocial] = func(p SimAgentPersona) SimAgent { return &SocialAgent{persona: p} }
	r.envs[KindMarket] = func() SimulationEnvironment { return NewMarketEnvironment() }
	r.agents[KindMarket] = func(p SimAgentPersona) SimAgent { return &MarketAgent{persona: p} }
	r.envs[KindNetwork] = func() SimulationEnvironment { return NewNetworkEnvironment() }
	r.agents[KindNetwork] = func(p SimAgentPersona) SimAgent { return &NetworkAgent{persona: p} }
	return r
}

// RegisterEnvironment registers (or overrides) the factory for kind.
// A nil factory or empty kind is rejected with ErrInvalid.
func (r *Registry) RegisterEnvironment(kind string, factory EnvironmentFactory) error {
	if kind == "" || factory == nil {
		return fmt.Errorf("%w: RegisterEnvironment needs a kind and a factory", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envs[kind] = factory
	return nil
}

// RegisterAgentFactory registers (or overrides) the persona->agent adapter
// for kind. When absent, the documented genericAgent fallback is used.
func (r *Registry) RegisterAgentFactory(kind string, factory AgentFactory) error {
	if kind == "" || factory == nil {
		return fmt.Errorf("%w: RegisterAgentFactory needs a kind and a factory", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agents[kind] = factory
	return nil
}

// NewEnvironment constructs an environment for kind.
func (r *Registry) NewEnvironment(kind string) (SimulationEnvironment, error) {
	r.mu.RLock()
	f, ok := r.envs[kind]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return f(), nil
}

// NewAgent constructs the platform agent for a persona. Unknown kinds fall
// back to genericAgent (documented above); only a kind with NO environment
// factory AND no agent factory is an error — but since the engine resolves
// the environment first, a missing agent factory always reaches the fallback.
func (r *Registry) NewAgent(kind string, p SimAgentPersona) SimAgent {
	r.mu.RLock()
	f, ok := r.agents[kind]
	r.mu.RUnlock()
	if !ok {
		return &genericAgent{persona: p, kind: kind}
	}
	return f(p)
}

// HasEnvironment reports whether kind is resolvable.
func (r *Registry) HasEnvironment(kind string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.envs[kind]
	return ok
}

// genericAgent is the documented fallback policy for custom environments.
type genericAgent struct {
	persona SimAgentPersona
	kind    string
}

// Persona implements SimAgent.
func (a *genericAgent) Persona() SimAgentPersona { return a.persona }

// Decide picks a verb from the persona's platform behavior (weighted) and
// a target from view.Facts["peers"] ([]string, uniform). No behavior or no
// peers => an "idle" action the environment rejects as a no-op.
func (a *genericAgent) Decide(_ context.Context, view EnvState, rnd *rand.Rand) Action {
	beh := a.persona.PlatformBehavior[a.kind]
	verb := pickWeighted(beh, nil, rnd)
	if verb == "" {
		return Action{Kind: "idle"}
	}
	peers := stringSlice(view.Facts["peers"])
	if len(peers) == 0 {
		return Action{Kind: verb}
	}
	return Action{Kind: verb, Target: peers[rnd.Intn(len(peers))]}
}

// --- shared decision helpers (used by every adapter's agent) ---------------

// behaviorFor returns the platform overlay for kind (missing => zero value).
func behaviorFor(p SimAgentPersona, kind string) PlatformBehavior { return p.PlatformBehavior[kind] }

// trait reads a persona trait in [0,1], defaulting to 0.5 when absent.
func trait(p SimAgentPersona, key string) float64 {
	v, ok := p.CoreIdentity.Traits[key]
	if !ok {
		return 0.5
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// pickWeighted selects one verb. weights (when non-empty) REPLACE the
// trait-derived defaults entirely; otherwise defaults are used as-is.
// Verbs outside beh.Actions (when set) are excluded. Returns "" when
// nothing is selectable (agent should idle).
func pickWeighted(beh PlatformBehavior, defaults map[string]float64, rnd *rand.Rand) string {
	w := make(map[string]float64, len(defaults))
	if len(beh.Weights) > 0 {
		for k, v := range beh.Weights {
			if v > 0 {
				w[k] = v
			}
		}
	} else {
		for k, v := range defaults {
			w[k] = v
		}
	}
	if len(beh.Actions) > 0 {
		allowed := make(map[string]bool, len(beh.Actions))
		for _, a := range beh.Actions {
			allowed[strings.TrimSpace(a)] = true
		}
		for k := range w {
			if !allowed[k] {
				delete(w, k)
			}
		}
	}
	if len(w) == 0 {
		return ""
	}
	keys := make([]string, 0, len(w))
	total := 0.0
	for _, k := range sortedKeys(w) {
		keys = append(keys, k)
		total += w[k]
	}
	if total <= 0 {
		return keys[0]
	}
	draw := rnd.Float64() * total
	for _, k := range keys {
		draw -= w[k]
		if draw < 0 {
			return k
		}
	}
	return keys[len(keys)-1]
}

// sortedKeys returns the map's keys sorted (determinism under map iteration).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stringSlice coerces an any holding []string (or []any of strings) back.
func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// ValidateConfig enforces the structural bounds of SimConfig against reg.
// Returns ErrInvalid-wrapped errors whose messages are API-safe (422).
func ValidateConfig(cfg SimConfig, reg *Registry) error {
	if cfg.Kind == "" {
		return fmt.Errorf("%w: kind is required", ErrInvalid)
	}
	if !reg.HasEnvironment(cfg.Kind) {
		return fmt.Errorf("%w: unknown environment kind %q", ErrInvalid, cfg.Kind)
	}
	if cfg.Rounds < 1 || cfg.Rounds > MaxRounds {
		return fmt.Errorf("%w: rounds must be in [1,%d], got %d", ErrInvalid, MaxRounds, cfg.Rounds)
	}
	if len(cfg.Agents) < 1 || len(cfg.Agents) > MaxAgents {
		return fmt.Errorf("%w: agents must be in [1,%d], got %d", ErrInvalid, MaxAgents, len(cfg.Agents))
	}
	seen := make(map[string]bool, len(cfg.Agents))
	for i, a := range cfg.Agents {
		if a.CoreIdentity.ID == "" {
			return fmt.Errorf("%w: agents[%d].core_identity.id is required", ErrInvalid, i)
		}
		if seen[a.CoreIdentity.ID] {
			return fmt.Errorf("%w: duplicate agent id %q", ErrInvalid, a.CoreIdentity.ID)
		}
		seen[a.CoreIdentity.ID] = true
		for plat, beh := range a.PlatformBehavior {
			if beh.Frequency != nil && (*beh.Frequency < 0 || *beh.Frequency > 1) {
				return fmt.Errorf("%w: agents[%d] platform %q frequency must be in [0,1]", ErrInvalid, i, plat)
			}
		}
	}
	for i, ev := range cfg.Activity {
		if ev.Round < 1 || ev.Round > cfg.Rounds {
			return fmt.Errorf("%w: activity[%d].round must be in [1,%d], got %d", ErrInvalid, i, cfg.Rounds, ev.Round)
		}
		if ev.Kind == "" {
			return fmt.Errorf("%w: activity[%d].kind is required", ErrInvalid, i)
		}
		if ev.Platform != "" && ev.Platform != cfg.Kind {
			return fmt.Errorf("%w: activity[%d].platform %q does not match run kind %q", ErrInvalid, i, ev.Platform, cfg.Kind)
		}
	}
	if cfg.Location != "" {
		if _, err := loadLocation(cfg.Location); err != nil {
			return fmt.Errorf("%w: location %q is not a valid IANA timezone", ErrInvalid, cfg.Location)
		}
	}
	return nil
}
