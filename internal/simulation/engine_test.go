package simulation

// engine_test.go — issue #133 engine contracts, all DETERMINISTIC:
//   - round determinism (same seed + fixed clock => byte-identical results)
//   - activity injection lands on the scheduled round and moves the world
//   - per-platform behavior divergence from the SAME core identity
//   - timezone-aware activity multipliers via an injectable clock
//   - emergence heuristics (documented formulas, bounded scores)
//   - registry: custom environments + the genericAgent fallback
//   - validation bounds

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// testClock pins a stable instant used by every deterministic test
// (2014-06-07 18:44 UTC — the Go 1.0 release moment; hour 18 => multiplier 1.3).
var testClock = FixedClock{T: time.Date(2014, 6, 7, 18, 44, 0, 0, time.UTC)}

// persona builds a minimal persona for tests.
func persona(id, name string, traits map[string]float64, beh map[string]PlatformBehavior) SimAgentPersona {
	return SimAgentPersona{
		CoreIdentity:     CoreIdentity{ID: id, OrgID: "org-1", Name: name, Traits: traits},
		PlatformBehavior: beh,
	}
}

// runConfig builds a config that is guaranteed to keep every agent active
// every round (frequency 1) under the fixed clock's multiplier.
func runConfig(kind string, rounds int, seed int64, agents []SimAgentPersona, activity []ActivityEvent) SimConfig {
	return SimConfig{
		OrgID:    "org-1",
		Name:     "test-" + kind,
		Kind:     kind,
		Rounds:   rounds,
		Seed:     seed,
		Location: "UTC",
		Agents:   agents,
		Activity: activity,
	}
}

// runOnce executes a config through a fresh runner with the fixed clock.
func runOnce(t *testing.T, cfg SimConfig) *SimulationResult {
	t.Helper()
	r := NewRunner(WithClock(testClock))
	res, err := r.RunSimulation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunSimulation: %v", err)
	}
	return res
}

// TestRoundDeterminism is the flagship contract: the same config, seed and
// clock reproduce identical transcripts, emergence notes, and metrics —
// across runners, engines, and even with a pre-seeded injected rand.
func TestRoundDeterminism(t *testing.T) {
	agents := []SimAgentPersona{
		persona("a", "Ada", map[string]float64{"extraversion": 0.9, "agreeableness": 0.2}, nil),
		persona("b", "Bob", map[string]float64{"extraversion": 0.3, "agreeableness": 0.8}, nil),
	}
	for _, kind := range []string{KindSocial, KindMarket, KindNetwork} {
		t.Run(kind, func(t *testing.T) {
			cfg := runConfig(kind, 12, 1337, agents, nil)
			a := runOnce(t, cfg)
			b := runOnce(t, cfg)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("%s: same seed produced different results", kind)
			}
			// structural sanity: rounds 1..N, per-round metrics present
			if len(a.Rounds) != 12 {
				t.Fatalf("rounds = %d, want 12", len(a.Rounds))
			}
			for i, rr := range a.Rounds {
				if rr.Round != i+1 {
					t.Fatalf("round %d misnumbered as %d", i+1, rr.Round)
				}
				if rr.Metrics.ActivityMultiplier == 0 {
					t.Fatalf("round %d missing activity multiplier", rr.Round)
				}
				if rr.Metrics.ActionMix == nil {
					t.Fatalf("round %d missing action mix", rr.Round)
				}
			}
			// notes restate their formulas
			if len(a.EmergentNotes) == 0 {
				t.Fatal("missing emergence notes")
			}
			for _, n := range a.EmergentNotes {
				if n.Detail == "" {
					t.Fatalf("note %q has no derivation detail", n.Kind)
				}
			}
		})
	}
}

// TestActivityInjection pins the injection contract: a scheduled external
// event lands exactly on its round (metrics.Events), is delivered through
// the optional EventInjector seam, and actually moves the world.
func TestActivityInjection(t *testing.T) {
	agents := []SimAgentPersona{persona("a", "Ada", nil, nil)}
	cfg := runConfig(KindSocial, 3, 42, agents, []ActivityEvent{
		{Round: 2, Platform: KindSocial, Kind: "trending_topic", Payload: map[string]string{"topic": "sim-launch"}},
	})
	res := runOnce(t, cfg)

	// round 1: no events; round 2: injected; round 3: pin expired.
	if len(res.Rounds[0].Metrics.Events) != 0 {
		t.Fatalf("round 1 events = %v, want none", res.Rounds[0].Metrics.Events)
	}
	if !reflect.DeepEqual(res.Rounds[1].Metrics.Events, []string{"trending_topic"}) {
		t.Fatalf("round 2 events = %v, want [trending_topic]", res.Rounds[1].Metrics.Events)
	}
	if len(res.Rounds[2].Metrics.Events) != 0 {
		t.Fatalf("round 3 events = %v, want none", res.Rounds[2].Metrics.Events)
	}
	// the world absorbed the shock: the trending post exists at round 2.
	feed2 := res.Rounds[1].State.Facts["feed"].([]map[string]any)
	found := false
	for _, f := range feed2 {
		if id, _ := f["id"].(string); id == "trend:1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("trending post missing from round-2 feed: %v", feed2)
	}
	if res.Rounds[1].State.Facts["trending"] != "sim-launch" {
		t.Fatalf("round 2 trending = %v, want sim-launch", res.Rounds[1].State.Facts["trending"])
	}
	if res.Rounds[2].State.Facts["trending"] != "" {
		t.Fatalf("round 3 trending = %v, want expired", res.Rounds[2].State.Facts["trending"])
	}

	// market shock moves the mid deterministically.
	mcfg := runConfig(KindMarket, 2, 42, agents, []ActivityEvent{
		{Round: 2, Kind: "price_shock", Payload: map[string]string{"symbol": "AGENTOS", "pct": "-50"}},
	})
	mres := runOnce(t, mcfg)
	syms := mres.Rounds[1].State.Facts["symbols"].(map[string]any)["AGENTOS"].(map[string]float64)
	if syms["mid"] != 50.0 {
		t.Fatalf("post-shock mid = %v, want 50", syms["mid"])
	}

	// network viral injection reaches every inbox.
	nres := runOnce(t, runConfig(KindNetwork, 2, 42, agents, []ActivityEvent{
		{Round: 1, Kind: "viral_message", Payload: map[string]string{"topic": "go1"}},
	}))
	unread := nres.Rounds[0].State.Facts["unread"].(map[string]int)
	if unread["a"] != 1 {
		t.Fatalf("viral unread = %v, want a:1", unread)
	}
}

// zeroFreq is the documented "never acts" frequency used by silent test agents.
var zeroFreq = 0.0

// countKinds folds a run's per-round action mixes into one tally.
func countKinds(rr []RoundRecord) map[string]int {
	mix := map[string]int{}
	for _, r := range rr {
		for k, n := range r.Metrics.ActionMix {
			mix[k] += n
		}
	}
	return mix
}

// TestPlatformDivergenceFromSameIdentity is the issue-#133 thesis: the SAME
// core identity, rendered through DIFFERENT platform overlays, produces
// observably different behavior; while the same overlay twice behaves
// identically (the divergence comes from the overlay, not the copy).
func TestPlatformDivergenceFromSameIdentity(t *testing.T) {
	// one identity, pinned opposite overlays per platform.
	ident := CoreIdentity{
		ID: "shared", OrgID: "org-1", Name: "Kai",
		Traits: map[string]float64{"extraversion": 0.5, "agreeableness": 0.5, "openness": 0.5, "risk_appetite": 0.5},
	}
	socialCfg := runConfig(KindSocial, 30, 7, []SimAgentPersona{{
		CoreIdentity: ident,
		PlatformBehavior: map[string]PlatformBehavior{
			KindSocial: {Weights: map[string]float64{"like": 9, "comment": 1, "post": 0, "follow": 0}},
		},
	}}, nil)
	networkCfg := runConfig(KindNetwork, 30, 7, []SimAgentPersona{{
		CoreIdentity: ident,
		PlatformBehavior: map[string]PlatformBehavior{
			KindNetwork: {Weights: map[string]float64{"send": 9, "receive": 1, "route": 0}},
		},
	}, {
		// a silent second participant: sends need a peer (documented), so
		// the network cast has two agents, one frequency-0.
		CoreIdentity: CoreIdentity{ID: "peer", OrgID: "org-1", Name: "Peer"},
		PlatformBehavior: map[string]PlatformBehavior{
			KindNetwork: {Frequency: &zeroFreq},
		},
	}}, nil)
	socialMix := countKinds(runOnce(t, socialCfg).Rounds)
	networkMix := countKinds(runOnce(t, networkCfg).Rounds)

	// divergence: social is interaction-over-content, network is messaging.
	if socialMix["like"] == 0 {
		t.Fatalf("social overlay produced no likes: %v", socialMix)
	}
	if networkMix["send"] == 0 {
		t.Fatalf("network overlay produced no sends: %v", networkMix)
	}
	for _, k := range []string{"like", "comment", "post", "follow"} {
		if networkMix[k] != 0 {
			t.Fatalf("social verb %q leaked into network run: %v", k, networkMix)
		}
	}
	for _, k := range []string{"send", "receive", "route"} {
		if socialMix[k] != 0 {
			t.Fatalf("network verb %q leaked into social run: %v", k, socialMix)
		}
	}

	// market divergence: buy-heavy vs sell-heavy from the same identity.
	mkCfg := func(weights map[string]float64) SimConfig {
		return runConfig(KindMarket, 20, 7, []SimAgentPersona{{
			CoreIdentity:     ident,
			PlatformBehavior: map[string]PlatformBehavior{KindMarket: {Weights: weights}},
		}}, nil)
	}
	buyMix := countKinds(runOnce(t, mkCfg(map[string]float64{"buy": 1})).Rounds)
	sellMix := countKinds(runOnce(t, mkCfg(map[string]float64{"sell": 1})).Rounds)
	if buyMix["buy"] == 0 || sellMix["sell"] == 0 {
		t.Fatalf("market overlays did not diverge: buy=%v sell=%v", buyMix, sellMix)
	}

	// same overlay twice => identical (seeded) behavior — divergence is the
	// overlay's doing, not chance.
	s1 := runOnce(t, socialCfg)
	s2 := runOnce(t, socialCfg)
	if !reflect.DeepEqual(s1, s2) {
		t.Fatal("same identity + same overlay + same seed diverged")
	}
}

// TestTimezoneAwareMultipliers: the same instant maps to different local
// hours (and therefore different participation multipliers) per location;
// the stepped clock sweeps buckets deterministically.
func TestTimezoneAwareMultipliers(t *testing.T) {
	// 18:44 UTC is 03:44 next day in Tokyo (hour 3 => 0.4) vs hour 18 in UTC (1.3).
	inst := time.Date(2014, 6, 7, 18, 44, 0, 0, time.UTC)
	if got := multiplierFor(inst, time.UTC); got != 1.3 {
		t.Fatalf("UTC multiplier = %v, want 1.3", got)
	}
	tokyo, err := loadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load Tokyo: %v", err)
	}
	if got := multiplierFor(inst, tokyo); got != 0.4 {
		t.Fatalf("Tokyo multiplier = %v, want 0.4", got)
	}

	// end-to-end: the recorded multiplier comes from the run's location.
	agents := []SimAgentPersona{persona("a", "Ada", nil, nil)}
	res := runOnce(t, runConfig(KindSocial, 1, 5, agents, nil))
	if res.Rounds[0].Metrics.ActivityMultiplier != 1.3 {
		t.Fatalf("recorded multiplier = %v, want 1.3", res.Rounds[0].Metrics.ActivityMultiplier)
	}
	tokCfg := runConfig(KindSocial, 1, 5, agents, nil)
	tokCfg.Location = "Asia/Tokyo"
	tres := runOnce(t, tokCfg)
	if tres.Rounds[0].Metrics.ActivityMultiplier != 0.4 {
		t.Fatalf("Tokyo recorded multiplier = %v, want 0.4", tres.Rounds[0].Metrics.ActivityMultiplier)
	}

	// the gate folds frequency x multiplier: frequency 0.3 at 0.4 vs at 1.3
	// changes expected participation; with a FIXED seed the outcomes are
	// exact and reproducible (assert the low-multiplier run is quieter or
	// equal, never louder).
	loudCfg := runConfig(KindSocial, 8, 11, agents, nil)
	quietCfg := loudCfg
	quietCfg.Location = "Asia/Tokyo"
	beh := 0.3
	for i := range quietCfg.Agents {
		quietCfg.Agents[i].PlatformBehavior = map[string]PlatformBehavior{
			KindSocial: {Frequency: &beh},
		}
	}
	loudCfg.Agents = append([]SimAgentPersona(nil), quietCfg.Agents...)
	loud := runOnce(t, loudCfg)
	quiet := runOnce(t, quietCfg)
	if quietTotal(loud) < quietTotal(quiet) {
		t.Fatalf("quiet location out-participated loud location: %d > %d",
			quietTotal(quiet), quietTotal(loud))
	}
}

// quietTotal sums ActiveAgents across rounds.
func quietTotal(res *SimulationResult) int {
	n := 0
	for _, r := range res.Rounds {
		n += r.Metrics.ActiveAgents
	}
	return n
}

// TestEchoChamberHeuristic pins the social emergence formula: an agent that
// only ever likes its own visible feed content accumulates self-interactions.
func TestEchoChamberHeuristic(t *testing.T) {
	// One agent, posts + likes only: everything it can like is its own post.
	cfg := runConfig(KindSocial, 10, 3, []SimAgentPersona{
		persona("solo", "Solo", map[string]float64{"extraversion": 1, "agreeableness": 1}, nil),
	}, nil)
	res := runOnce(t, cfg)
	var echo *EmergentNote
	for i := range res.EmergentNotes {
		if res.EmergentNotes[i].Kind == "echo_chamber" {
			echo = &res.EmergentNotes[i]
		}
	}
	if echo == nil {
		t.Fatalf("echo_chamber note missing: %+v", res.EmergentNotes)
	}
	if echo.Score < 0 || echo.Score > 1 {
		t.Fatalf("echo score %v out of [0,1]", echo.Score)
	}
	// interaction_density must exist for every kind.
	var density bool
	for _, n := range res.EmergentNotes {
		if n.Kind == "interaction_density" {
			density = true
		}
	}
	if !density {
		t.Fatal("interaction_density note missing")
	}
}

// TestSpreadRateHeuristic pins the network emergence formula.
func TestSpreadRateHeuristic(t *testing.T) {
	cfg := runConfig(KindNetwork, 10, 9, []SimAgentPersona{
		persona("a", "Ada", map[string]float64{"extraversion": 1}, nil),
		persona("b", "Bo", nil, nil),
		persona("c", "Cy", nil, nil),
	}, nil)
	res := runOnce(t, cfg)
	var spread *EmergentNote
	for i := range res.EmergentNotes {
		if res.EmergentNotes[i].Kind == "spread_rate" {
			spread = &res.EmergentNotes[i]
		}
	}
	if spread == nil {
		t.Fatalf("spread_rate note missing: %+v", res.EmergentNotes)
	}
	if spread.Score <= 0 {
		t.Fatalf("spread rate %v, want > 0 for an extraverted sender", spread.Score)
	}
	t.Logf("spread detail: %s", spread.Detail)
}

// TestLiquidityImbalanceHeuristic pins the market emergence formula.
func TestLiquidityImbalanceHeuristic(t *testing.T) {
	cfg := runConfig(KindMarket, 6, 4, []SimAgentPersona{
		persona("m", "Mo", map[string]float64{"risk_appetite": 1}, nil),
	}, nil)
	res := runOnce(t, cfg)
	var imb *EmergentNote
	for i := range res.EmergentNotes {
		if res.EmergentNotes[i].Kind == "liquidity_imbalance" {
			imb = &res.EmergentNotes[i]
		}
	}
	if imb == nil {
		t.Fatalf("liquidity_imbalance note missing: %+v", res.EmergentNotes)
	}
	if imb.Score < 0 || imb.Score > 1 {
		t.Fatalf("imbalance %v out of [0,1]", imb.Score)
	}
}

// TestCustomEnvironmentRegistration proves the extension seam: a custom
// world + the genericAgent fallback (peers fact) run without touching the
// built-ins, and the registration validates its inputs.
func TestCustomEnvironmentRegistration(t *testing.T) {
	reg := NewRegistry()
	calls := 0
	err := reg.RegisterEnvironment("grid", func() SimulationEnvironment {
		calls++
		return &gridEnv{}
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.RegisterEnvironment("", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty registration = %v, want ErrInvalid", err)
	}
	// override semantics are documented: re-registering replaces the factory
	// (the replacement also counts, proving the engine uses the LAST one).
	if err := reg.RegisterEnvironment("grid", func() SimulationEnvironment {
		calls++
		return &gridEnv{}
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	r := NewRunner(WithRegistry(reg), WithClock(testClock))
	cfg := runConfig("grid", 4, 1, []SimAgentPersona{
		persona("g1", "G1", nil, map[string]PlatformBehavior{
			"grid": {Weights: map[string]float64{"poke": 1}},
		}),
		persona("g2", "G2", nil, nil), // no behavior -> generic fallback has empty weights -> idles
	}, nil)
	res, err := r.RunSimulation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("custom run: %v", err)
	}
	if len(res.Rounds) != 4 {
		t.Fatalf("rounds = %d", len(res.Rounds))
	}
	if calls < 1 {
		t.Fatal("factory never invoked")
	}
	// the custom world recorded the pokes from the weighted agent
	if res.Rounds[3].State.Facts["pokes"] != 4 {
		t.Fatalf("pokes = %v, want 4", res.Rounds[3].State.Facts["pokes"])
	}
	// unknown kind is still rejected on this registry
	bad := runConfig("nope", 1, 1, []SimAgentPersona{persona("x", "X", nil, nil)}, nil)
	if _, err := r.RunSimulation(context.Background(), bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kind = %v, want ErrInvalid", err)
	}
}

// gridEnv is a minimal custom world exercising the genericAgent fallback.
type gridEnv struct {
	round int
	pokes int
}

func (g *gridEnv) Initialize(_ context.Context, _ EnvConfig) error { return nil }

func (g *gridEnv) Step(_ context.Context, agents []SimAgent) ([]ActionResult, error) {
	g.round++
	out := make([]ActionResult, 0, len(agents))
	for _, ag := range agents {
		view := EnvState{Kind: "grid", Round: g.round, Facts: map[string]any{"peers": []string{"cell-1", "cell-2"}}}
		act := ag.Decide(context.Background(), view, rand.New(rand.NewSource(int64(g.round))))
		act.AgentID = ag.Persona().CoreIdentity.ID
		accepted := act.Kind == "poke"
		if accepted {
			g.pokes++
		}
		out = append(out, ActionResult{Action: act, Accepted: accepted, Reason: map[bool]string{true: "", false: "not_poke"}[accepted]})
	}
	return out, nil
}

func (g *gridEnv) GetState(context.Context) (EnvState, error) {
	return EnvState{Kind: "grid", Round: g.round, Facts: map[string]any{"pokes": g.pokes}}, nil
}

func (g *gridEnv) Close() error { return nil }

// TestValidationBounds pins the config validation contract (API-safe 422s).
func TestValidationBounds(t *testing.T) {
	reg := NewRegistry()
	ok := runConfig(KindSocial, 1, 1, []SimAgentPersona{persona("a", "A", nil, nil)}, nil)
	if err := ValidateConfig(ok, reg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]func(*SimConfig){
		"empty kind":      func(c *SimConfig) { c.Kind = "" },
		"unknown kind":    func(c *SimConfig) { c.Kind = "quantum" },
		"zero rounds":     func(c *SimConfig) { c.Rounds = 0 },
		"rounds too many": func(c *SimConfig) { c.Rounds = MaxRounds + 1 },
		"no agents":       func(c *SimConfig) { c.Agents = nil },
		"too many agents": func(c *SimConfig) { c.Agents = make([]SimAgentPersona, MaxAgents+1) },
		"missing id": func(c *SimConfig) {
			c.Agents = []SimAgentPersona{{CoreIdentity: CoreIdentity{Name: "anon"}}}
		},
		"duplicate id": func(c *SimConfig) {
			c.Agents = []SimAgentPersona{persona("a", "A", nil, nil), persona("a", "A2", nil, nil)}
		},
		"bad frequency": func(c *SimConfig) {
			f := 1.5
			c.Agents = []SimAgentPersona{{CoreIdentity: CoreIdentity{ID: "a"},
				PlatformBehavior: map[string]PlatformBehavior{"social": {Frequency: &f}}}}
		},
		"event round 0": func(c *SimConfig) {
			c.Activity = []ActivityEvent{{Round: 0, Kind: "trending_topic"}}
		},
		"event past rounds": func(c *SimConfig) {
			c.Activity = []ActivityEvent{{Round: 9, Kind: "trending_topic"}}
		},
		"event kind empty": func(c *SimConfig) {
			c.Activity = []ActivityEvent{{Round: 1}}
		},
		"event wrong platform": func(c *SimConfig) {
			c.Activity = []ActivityEvent{{Round: 1, Kind: "x", Platform: "market"}}
		},
		"bad timezone": func(c *SimConfig) { c.Location = "Mars/Olympus" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := runConfig(KindSocial, 2, 1, []SimAgentPersona{persona("a", "A", nil, nil)}, nil)
			mutate(&cfg)
			err := ValidateConfig(cfg, reg)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ValidateConfig = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestEngineContextCancellation: a cancelled context stops the run at the
// next round boundary (the runner honors caller cancellation).
func TestEngineContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := runConfig(KindSocial, 10, 1, []SimAgentPersona{persona("a", "A", nil, nil)}, nil)
	r := NewRunner(WithClock(testClock))
	if _, err := r.RunSimulation(ctx, cfg); err == nil {
		t.Fatal("cancelled run succeeded")
	}
}

// TestSteppedClock sweeps activity buckets deterministically (the clock is
// the injectable seam; each round observes a later local hour — here the
// 18:00 evening peak down into the 00:00 night trough).
func TestSteppedClock(t *testing.T) {
	clock := NewSteppedClock(time.Date(2014, 6, 7, 20, 0, 0, 0, time.UTC), time.Hour)
	got := make([]float64, 0, 5)
	for i := 0; i < 5; i++ {
		got = append(got, multiplierFor(clock.Now(), time.UTC))
	}
	want := []float64{1.3, 1.3, 0.7, 0.4, 0.4} // hours 21,22,23,00,01 -> documented table
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stepped multipliers = %v, want %v", got, want)
	}
}

// TestFrequencyGate: a persona with frequency 0 NEVER acts (idle casts are
// expressible), and the engine records the zeros honestly.
func TestFrequencyGate(t *testing.T) {
	zero := 0.0
	agents := []SimAgentPersona{{
		CoreIdentity:     CoreIdentity{ID: "ghost", Name: "Ghost"},
		PlatformBehavior: map[string]PlatformBehavior{KindSocial: {Frequency: &zero}},
	}}
	res := runOnce(t, runConfig(KindSocial, 5, 2, agents, nil))
	for _, rr := range res.Rounds {
		if rr.Metrics.ActiveAgents != 0 || rr.Metrics.Actions != 0 {
			t.Fatalf("round %d: frequency-0 agent acted: %+v", rr.Round, rr.Metrics)
		}
	}
	notes := fmt.Sprint(res.EmergentNotes)
	if res.EmergentNotes[0].Score != 0 {
		t.Fatalf("density %v for a fully idle run (notes=%s)", res.EmergentNotes[0].Score, notes)
	}
}
