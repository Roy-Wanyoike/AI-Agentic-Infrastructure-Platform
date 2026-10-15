// shutdown.go — issue #107: graceful shutdown for the agent runtime.
//
// Two contracts live here:
//
//  1. GRACEFUL RUN CANCELLATION. When the caller's context is cancelled
//     (context.Canceled — user cancellation or worker shutdown; NOT the
//     run's own wall-clock deadline), the loop must not abandon work
//     mid-flight: the in-flight model/tool call runs to ITS completion (its
//     result — usually a cancellation error — is recorded as a normal step),
//     a final cancellation marker step summarizing the partial state is
//     recorded, the run.cancelled event is published through the event seam,
//     and the loop unwinds with the typed ErrRunCancelled outcome. The run
//     status mapping is unchanged: cancellation surfaces as the existing
//     FAILED status with the typed cause on run.Error (the pre-existing
//     contract — "cancellation is recorded as failure with its cause" — is
//     preserved; callers distinguish the outcome via errors.Is).
//
//     Recording uses context.WithoutCancel: a cancelled caller context must
//     not prevent the partial results from reaching the step recorder
//     (Postgres inserts fail on a cancelled context otherwise).
//
//  2. RUNNER SHUTDOWN GATE. Runner.Shutdown(ctx) / Runner.Stop implement the
//     worker-facing stop semantics: Stop marks the runner as shutting down
//     (new runs are refused with ErrShuttingDown immediately) and Shutdown
//     additionally WAITS for the in-flight runs to complete, bounded by the
//     caller's context (the worker passes its grace period; default 10s,
//     configurable via AGENTOS_WORKER_GRACE_SECONDS). A runner that drained
//     cleanly returns nil; a runner whose grace period elapsed with runs
//     still in flight returns an error wrapping ErrShutdownTimeout. Shutdown
//     is irreversible — a stopped runner accepts no new runs.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"agentos/internal/events"
	"agentos/internal/policies"
)

// Typed shutdown outcomes. Use errors.Is to classify.
var (
	// ErrRunCancelled is the typed outcome of a gracefully cancelled run
	// (issue #107): the current step completed, partial results are recorded
	// and the run.cancelled event was emitted.
	ErrRunCancelled = errors.New("runtime: run cancelled")
	// ErrShuttingDown is returned to callers who try to START a run on a
	// runner that has stopped accepting new runs.
	ErrShuttingDown = errors.New("runtime: runner is shutting down")
	// ErrShutdownTimeout is returned by Shutdown when the grace period
	// elapsed with runs still in flight.
	ErrShutdownTimeout = errors.New("runtime: shutdown grace period elapsed")
)

// DefaultShutdownGrace is the default worker grace period (issue #107):
// after SIGTERM the worker stops accepting new runs and lets in-flight runs
// finish for up to this long.
const DefaultShutdownGrace = 10 * time.Second

// EnvShutdownGrace configures the worker grace period in seconds.
const EnvShutdownGrace = "AGENTOS_WORKER_GRACE_SECONDS"

// ShutdownGraceFromEnv reads AGENTOS_WORKER_GRACE_SECONDS (whole seconds).
// Unset -> DefaultShutdownGrace; invalid or negative values fall back to the
// default (bad env config must never change shutdown semantics silently in
// the unsafe direction; 0 is honored as "no grace" when set explicitly).
func ShutdownGraceFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv(EnvShutdownGrace))
	if v == "" {
		return DefaultShutdownGrace
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return DefaultShutdownGrace
	}
	return time.Duration(n) * time.Second
}

// --- shutdown gate ----------------------------------------------------------

// begin admits a run through the shutdown gate: it fails with
// ErrShuttingDown once the runner stopped accepting new runs.
func (r *Runner) begin() error {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.stopping {
		return ErrShuttingDown
	}
	r.inflight++
	return nil
}

// finish releases one admitted run and wakes a waiting Shutdown when the
// last in-flight run drained.
func (r *Runner) finish() {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	r.inflight--
	if r.stopping && r.inflight == 0 && r.drained != nil {
		close(r.drained)
		r.drained = nil
	}
}

// Stop marks the runner as shutting down: new runs are refused with
// ErrShuttingDown; in-flight runs keep executing. Non-blocking; see Shutdown
// for the draining variant. Idempotent.
func (r *Runner) Stop() {
	if r == nil {
		return
	}
	r.gateMu.Lock()
	r.stopping = true
	r.gateMu.Unlock()
}

// Shutdown stops accepting new runs and waits for the in-flight runs to
// complete, bounded by ctx (the worker passes its grace period). Returns nil
// once everything drained; an error wrapping ErrShutdownTimeout when ctx
// expired with runs still in flight (the caller may retry with a fresh
// context — the stop state is sticky). Safe to call concurrently and on a
// nil runner.
func (r *Runner) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.gateMu.Lock()
	r.stopping = true
	if r.inflight == 0 {
		r.gateMu.Unlock()
		return nil
	}
	if r.drained == nil {
		r.drained = make(chan struct{})
	}
	drained := r.drained
	r.gateMu.Unlock()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		r.gateMu.Lock()
		n := r.inflight
		r.gateMu.Unlock()
		return fmt.Errorf("%w: %d run(s) still in flight", ErrShutdownTimeout, n)
	}
}

// InFlight reports the number of currently executing runs (diagnostics).
func (r *Runner) InFlight() int {
	if r == nil {
		return 0
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	return r.inflight
}

// --- graceful cancellation ----------------------------------------------------

// isCancellation reports whether err is a caller cancellation (NOT a
// deadline: the wall-clock budget keeps its existing ErrMaxRuntimeExceeded
// semantics).
func isCancellation(err error) bool {
	return err != nil && errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// runCancelledError is the typed graceful-cancellation outcome (issue #107):
// errors.Is(err, ErrRunCancelled) classifies it while Unwrap preserves the
// caller's cause, so errors.Is(err, context.Canceled) — the pre-#107 way to
// recognize user cancellation — keeps working for every existing caller.
type runCancelledError struct{ cause error }

// Error implements error with the typed outcome as the prefix.
func (e *runCancelledError) Error() string {
	if e == nil || e.cause == nil {
		return ErrRunCancelled.Error()
	}
	return fmt.Sprintf("%v: %v", ErrRunCancelled, e.cause)
}

// Is matches the ErrRunCancelled sentinel.
func (e *runCancelledError) Is(target error) bool { return target == ErrRunCancelled }

// Unwrap exposes the caller's cause (context.Canceled and friends).
func (e *runCancelledError) Unwrap() error { return e.cause }

// unwindCancelled terminates a gracefully cancelled run (issue #107): the
// current step has already completed (its outcome — usually the cancellation
// error — was recorded as a normal step by the loop), so this records the
// final cancellation marker with the partial results, publishes the
// run.cancelled event through the event seam (best-effort) and returns the
// typed outcome. The cause is preserved for errors.Is classification.
func (r *Runner) unwindCancelled(ctx context.Context, run *Run, cause error) error {
	causeText := ""
	if cause != nil {
		causeText = cause.Error()
	}
	r.recordCancellationMarker(ctx, run, causeText)
	r.emitRunEvent(ctx, events.EventRunCancelled, run.ID, map[string]any{
		"run_id":   run.ID,
		"agent_id": run.AgentID,
		"steps":    run.Steps,
		"reason":   causeText,
	})
	return &runCancelledError{cause: cause}
}

// recordCancellationMarker records the final partial-results step of a
// gracefully cancelled run: type "cancelled", status failed, naming the step
// the run stopped at (run.Steps — the loop iteration the cancellation landed
// in; the individual step records on the timeline show which of them
// succeeded). Recording happens on a context that survives the caller's
// cancellation (see the file header).
func (r *Runner) recordCancellationMarker(ctx context.Context, run *Run, cause string) {
	recordCtx := ctx
	if recordCtx == nil {
		recordCtx = context.Background()
	}
	recordCtx = context.WithoutCancel(recordCtx)
	r.record(recordCtx, run.ID, Step{
		Index:  run.Steps + 1,
		Type:   StepTypeCancelled,
		Name:   "run",
		Status: StepFailed,
		Input:  fmt.Sprintf("run cancelled at step %d (%s)", run.Steps, cause),
		Error:  fmt.Sprintf("%s: %s", ErrRunCancelled, cause),
	})
}

// emitRunEvent publishes a run lifecycle event through the optional event
// seam (issue #107). Best-effort by contract: no publisher wired, no tenant
// scope on the context (the events contract requires one) or a publish
// failure never breaks the run — observability must not break execution.
// The event is published on a context that survives caller cancellation so a
// run.cancelled event still reaches the bus after the run's context died.
func (r *Runner) emitRunEvent(ctx context.Context, eventType, runID string, payload map[string]any) {
	if r == nil || r.eventPub == nil {
		return
	}
	tenant := policies.RunScopeFromContext(ctx).OrganizationID
	if tenant == "" {
		return
	}
	pubCtx := context.WithoutCancel(ctx)
	event := events.NewEvent(eventType, tenant, "run", runID, payload)
	_ = r.eventPub.Publish(pubCtx, event)
}
