package synth

// fixtures_test.go — issue #149: naming convention, explicit teardown
// registry semantics (LIFO, auditable, no implicit cleanup) and the
// FixtureSet/Teardown adapters.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSyntheticRecordName(t *testing.T) {
	tests := []struct {
		persona string
		random  string
		want    string
		wantErr bool
	}{
		{PersonaVisitor, "a1b2c3d4", "synth-visitor-a1b2c3d4", false},
		{PersonaNewUser, "0123456789abcdef", "synth-new_user-0123456789abcdef", false},
		{"custom_persona", "zz99xx", "synth-custom_persona-zz99xx", false},
		{"", "a1b2c3d4", "", true},
		{"Bad Persona", "a1b2c3d4", "", true}, // spaces/casing
		{"visitor", "short", "", true},        // < 6 random chars
		{"visitor", "A1B2C3", "", true},       // uppercase random
		{"visitor", "", "", true},             // empty random
	}
	for _, tc := range tests {
		got, err := SyntheticRecordName(tc.persona, tc.random)
		if tc.wantErr {
			if err == nil || !errors.Is(err, ErrInvalid) {
				t.Errorf("SyntheticRecordName(%q,%q) err = %v, want ErrInvalid", tc.persona, tc.random, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("SyntheticRecordName(%q,%q) unexpected err: %v", tc.persona, tc.random, err)
			continue
		}
		if got != tc.want {
			t.Errorf("SyntheticRecordName(%q,%q) = %q, want %q", tc.persona, tc.random, got, tc.want)
		}
		if !IsSyntheticRecordName(got) {
			t.Errorf("IsSyntheticRecordName(%q) = false for its own output", got)
		}
	}
	// Anything that could be real customer data must fail the check.
	for _, bad := range []string{"cust-123456", "user_42", "synth", "synth-", "synth-alice", "synth-alice-XY", "SYNTH-alice-abc123", "synth-alice-", "synth--abc123"} {
		if IsSyntheticRecordName(bad) {
			t.Errorf("IsSyntheticRecordName(%q) = true, want false", bad)
		}
	}
	if got := RandomSuffix(); len(got) != 16 {
		t.Errorf("RandomSuffix() length = %d, want 16 hex chars", len(got))
	}
}

func newTestGuards(t *testing.T, persona string) *SyntheticRecordGuards {
	t.Helper()
	g, err := NewSyntheticRecordGuards(persona)
	if err != nil {
		t.Fatalf("NewSyntheticRecordGuards: %v", err)
	}
	clock := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	g.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	g.rng = func() string { return "f1e2d3c4" }
	return g
}

func TestSyntheticRecordGuardsRegister(t *testing.T) {
	g := newTestGuards(t, PersonaVisitor)

	name := g.NewName()
	if name != "synth-visitor-f1e2d3c4" {
		t.Fatalf("NewName() = %q", name)
	}
	if err := g.RegisterFunc("user", name, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("RegisterFunc: %v", err)
	}

	tests := []struct {
		name     string
		kind     string
		record   string
		teardown Teardown
		wantErr  error
	}{
		{"nil teardown", "user", "synth-visitor-999999", nil, ErrInvalid},
		{"nil teardown func", "user", "synth-visitor-999998", TeardownFunc(nil), ErrInvalid},
		{"bad kind", "User", "synth-visitor-888888", TeardownFunc(func(ctx context.Context) error { return nil }), ErrInvalid},
		{"empty kind", "", "synth-visitor-777777", TeardownFunc(func(ctx context.Context) error { return nil }), ErrInvalid},
		{"non-synthetic name", "user", "cust-123456", TeardownFunc(func(ctx context.Context) error { return nil }), ErrInvalid},
		{"wrong persona namespace", "user", "synth-administrator-123456", TeardownFunc(func(ctx context.Context) error { return nil }), ErrInvalid},
		{"duplicate pending name", "user", name, TeardownFunc(func(ctx context.Context) error { return nil }), ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := g.Register(tc.kind, tc.record, tc.teardown)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Register(%q,%q) = %v, want %v", tc.kind, tc.record, err, tc.wantErr)
			}
		})
	}

	pending := g.Pending()
	if len(pending) != 1 || pending[0].Name != name || pending[0].Persona != PersonaVisitor {
		t.Fatalf("Pending() = %+v", pending)
	}
	// Nothing ran yet: no implicit cleanup, no audit entries before an
	// explicit TeardownAll call.
	if audit := g.Audit(); len(audit) != 0 {
		t.Fatalf("audit entries exist before any explicit teardown: %+v", audit)
	}
}

func TestTeardownAllLIFOOrderAndAudit(t *testing.T) {
	g := newTestGuards(t, PersonaStandardCustomer)

	var order []string
	mk := func(name string) error {
		return g.RegisterFunc("record", name, func(ctx context.Context) error {
			order = append(order, name)
			return nil
		})
	}
	for _, n := range []string{"synth-standard_customer-aaaaaa", "synth-standard_customer-bbbbbb", "synth-standard_customer-cccccc"} {
		if err := mk(n); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
	audit, err := g.TeardownAll(context.Background())
	if err != nil {
		t.Fatalf("TeardownAll: %v", err)
	}
	// LIFO: newest registration torn down first.
	want := []string{"synth-standard_customer-cccccc", "synth-standard_customer-bbbbbb", "synth-standard_customer-aaaaaa"}
	if !slices.Equal(order, want) {
		t.Fatalf("teardown order = %v, want %v (LIFO)", order, want)
	}
	if len(audit) != 3 {
		t.Fatalf("audit entries = %d, want 3", len(audit))
	}
	for _, a := range audit {
		if !a.OK || a.Err != "" || a.Kind != "record" || a.At.IsZero() {
			t.Fatalf("bad audit entry: %+v", a)
		}
	}
	if len(g.Pending()) != 0 {
		t.Fatalf("registry not empty after successful teardown: %+v", g.Pending())
	}
	if len(g.Audit()) != 3 {
		t.Fatalf("audit log = %d entries, want 3", len(g.Audit()))
	}
	// Second call is a no-op (nothing pending) and stays successful.
	if _, err := g.TeardownAll(context.Background()); err != nil {
		t.Fatalf("second TeardownAll: %v", err)
	}
	if len(g.Audit()) != 3 {
		t.Fatalf("no-op teardown appended audit entries: %d", len(g.Audit()))
	}
}

func TestTeardownAllFailureRetrySemantics(t *testing.T) {
	g := newTestGuards(t, PersonaRestrictedUser)

	calls := 0
	flake := "synth-restricted_user-flaky1"
	ok := "synth-restricted_user-okok12"
	if err := g.RegisterFunc("record", flake, func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("network partition")
		}
		return nil
	}); err != nil {
		t.Fatalf("register flake: %v", err)
	}
	if err := g.RegisterFunc("user", ok, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("register ok: %v", err)
	}

	audit, err := g.TeardownAll(context.Background())
	if err == nil {
		t.Fatalf("TeardownAll with failing teardown = nil error, want failure")
	}
	if !strings.Contains(err.Error(), "network partition") {
		t.Fatalf("err = %v, want joined underlying failure", err)
	}
	if len(audit) != 2 {
		t.Fatalf("audit = %d entries, want 2 (every attempt audited)", len(audit))
	}
	// LIFO: the ok record (registered last) is torn down first; the flaky
	// one is audited as failed with its error message.
	if audit[0].Name != ok || !audit[0].OK {
		t.Fatalf("first attempt = %+v, want %s success", audit[0], ok)
	}
	if audit[1].Name != flake || audit[1].OK || audit[1].Err == "" {
		t.Fatalf("failed attempt not audited: %+v", audit[1])
	}
	// Both teardowns ran despite the first failing (no skipping).
	if calls != 1 {
		t.Fatalf("flake teardown calls = %d, want 1", calls)
	}
	// The failed entry STAYS registered for explicit retry; the successful
	// one is gone.
	pending := g.Pending()
	if len(pending) != 1 || pending[0].Name != flake {
		t.Fatalf("pending after partial failure = %+v, want only %s", pending, flake)
	}
	// Retry succeeds and is audited.
	audit2, err := g.TeardownAll(context.Background())
	if err != nil {
		t.Fatalf("retry TeardownAll: %v", err)
	}
	if calls != 2 || len(audit2) != 1 || !audit2[0].OK {
		t.Fatalf("retry not executed/audited: calls=%d audit=%+v", calls, audit2)
	}
	if len(g.Pending()) != 0 {
		t.Fatalf("registry not empty after retry")
	}
	// Full audit trail: 3 attempts total across both runs.
	if len(g.Audit()) != 3 {
		t.Fatalf("audit log = %d, want 3", len(g.Audit()))
	}
}

func TestTeardownAllHonorsContext(t *testing.T) {
	g := newTestGuards(t, PersonaSlowNetworkUser)
	ctx, cancel := context.WithCancel(context.Background())
	if err := g.RegisterFunc("record", "synth-slow_network_user-abc123", func(ctx context.Context) error {
		cancel() // teardown observes the run context
		return ctx.Err()
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := g.TeardownAll(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestGuardsRejectBadPersona(t *testing.T) {
	if _, err := NewSyntheticRecordGuards("Bad Persona"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	g := newTestGuards(t, PersonaAccessibilityUser)
	if g.Persona() != PersonaAccessibilityUser {
		t.Fatalf("Persona() = %q", g.Persona())
	}
}

func TestFixtureSetFuncAndComposeTeardown(t *testing.T) {
	var events []string
	fs := FixtureSetFunc(func(ctx context.Context) (Teardown, error) {
		events = append(events, "setup")
		return TeardownFunc(func(ctx context.Context) error {
			events = append(events, "teardown")
			return nil
		}), nil
	})
	td, err := fs.Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := td.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !slices.Equal(events, []string{"setup", "teardown"}) {
		t.Fatalf("events = %v", events)
	}

	// ComposeTeardown: all run, errors joined.
	var ran []string
	composed := ComposeTeardown(
		TeardownFunc(func(ctx context.Context) error { ran = append(ran, "a"); return nil }),
		TeardownFunc(func(ctx context.Context) error { ran = append(ran, "b"); return errors.New("boom") }),
		TeardownFunc(func(ctx context.Context) error { ran = append(ran, "c"); return nil }),
	)
	err = composed.Teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("composed err = %v, want boom", err)
	}
	if !slices.Equal(ran, []string{"a", "b", "c"}) {
		t.Fatalf("compose order = %v, want [a b c] (middle failure must not skip)", ran)
	}
}

// fullFixtureRoundTrip wires a FixtureSet through the guards the way a
// journey run is expected to (#148 consumes this contract).
func TestFullFixtureRoundTrip(t *testing.T) {
	g := newTestGuards(t, PersonaNewUser)
	name := g.NewName()
	fs := FixtureSetFunc(func(ctx context.Context) (Teardown, error) {
		if err := g.RegisterFunc("user", name, func(ctx context.Context) error {
			return nil // DELETE /v1/test-users/<name> in production code
		}); err != nil {
			return nil, err
		}
		return g.TeardownAllFunc(), nil
	})
	td, err := fs.Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if len(g.Pending()) != 1 {
		t.Fatalf("setup did not register the synthetic user")
	}
	if err := td.Teardown(context.Background()); err != nil {
		t.Fatalf("explicit teardown: %v", err)
	}
	if len(g.Pending()) != 0 {
		t.Fatalf("pending after explicit teardown: %+v", g.Pending())
	}
	if len(g.Audit()) != 1 || !g.Audit()[0].OK {
		t.Fatalf("audit after teardown: %+v", g.Audit())
	}
}
