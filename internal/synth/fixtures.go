package synth

// fixtures.go — issue #149 (10-q): fixture sets for journey runs and the
// synthetic-record guards that keep test data strictly synthetic and
// explicitly torn down.
//
// Contract:
//   - A FixtureSet provisions the state a journey needs (synthetic users,
//     synthetic records, synthetic payment methods) and hands back an
//     explicit Teardown. Setup may create ONLY synthetic records — names are
//     namespaced `synth-<persona>-<random>` and validated by the guards.
//   - Teardown is NEVER implicit: nothing runs in a finalizer, a background
//     goroutine or a context cancel hook. The ONLY way state disappears is
//     an explicit call to SyntheticRecordGuards.TeardownAll, and every
//     attempt is appended to an auditable log (TeardownAuditEntry).
//   - Failed teardowns STAY in the registry (retryable) and are surfaced as
//     errors — a fixture leak is a finding, not a silent best effort.

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Teardown explicitly restores the state created by a FixtureSet.Setup.
// Implementations must be idempotent-friendly: TeardownAll may retry a
// teardown after a partial failure.
type Teardown interface {
	Teardown(ctx context.Context) error
}

// TeardownFunc adapts a function to the Teardown interface.
type TeardownFunc func(ctx context.Context) error

// Teardown implements Teardown.
func (f TeardownFunc) Teardown(ctx context.Context) error { return f(ctx) }

// ComposeTeardown returns a single Teardown that runs the given teardowns in
// order and joins their errors (all of them run — a middle failure never
// skips the rest, mirroring TeardownAll semantics).
func ComposeTeardown(ts ...Teardown) Teardown {
	return TeardownFunc(func(ctx context.Context) error {
		var errs []error
		for i, t := range ts {
			if t == nil {
				continue
			}
			if err := t.Teardown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("teardown[%d]: %w", i, err))
			}
		}
		switch len(errs) {
		case 0:
			return nil
		case 1:
			return errs[0]
		default:
			return fmt.Errorf("%w: %d teardown failures", ErrInvalid, len(errs))
		}
	})
}

// FixtureSet provisions synthetic-only state for a journey run and returns
// the explicit Teardown for it. Setup receives the run's context: honoring
// ctx cancellation DURING setup is correct; there is no implicit cleanup
// afterwards — whatever was created must be torn down through the guards
// registry even when setup fails midway (register partial state first).
type FixtureSet interface {
	Setup(ctx context.Context) (Teardown, error)
}

// FixtureSetFunc adapts a setup function to the FixtureSet interface.
type FixtureSetFunc func(ctx context.Context) (Teardown, error)

// Setup implements FixtureSet.
func (f FixtureSetFunc) Setup(ctx context.Context) (Teardown, error) { return f(ctx) }

// syntheticRecordNameRe pins the namespaced naming convention:
// synth-<persona>-<random> where persona is a lowercase slug and random is
// at least 6 characters of [a-z0-9] (see RandomSuffix).
var syntheticRecordNameRe = regexp.MustCompile(`^synth-[a-z0-9_]{1,64}-[a-z0-9]{6,64}$`)

// recordKindRe pins the record kind vocabulary shape (lowercase identifier).
var recordKindRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// personaSlugRe pins the persona component of synthetic names.
var personaSlugRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// SyntheticRecordName returns the canonical synthetic test record name for a
// persona: "synth-<persona>-<random>". Records that do not match this
// convention are refused by the guards — real customer data can never be
// adopted into the synthetic test corpus by accident.
func SyntheticRecordName(persona, random string) (string, error) {
	if !personaSlugRe.MatchString(persona) {
		return "", fmt.Errorf("%w: persona slug %q must match [a-z0-9_]{1,64}", ErrInvalid, persona)
	}
	if !regexp.MustCompile(`^[a-z0-9]{6,64}$`).MatchString(random) {
		return "", fmt.Errorf("%w: random suffix %q must match [a-z0-9]{6,64}", ErrInvalid, random)
	}
	return "synth-" + persona + "-" + random, nil
}

// IsSyntheticRecordName reports whether name follows the
// synth-<persona>-<random> convention.
func IsSyntheticRecordName(name string) bool {
	return syntheticRecordNameRe.MatchString(name)
}

// RandomSuffix returns 8 bytes of crypto entropy as 16 lowercase hex chars —
// the default random component for synthetic record names.
func RandomSuffix() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic("synth: entropy source unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// TeardownEntry is one registered synthetic record awaiting explicit teardown.
type TeardownEntry struct {
	Kind         string // e.g. "user", "record", "payment_method"
	Name         string // synth-<persona>-<random>
	Persona      string
	RegisteredAt time.Time
}

// TeardownAuditEntry is one append-only audit record of a teardown attempt.
// The registry is the audit trail: what was created, when, by which persona
// profile, and whether the explicit teardown succeeded.
type TeardownAuditEntry struct {
	Kind string
	Name string
	OK   bool
	Err  string
	At   time.Time
}

// SyntheticRecordGuards is the explicit, auditable teardown registry for
// synthetic test records created during fixture setup. All methods are
// safe for concurrent use. There is NO implicit cleanup path: no finalizers,
// no timers, no context hooks — only TeardownAll (or a retry of it) removes
// registered state.
type SyntheticRecordGuards struct {
	persona string

	mu        sync.Mutex
	now       func() time.Time
	rng       func() string
	registry  map[string]TeardownEntry // name → entry (pending teardown)
	teardowns map[string]Teardown      // name → teardown capability (same lock as registry)
	order     []string                 // registration order (for LIFO teardown)
	audit     []TeardownAuditEntry
}

// NewSyntheticRecordGuards returns a guard registry bound to one persona
// slug (used for name namespacing and audit attribution).
func NewSyntheticRecordGuards(persona string) (*SyntheticRecordGuards, error) {
	if !personaSlugRe.MatchString(persona) {
		return nil, fmt.Errorf("%w: guard persona slug %q must match [a-z0-9_]{1,64}", ErrInvalid, persona)
	}
	return &SyntheticRecordGuards{
		persona:   persona,
		now:       time.Now,
		rng:       RandomSuffix,
		registry:  make(map[string]TeardownEntry),
		teardowns: make(map[string]Teardown),
	}, nil
}

// Persona returns the persona slug the guards are bound to.
func (g *SyntheticRecordGuards) Persona() string { return g.persona }

// NewName returns a fresh synthetic record name under this persona.
func (g *SyntheticRecordGuards) NewName() string {
	name, err := SyntheticRecordName(g.persona, g.rng())
	if err != nil { // rng is ours; unreachable unless injected badly
		panic("synth: guard name generation failed: " + err.Error())
	}
	return name
}

// Register adds a created record to the explicit teardown registry. The name
// MUST follow the synthetic naming convention and MUST NOT already be
// pending — duplicates indicate a fixture bug and are refused loudly.
func (g *SyntheticRecordGuards) Register(kind, name string, t Teardown) error {
	if !recordKindRe.MatchString(kind) {
		return fmt.Errorf("%w: record kind %q must match [a-z][a-z0-9_]{0,63}", ErrInvalid, kind)
	}
	if !IsSyntheticRecordName(name) {
		return fmt.Errorf("%w: record name %q violates the synth-<persona>-<random> convention", ErrInvalid, name)
	}
	if t == nil {
		return fmt.Errorf("%w: teardown for %s must not be nil", ErrInvalid, name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, pending := g.registry[name]; pending {
		return fmt.Errorf("%w: record %s is already registered for teardown", ErrInvalid, name)
	}
	g.registry[name] = TeardownEntry{
		Kind:         kind,
		Name:         name,
		Persona:      g.persona,
		RegisteredAt: g.now(),
		// teardown capability stored out-of-band (unexported map below).
	}
	g.teardowns[name] = t
	g.order = append(g.order, name)
	return nil
}

// RegisterFunc is Register with a function teardown.
func (g *SyntheticRecordGuards) RegisterFunc(kind, name string, fn func(ctx context.Context) error) error {
	return g.Register(kind, name, TeardownFunc(fn))
}

// Pending returns the records still awaiting teardown, oldest registration
// first. The returned entries carry no teardown capability.
func (g *SyntheticRecordGuards) Pending() []TeardownEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]TeardownEntry, 0, len(g.order))
	for _, name := range g.order {
		if e, ok := g.registry[name]; ok {
			out = append(out, e)
		}
	}
	return out
}

// Audit returns a copy of the teardown audit log (chronological).
func (g *SyntheticRecordGuards) Audit() []TeardownAuditEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]TeardownAuditEntry, len(g.audit))
	copy(out, g.audit)
	return out
}

// TeardownAll runs the pending teardowns EXPLICITLY, newest registration
// first (LIFO: dependent records usually die before their parents), audits
// every attempt, and removes successful entries from the registry. Failed
// teardowns STAY registered so a later TeardownAll can retry them; all
// pending teardowns run even after failures (errors are joined). Nothing
// happens implicitly — if this method is never called, the records remain
// and the registry keeps accounting for them.
func (g *SyntheticRecordGuards) TeardownAll(ctx context.Context) ([]TeardownAuditEntry, error) {
	g.mu.Lock()
	// Snapshot LIFO order; only currently-registered entries.
	var names []string
	for i := len(g.order) - 1; i >= 0; i-- {
		name := g.order[i]
		if _, ok := g.registry[name]; ok {
			names = append(names, name)
		}
	}
	type job struct {
		entry TeardownEntry
		t     Teardown
	}
	jobs := make([]job, 0, len(names))
	for _, name := range names {
		jobs = append(jobs, job{entry: g.registry[name], t: g.teardowns[name]})
	}
	g.mu.Unlock()

	var (
		errs  []error
		audit = make([]TeardownAuditEntry, 0, len(jobs))
	)
	for _, jb := range jobs {
		err := jb.t.Teardown(ctx)
		entry := TeardownAuditEntry{
			Kind: jb.entry.Kind,
			Name: jb.entry.Name,
			OK:   err == nil,
			At:   g.now(),
		}
		if err != nil {
			entry.Err = err.Error()
			errs = append(errs, fmt.Errorf("teardown %s (%s): %w", jb.entry.Name, jb.entry.Kind, err))
		}
		audit = append(audit, entry)

		g.mu.Lock()
		if err == nil {
			delete(g.registry, jb.entry.Name)
			delete(g.teardowns, jb.entry.Name)
		}
		g.audit = append(g.audit, entry)
		g.mu.Unlock()
	}

	// Compact the registration order of fully-torn-down names.
	g.mu.Lock()
	kept := g.order[:0]
	for _, name := range g.order {
		if _, ok := g.registry[name]; ok {
			kept = append(kept, name)
		}
	}
	g.order = kept
	g.mu.Unlock()

	switch len(errs) {
	case 0:
		return audit, nil
	case 1:
		return audit, errs[0]
	default:
		return audit, fmt.Errorf("%w: %d/%d teardowns failed", ErrInvalid, len(errs), len(jobs))
	}
}
