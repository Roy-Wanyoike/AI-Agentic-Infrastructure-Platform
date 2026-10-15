package main

// recovery.go — issue #115: the stale-run recovery sweeper (worker side).
//
// A worker crash (or a lost worker) leaves runs stuck in running/cancelling
// forever. This background loop runs one recovery pass every interval
// (default 1m) through runs.Service.RecoverStaleRuns:
//
//   - stale = no checkpoint/heartbeat newer than AGENTOS_RUN_STALE_SECONDS
//     (default 300) AND no status update in the same window;
//   - a stale RUNNING run WITH a checkpoint is marked paused (resumable from
//     the latest checkpoint); without one it is marked failed;
//   - a stale CANCELLING run is confirmed cancelled;
//   - every reconciliation emits a run.recovered platform event.
//
// Enablement: AGENTOS_RUN_RECOVERY (default ON). The sweeper is only
// CONSTRUCTED in postgres mode — the sweep reads the durable runs rows the
// whole cluster shares; an in-memory worker holds no runs state to reconcile.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"agentos/internal/runs"
)

// EnvRunRecovery toggles the sweeper (default on in postgres mode).
const EnvRunRecovery = "AGENTOS_RUN_RECOVERY"

// EnvRunStaleSeconds configures the staleness threshold in whole seconds.
const EnvRunStaleSeconds = "AGENTOS_RUN_STALE_SECONDS"

// runRecoveryInterval is the sweep cadence (one pass per minute matches the
// workflow recovery worker's rhythm; bounded by the limit parameter).
const runRecoveryInterval = time.Minute

// runRecoveryFromEnv reports whether the sweeper should start. Default ON;
// only an explicit falsey value disables it (bad config must not silently
// turn durability off — the unsafe direction is the default).
func runRecoveryFromEnv(get func(string) string) bool {
	if get == nil {
		get = os.Getenv
	}
	switch strings.ToLower(strings.TrimSpace(get(EnvRunRecovery))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// runStaleAfterFromEnv reads AGENTOS_RUN_STALE_SECONDS (whole seconds).
// Unset or invalid → runs.DefaultRunStaleAfter (300s).
func runStaleAfterFromEnv(get func(string) string) time.Duration {
	if get == nil {
		get = os.Getenv
	}
	v := strings.TrimSpace(get(EnvRunStaleSeconds))
	if v == "" {
		return runs.DefaultRunStaleAfter
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return runs.DefaultRunStaleAfter
	}
	return time.Duration(n) * time.Second
}

// runRecoverySweeper drives the periodic RecoverStaleRuns pass. now is the
// injectable clock seam (tests advance it deterministically — no flaky
// sleeps; the interval ticker itself is bounded by ctx cancellation).
type runRecoverySweeper struct {
	svc        *runs.Service
	staleAfter time.Duration
	limit      int
	log        *slog.Logger
}

func newRunRecoverySweeper(svc *runs.Service, staleAfter time.Duration, log *slog.Logger) *runRecoverySweeper {
	return &runRecoverySweeper{
		svc:        svc,
		staleAfter: staleAfter,
		limit:      100,
		log:        log,
	}
}

// sweepOnce runs one pass and returns the reconciliations (also logged).
func (s *runRecoverySweeper) sweepOnce(ctx context.Context) ([]runs.RunRecovery, error) {
	if s == nil || s.svc == nil {
		return nil, errors.New("worker: recovery sweeper has no runs service")
	}
	got, err := s.svc.RecoverStaleRuns(ctx, s.staleAfter, s.limit)
	if err != nil {
		return got, err
	}
	for _, rec := range got {
		s.log.Info("run recovered",
			"run_id", rec.RunID,
			"organization_id", rec.OrganizationID,
			"from", string(rec.From),
			"to", string(rec.To),
			"stale_after_seconds", int(s.staleAfter.Seconds()))
	}
	return got, nil
}

// Run loops until ctx is cancelled. A failing pass is logged (best-effort:
// a transient database error must not kill the worker's durability loop).
func (s *runRecoverySweeper) Run(ctx context.Context) error {
	if s == nil || s.svc == nil {
		return errors.New("worker: recovery sweeper has no runs service")
	}
	ticker := time.NewTicker(runRecoveryInterval)
	defer ticker.Stop()
	// One immediate pass at startup: runs stuck by a PREVIOUS worker are
	// reconciled as soon as this one comes up, not a full interval later.
	if _, err := s.sweepOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Warn("run recovery pass failed", "error", err.Error())
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := s.sweepOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Warn("run recovery pass failed", "error", err.Error())
			}
		}
	}
}
