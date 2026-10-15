package main

// shutdown.go — issue #107 worker wiring: graceful shutdown on SIGTERM/SIGINT.
//
// Contract: the first signal ends task consumption (the main loop in main.go
// exits), every runner in the process is marked as shutting down (new runs
// are refused with runtime.ErrShuttingDown) and the in-flight runs get the
// grace period (AGENTOS_WORKER_GRACE_SECONDS, default 10s, runtime.ShutdownGraceFromEnv)
// to finish — the runtime lets their current step complete, records the
// partial results and releases the run gate. Shutdown returns once everything
// drained, or ErrShutdownTimeout when the grace elapses with runs still in
// flight; the timeout is logged (not fatal) because the process exit bounds
// what remains — run contexts are never cancelled by the shutdown path, so a
// draining run is never aborted mid-step.

import (
	"context"
	"log/slog"
	"time"

	"agentos/internal/runtime"
)

// drainRunners performs the graceful shutdown handshake with every runner of
// the worker process: the base runner and all per-org runners of the MCP
// cache (the path tasks actually execute on). grace bounds the drain wait; a
// grace timeout is logged, never fatal. Safe to call with nil caches (the
// zero-infrastructure wiring passes real caches; tests may pass nil).
func drainRunners(base *runtime.Runner, orgRunners *orgRunnerCache, grace time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if orgRunners != nil {
		orgRunners.stopAll()
	}
	if base != nil {
		base.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	drained := true
	if orgRunners != nil {
		if err := orgRunners.shutdownAll(ctx); err != nil {
			drained = false
			log.Warn("worker shutdown: grace period elapsed with runs still in flight", "error", err.Error())
		}
	}
	if base != nil {
		if err := base.Shutdown(ctx); err != nil {
			drained = false
			log.Warn("worker shutdown: base runner still in flight after grace period", "error", err.Error())
		}
	}
	if drained {
		log.Info("worker shutdown drained", "grace_seconds", grace.Seconds())
	}
}
