package loadtest

import (
	"context"
	"sync/atomic"
	"time"
)

// rampStep is the measured result of one concurrency level.
type rampStep struct {
	Workers   int     `json:"workers"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	MaxMs     float64 `json:"max_ms"`
	RPS       float64 `json:"rps"`
}

// scenarioConcurrency ramps the closed-loop worker count geometrically until
// the breaking point (error rate >= threshold) and reports the maximum
// sustainable concurrency plus the latency knee (first step whose P95 exceeds
// KneeFactor x the first step's P95).
type scenarioConcurrency struct{}

func (scenarioConcurrency) Name() string { return "concurrency" }

func (s scenarioConcurrency) Run(ctx context.Context, env *Env) (*Report, error) {
	cfg := env.Config()
	b := newReportBuilder(s.Name(), cfg)

	tenant, agentID, err := provisionAgent(ctx, env, b, "concurrency")
	if err != nil {
		return b.build(0, nil, nil), err
	}
	sampler := env.startSampling(ctx, tenant.Token)

	var breaking, sustainable, knee int
	var steps []rampStep
	broke := false
	workers := cfg.RampStart
	rampStart := time.Now()

	for workers <= cfg.MaxWorkers {
		c := newCollector()
		var seq atomic.Int64
		stepStart := time.Now()
		env.closedLoopPhase(ctx, workers, cfg.RampStep, func() {
			seq.Add(1)
			_, _ = env.CreateRun(ctx, c, tenant, agentID, "load")
		})
		elapsed := time.Since(stepStart)

		reqs, errs, _, _ := c.totals()
		_, _, p50, p95, _, _, maxMs := c.stats()
		step := rampStep{
			Workers:   workers,
			Requests:  reqs,
			Errors:    errs,
			ErrorRate: errorRate(reqs, errs),
			P50Ms:     p50,
			P95Ms:     p95,
			MaxMs:     maxMs,
			RPS:       float64(reqs) / elapsed.Seconds(),
		}
		steps = append(steps, step)
		b.op("create_run").merge(c)

		if knee == 0 && len(steps) > 1 && p95 > steps[0].P95Ms*cfg.KneeFactor {
			knee = workers
		}
		if step.ErrorRate >= cfg.ErrorThreshold {
			broke = true
			breaking = workers
			break
		}
		sustainable = workers
		workers = int(float64(workers)*cfg.RampFactor) + 1
	}
	elapsed := time.Since(rampStart)

	extra := map[string]any{
		"agent_id":          agentID,
		"steps":             steps,
		"max_sustainable":   sustainable,
		"knee":              knee,
		"knee_factor":       cfg.KneeFactor,
		"breaking_point":    breaking,
		"broke":             broke,
		"error_threshold":   cfg.ErrorThreshold,
		"first_step_p95_ms": round2(steps[0].P95Ms),
	}
	if broke {
		extra["verdict"] = "breaking point reached"
	} else {
		extra["verdict"] = "no breaking point within max-workers"
	}
	return b.build(elapsed, sampler.Report(cfg.SampleInterval), extra), nil
}
