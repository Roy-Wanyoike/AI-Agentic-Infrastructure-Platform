package scheduler

import (
	"context"
	"log/slog"
	"time"

	"agentos/internal/queue"
	"agentos/internal/runs"
)

// worker.go implements the scheduler trigger loop: every poll it fetches due
// schedules (Service.Due), claims each slot (catch-up protection), creates a
// run through the runs service and enqueues an "agent.run" task — the same
// payload shape cmd/worker's processTask consumes.
//
// The loop is started EXPLICITLY by the embedder (see docs/wiring/scheduler.md):
//
//	schedWorker := scheduler.NewWorker(schedSvc, runsSvc, queueSvc, 30*time.Second)
//	go schedWorker.Run(ctx)

const (
	DefaultPollInterval = 30 * time.Second
	// scheduleTaskType is the queue task type for schedule-triggered runs;
	// it matches the worker's existing agent.run consumer.
	scheduleTaskType = "agent.run"
)

// Worker fires due schedules. runsSvc and queueSvc may be nil in degenerate
// embeds (the claim/advance bookkeeping still happens, nothing is enqueued).
type Worker struct {
	svc          *Service
	runsSvc      *runs.Service
	queueSvc     *queue.Queue
	pollInterval time.Duration
	clock        Clock
	logr         *slog.Logger
}

// NewWorker builds the scheduler trigger worker with the contract-pinned
// signature. Use WithClock/WithLogger to customize for tests.
func NewWorker(svc *Service, runsSvc *runs.Service, queueSvc *queue.Queue, pollInterval time.Duration) *Worker {
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	return &Worker{
		svc:          svc,
		runsSvc:      runsSvc,
		queueSvc:     queueSvc,
		pollInterval: pollInterval,
		clock:        realClock{},
		logr:         slog.Default(),
	}
}

// WithClock injects a fake clock (tests only) and returns the worker.
func (w *Worker) WithClock(c Clock) *Worker {
	if w == nil || c == nil {
		return w
	}
	w.clock = c
	return w
}

// WithLogger sets a structured logger and returns the worker.
func (w *Worker) WithLogger(l *slog.Logger) *Worker {
	if w == nil || l == nil {
		return w
	}
	w.logr = l
	return w
}

// Run blocks, ticking every pollInterval until ctx is cancelled. One tick runs
// immediately so schedules that became due while the worker was down fire on
// startup (at most once per due instant, per ClaimForFire guarantees).
func (w *Worker) Run(ctx context.Context) {
	if w == nil || w.svc == nil {
		return
	}
	interval := w.pollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	w.Tick(ctx, w.clock.Now())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(ctx, w.clock.Now())
		}
	}
}

// Tick performs one poll: fetch due schedules, claim each firing slot, create
// a run and enqueue it. Returns the number of schedules fired. Exposed so
// tests (fake clock) and embedders can drive the loop deterministically.
func (w *Worker) Tick(ctx context.Context, now time.Time) int {
	if w == nil || w.svc == nil {
		return 0
	}
	now = now.UTC()
	due, err := w.svc.Due(ctx, now)
	if err != nil {
		w.logr.Error("scheduler: due query failed", "error", err.Error())
		return 0
	}
	fired := 0
	for _, sched := range due {
		// Claim first: the conditional update (status active + next_run_at
		// still due) makes the fire at-most-once across restarts. If the
		// process dies after the claim but before enqueueing, the schedule
		// still advances — a missed run is preferred over a duplicated one.
		claimed, ok, cerr := w.svc.ClaimForFire(ctx, sched.ID, now)
		if cerr != nil {
			w.logr.Error("scheduler: claim failed",
				"schedule_id", sched.ID, "organization_id", sched.OrganizationID, "error", cerr.Error())
			continue
		}
		if !ok {
			continue // claimed by a concurrent worker
		}
		w.fire(ctx, claimed, now)
		fired++
	}
	return fired
}

// fire creates the run for a claimed schedule and enqueues it, then records
// last_run_id. Errors here never undo the claim (documented at-most-once).
func (w *Worker) fire(ctx context.Context, sched *Schedule, now time.Time) {
	runID := ""
	if w.runsSvc != nil {
		// Tenant guard: the run row is created with the schedule's own
		// organization_id (never client-supplied).
		run, err := w.runsSvc.CreateRunCtx(ctx, sched.OrganizationID, sched.AgentID, sched.Input)
		if err != nil {
			w.logr.Error("scheduler: run creation failed",
				"schedule_id", sched.ID, "organization_id", sched.OrganizationID,
				"agent_id", sched.AgentID, "error", err.Error())
			return
		}
		runID = run.ID
	}
	if w.queueSvc != nil {
		payload := map[string]any{
			"organization_id": sched.OrganizationID,
			"agent_id":        sched.AgentID,
			"input":           sched.Input,
			"schedule_id":     sched.ID,
			"trigger":         "schedule",
		}
		if runID != "" {
			payload["run_id"] = runID
		}
		if task := w.queueSvc.Enqueue(scheduleTaskType, payload); task == nil {
			w.logr.Error("scheduler: enqueue failed", "schedule_id", sched.ID, "run_id", runID)
			return
		}
	}
	if runID != "" {
		if err := w.svc.SetLastRun(ctx, sched.ID, runID); err != nil {
			w.logr.Warn("scheduler: could not record last_run_id",
				"schedule_id", sched.ID, "run_id", runID, "error", err.Error())
		}
	}
	w.logr.Info("scheduler: fired",
		"schedule_id", sched.ID,
		"organization_id", sched.OrganizationID,
		"kind", sched.Kind,
		"run_id", runID,
		"fired_at", now.Format(time.RFC3339))
}
