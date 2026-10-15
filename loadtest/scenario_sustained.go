package loadtest

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// tsOf converts a wall-clock time onto the collector timestamp axis (Unix
// seconds).
func tsOf(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// provisionAgent walks the canonical journey up to a run-ready agent:
// register -> login -> create agent -> draft/building/active transitions.
// One-time setup lands in the scenario's setup collector (reported, excluded
// from totals and from the comparison gate).
func provisionAgent(ctx context.Context, env *Env, b *reportBuilder, name string) (*Tenant, string, error) {
	setup := b.setupOp("provision")
	tenant, err := env.RegisterTenant(ctx, setup)
	if err != nil {
		return nil, "", err
	}
	agentC := b.setupOp("agent")
	agentID, err := env.CreateAgent(ctx, agentC, tenant, name+"-agent")
	if err != nil {
		return nil, "", err
	}
	if err := env.ActivateAgent(ctx, agentC, tenant, agentID); err != nil {
		return nil, "", err
	}
	return tenant, agentID, nil
}

// scenarioSustained drives a steady RPS at the run-creation path
// (POST /v1/runs against one ACTIVE agent) with the unauthenticated
// GET /healthz as the control op (~RPS/10 extra requests, no tenant work).
// It reports P50/P95/P99/mean/max per op, achieved RPS and error rate.
type scenarioSustained struct{}

func (scenarioSustained) Name() string { return "sustained" }

func (s scenarioSustained) Run(ctx context.Context, env *Env) (*Report, error) {
	cfg := env.Config()
	b := newReportBuilder(s.Name(), cfg)

	tenant, agentID, err := provisionAgent(ctx, env, b, "sustained")
	if err != nil {
		return b.build(0, nil, nil), err
	}
	sampler := env.startSampling(ctx, tenant.Token)

	// Warmup: same pacing, samples discarded.
	if cfg.Warmup > 0 {
		warm := newCollector()
		env.pacedPhase(ctx, warm, cfg.Warmup, func() {
			_, _ = env.CreateRun(ctx, warm, tenant, agentID, "warmup")
		})
	}

	// Measurement phase: create_run as the primary op, healthz piggybacked
	// every 10th dispatch as the control op.
	primary := b.op("create_run")
	control := b.op("healthz")
	var seq atomic.Int64
	started := time.Now()
	env.pacedPhase(ctx, primary, cfg.Duration, func() {
		n := seq.Add(1)
		_, _ = env.CreateRun(ctx, primary, tenant, agentID, "load")
		if n%10 == 0 {
			env.Healthz(ctx, control)
		}
	})
	elapsed := time.Since(started)

	return b.build(elapsed, sampler.Report(cfg.SampleInterval), map[string]any{
		"agent_id": agentID,
		"note":     "runs stay QUEUED in in-memory mode (no worker); the measured op is the create-run request path",
	}), nil
}

// scenarioSpike measures a burst: a baseline period establishes the
// steady-state P95, the burst multiplies the offered load (10x default) for
// SpikeDuration, and the recovery phase tracks how long the rolling P95 takes
// to return to the baseline envelope (within RecoveryTolerance of the
// baseline P95, measured over a rolling RecoveryWindowSec window).
type scenarioSpike struct{}

func (scenarioSpike) Name() string { return "spike" }

func (s scenarioSpike) Run(ctx context.Context, env *Env) (*Report, error) {
	cfg := env.Config()
	b := newReportBuilder(s.Name(), cfg)

	tenant, agentID, err := provisionAgent(ctx, env, b, "spike")
	if err != nil {
		return b.build(0, nil, nil), err
	}
	sampler := env.startSampling(ctx, tenant.Token)

	primary := b.op("create_run")
	control := b.op("healthz")
	var seq atomic.Int64
	phaseFn := func(c *collector, d time.Duration, rps float64) {
		env.pacedPhaseRPS(ctx, c, d, rps, func() {
			n := seq.Add(1)
			_, _ = env.CreateRun(ctx, c, tenant, agentID, "load")
			if n%10 == 0 {
				env.Healthz(ctx, control)
			}
		})
	}

	// 1. Baseline period: establishes the steady-state P95.
	baseC := newCollector()
	baselineStart := time.Now()
	phaseFn(baseC, cfg.BaselineDuration, cfg.RPS)
	baseP95 := Percentile(baseC.slice(), 95)
	threshold := baseP95 * cfg.RecoveryTolerance

	// 2. Burst: RPS * multiplier for spike duration.
	burstC := newCollector()
	phaseFn(burstC, cfg.SpikeDuration, cfg.RPS*float64(cfg.SpikeMultiplier))
	burstP95 := Percentile(burstC.slice(), 95)

	// 3. Recovery: back at baseline RPS; a checker polls the rolling window
	// of the recovery-phase samples and stops the phase early once the P95
	// is back under the threshold.
	recC := newCollector()
	burstEnd := time.Now()
	recCtx, cancelRec := context.WithCancel(ctx)
	var recovered bool
	var recoveryTime atomic.Value // time.Duration set before cancelRec
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-recCtx.Done():
				return
			case <-t.C:
				lat, ts := recC.timeline()
				w := window(lat, ts, nowTS(), cfg.RecoveryWindowSec)
				if len(w) >= 20 && Percentile(w, 95) <= threshold {
					recovered = true
					recoveryTime.Store(time.Since(burstEnd))
					cancelRec()
					return
				}
			}
		}
	}()
	phaseFn(recC, cfg.RecoveryTimeout, cfg.RPS)
	cancelRec()
	<-done
	var recoveryDur time.Duration
	if v, ok := recoveryTime.Load().(time.Duration); ok {
		recoveryDur = v
	}

	lat, ts := recC.timeline()
	postP95 := Percentile(window(lat, ts, nowTS(), cfg.RecoveryWindowSec), 95)

	// Re-home the phase samples under one op: the primary op stats are the
	// union of baseline + burst + recovery phases.
	primary.merge(baseC)
	primary.merge(burstC)
	primary.merge(recC)
	elapsed := time.Since(baselineStart)

	return b.build(elapsed, sampler.Report(cfg.SampleInterval), map[string]any{
		"agent_id":              agentID,
		"baseline_p95_ms":       round2(baseP95),
		"burst_rps":             round2(cfg.RPS * float64(cfg.SpikeMultiplier)),
		"burst_p95_ms":          round2(burstP95),
		"recovery_threshold_ms": round2(threshold),
		"recovered":             recovered,
		"recovery_time_s":       secondsOrMinus1(recovered, recoveryDur),
		"post_window_p95_ms":    round2(postP95),
		"note":                  "recovery = rolling window p95 back within recovery-tolerance of the baseline p95",
	}), nil
}

// scenarioSoak runs a long, steady load (duration configurable; CI runs a
// short variant) while sampling /metrics on an interval. Leak detection uses
// the process metrics available over /metrics: the agentos_queue_length gauge
// trend vs the enqueue rate, plus first-half vs second-half error-rate and
// P95 shifts. NOTE: the API does not expose /debug/pprof or a goroutine/heap/
// FD gauge (verified for issue #125) — true goroutine/heap/FD leak detection
// needs those endpoints and is recorded as a follow-up.
type scenarioSoak struct{}

func (scenarioSoak) Name() string { return "soak" }

func (s scenarioSoak) Run(ctx context.Context, env *Env) (*Report, error) {
	cfg := env.Config()
	b := newReportBuilder(s.Name(), cfg)

	tenant, agentID, err := provisionAgent(ctx, env, b, "soak")
	if err != nil {
		return b.build(0, nil, nil), err
	}
	sampler := env.startSampling(ctx, tenant.Token)

	primary := b.op("create_run")
	control := b.op("healthz")
	var seq atomic.Int64
	started := time.Now()
	env.pacedPhase(ctx, primary, cfg.Duration, func() {
		n := seq.Add(1)
		_, _ = env.CreateRun(ctx, primary, tenant, agentID, "load")
		if n%10 == 0 {
			env.Healthz(ctx, control)
		}
	})
	elapsed := time.Since(started)

	// Leak verdict from the sampled trends + phase-shifted latency/error.
	resources := sampler.Report(cfg.SampleInterval)
	verdict, reasons := soakVerdict(cfg, resources, primary, tsOf(started), elapsed)

	return b.build(elapsed, resources, map[string]any{
		"agent_id":         agentID,
		"leak_verdict":     verdict,
		"leak_reasons":     reasons,
		"queue_trend":      shortTrend(resources.Families["agentos_queue_length"]),
		"runs_total_trend": shortTrend(resources.Families["agentos_runs_total"]),
		"note":             "goroutine/heap/FD trend lines require /debug/pprof or runtime gauges (not exposed by the API) — follow-up",
	}), nil
}

// soakVerdict applies the /metrics-based leak heuristics:
//   - queue growth may exceed the enqueue rate only by the configured factor
//     (a queue growing FASTER than the load means the process retains work it
//     was never offered);
//   - the error rate must not climb by more than 2pp between the first and
//     second half of the run;
//   - the P95 must not climb by more than 50% (+10ms noise floor).
func soakVerdict(cfg *Config, rr *ResourceReport, primary *collector, startedAt float64, elapsed time.Duration) (string, []string) {
	var reasons []string
	halfSec := elapsed.Seconds() / 2
	t1 := startedAt
	t2 := startedAt + halfSec
	t3 := startedAt + elapsed.Seconds()
	reqs1, errs1 := primary.totalsWindow(t1, t2)
	reqs2, errs2 := primary.totalsWindow(t2, t3)
	lat, ts := primary.timeline()
	p95a := Percentile(window(lat, ts, t2, halfSec), 95)
	p95b := Percentile(window(lat, ts, t3, halfSec), 95)

	if tr, ok := rr.Families["agentos_queue_length"]; ok {
		enqueueRate := float64(reqs1+reqs2) / elapsed.Seconds()
		if enqueueRate > 0 && tr.SlopePerSec > enqueueRate*cfg.LeakSlopeThreshold {
			reasons = append(reasons, fmt.Sprintf("agentos_queue_length grows %.3f/s vs enqueue rate %.3f/s (factor %.1f > %.1f)",
				tr.SlopePerSec, enqueueRate, tr.SlopePerSec/enqueueRate, cfg.LeakSlopeThreshold))
		}
	}
	er1 := errorRate(reqs1, errs1)
	er2 := errorRate(reqs2, errs2)
	if er2-er1 > 0.02 {
		reasons = append(reasons, fmt.Sprintf("error rate climbs between halves: %.4f -> %.4f", er1, er2))
	}
	if p95a > 0 && p95b > p95a*1.5+10 {
		reasons = append(reasons, fmt.Sprintf("p95 climbs between halves: %.2fms -> %.2fms", p95a, p95b))
	}
	if len(reasons) > 0 {
		return "suspected", reasons
	}
	return "pass", reasons
}

func secondsOrMinus1(ok bool, d time.Duration) float64 {
	if !ok {
		return -1
	}
	return round2(d.Seconds())
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}
