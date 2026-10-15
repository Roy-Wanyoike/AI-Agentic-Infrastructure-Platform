package main

// shutdown_test.go — issue #107 tests for the worker graceful-shutdown wiring
// (cmd/worker/shutdown.go + the orgRunnerCache stop semantics). Short grace
// durations (tens of milliseconds) keep the suite fast; the in-flight run is
// gated on a channel the test controls, so nothing here races a fixed sleep.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agentos/internal/agents"
	"agentos/internal/models"
	"agentos/internal/runtime"
	"agentos/internal/tools"
)

// gatedWorkerProvider blocks every Complete call until the test closes the
// release channel or the run context is cancelled (a real provider under
// cancellation).
type gatedWorkerProvider struct {
	mu      sync.Mutex
	release chan struct{}
}

func (p *gatedWorkerProvider) Name() string { return "gated-worker" }

func (p *gatedWorkerProvider) Complete(ctx context.Context, _ models.CompletionRequest) (*models.CompletionResponse, error) {
	p.mu.Lock()
	release := p.release
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-release:
		return &models.CompletionResponse{Text: "done", Model: "gated-worker"}, nil
	}
}

// waitForInFlight polls the runner gate until it holds want runs (bounded
// poll; the sleep is a poll interval, not a timing assumption).
func waitForInFlightWorker(t *testing.T, r *runtime.Runner, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.InFlight() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("runner never reached in-flight=%d (now %d)", want, r.InFlight())
}

func newWorkerAgent(t *testing.T, svc *agents.Service) *agents.Agent {
	t.Helper()
	agent, err := svc.Create("org-1", "Worker Agent", "shutdown test", "instructions", "test-model")
	if err != nil {
		t.Fatalf("agent create failed: %v", err)
	}
	return agent
}

func TestDrainRunnersIdleReturnsQuicklyAndStopsAccepting(t *testing.T) {
	base := runtime.NewRunner(agents.NewService(), tools.NewRegistry())
	orgRunners := newOrgRunnerCache(agents.NewService(), nil, nil, nil, nil, nil, nil, slog.Default())

	start := time.Now()
	drainRunners(base, orgRunners, 5*time.Second, slog.Default())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("idle drain must return immediately, took %v", elapsed)
	}

	// Every runner of the process refuses new runs after the drain.
	agent := newWorkerAgent(t, agents.NewService())
	if _, err := base.RunWithID(context.Background(), "run-base", agent.ID, "x"); !errors.Is(err, runtime.ErrShuttingDown) {
		t.Fatalf("base runner must refuse runs after drain, got %v", err)
	}
	orgRunner := orgRunners.forOrg(context.Background(), "org-1")
	if _, err := orgRunner.RunWithID(context.Background(), "run-org", agent.ID, "x"); !errors.Is(err, runtime.ErrShuttingDown) {
		t.Fatalf("org runner must refuse runs after drain, got %v", err)
	}
}

func TestDrainRunnersHonorsShortGraceWithInflightRun(t *testing.T) {
	release := make(chan struct{})
	provider := &gatedWorkerProvider{release: release}
	agentService := agents.NewService()
	agent := newWorkerAgent(t, agentService)

	base := runtime.NewRunnerWithOptions(agentService, tools.NewRegistry(), runtime.WithProvider(provider))
	orgRunners := newOrgRunnerCache(agentService, provider, nil, nil, nil, nil, nil, slog.Default())

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if _, err := base.Run(context.Background(), agent.ID, "x"); err != nil {
			t.Errorf("run returned error: %v", err)
		}
	}()
	waitForInFlightWorker(t, base, 1)

	// The 50ms grace is honored: the drain waits for it (the run is gated) and
	// then gives up with a logged timeout — it does not cancel the run and it
	// does not block for the run's full (unbounded) duration.
	start := time.Now()
	drainRunners(base, orgRunners, 50*time.Millisecond, slog.Default())
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("grace period not honored: %v", elapsed)
	}
	select {
	case <-runDone:
		t.Fatal("the in-flight run must NOT be cancelled by the shutdown path")
	default:
	}

	// Draining runs are never aborted: release the run, it completes normally,
	// and a fresh (bounded) Shutdown succeeds immediately.
	close(release)
	<-runDone
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := base.Shutdown(ctx); err != nil {
		t.Fatalf("drained runner must shut down cleanly, got %v", err)
	}
}

func TestDrainRunnersWaitsForInflightRunWithinGrace(t *testing.T) {
	release := make(chan struct{})
	provider := &gatedWorkerProvider{release: release}
	agentService := agents.NewService()
	agent := newWorkerAgent(t, agentService)

	base := runtime.NewRunnerWithOptions(agentService, tools.NewRegistry(), runtime.WithProvider(provider))
	orgRunners := newOrgRunnerCache(agentService, provider, nil, nil, nil, nil, nil, slog.Default())

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if _, err := base.Run(context.Background(), agent.ID, "x"); err != nil {
			t.Errorf("run returned error: %v", err)
		}
	}()
	waitForInFlightWorker(t, base, 1)

	// The run finishes well within the grace period: drainRunners must WAIT
	// for it (not time out) and return with everything drained.
	go func() {
		time.Sleep(20 * time.Millisecond) // well under the 2s grace below
		close(release)
	}()
	start := time.Now()
	drainRunners(base, orgRunners, 2*time.Second, slog.Default())
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("drain must return as soon as the run drained, took %v", elapsed)
	}
	<-runDone
}

func TestOrgRunnerCacheStopAllRefusesOldAndNewRunners(t *testing.T) {
	agentService := agents.NewService()
	orgRunners := newOrgRunnerCache(agentService, nil, nil, nil, nil, nil, nil, slog.Default())
	// issue #107 wiring setters are startup-only; exercise them (zero values
	// keep the safe defaults).
	orgRunners.setRuntimeConfig(runtime.RuntimeConfigFromEnv())
	orgRunners.setEventPublisher(nil)

	agent := newWorkerAgent(t, agentService)
	cached := orgRunners.forOrg(context.Background(), "org-1")
	if run, err := cached.Run(context.Background(), agent.ID, "hello"); err != nil || run.Status != runtime.StatusCompleted {
		t.Fatalf("offline run on a live org runner must complete: %v / %+v", err, run)
	}

	orgRunners.stopAll()
	if _, err := cached.RunWithID(context.Background(), "run-1", agent.ID, "x"); !errors.Is(err, runtime.ErrShuttingDown) {
		t.Fatalf("cached runner must refuse runs after stopAll, got %v", err)
	}
	// A runner built after stopAll is born stopped (no run can slip through
	// the drain on a fresh org).
	fresh := orgRunners.forOrg(context.Background(), "org-2")
	if _, err := fresh.RunWithID(context.Background(), "run-2", agent.ID, "x"); !errors.Is(err, runtime.ErrShuttingDown) {
		t.Fatalf("runner built after stopAll must refuse runs, got %v", err)
	}
	// stopAll is idempotent and shutdownAll on an idle cache drains clean.
	orgRunners.stopAll()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := orgRunners.shutdownAll(ctx); err != nil {
		t.Fatalf("idle shutdownAll must drain clean, got %v", err)
	}
}

func TestShutdownGraceEnvFeedsDrainPath(t *testing.T) {
	// The worker reads AGENTOS_WORKER_GRACE_SECONDS through
	// runtime.ShutdownGraceFromEnv; short explicit values are honored verbatim
	// so operators can tune the drain (and tests run with tiny durations).
	t.Setenv(runtime.EnvShutdownGrace, "1")
	if got := runtime.ShutdownGraceFromEnv(); got != time.Second {
		t.Fatalf("explicit grace must be honored, got %v", got)
	}
	t.Setenv(runtime.EnvShutdownGrace, "bogus")
	if got := runtime.ShutdownGraceFromEnv(); got != runtime.DefaultShutdownGrace {
		t.Fatalf("invalid grace must keep the default, got %v", got)
	}
}
