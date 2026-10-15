package main

// messaging.go wires the AGENTOS_MESSAGING task-dispatch plane (issue #116)
// into the worker binary. Selection (resolved by queue.MessagingModeFromEnv):
//
//      AGENTOS_MESSAGING=memory (default) -> newMessagingDispatcher returns a
//      memory-effective Dispatcher (Dispatch == Enqueue on the platform queue the
//      worker already drains) and startMessagingConsumer is a no-op: the
//      zero-infrastructure dev topology is byte-for-byte unchanged.
//
//      AGENTOS_MESSAGING=nats -> run tasks ride agentos.runs.create (published by
//      the API process, dispatch.go); this process consumes them through the
//      framework Consumer and executes each decoded envelope via processTask —
//      the SAME task path the platform queue feeds (one executor, two planes).
//      Broker-down at startup fails fast here: a worker that cannot reach the
//      broker would silently idle while runs pile up.
//
//      AGENTOS_MESSAGING=hybrid -> same consumption, but the API-side fallback
//      keeps enqueuing into the platform queue during broker outages, so this
//      process keeps draining BOTH planes: the main loop (queue.Worker) for
//      memory/fallback tasks and the messaging Consumer for broker tasks.
//
// Workflow coordination (req 3): every finished workflow node is fanned out
// onto agentos.workflow.node.completed (publishNodeCompleted); the fan-out
// listener is wired to the workflow service's EXISTING recovery/re-enqueue
// seam — see messagingNodeTrigger for the documented choice.

import (
	"context"
	"log/slog"
	"os"
	"time"

	"agentos/internal/observability"
	"agentos/internal/queue"
	"agentos/internal/workflows"
)

// newMessagingDispatcher resolves AGENTOS_MESSAGING/AGENTOS_NATS_URL and
// builds the mode-aware Dispatcher over the worker's platform queue (the
// hybrid fallback target and the memory-mode dispatch target).
func newMessagingDispatcher(q *queue.Queue, logr *slog.Logger, m *observability.Metrics) (*queue.Dispatcher, error) {
	d, err := queue.NewDispatcherFromEnv(q,
		queue.WithDispatcherLogger(logr),
		queue.WithDispatcherMetrics(m),
		queue.WithDispatcherConcurrency(1), // parity with the queue/pull loops (see startMessagingConsumer)
	)
	if err != nil {
		return nil, err
	}
	logr.Info("messaging dispatch plane selected",
		"mode", string(d.Mode()), "effective", string(d.EffectiveMode()), "degraded", d.Degraded())
	return d, nil
}

// startMessagingConsumer starts the framework Consumer for the messaging
// plane subjects (no-op in memory-effective mode: no consumer exists).
//
// WIRING CHOICES (documented per the issue #116 contract):
//
//   - The executor is processTask itself — the exact function the platform
//     queue loop and the API-pull loop already run — so a run task behaves
//     identically no matter which plane delivered it.
//   - Consumer concurrency stays at the framework default's conservative
//     sibling: the worker processes ONE task at a time on every existing
//     path (queue.Worker.ProcessNext, the pull loop), so the messaging
//     consumer is capped the same way (WithDispatcherConcurrency(1) at
//     construction) rather than opening a new, wider execution class.
//   - The consumer shares the process shutdown signal context: Stop(grace)
//     drains in-flight handlers exactly like drainRunners bounds the queue
//     loop. The returned stop func is idempotent.
func startMessagingConsumer(ctx context.Context, d *queue.Dispatcher, exec func(context.Context, *queue.Task) error, trigger func(context.Context, *queue.NodeCompletedEnvelope) error, grace time.Duration, logr *slog.Logger) func() {
	if d == nil || d.Consumer() == nil {
		return func() {} // memory mode: nothing to start
	}
	d.OnRun(exec)
	d.OnNodeCompleted(trigger)
	if err := d.Consumer().Start(ctx); err != nil {
		// Start only fails on configuration/stream errors (broker-down is
		// retried by the supervision loop) — a config error here is a
		// deployment mistake, not a transient outage.
		logr.Error("messaging consumer start failed", "error", err.Error())
		os.Exit(1)
	}
	return func() {
		if err := d.Consumer().Stop(grace); err != nil {
			logr.Warn("messaging consumer stop reported an error", "error", err.Error())
		}
	}
}

// messagingNodeTrigger returns the fan-out listener for
// agentos.workflow.node.completed. DOCUMENTED CHOICE: "what runs next" stays
// a single point of truth inside the workflow service — this package cannot
// import the DSL scheduler (internal/queue imports internal/workflows would
// cycle) and re-implementing DAG advancement here would fork that truth. The
// trigger therefore reuses the service's existing recovery/re-enqueue seam
// (RecoverIncompleteWorkflowRuns): the guarded, row-locked sweep that
// re-enqueues the next pending node of every incomplete run of the org —
// the same seam the periodic recovery worker and the durable-run watchdog
// already drive. Consequences, honestly stated: the listener is
// org-granular (it may re-enqueue pending nodes of other incomplete runs of
// the same org, which is idempotent by construction), and a node-completed
// fact for an unknown/foreign org is consumed + acked as a no-op rather
// than dead-lettered (fan-out is best-effort coordination; the periodic
// recovery sweep is the durability net, exactly as in memory mode).
func messagingNodeTrigger(wfSvc *workflows.Service) func(context.Context, *queue.NodeCompletedEnvelope) error {
	return func(ctx context.Context, env *queue.NodeCompletedEnvelope) error {
		if env == nil || env.OrganizationID == "" {
			return nil // nothing actionable without a tenant scope
		}
		_, err := wfSvc.RecoverIncompleteWorkflowRuns(ctx, env.OrganizationID)
		return err
	}
}

// publishNodeCompleted fans one finished workflow node out onto
// agentos.workflow.node.completed (issue #116 req 3). Memory mode has no
// fan-out plane and returns immediately (single-process deployments
// coordinate through the DAG executor directly). Publish failures are
// logged + counted by the dispatcher and deliberately swallowed here: the
// notification is a derived fact — the periodic recovery sweep re-enqueues
// missed nodes — so a failed fan-out must never fail the run that just
// completed its node.
func publishNodeCompleted(d *queue.Dispatcher, env queue.NodeCompletedEnvelope) {
	if d == nil {
		return
	}
	_ = d.PublishNodeCompleted(env)
}
