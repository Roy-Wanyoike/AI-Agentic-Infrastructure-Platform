package loadtest

import (
	"flag"
	"fmt"
	"time"
)

// Config carries every tunable of the load suite. Each scenario reads the
// subset it needs; the agentload CLI registers the full union via
// RegisterFlags so operators can tune any scenario from the command line.
type Config struct {
	// Base is the API origin, e.g. http://127.0.0.1:8080.
	Base string
	// Prefix is the versioned route prefix ("/v1" or "/api/v1").
	Prefix string
	// Duration is the measurement window of the sustained/soak/workflow/
	// lifecycle scenarios (spike uses BaselineDuration + SpikeDuration).
	Duration time.Duration
	// Warmup is a paced warm-up phase whose samples are discarded.
	Warmup time.Duration
	// RPS is the target request rate of open-loop scenarios (create_run ops;
	// the unauthenticated healthz control adds ~RPS/10 extra requests).
	RPS float64
	// Workers is the generator pool size for open-loop scenarios and the
	// starting/only concurrency for the closed-loop lifecycle churn.
	Workers int
	// Timeout is the per-request client timeout.
	Timeout time.Duration
	// Password is the password used for generated tenants.
	Password string
	// QueueBuffer is the dispatch-queue depth between pacer and workers; a
	// full queue counts a client-side drop (generator saturation signal).
	QueueBuffer int

	// Spike parameters: baseline period, burst = RPS*Multiplier for
	// SpikeDuration, then recovery window back at RPS.
	SpikeMultiplier   int
	SpikeDuration     time.Duration
	BaselineDuration  time.Duration
	RecoveryTimeout   time.Duration
	RecoveryTolerance float64 // recovered when rolling p95 <= baseline p95 * tolerance
	RecoveryWindowSec float64 // rolling window for the recovery check

	// Soak parameters.
	SampleInterval     time.Duration // /metrics sampling cadence
	LeakSlopeThreshold float64       // queue-length growth allowance vs enqueue rate (x)

	// Concurrency ramp parameters.
	RampStart      int           // first worker count
	RampFactor     float64       // geometric step factor
	RampStep       time.Duration // wall time per step
	MaxWorkers     int           // upper bound of the ramp
	ErrorThreshold float64       // breaking-point error rate (0.05 = 5%)
	KneeFactor     float64       // latency knee: p95 > KneeFactor * first-step p95

	// Workflow parameters.
	FanOut int // fan-out width of the stress DAG
}

// DefaultConfig returns the suite defaults (overridable per flag).
func DefaultConfig() *Config {
	return &Config{
		Base:               "http://127.0.0.1:8080",
		Prefix:             "/v1",
		Duration:           60 * time.Second,
		Warmup:             5 * time.Second,
		RPS:                100,
		Workers:            16,
		Timeout:            5 * time.Second,
		Password:           "loadtest-passw0rd",
		QueueBuffer:        4096,
		SpikeMultiplier:    10,
		SpikeDuration:      30 * time.Second,
		BaselineDuration:   10 * time.Second,
		RecoveryTimeout:    60 * time.Second,
		RecoveryTolerance:  1.2,
		RecoveryWindowSec:  3,
		SampleInterval:     5 * time.Second,
		LeakSlopeThreshold: 1.5,
		RampStart:          1,
		RampFactor:         2,
		RampStep:           3 * time.Second,
		MaxWorkers:         256,
		ErrorThreshold:     0.05,
		KneeFactor:         2.0,
		FanOut:             3,
	}
}

// RegisterFlags registers every scenario tunable on fs (union of all
// scenarios; each scenario validates the subset it needs).
func RegisterFlags(fs *flag.FlagSet, cfg *Config) {
	fs.StringVar(&cfg.Base, "base", cfg.Base, "API base URL")
	fs.StringVar(&cfg.Prefix, "prefix", cfg.Prefix, "API route prefix (/v1 or /api/v1)")
	fs.DurationVar(&cfg.Duration, "d", cfg.Duration, "measurement duration (sustained/soak/workflow/lifecycle)")
	fs.DurationVar(&cfg.Warmup, "warmup", cfg.Warmup, "warmup duration, samples discarded (0 disables)")
	fs.Float64Var(&cfg.RPS, "rps", cfg.RPS, "target requests/sec of the primary op (open-loop scenarios)")
	fs.IntVar(&cfg.Workers, "workers", cfg.Workers, "generator worker pool size (closed-loop scenarios use one worker goroutine each)")
	fs.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "per-request timeout")
	fs.StringVar(&cfg.Password, "password", cfg.Password, "password for generated tenants")
	fs.IntVar(&cfg.QueueBuffer, "queue-buffer", cfg.QueueBuffer, "dispatch queue depth between pacer and workers")
	fs.IntVar(&cfg.SpikeMultiplier, "spike-multiplier", cfg.SpikeMultiplier, "spike: burst = rps * multiplier")
	fs.DurationVar(&cfg.SpikeDuration, "spike-duration", cfg.SpikeDuration, "spike: burst duration")
	fs.DurationVar(&cfg.BaselineDuration, "baseline-duration", cfg.BaselineDuration, "spike: baseline period before the burst")
	fs.DurationVar(&cfg.RecoveryTimeout, "recovery-timeout", cfg.RecoveryTimeout, "spike: max time to wait for recovery")
	fs.Float64Var(&cfg.RecoveryTolerance, "recovery-tolerance", cfg.RecoveryTolerance, "spike: recovered when rolling p95 <= baseline p95 * tolerance")
	fs.Float64Var(&cfg.RecoveryWindowSec, "recovery-window", cfg.RecoveryWindowSec, "spike: rolling window (seconds) for the recovery p95")
	fs.DurationVar(&cfg.SampleInterval, "sample-interval", cfg.SampleInterval, "soak: /metrics sampling cadence")
	fs.Float64Var(&cfg.LeakSlopeThreshold, "leak-slope-x", cfg.LeakSlopeThreshold, "soak: queue may grow up to enqueue rate * this factor before flagging")
	fs.IntVar(&cfg.RampStart, "ramp-start", cfg.RampStart, "concurrency: first worker count")
	fs.Float64Var(&cfg.RampFactor, "ramp-factor", cfg.RampFactor, "concurrency: geometric step factor")
	fs.DurationVar(&cfg.RampStep, "ramp-step", cfg.RampStep, "concurrency: wall time per ramp step")
	fs.IntVar(&cfg.MaxWorkers, "max-workers", cfg.MaxWorkers, "concurrency: upper bound of the ramp")
	fs.Float64Var(&cfg.ErrorThreshold, "error-threshold", cfg.ErrorThreshold, "concurrency: breaking-point error rate (0.05 = 5%)")
	fs.Float64Var(&cfg.KneeFactor, "knee-factor", cfg.KneeFactor, "concurrency: latency knee factor vs first-step p95")
	fs.IntVar(&cfg.FanOut, "fan-out", cfg.FanOut, "workflow: fan-out width of the stress DAG")
}

// Validate checks the invariants the scenarios rely on (per scenario).
func (c *Config) Validate(scenario string) error {
	if c.Base == "" {
		return fmt.Errorf("-base is required")
	}
	if c.RPS <= 0 {
		return fmt.Errorf("-rps must be > 0")
	}
	if c.Workers <= 0 {
		return fmt.Errorf("-workers must be > 0")
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("-timeout must be > 0")
	}
	switch scenario {
	case "sustained", "soak", "workflow", "lifecycle":
		if c.Duration <= 0 {
			return fmt.Errorf("-d must be > 0 for %s", scenario)
		}
	case "spike":
		if c.SpikeMultiplier < 2 {
			return fmt.Errorf("-spike-multiplier must be >= 2")
		}
		if c.BaselineDuration <= 0 || c.SpikeDuration <= 0 {
			return fmt.Errorf("spike requires -baseline-duration > 0 and -spike-duration > 0")
		}
		if c.RecoveryTolerance <= 1 {
			return fmt.Errorf("-recovery-tolerance must be > 1")
		}
	case "concurrency":
		if c.RampStart < 1 || c.RampFactor < 1.1 || c.RampStep <= 0 || c.MaxWorkers < c.RampStart {
			return fmt.Errorf("concurrency requires -ramp-start >= 1, -ramp-factor >= 1.1, -ramp-step > 0, -max-workers >= -ramp-start")
		}
		if c.ErrorThreshold <= 0 || c.ErrorThreshold >= 1 {
			return fmt.Errorf("-error-threshold must be in (0,1)")
		}
		if c.KneeFactor <= 1 {
			return fmt.Errorf("-knee-factor must be > 1")
		}
	}
	if scenario == "workflow" && c.FanOut < 1 {
		return fmt.Errorf("-fan-out must be >= 1")
	}
	return nil
}

// configMap renders the config for the JSON report (keyed by flag names).
func (c *Config) configMap() map[string]any {
	return map[string]any{
		"base":                c.Base,
		"prefix":              c.Prefix,
		"duration_s":          c.Duration.Seconds(),
		"warmup_s":            c.Warmup.Seconds(),
		"rps":                 c.RPS,
		"workers":             c.Workers,
		"timeout_s":           c.Timeout.Seconds(),
		"spike_multiplier":    c.SpikeMultiplier,
		"spike_duration_s":    c.SpikeDuration.Seconds(),
		"baseline_duration_s": c.BaselineDuration.Seconds(),
		"recovery_timeout_s":  c.RecoveryTimeout.Seconds(),
		"recovery_tolerance":  c.RecoveryTolerance,
		"sample_interval_s":   c.SampleInterval.Seconds(),
		"ramp_start":          c.RampStart,
		"ramp_factor":         c.RampFactor,
		"ramp_step_s":         c.RampStep.Seconds(),
		"max_workers":         c.MaxWorkers,
		"error_threshold":     c.ErrorThreshold,
		"knee_factor":         c.KneeFactor,
		"fan_out":             c.FanOut,
	}
}
