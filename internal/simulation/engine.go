package simulation

// engine.go — the round-based simulation engine (issue #133).
//
// Responsibilities, mapped to the issue:
//
//   - Configurable rounds:          SimConfig.Rounds, [1,MaxRounds].
//   - Per-round action tracking + metrics: RoundRecord{Actions,Metrics,State}.
//   - Activity injection:           SimConfig.Activity — external events at
//     specific rounds, delivered to environments implementing the optional
//     EventInjector interface BEFORE Step (keeps the pinned
//     SimulationEnvironment signature clean).
//   - Deterministic seeds:          all randomness derives from SimConfig.Seed
//     via *rand.Rand (per-round per-agent child streams); injectable for
//     tests through EngineConfig.Rand.
//   - Timezone-aware activity multipliers: multiplierFor maps the LOCAL hour
//     (SimConfig.Location, IANA; "" = UTC) to a participation multiplier;
//     the clock is injectable (Clock) so tests pin instants.
//   - Emergence heuristics:         computeEmergence (documented formulas,
//     each note restates its formula + raw counts in Detail).
//
// Determinism contract: same SimConfig (seed, rounds, agents, activity,
// params) + same clock => byte-identical SimulationResult. Every stochastic
// choice flows through rand streams derived from the seed in a fixed order
// (round -> agent index -> gate draw -> verb draw -> target draw), and every
// map iteration is over sorted keys.

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	// tzdata guarantees time.LoadLocation works in scratch/distroless
	// containers even when a binary imports only this package (main.go
	// embeds it too; harmless duplication).
	_ "time/tzdata"
)

// Execution bounds (mirrors the evaluations package' bounded-execution
// contract: one simulation must never monopolize the process).
const (
	// MaxRounds caps a single run.
	MaxRounds = 500
	// MaxAgents caps the cast of a single run.
	MaxAgents = 64
)

// SimConfig is the wire-level configuration for one simulation (HTTP body).
type SimConfig struct {
	OrgID    string            `json:"org_id"` // stamped from auth claims server-side
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`     // social | market | network | custom
	Rounds   int               `json:"rounds"`   // [1,MaxRounds]
	Seed     int64             `json:"seed"`     // determinism anchor
	Location string            `json:"location"` // IANA tz for activity multipliers ("" = UTC)
	Agents   []SimAgentPersona `json:"agents"`
	Activity []ActivityEvent   `json:"activity,omitempty"`
	Params   map[string]string `json:"params,omitempty"` // adapter tuning (feed_depth, symbols, ...)
}

// loadLocation wraps time.LoadLocation (the tzdata import guarantees IANA
// databases in static builds).
func loadLocation(name string) (*time.Location, error) { return time.LoadLocation(name) }

// activityMultiplierByHour is the documented piecewise local-hour table:
//
//	00–05 night trough 0.4 · 06–09 morning ramp 1.0 · 10–13 midday 0.8
//	14–17 afternoon 1.0 · 18–22 evening peak 1.3 · 23 wind-down 0.7
//
// A round's multiplier = table[local hour at round start]. With a real
// clock a short run usually lands in one bucket (documented granularity);
// inject a SteppedClock to sweep buckets deterministically.
var activityMultiplierByHour = [24]float64{
	0: 0.4, 1: 0.4, 2: 0.4, 3: 0.4, 4: 0.4, 5: 0.4,
	6: 1.0, 7: 1.0, 8: 1.0, 9: 1.0,
	10: 0.8, 11: 0.8, 12: 0.8, 13: 0.8,
	14: 1.0, 15: 1.0, 16: 1.0, 17: 1.0,
	18: 1.3, 19: 1.3, 20: 1.3, 21: 1.3, 22: 1.3,
	23: 0.7,
}

// multiplierFor maps an instant + location to the participation multiplier.
func multiplierFor(now time.Time, loc *time.Location) float64 {
	return activityMultiplierByHour[now.In(loc).Hour()]
}

// gateProbability folds the persona frequency with the round multiplier:
// the agent participates iff draw < Frequency × multiplier.
func gateProbability(beh PlatformBehavior, multiplier float64) float64 {
	f := 1.0
	if beh.Frequency != nil {
		f = *beh.Frequency
	}
	p := f * multiplier
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// EngineConfig is the engine-side configuration (SimConfig plus the
// injectable seams). Runner converts one into the other; direct Engine
// construction is available to embedders and tests.
type EngineConfig struct {
	Config SimConfig
	// Clock is injectable; nil => RealClock.
	Clock Clock
	// Rand is injectable; nil => rand.New(rand.NewSource(Seed)).
	Rand *rand.Rand
	// Registry resolves kind -> environment/agent factories; nil => NewRegistry().
	Registry *Registry
}

// Engine executes one simulation run over a registered environment.
type Engine struct {
	cfg    EngineConfig
	reg    *Registry
	env    SimulationEnvironment
	agents []SimAgent
	rnd    *rand.Rand
	clock  Clock
	loc    *time.Location
	events map[int][]ActivityEvent
	rounds int
}

// NewEngine validates cfg and wires the world for a single run.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	reg := cfg.Registry
	if reg == nil {
		reg = NewRegistry()
	}
	if err := ValidateConfig(cfg.Config, reg); err != nil {
		return nil, err
	}
	env, err := reg.NewEnvironment(cfg.Config.Kind)
	if err != nil {
		return nil, err
	}
	agents := make([]SimAgent, 0, len(cfg.Config.Agents))
	for _, p := range cfg.Config.Agents {
		agents = append(agents, reg.NewAgent(cfg.Config.Kind, p))
	}
	loc := time.UTC
	if cfg.Config.Location != "" {
		loc, err = loadLocation(cfg.Config.Location)
		if err != nil {
			return nil, fmt.Errorf("%w: location %q is not a valid IANA timezone", ErrInvalid, cfg.Config.Location)
		}
	}
	rnd := cfg.Rand
	if rnd == nil {
		rnd = rand.New(rand.NewSource(cfg.Config.Seed))
	}
	clock := cfg.Clock
	if clock == nil {
		clock = RealClock{}
	}
	events := make(map[int][]ActivityEvent)
	for _, ev := range cfg.Config.Activity {
		events[ev.Round] = append(events[ev.Round], ev)
	}
	return &Engine{
		cfg:    cfg,
		reg:    reg,
		env:    env,
		agents: agents,
		rnd:    rnd,
		clock:  clock,
		loc:    loc,
		events: events,
		rounds: cfg.Config.Rounds,
	}, nil
}

// Run executes all rounds and returns the transcript + emergence notes.
// The environment is always Closed (deferred), even on error paths.
func (e *Engine) Run(ctx context.Context) (*SimulationResult, error) {
	if len(e.agents) == 0 {
		return nil, ErrNoAgents
	}
	started := e.clock.Now()
	if err := e.env.Initialize(ctx, EnvConfig{
		Kind:   e.cfg.Config.Kind,
		OrgID:  e.cfg.Config.OrgID,
		Seed:   e.cfg.Config.Seed,
		Agents: e.cfg.Config.Agents,
		Params: e.cfg.Config.Params,
		Clock:  e.clock,
	}); err != nil {
		return nil, fmt.Errorf("initialize %s environment: %w", e.cfg.Config.Kind, err)
	}
	defer func() { _ = e.env.Close() }()

	rounds := make([]RoundRecord, 0, e.rounds)
	for r := 1; r <= e.rounds; r++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("simulation cancelled at round %d: %w", r, err)
		}
		rec, err := e.runRound(ctx, r)
		if err != nil {
			return nil, fmt.Errorf("round %d: %w", r, err)
		}
		rounds = append(rounds, rec)
	}
	finished := e.clock.Now()
	return &SimulationResult{
		Kind:          e.cfg.Config.Kind,
		Rounds:        rounds,
		EmergentNotes: computeEmergence(e.cfg.Config.Kind, e.agents, rounds),
		StartedAt:     started,
		FinishedAt:    finished,
	}, nil
}

// runRound executes one round: gate agents by the timezone-aware activity
// multiplier, inject the round's external events, Step the world, record.
func (e *Engine) runRound(ctx context.Context, round int) (RoundRecord, error) {
	multiplier := multiplierFor(e.clock.Now(), e.loc)

	// Activity gate: only agents passing the probabilistic participation
	// gate act this round; the draw order is the stable agent order.
	active := make([]SimAgent, 0, len(e.agents))
	for _, ag := range e.agents {
		child := rand.New(rand.NewSource(e.rnd.Int63()))
		if child.Float64() < gateProbability(behaviorFor(ag.Persona(), e.cfg.Config.Kind), multiplier) {
			active = append(active, ag)
		}
	}

	// Activity injection (optional interface discovery — see EventInjector).
	var eventKinds []string
	if len(e.events[round]) > 0 {
		if inj, ok := e.env.(EventInjector); ok {
			for _, ev := range e.events[round] {
				if err := inj.InjectEvent(ctx, ev); err != nil {
					return RoundRecord{}, fmt.Errorf("inject event %q: %w", ev.Kind, err)
				}
				eventKinds = append(eventKinds, ev.Kind)
			}
		}
	}

	results, err := e.env.Step(ctx, active)
	if err != nil {
		return RoundRecord{}, err
	}
	state, err := e.env.GetState(ctx)
	if err != nil {
		return RoundRecord{}, err
	}

	mix := make(map[string]int)
	accepted, rejected := 0, 0
	for _, res := range results {
		mix[res.Action.Kind]++
		if res.Accepted {
			accepted++
		} else {
			rejected++
		}
	}
	return RoundRecord{
		Round:   round,
		Actions: results,
		Metrics: RoundMetrics{
			Round:              round,
			TotalAgents:        len(e.agents),
			ActiveAgents:       len(active),
			Actions:            len(results),
			Accepted:           accepted,
			Rejected:           rejected,
			ActionMix:          mix,
			ActivityMultiplier: multiplier,
			Events:             eventKinds,
		},
		State: state,
	}, nil
}

// --- emergence heuristics (issue #133) -------------------------------------
//
// Each heuristic is a bounded score with a formula restated in Detail.
// They are heuristics by design: cheap, deterministic, explainable signals
// over the transcript — NOT causal claims.

// computeEmergence evaluates the documented heuristics for a finished run.
// interaction_density runs for every kind; the platform-specific ones fire
// only for their kind (echo_chamber: social, spread_rate: network,
// liquidity_imbalance: market).
func computeEmergence(kind string, agents []SimAgent, rounds []RoundRecord) []EmergentNote {
	agentIDs := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentIDs[a.Persona().CoreIdentity.ID] = true
	}

	// interaction_density — accepted, targeted actions per agent-round:
	//
	//      density = |{accepted actions with Target != ""}| / max(1, agents × rounds)
	//
	// Interpretation: ≈1 means every agent acted on a concrete object in
	// every round; ≈0 means the cast mostly idled or was rejected.
	targeted, agentRounds := 0, len(agents)*len(rounds)
	for _, rr := range rounds {
		for _, res := range rr.Actions {
			if res.Accepted && res.Action.Target != "" {
				targeted++
			}
		}
	}
	notes := []EmergentNote{{
		Kind:  "interaction_density",
		Score: round2(float64(targeted) / math.Max(1, float64(agentRounds))),
		Detail: fmt.Sprintf("|{accepted, Target≠\"\"}|=%d / max(1, agents×rounds=%d) → %.3f",
			targeted, agentRounds, round2(float64(targeted)/math.Max(1, float64(agentRounds)))),
	}}

	switch kind {
	case KindSocial:
		notes = append(notes, echoChamberNote(rounds))
	case KindNetwork:
		notes = append(notes, spreadRateNote(rounds, agentIDs))
	case KindMarket:
		notes = append(notes, liquidityImbalanceNote(rounds))
	}
	return notes
}

// echoChamberNote — social only. Agents interacting with their OWN content:
//
//	echo = |{accepted like/comment on own post}| / max(1, |{accepted like/comment}|)
//
// 1.0 = pure self-reinforcement (an echo chamber); 0 = all interaction is
// cross-agent.
func echoChamberNote(rounds []RoundRecord) EmergentNote {
	self, total := 0, 0
	for _, rr := range rounds {
		for _, res := range rr.Actions {
			if !res.Accepted || (res.Action.Kind != "like" && res.Action.Kind != "comment") {
				continue
			}
			total++
			if strings.HasPrefix(res.Action.Target, res.Action.AgentID+":") {
				self++
			}
		}
	}
	score := round2(float64(self) / math.Max(1, float64(total)))
	return EmergentNote{
		Kind:  "echo_chamber",
		Score: score,
		Detail: fmt.Sprintf("own-post like/comment=%d / max(1, all like/comment=%d) → %.3f (1.0=pure echo chamber)",
			self, total, score),
	}
}

// spreadRateNote — network only. Reach of the message cascade:
//
//	spread = |{unique recipients of accepted send/route}| / max(1, agents-1)
//
// 1.0 = every other agent received at least one message; the heuristic is
// the fraction of the cast the communication graph reached.
func spreadRateNote(rounds []RoundRecord, agentIDs map[string]bool) EmergentNote {
	reached := make(map[string]bool)
	for _, rr := range rounds {
		for _, res := range rr.Actions {
			if res.Accepted && (res.Action.Kind == "send" || res.Action.Kind == "route") {
				if res.Action.Target != "" {
					reached[res.Action.Target] = true
				}
			}
		}
	}
	denom := math.Max(1, float64(len(agentIDs)-1))
	score := round2(float64(len(reached)) / denom)
	return EmergentNote{
		Kind:  "spread_rate",
		Score: score,
		Detail: fmt.Sprintf("unique recipients=%d / max(1, agents-1=%d) → %.3f (1.0=full cast reached)",
			len(reached), int(denom), score),
	}
}

// liquidityImbalanceNote — market only. Resting-order imbalance at the
// FINAL state:
//
//	imbalance = |bids - asks| / max(1, bids + asks)
//
// 1.0 = one-sided book (pure demand or pure supply); 0 = balanced book.
func liquidityImbalanceNote(rounds []RoundRecord) EmergentNote {
	if len(rounds) == 0 {
		return EmergentNote{Kind: "liquidity_imbalance", Score: 0, Detail: "no rounds"}
	}
	facts := rounds[len(rounds)-1].State.Facts
	bids, asks := factInt(facts["resting_bids"]), factInt(facts["resting_asks"])
	score := round2(math.Abs(float64(bids-asks)) / math.Max(1, float64(bids+asks)))
	return EmergentNote{
		Kind:  "liquidity_imbalance",
		Score: score,
		Detail: fmt.Sprintf("final |bids=%d - asks=%d| / max(1, %d) → %.3f (0=balanced book)",
			bids, asks, bids+asks, score),
	}
}

// round2 quantizes scores to 2 decimals (stable presentation).
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// factInt reads a numeric fact tolerating the int/float shapes a snapshot
// can carry (live facts are ints; JSON round-trips produce float64).
func factInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	default:
		return 0
	}
}

// --- Runner -----------------------------------------------------------------

// Runner is the service-facing entry point: it owns the registry and clock
// defaults and turns a SimConfig into a SimulationResult.
type Runner struct {
	reg   *Registry
	clock Clock
}

// RunnerOption customizes a Runner.
type RunnerOption func(*Runner)

// WithRegistry overrides the runner's registry (custom environments).
func WithRegistry(reg *Registry) RunnerOption {
	return func(r *Runner) { r.reg = reg }
}

// WithClock pins the clock (deterministic tests).
func WithClock(c Clock) RunnerOption {
	return func(r *Runner) { r.clock = c }
}

// NewRunner returns a Runner with the built-in registry and RealClock.
func NewRunner(opts ...RunnerOption) *Runner {
	r := &Runner{reg: NewRegistry(), clock: RealClock{}}
	for _, o := range opts {
		o(r)
	}
	return r
}

// RunSimulation validates and executes cfg synchronously. The caller owns
// cancellation: a cancelled ctx aborts at the next round boundary.
func (r *Runner) RunSimulation(ctx context.Context, cfg SimConfig) (*SimulationResult, error) {
	if err := ValidateConfig(cfg, r.reg); err != nil {
		return nil, err
	}
	e, err := NewEngine(EngineConfig{Config: cfg, Registry: r.reg, Clock: r.clock})
	if err != nil {
		return nil, err
	}
	return e.Run(ctx)
}

// Registry exposes the runner's registry (for handler-side validation).
func (r *Runner) Registry() *Registry { return r.reg }

// Compile-time interface checks (fail the build, never the review).
var (
	_ Clock = RealClock{}
	_ Clock = FixedClock{}
	_ Clock = (*SteppedClock)(nil)
)
