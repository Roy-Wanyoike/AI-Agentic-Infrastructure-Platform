package simulation

// types.go — issue #133 (dual-platform simulation engine): shared contract
// for cross-platform agent behavior modeling.
//
// The premise of issue #133: simulated agents share ONE core identity but
// EXHIBIT platform-specific behavior. A persona that is extroverted on the
// "social" platform may be risk-averse on the "market" platform — the core
// identity is shared, the rendered behavior diverges per platform. The
// contract below encodes exactly that split:
//
//      SimAgentPersona = CoreIdentity (shared) + PlatformBehavior map (per-kind)
//
// COUPLING NOTE (deliberate, issue #133): CoreIdentity is a LIGHTWEIGHT,
// Persona-SHAPED struct defined here — this package does NOT import
// internal/synth. Coupling the simulation engine to the synthetic-assurance
// registry would (a) drag the registry's store/verification machinery into
// every simulation, and (b) freeze two tracks to one artifact's release
// cadence. The field mapping is documented on CoreIdentity so a synth
// persona can be projected onto a sim agent with a trivial, explicit copy
// at the call site (synth.Persona -> CoreIdentity):
//
//      synth.Persona.ID/OrgID/Name/RoleHints/Constraints -> same-named fields
//      (synth has no trait vocabulary; Traits are simulation-local additions).
//
// Dual-mode house style: the simulation RUNTIME is in-memory by design in
// BOTH API modes this round (no simulation migration is assigned — same
// documented scope decision as internal/orchestration). Determinism is a
// first-class contract: every stochastic surface (action selection, gate
// sampling, target choice) flows through injectable randomness
// (*rand.Rand derived from SimConfig.Seed) and injectable time (Clock), so
// the same config + seed + fixed clock reproduce byte-identical results.

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// Contract-wide errors (wrap with %w for context; never shadow).
var (
	// ErrNotFound is returned when a simulation record does not exist
	// within the caller's organization (tenant guard: foreign ids are
	// "not found" — no existence leak across tenants).
	ErrNotFound = errors.New("simulation: not found")
	// ErrInvalid is returned when a simulation config fails validation
	// (bounds, unknown kinds, structural integrity). The message is
	// safe to surface to API callers (422).
	ErrInvalid = errors.New("simulation: invalid")
	// ErrUnknownKind is returned when an environment kind has no
	// registered factory in the active Registry.
	ErrUnknownKind = errors.New("simulation: unknown environment kind")
	// ErrNoAgents is returned when a config carries no agent personas.
	ErrNoAgents = errors.New("simulation: no agents")
)

// Simulation status lifecycle (the HTTP surface exposes these verbatim).
const (
	StatusQueued   = "queued"  // created, async run not started yet
	StatusRunning  = "running" // engine executing rounds
	StatusComplete = "completed"
	StatusFailed   = "failed"
)

// Built-in environment kinds (adapters in social.go / market.go / network.go).
const (
	KindSocial  = "social"  // post/like/follow/comment over a shared feed
	KindMarket  = "market"  // buy/sell/bid against a price-time order book
	KindNetwork = "network" // send/receive/route over a message graph
)

// CoreIdentity is the platform-independent half of a simulated agent: who
// the agent IS, independent of where it acts. It is intentionally shaped
// like (but decoupled from) internal/synth.Persona — see the file header
// for the projection mapping and the rationale.
type CoreIdentity struct {
	ID          string            `json:"id"`          // stable within the simulation
	OrgID       string            `json:"org_id"`      // tenant scope (echoed from SimConfig)
	Name        string            `json:"name"`        // human label
	RoleHints   []string          `json:"role_hints"`  // e.g. "power_user", "lurker"
	Constraints map[string]string `json:"constraints"` // e.g. "network=slow3g"
	// Traits are numeric propensities in [0,1] (missing -> 0.5 neutral).
	// Documented keys consumed by the built-in adapters:
	//   extraversion   — initiating behavior (post/send)
	//   agreeableness  — reciprocal behavior (like/receive)
	//   openness       — exploratory behavior (follow/route, novel peers)
	//   risk_appetite  — market aggression (buy vs sell)
	Traits map[string]float64 `json:"traits,omitempty"`
}

// PlatformBehavior is the per-platform overlay: how the persona ACTS on one
// platform. The SAME CoreIdentity with different overlays must produce
// observably different action mixes (the divergence contract, tested in
// engine_test.go).
type PlatformBehavior struct {
	// Actions optionally restricts the vocabulary this persona uses on the
	// platform (subset of the adapter's action verbs, e.g. {"post","like"}).
	// Empty = full default vocabulary. Unknown verbs are rejected by the
	// environment at apply time ("unknown_action").
	Actions []string `json:"actions,omitempty"`
	// Frequency gates participation per round: the agent attempts an action
	// only if a uniform draw < Frequency × activity_multiplier (round,
	// timezone and event dependent). nil = always active (1.0); clamped to
	// [0,1] by validation.
	Frequency *float64 `json:"frequency,omitempty"`
	// Weights optionally pins the per-action selection weights (normalized
	// internally). When non-empty these REPLACE the trait-derived defaults
	// for the listed verbs (explicit overlay wins; unlisted verbs keep
	// their default weight).
	Weights map[string]float64 `json:"weights,omitempty"`
}

// SimAgentPersona is one simulated agent: shared identity + platform overlays.
type SimAgentPersona struct {
	CoreIdentity     CoreIdentity                `json:"core_identity"`
	PlatformBehavior map[string]PlatformBehavior `json:"platform_behavior"` // key = environment kind
}

// Action is one atomic agent decision in a platform vocabulary. Agents
// propose; environments dispose (ActionResult).
type Action struct {
	AgentID string            `json:"agent_id"` // stamped by the environment
	Kind    string            `json:"kind"`     // platform verb, e.g. "post", "buy", "send"
	Target  string            `json:"target"`   // object id (post, symbol, peer, message)
	Payload map[string]string `json:"payload,omitempty"`
}

// ActionResult is the environment's verdict on one attempted action.
type ActionResult struct {
	Action   Action    `json:"action"`
	Accepted bool      `json:"accepted"`
	Reason   string    `json:"reason,omitempty"` // rejection class, e.g. "no_liquidity"
	Detail   string    `json:"detail,omitempty"` // human-readable outcome, e.g. "filled 2/3 @ 100.50"
	At       time.Time `json:"at"`               // engine clock at apply time
}

// EnvConfig is the environment-side configuration handed to
// SimulationEnvironment.Initialize. Params keys are adapter-defined and
// documented on each adapter (e.g. social "feed_depth", market "symbols").
type EnvConfig struct {
	Kind   string            `json:"kind"`
	OrgID  string            `json:"org_id"`
	Seed   int64             `json:"seed"`
	Agents []SimAgentPersona `json:"agents"`
	Params map[string]string `json:"params,omitempty"`
	// Clock is the run's injectable time source (engine-provided). Adapters
	// stamp ActionResult.At from it so transcripts stay deterministic under
	// a fixed clock; nil is tolerated (RealClock fallback).
	Clock Clock `json:"-"`
}

// EnvState is a point-in-time SNAPSHOT of the observable environment.
// Facts are JSON-serializable, adapter-defined, and must be safe to hand to
// agents ( Decide) and to API responses alike. Snapshots are copies —
// mutating one never leaks into adapter internals.
type EnvState struct {
	Kind     string         `json:"kind"`
	Round    int            `json:"round"`
	AgentIDs []string       `json:"agent_ids"` // sorted
	Facts    map[string]any `json:"facts"`
}

// SimulationEnvironment is the pinned seam between the round-based engine
// and a platform world. Implementations own their in-memory state, consume
// the platform's small deterministic action vocabulary, and render snapshots.
type SimulationEnvironment interface {
	// Initialize prepares (or resets) the world for one simulation run.
	Initialize(ctx context.Context, cfg EnvConfig) error
	// Step executes one round: each listed agent proposes exactly one
	// action (Decide), which the environment applies in list order. The
	// returned results align 1:1 with the agents argument.
	Step(ctx context.Context, agents []SimAgent) ([]ActionResult, error)
	// GetState renders the current snapshot (copy semantics).
	GetState(ctx context.Context) (EnvState, error)
	// Close releases the world (adapters: no-op; custom worlds: free).
	Close() error
}

// EventInjector is an OPTIONAL extension interface: environments that can
// absorb external events implement it. The engine discovers it by type
// assertion (the pinned SimulationEnvironment signature stays clean) and
// delivers the round's scheduled events BEFORE calling Step. Events are
// the "activity injection" contract of issue #133: external shocks
// (trending topics, price shocks, viral messages) applied at specific
// rounds so runs can be steered without bespoke configs.
type EventInjector interface {
	InjectEvent(ctx context.Context, ev ActivityEvent) error
}

// SimAgent is one participant inside an environment. Concrete agents are
// produced per platform by AgentFactory registrations (social.go et al);
// a documented generic fallback exists for custom environments (registry.go).
type SimAgent interface {
	// Persona exposes the shared identity + platform overlays.
	Persona() SimAgentPersona
	// Decide returns the single action the agent takes this round. rnd is
	// the per-agent deterministic stream (engine/env derived); view is the
	// environment snapshot to reason over. Implementations MUST be
	// side-effect free — the environment owns all state mutation.
	Decide(ctx context.Context, view EnvState, rnd *rand.Rand) Action
}

// ActivityEvent is an external event injected at a specific round.
// Platform-specific payloads are documented on each adapter:
//
//	social  kind "trending_topic"  payload {"topic": string}
//	market  kind "price_shock"     payload {"symbol","pct"}   (pct may be negative)
//	network kind "viral_message"    payload {"topic": string}
type ActivityEvent struct {
	Round    int               `json:"round"`              // 1-based; must be within [1,Rounds]
	Platform string            `json:"platform,omitempty"` // "", or the run kind
	Kind     string            `json:"kind"`
	Payload  map[string]string `json:"payload,omitempty"`
}

// RoundMetrics is the per-round quantitative footprint (issue #133:
// per-round action tracking + metrics).
type RoundMetrics struct {
	Round        int            `json:"round"`
	TotalAgents  int            `json:"total_agents"`
	ActiveAgents int            `json:"active_agents"` // passed the activity gate
	Actions      int            `json:"actions"`       // attempted (= len(ActionResults))
	Accepted     int            `json:"accepted"`
	Rejected     int            `json:"rejected"`
	ActionMix    map[string]int `json:"action_mix"` // verb -> attempted count
	// ActivityMultiplier is the timezone-aware participation multiplier in
	// force this round (engine.go multiplierFor — documented piecewise
	// table over the local hour in the configured location).
	ActivityMultiplier float64  `json:"activity_multiplier"`
	Events             []string `json:"events,omitempty"` // external event kinds applied this round
}

// RoundRecord is the full transcript of one round: what happened, the
// metrics, and the post-round world snapshot.
type RoundRecord struct {
	Round   int            `json:"round"`
	Actions []ActionResult `json:"actions"`
	Metrics RoundMetrics   `json:"metrics"`
	State   EnvState       `json:"state"`
}

// EmergentNote is one emergence heuristic evaluated over the completed run.
// Kind is one of the documented heuristics on engine.go
// (interaction_density, echo_chamber, spread_rate, liquidity_imbalance);
// Detail always restates the formula and the raw counts so a score is
// never presented without its derivation.
type EmergentNote struct {
	Kind   string  `json:"kind"`
	Score  float64 `json:"score"`
	Detail string  `json:"detail"`
}

// SimulationResult is the terminal product of a run (issue #133):
// per-round transcripts + emergence heuristics.
type SimulationResult struct {
	Kind          string         `json:"kind"`
	Rounds        []RoundRecord  `json:"rounds"`
	EmergentNotes []EmergentNote `json:"emergent_notes"`
	StartedAt     time.Time      `json:"started_at"`
	FinishedAt    time.Time      `json:"finished_at"`
}

// Clock is the injectable time source (deterministic tests pin a fixed or
// stepped implementation; production uses RealClock).
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock (wall time, UTC-agnostic — the engine
// converts to the configured location).
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// FixedClock always reports the same instant (deterministic tests).
type FixedClock struct{ T time.Time }

// Now implements Clock.
func (c FixedClock) Now() time.Time { return c.T }

// SteppedClock advances by Step on every Now call — useful when a test
// wants each round to observe a different local hour deterministically.
type SteppedClock struct {
	T    time.Time
	Step time.Duration
}

// Now implements Clock (advances then reports — the first call observes T+Step).
func (c *SteppedClock) Now() time.Time {
	c.T = c.T.Add(c.Step)
	return c.T
}

// NewSteppedClock returns a SteppedClock starting at t.
func NewSteppedClock(t time.Time, step time.Duration) *SteppedClock {
	return &SteppedClock{T: t, Step: step}
}
