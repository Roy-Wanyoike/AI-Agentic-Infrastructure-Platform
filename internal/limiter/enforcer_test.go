package limiter

// enforcer_test.go — the enforcement facade (issue #124 requirement 3 + 4):
// org+agent aggregation for run creation, tool/model scope checks, TPM
// post-call accounting and the rate_limited_total{scope,org} counter through
// the observability seam.

import (
	"context"
	"testing"
	"time"

	"agentos/internal/observability"
)

func newEnforcer(t *testing.T, store *MemoryStore, opts ...EnforcerOption) (*Enforcer, func(time.Duration)) {
	t.Helper()
	// ONE resolver shared by the engine and the Enforcer — the same wiring
	// newApp uses, so UpsertConfig/DeleteConfig invalidation reaches the
	// per-check resolution path immediately.
	_, clock, advance := newTestClock()
	res := NewResolver(store, WithResolverClock(clock))
	l := NewLimiter(nil, res, WithClock(clock))
	return NewEnforcer(l, res, opts...), advance
}

func TestAllowRunChecksOrgAndAgentScopes(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeOrg, RPM: 3, Enabled: true})
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 2, Enabled: true})
	e, _ := newEnforcer(t, store)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if d := e.AllowRun(ctx, "org-1", "a1"); !d.Allowed {
			t.Fatalf("call %d denied: %+v", i, d)
		}
	}
	// 3rd call: agent RPM (2/2) denies while the org window still has one.
	d := e.AllowRun(ctx, "org-1", "a1")
	if d.Allowed || d.Scope != ScopeAgent {
		t.Fatalf("3rd call must deny on the agent scope: %+v", d)
	}
	if got := d.Remaining["org:rpm"]; got != 1 {
		t.Fatalf("org remaining must still be reported: %d", got)
	}
	// A different agent still fits under the org scope (3rd org request).
	if d := e.AllowRun(ctx, "org-1", "a2"); !d.Allowed {
		t.Fatalf("a2 call must be allowed by the org window: %+v", d)
	}
	// 4th org request: the org scope denies.
	d = e.AllowRun(ctx, "org-1", "a2")
	if d.Allowed || d.Scope != ScopeOrg {
		t.Fatalf("4th org call must deny on the org scope: %+v", d)
	}
}

func TestAllowRunAggregatesRetryAfter(t *testing.T) {
	store := NewMemoryStore()
	// Org default RPD 1 + agent RPM 1: the 2nd call denies on BOTH windows;
	// the aggregated retry is the shorter one (minute window).
	upsert(t, store, "org-1", Config{Scope: ScopeOrg, RPD: 1, Enabled: true})
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, RPM: 1, Enabled: true})
	e, _ := newEnforcer(t, store)
	ctx := context.Background()

	if d := e.AllowRun(ctx, "org-1", "a1"); !d.Allowed {
		t.Fatalf("first call: %+v", d)
	}
	d := e.AllowRun(ctx, "org-1", "a1")
	if d.Allowed {
		t.Fatal("second call must deny")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > MinuteWindow {
		t.Fatalf("aggregated retry must be the minimum over denied windows: %v", d.RetryAfter)
	}
}

func TestAllowToolDenialConsumesNothing(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeTool, Subject: "http_request", RPM: 1, Enabled: true})
	e, advance := newEnforcer(t, store)
	ctx := context.Background()

	if d := e.AllowTool(ctx, "org-1", "http_request"); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	for i := 0; i < 3; i++ {
		if d := e.AllowTool(ctx, "org-1", "http_request"); d.Allowed {
			t.Fatalf("denied call %d must not pass", i)
		}
	}
	advance(MinuteWindow + time.Second)
	if d := e.AllowTool(ctx, "org-1", "http_request"); !d.Allowed {
		t.Fatalf("one slot after the window: %+v", d)
	}
	// Unrelated tools are unaffected.
	if d := e.AllowTool(ctx, "org-1", "calculator"); !d.Allowed {
		t.Fatalf("unlimited tool: %+v", d)
	}
}

func TestModelRPMThenTPMAccounting(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeModel, Subject: "gpt-test", RPM: 2, TPM: 100, Enabled: true})
	e, advance := newEnforcer(t, store)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if d := e.AllowModel(ctx, "org-1", "gpt-test", Tokens{Requests: 1}); !d.Allowed {
			t.Fatalf("call %d: %+v", i, d)
		}
		e.ReportModelTokens(ctx, "org-1", "gpt-test", 40)
	}
	// 3rd call: RPM (2/2) denies with 0 remaining; TPM (80/100) still fits.
	d := e.AllowModel(ctx, "org-1", "gpt-test", Tokens{Requests: 1})
	if d.Allowed {
		t.Fatal("3rd call must deny on rpm")
	}
	if got := d.Remaining["model:rpm"]; got != 0 {
		t.Fatalf("rpm remaining = %d, want 0: %+v", got, d)
	}
	if got := d.Remaining["model:tpm"]; got != 20 {
		t.Fatalf("tpm remaining = %d, want 20: %+v", got, d)
	}
	// A zero-token report is ignored; a real report lands in the window.
	e.ReportModelTokens(ctx, "org-1", "gpt-test", 0)
	e.ReportModelTokens(ctx, "org-1", "gpt-test", 30) // 110 > 100
	// The 4th call at the same instant is denied by BOTH windows: RPM is
	// full (2/2) and the TPM pre-call check sees 110 recorded tokens against
	// the 100 cap — accounting-only reports deny new calls.
	d = e.AllowModel(ctx, "org-1", "gpt-test", Tokens{Requests: 1})
	if d.Allowed {
		t.Fatal("4th call must deny with the TPM window over cap")
	}
	if got := d.Remaining["model:tpm"]; got != 0 {
		t.Fatalf("tpm remaining = %d, want 0", got)
	}
	if got := d.Remaining["model:rpm"]; got != 0 {
		t.Fatalf("rpm remaining = %d, want 0", got)
	}
	// Past the minute BOTH windows slide together (TPM is a tokens-PER-MINUTE
	// budget over the same one-minute window as RPM, so the accounting-only
	// overage ages out with the RPM usage).
	advance(MinuteWindow + time.Second)
	if d := e.AllowModel(ctx, "org-1", "gpt-test", Tokens{Requests: 1}); !d.Allowed {
		t.Fatalf("both windows freed past the minute: %+v", d)
	}
}

func TestEnforcerMetrics(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 1, Enabled: true})
	m := observability.NewMetrics()
	e, _ := newEnforcer(t, store, WithMetrics(m))
	ctx := context.Background()

	if d := e.AllowRun(ctx, "org-1", "a1"); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if d := e.AllowRun(ctx, "org-1", "a1"); d.Allowed {
		t.Fatal("second must deny")
	}
	counts, gauges := m.Snapshot()
	if got := counts[MetricKey(ScopeAgent, "org-1")]; got != 1 {
		t.Fatalf("agent denial counter = %d, want 1: %v", got, counts)
	}
	if got := counts[MetricKey(ScopeOrg, "org-1")]; got != 0 {
		t.Fatalf("org counter must be untouched: %d", got)
	}
	// The remaining gauge reflects the last decision for the window
	// (last-value gauge through Observe; exposed by the Prometheus render).
	if _, ok := gauges[RemainingGaugeKey(ScopeAgent, "org-1", windowRPM)]; !ok {
		t.Fatalf("remaining gauge missing: %v", gauges)
	}
}

func TestEnforcerCRUDInvalidatesCache(t *testing.T) {
	store := NewMemoryStore()
	e, _ := newEnforcer(t, store)
	ctx := context.Background()

	cfg := Config{Scope: ScopeAgent, Subject: "a1", RPM: 1, Enabled: true}
	if err := e.UpsertConfig(ctx, "org-1", cfg); err != nil {
		t.Fatalf("UpsertConfig: %v", err)
	}
	if d := e.AllowRun(ctx, "org-1", "a1"); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if d := e.AllowRun(ctx, "org-1", "a1"); d.Allowed {
		t.Fatal("second must deny")
	}
	// Raising the limit is visible IMMEDIATELY (no TTL wait).
	cfg.RPM = 10
	if err := e.UpsertConfig(ctx, "org-1", cfg); err != nil {
		t.Fatalf("UpsertConfig 2: %v", err)
	}
	if d := e.AllowRun(ctx, "org-1", "a1"); !d.Allowed {
		t.Fatalf("raised limit must apply at once: %+v", d)
	}
	// Delete restores unlimited immediately.
	if err := e.DeleteConfig(ctx, "org-1", ScopeAgent, "a1"); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
	if d := e.AllowRun(ctx, "org-1", "a1"); !d.Allowed {
		t.Fatalf("deleted limits never deny: %+v", d)
	}
	// Deleting an absent row is ErrConfigNotFound.
	if err := e.DeleteConfig(ctx, "org-1", ScopeAgent, "a1"); err != ErrConfigNotFound {
		t.Fatalf("want ErrConfigNotFound, got %v", err)
	}
}

func TestNoopGate(t *testing.T) {
	ctx := context.Background()
	if d := Noop.AllowRun(ctx, "org", "agent"); !d.Allowed {
		t.Fatalf("noop must allow: %+v", d)
	}
	if d := Noop.AllowTool(ctx, "org", "tool"); !d.Allowed {
		t.Fatalf("noop must allow: %+v", d)
	}
	if d := Noop.AllowModel(ctx, "org", "model", Tokens{Requests: 1, Tokens: 5}); !d.Allowed {
		t.Fatalf("noop must allow: %+v", d)
	}
	Noop.ReportModelTokens(ctx, "org", "model", 100) // must not panic
	// The Enforcer implements the same seam (consumer-side interfaces).
	var _ Gate = Noop
}
