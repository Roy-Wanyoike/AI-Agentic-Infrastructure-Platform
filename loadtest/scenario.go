package loadtest

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Scenario is one load profile. Run receives a live environment and returns
// a complete report; it must honour ctx cancellation (graceful shutdown:
// stop generating, drain in-flight requests, still return partial results).
type Scenario interface {
	// Name is the stable identifier used by the CLI (-scenario) and in
	// reports.
	Name() string
	// Run executes the scenario. A returned error means the scenario could
	// not run (setup failure, bad config) — partial results are still
	// returned for reporting.
	Run(ctx context.Context, env *Env) (*Report, error)
}

// registry holds the built-in scenarios.
var registry = map[string]Scenario{}

func register(s Scenario) { registry[s.Name()] = s }

// The six issue #125 scenarios. Registration is centralised so the CLI and
// tests see exactly one authoritative list.
func init() {
	register(scenarioSustained{})
	register(scenarioSpike{})
	register(scenarioSoak{})
	register(scenarioConcurrency{})
	register(scenarioWorkflow{})
	register(scenarioAgent{})
}

// ScenarioNames lists the built-in scenarios in registration order.
func ScenarioNames() []string {
	return []string{
		"sustained", "spike", "soak", "concurrency", "workflow", "lifecycle",
	}
}

// LookupScenario finds a scenario by name.
func LookupScenario(name string) (Scenario, error) {
	s, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown scenario %q (available: %v)", name, ScenarioNames())
	}
	return s, nil
}

// pacer issues one tick per period until its context is done. For RPS above
// 500 the pacer batches (period is clamped to >= 2ms and each tick carries
// multiple units) so a single goroutine can drive high rates.
type pacer struct {
	period time.Duration
	burst  int
}

func newPacer(rps float64) *pacer {
	burst := 1
	period := time.Duration(float64(time.Second) / rps)
	if period < 2*time.Millisecond {
		burst = int(float64(rps) * 0.002)
		if burst < 1 {
			burst = 1
		}
		period = 2 * time.Millisecond
	}
	return &pacer{period: period, burst: burst}
}

// run paces until ctx is done, invoking emit(n) with the number of units per
// tick. emit must be cheap and non-blocking.
func (p *pacer) run(ctx context.Context, emit func(n int)) {
	t := time.NewTicker(p.period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			emit(p.burst)
		}
	}
}

// pacedPhase drives fn at cfg.RPS for d using cfg.Workers goroutines over the
// shared pooled client. work is a buffered dispatch queue: when the workers
// cannot keep up, the overload is counted as client-side drops (generator
// saturation) rather than silently distorting latency. fn is invoked from
// multiple goroutines and must be safe for concurrent use; it must NOT use
// the phase context for its HTTP calls — graceful shutdown means generation
// stops at the deadline while in-flight requests still complete (the client
// timeout bounds them). The phase returns once the workers drain the queue.
func (e *Env) pacedPhase(ctx context.Context, c *collector, d time.Duration, fn func()) {
	e.pacedPhaseRPS(ctx, c, d, e.cfg.RPS, fn)
}

// pacedPhaseRPS is pacedPhase with an explicit offered rate (the spike
// scenario raises it for the burst phase without mutating the config).
func (e *Env) pacedPhaseRPS(ctx context.Context, c *collector, d time.Duration, rps float64, fn func()) {
	if d <= 0 || rps <= 0 {
		return
	}
	cfg := e.cfg
	p := newPacer(rps)
	work := make(chan struct{}, cfg.QueueBuffer)
	var wg sync.WaitGroup

	phaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Dispatch loop: pacer ticks -> work queue (bounded).
	go func() {
		defer close(work)
		p.run(phaseCtx, func(n int) {
			for i := 0; i < n; i++ {
				select {
				case work <- struct{}{}:
				default:
					c.drop() // generator saturation: request never issued
				}
			}
		})
	}()

	// Worker pool.
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				fn()
			}
		}()
	}

	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
	// Graceful drain: stop dispatching, let the workers finish the queued
	// units.
	cancel()
	wg.Wait()
}

// closedLoopPhase runs n workers, each immediately re-issuing fn in a tight
// loop (no pacing) until the step deadline — the closed-loop shape the
// concurrency ramp uses to find the saturation point.
func (e *Env) closedLoopPhase(ctx context.Context, n int, d time.Duration, fn func()) {
	if d <= 0 || n <= 0 {
		return
	}
	stepCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stepCtx.Done():
					return
				default:
					fn()
				}
			}
		}()
	}
	wg.Wait()
}

// startSampling runs a /metrics ResourceSampler for the lifetime of ctx and
// returns the sampler (call Report() after ctx is done).
func (e *Env) startSampling(ctx context.Context, token string) *ResourceSampler {
	s := NewResourceSampler(e.client, e.apiURL("/metrics"), token, e.cfg.SampleInterval)
	go s.Run(ctx)
	return s
}
