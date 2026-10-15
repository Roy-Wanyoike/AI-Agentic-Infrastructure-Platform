package simulation

// service_test.go — issue #133 service contract: org isolation (the
// non-negotiable tenant guard), async status lifecycle, and copy semantics.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// newTestService builds a service with the deterministic clock and a fixed
// id generator (ids count up: sim-1, sim-2, ...).
func newTestService() *Service {
	n := 0
	return NewService(
		WithRunner(NewRunner(WithClock(testClock))),
		WithIDGenerator(func() string { n++; return "sim-" + string(rune('0'+n)) }),
	)
}

// simpleCfg builds a minimal valid config.
func simpleCfg() SimConfig {
	return runConfig(KindSocial, 3, 5, []SimAgentPersona{persona("a", "Ada", nil, nil)}, nil)
}

// TestServiceOrgIsolation pins the tenant guard: foreign-org reads are
// ErrNotFound (indistinguishable from unknown ids — no existence leak),
// and listings only ever contain the caller's org's records.
func TestServiceOrgIsolation(t *testing.T) {
	svc := newTestService()
	if _, err := svc.Create(context.Background(), "org-1", simpleCfg()); err != nil {
		t.Fatalf("create org-1: %v", err)
	}
	if _, err := svc.Create(context.Background(), "org-2", simpleCfg()); err != nil {
		t.Fatalf("create org-2: %v", err)
	}

	// list is org-scoped
	l1, err := svc.List("org-1")
	if err != nil {
		t.Fatalf("list org-1: %v", err)
	}
	l2, err := svc.List("org-2")
	if err != nil {
		t.Fatalf("list org-2: %v", err)
	}
	if len(l1) != 1 || len(l2) != 1 {
		t.Fatalf("list sizes = %d/%d, want 1/1", len(l1), len(l2))
	}
	if l1[0].OrgID != "org-1" || l2[0].OrgID != "org-2" {
		t.Fatalf("list org leakage: %s / %s", l1[0].OrgID, l2[0].OrgID)
	}

	// cross-org GET is the same 404 as a missing id
	if _, err := svc.Get("org-2", l1[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org get = %v, want ErrNotFound", err)
	}
	if _, err := svc.Get("org-1", "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown get = %v, want ErrNotFound", err)
	}
}

// TestServiceAsyncLifecycle pins the queued -> running -> completed flow
// with the result landing on the record, and Create-time validation (422s).
func TestServiceAsyncLifecycle(t *testing.T) {
	svc := newTestService()
	sim, err := svc.Create(context.Background(), "org-1", simpleCfg())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sim.Status != StatusQueued && sim.Status != StatusRunning {
		t.Fatalf("initial status = %q", sim.Status)
	}
	// wait (bounded) for the async run to land
	deadline := time.Now().Add(5 * time.Second)
	for {
		sim, err = svc.Get("org-1", sim.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if sim.Status == StatusComplete || sim.Status == StatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("simulation did not finish: status=%q", sim.Status)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if sim.Status != StatusComplete {
		t.Fatalf("status = %q error=%q", sim.Status, sim.Error)
	}
	if sim.Result == nil || len(sim.Result.Rounds) != 3 {
		t.Fatalf("result rounds = %v", sim.Result)
	}
	if sim.Config.OrgID != "org-1" {
		t.Fatalf("config org = %q, want claimed org", sim.Config.OrgID)
	}

	// invalid configs are rejected BEFORE the record exists (fail fast)
	bad := simpleCfg()
	bad.Kind = "warp-drive"
	if _, err := svc.Create(context.Background(), "org-1", bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad kind create = %v, want ErrInvalid", err)
	}
	if _, err := svc.Create(context.Background(), "", simpleCfg()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty org create = %v, want ErrInvalid", err)
	}
	// tenant override attempts are ignored: cfg.OrgID is force-stamped
	spoof := simpleCfg()
	spoof.OrgID = "org-evil"
	s2, err := svc.Create(context.Background(), "org-1", spoof)
	if err != nil {
		t.Fatalf("spoof create: %v", err)
	}
	if s2.Config.OrgID != "org-1" {
		t.Fatalf("client-supplied org leaked: %q", s2.Config.OrgID)
	}
}

// TestServiceFailurePath: a run that errors lands in the terminal FAILED
// status with the error message preserved (here: a broken custom world that
// always fails Initialize).
func TestServiceFailurePath(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterEnvironment("broken", func() SimulationEnvironment {
		return brokenEnv{}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := NewService(WithRunner(NewRunner(WithRegistry(reg), WithClock(testClock))))
	cfg := runConfig("broken", 3, 1, []SimAgentPersona{persona("a", "Ada", nil, nil)}, nil)
	sim, err := svc.Create(context.Background(), "org-1", cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		sim, err = svc.Get("org-1", sim.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if sim.Status == StatusFailed || sim.Status == StatusComplete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("simulation did not finish: status=%q", sim.Status)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if sim.Status != StatusFailed || !strings.Contains(sim.Error, "boom") {
		t.Fatalf("status=%q error=%q, want failed with boom message", sim.Status, sim.Error)
	}
	if sim.Result != nil {
		t.Fatal("failed run must not carry a result")
	}
}

// brokenEnv is a custom world whose Initialize always fails (failure-path
// injection; also proves broken worlds cannot corrupt the service).
type brokenEnv struct{}

// Initialize implements SimulationEnvironment — always fails.
func (brokenEnv) Initialize(context.Context, EnvConfig) error { return errors.New("boom") }

// Step implements SimulationEnvironment (never reached).
func (brokenEnv) Step(context.Context, []SimAgent) ([]ActionResult, error) { return nil, nil }

// GetState implements SimulationEnvironment.
func (brokenEnv) GetState(context.Context) (EnvState, error) { return EnvState{}, nil }

// Close implements SimulationEnvironment.
func (brokenEnv) Close() error { return nil }
