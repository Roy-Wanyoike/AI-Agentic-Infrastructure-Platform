package synth

// worker.go — issue #148 (wave-10 track 10-p): the WORKER CONTRACT for the
// Synthetic Assurance subsystem plus the browser (Playwright) worker.
//
// OWNERSHIP: worker.go / worker_test.go / worker_api.go / worker_api_test.go /
// worker_supervisor.go / worker_supervisor_test.go / playwright_cli.go /
// evidence_store.go belong to track 10-p. internal/synth/types.go is the
// pinned cross-track contract (Run, StepEvent, RunStatus, RunKind, Journey,
// JourneyStep, Assertion) and is NOT modified by this track. Sibling-track
// files (service.go, store.go, journeys.go, evidence.go, findings.go, ...)
// are deliberately NOT referenced here — the orchestrator wires workers into
// the registry later.
//
// Execution model (bounded by construction):
//   - every run is driven through the pending→queued→running→terminal state
//     machine below; illegal transitions are programming errors and fail loud;
//   - per-run step budget (Environment.BudgetPerRun, default 200), per-step
//     timeout, wall-clock cap, per-worker in-flight cap, rate cap;
//   - cancellation via parent context (→ RunInconclusive: interrupted runs
//     are NEVER silently lost) and via RunOptions.KillSwitch (→ RunCancelled:
//     a deliberate operator kill);
//   - deterministic assertions are evaluated WORKER-SIDE against driver /
//     HTTP observations — model confidence can never mark a step passed;
//   - StepEvent.Status spells passed | failed | skipped | blocked |
//     inconclusive and those five are distinct outcomes (see StepPassed..).
//
// The Playwright runtime itself is reached through the pluggable
// BrowserDriver seam: tests use the deterministic fake (driverContractFake in
// worker_test.go); playwright_cli.go adapts the real `npx playwright` CLI and
// honestly reports its runtime requirement — it is never faked.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Tunables (worker-local defaults; Environment/RunOptions override).
const (
	// DefaultBudgetPerRun is the per-run step budget when neither
	// RunOptions.MaxSteps nor Environment.BudgetPerRun is set.
	DefaultBudgetPerRun = 200
	// DefaultStepTimeout bounds one step when the journey does not set
	// JourneyStep.TimeoutMS.
	DefaultStepTimeout = 30 * time.Second
	// DefaultRunWallClock bounds a whole run regardless of step progress.
	DefaultRunWallClock = 15 * time.Minute
	// DefaultWorkerInFlight caps concurrent runs per worker instance.
	DefaultWorkerInFlight = 4
	// DefaultRateWindow is the sliding window for Environment.RateCapPerMin.
	DefaultRateWindow = time.Minute
)

// Worker-level errors (contract-wide errors live in types.go; these are
// worker-mechanics sentinels owned by #148).
var (
	// ErrWorkerBusy is returned when the per-worker in-flight cap is hit.
	ErrWorkerBusy = errors.New("synth: worker at in-flight capacity")
	// ErrNoDriver is returned when a BrowserWorker is executed without a
	// driver wired.
	ErrNoDriver = errors.New("synth: browser driver is required")
	// ErrNoWorkerForKind is returned by the supervisor when no worker is
	// registered for the requested RunKind.
	ErrNoWorkerForKind = errors.New("synth: no worker registered for run kind")
	// ErrPrivateTarget is returned when the SSRF guard refuses a
	// private/loopback/link-local destination that is not allowlisted.
	ErrPrivateTarget = errors.New("synth: private/loopback/link-local target refused (not allowlisted)")
	// ErrUnresolvableTarget is returned when a destination host cannot be
	// resolved for the SSRF safety check (fail-closed).
	ErrUnresolvableTarget = errors.New("synth: destination host could not be resolved for scope check")
)

// StepEvent.Status spellings (types.go pins the vocabulary; these constants
// are the canonical spellings used by #148 workers).
const (
	StepPassed       = "passed"
	StepFailed       = "failed"
	StepSkipped      = "skipped"
	StepBlocked      = "blocked"
	StepInconclusive = "inconclusive"
)

// StepEvent.Type spellings.
const (
	EventAction     = "action"
	EventAssertion  = "assertion"
	EventConsole    = "console"
	EventNetwork    = "network"
	EventError      = "error"
	EventNavigation = "navigation"
)

// RedactedPlaceholder replaces redacted secret material in metadata.
const RedactedPlaceholder = "[REDACTED]"

// maxDetailChars bounds failure detail strings kept on events / run.Error.
const maxDetailChars = 512

// RunOptions carries per-execution knobs supplied by the orchestrator. The
// struct is part of the #148 worker contract.
type RunOptions struct {
	// AppVersion pins the build/release under test (release comparison).
	AppVersion string
	// MaxSteps overrides the per-run step budget when > 0.
	MaxSteps int
	// BudgetRef links the run to a billing/budget record (metering seam,
	// consumed by the orchestrator; workers only propagate it).
	BudgetRef string
	// KillSwitch, when closed, cancels the run at the next step boundary
	// (RunCancelled + ErrKilled). Nil means "no kill switch".
	KillSwitch <-chan struct{}
}

// Worker executes one journey run of its Kind. Implementations: BrowserWorker
// (this file), APIWorker (worker_api.go). The orchestrator (future round)
// drives Workers through the supervisor.
//
// Contract: Execute returns (run, nil) whenever the worker itself drove the
// run to a TERMINAL status (completed, failed, cancelled, inconclusive) —
// the outcome lives on the Run record, not in the error. A non-nil error
// means the run never entered execution (validation, capacity, missing
// wiring) and the returned run is nil. ErrKilled / ErrBudgetExceeded /
// ErrScopeViolation messages land in Run.Error with the matching terminal
// status.
type Worker interface {
	Execute(ctx context.Context, j *Journey, env *Environment, p *Persona, opts RunOptions) (*Run, error)
	Kind() RunKind
	Healthy() bool
}

// InFlightCap is optionally implemented by workers to report their
// per-worker concurrency limit to the supervisor (0 = unknown).
type InFlightCap interface {
	InFlightCap() int
}

// Clock is the injectable time source: production uses SystemClock, tests
// use ManualClock for fully deterministic scheduling (no sleeps anywhere in
// this package).
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// SystemClock is the production clock (UTC).
var SystemClock Clock = systemClock{}

// ManualClock is a controllable Clock for deterministic tests.
type ManualClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewManualClock starts a manual clock at the given instant.
func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{t: start.UTC()}
}

// Now implements Clock.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---------------------------------------------------------------------------
// Run state machine (#148): every status move of a synth run is validated.
// ---------------------------------------------------------------------------

// TransitionError describes an illegal RunStatus move. It wraps ErrInvalid.
type TransitionError struct {
	From RunStatus
	To   RunStatus
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("synth: illegal run status transition %s -> %s", e.From, e.To)
}

func (e *TransitionError) Unwrap() error { return ErrInvalid }

// runTransitions is the authoritative #148 transition table. Terminal states
// (completed, failed, cancelled, inconclusive) have no outgoing edges — the
// supervisor's stale-recovery wins over a wedged worker precisely because of
// this.
var runTransitions = map[RunStatus][]RunStatus{
	RunPending:    {RunQueued, RunCancelled, RunFailed},
	RunQueued:     {RunRunning, RunCancelled, RunInconclusive},
	RunRunning:    {RunCompleted, RunFailed, RunCancelled, RunCancelling, RunPaused, RunInconclusive},
	RunPaused:     {RunRunning, RunCancelled, RunInconclusive},
	RunCancelling: {RunCancelled, RunInconclusive},
}

// RunStateMachine validates and applies RunStatus transitions. It is
// stateless (the table is package-level) and safe for concurrent use across
// different runs; a single Run must be mutated by one goroutine at a time.
type RunStateMachine struct{}

// CanTransition reports whether from -> to is legal.
func (RunStateMachine) CanTransition(from, to RunStatus) bool {
	for _, next := range runTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Terminal reports whether s is a final status (no outgoing transitions).
func (RunStateMachine) Terminal(s RunStatus) bool {
	_, ok := runTransitions[s]
	return !ok
}

// Transition applies to->r, stamping UpdatedAt, StartedAt (first running)
// and FinishedAt (terminal). Illegal moves return *TransitionError without
// mutating r.
func (m RunStateMachine) Transition(r *Run, to RunStatus, at time.Time) error {
	if r == nil {
		return fmt.Errorf("synth: cannot transition nil run: %w", ErrInvalid)
	}
	if !m.CanTransition(r.Status, to) {
		return &TransitionError{From: r.Status, To: to}
	}
	r.Status = to
	r.UpdatedAt = at
	if to == RunRunning && r.StartedAt == nil {
		started := at
		r.StartedAt = &started
	}
	if m.Terminal(to) {
		fin := at
		r.FinishedAt = &fin
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cancellation plumbing: kill switch + parent context -> derived run context.
// ---------------------------------------------------------------------------

// haltContext derives a run context that is cancelled when the parent ctx is
// cancelled OR the kill switch closes. The returned CancelFunc must be called
// (defer) so the watcher goroutine never leaks.
func haltContext(ctx context.Context, kill <-chan struct{}) (context.Context, context.CancelFunc) {
	runCtx, cancel := context.WithCancel(ctx)
	if kill == nil {
		return runCtx, cancel
	}
	go func() {
		select {
		case <-kill:
			cancel()
		case <-runCtx.Done():
		}
	}()
	return runCtx, cancel
}

// haltStatus classifies a pending halt at a step boundary. Kill switch wins
// over parent-context cancellation (a deliberate operator kill must not be
// misread as harness death). Halted runs become:
//   - kill switch            -> RunCancelled (ErrKilled message)
//   - parent ctx cancelled   -> RunInconclusive (interrupted, never lost)
func haltStatus(ctx context.Context, kill <-chan struct{}) (RunStatus, string, bool) {
	if kill != nil {
		select {
		case <-kill:
			return RunCancelled, ErrKilled.Error(), true
		default:
		}
	}
	if err := ctx.Err(); err != nil {
		return RunInconclusive, truncate("synth: execution interrupted: "+err.Error(), maxDetailChars), true
	}
	return "", "", false
}

// ---------------------------------------------------------------------------
// Budget, redaction and scope helpers shared by both workers.
// ---------------------------------------------------------------------------

// effectiveBudget resolves the per-run step budget: RunOptions.MaxSteps
// wins, then Environment.BudgetPerRun, then DefaultBudgetPerRun.
func effectiveBudget(opts RunOptions, env *Environment) int {
	if opts.MaxSteps > 0 {
		return opts.MaxSteps
	}
	if env != nil && env.BudgetPerRun > 0 {
		return env.BudgetPerRun
	}
	return DefaultBudgetPerRun
}

// defaultRedactedHeaders is the default metadata denylist: credential-bearing
// headers never enter RedactedMeta or evidence metadata.
var defaultRedactedHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
}

// RedactHeaders returns a copy of h with credential-bearing headers
// (authorization, proxy-authorization, cookie, set-cookie) replaced by
// RedactedPlaceholder. Unknown headers pass through — the denylist is
// deliberately conservative.
func RedactHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if _, deny := defaultRedactedHeaders[strings.ToLower(k)]; deny {
			out[k] = RedactedPlaceholder
			continue
		}
		out[k] = v
	}
	return out
}

// RedactHeader flattens a net/http-style multi-value header map through
// RedactHeaders (multi-value headers are joined with ", ").
func RedactHeader(h map[string][]string) map[string]string {
	flat := make(map[string]string, len(h))
	for k, vs := range h {
		flat[k] = strings.Join(vs, ", ")
	}
	return RedactHeaders(flat)
}

// sanitizeURL renders scheme://host/path for metadata — query strings,
// fragments and userinfo are dropped by default because they routinely carry
// tokens.
func sanitizeURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.User = nil
	c.RawQuery = ""
	c.Fragment = ""
	c.RawFragment = ""
	return c.String()
}

// truncate bounds s to n runes, appending an ellipsis marker when cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// domainAllowed reports whether host is authorized by the environment
// allowlist. Matching is case-insensitive on the bare hostname (no port):
//   - empty allowlist  -> fence off (any host allowed; the SSRF private-IP
//     guard still applies for the API worker, and browser adapters enforce
//     their own policy);
//   - "*"              -> any host allowed;
//   - "*.example.com"  -> any subdomain of example.com;
//   - otherwise        -> exact match.
func domainAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host, "]") {
		host = host[:i] // strip port
	}
	host = strings.Trim(host, "[]")
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "*" || a == host:
			return true
		case strings.HasPrefix(a, "*."):
			suffix := strings.TrimPrefix(a, "*")
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		}
	}
	return false
}

// rateLimiter is a sliding-window per-run rate cap backing
// Environment.RateCapPerMin. Worker-local by design: the orchestrator owns
// global caps; a run never exceeds its environment's per-minute allowance.
type rateLimiter struct {
	cap    int
	window time.Duration
	clock  Clock
	mu     sync.Mutex
	hits   []time.Time
}

func newRateLimiter(cap int, window time.Duration, clock Clock) *rateLimiter {
	return &rateLimiter{cap: cap, window: window, clock: clock}
}

// allow records one hit if the window has capacity.
func (l *rateLimiter) allow() bool {
	if l == nil || l.cap <= 0 {
		return true
	}
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.hits[:0]
	for _, t := range l.hits {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	l.hits = kept
	if len(l.hits) >= l.cap {
		return false
	}
	l.hits = append(l.hits, now)
	return true
}

// ---------------------------------------------------------------------------
// BrowserDriver seam + BrowserWorker.
// ---------------------------------------------------------------------------

// NavigationObservation captures what a Navigate call saw; it feeds the
// api_status / url_pattern / latency_below_ms assertions in browser runs.
type NavigationObservation struct {
	URL    string // final URL after redirects
	Status int    // main-frame HTTP status (0 = unknown)
	LoadMS int64  // measured load latency in milliseconds
	Title  string
}

// A11yViolation is one accessibility violation returned by a driver scan
// (axe-core semantics; see playwright_cli.go).
type A11yViolation struct {
	Rule     string
	Impact   string
	Selector string
}

// BrowserDriver is the pluggable browser automation seam behind
// BrowserWorker. Implementations serve one run at a time (single driver
// instance per in-flight run). The element lookup convention: an empty
// selector means the page body (whole-page text).
type BrowserDriver interface {
	Navigate(ctx context.Context, url string) (NavigationObservation, error)
	Click(ctx context.Context, selector string) error
	Fill(ctx context.Context, selector, value string) error
	Select(ctx context.Context, selector, value string) error
	Wait(ctx context.Context, selector string, timeout time.Duration) error
	ElementVisible(ctx context.Context, selector string) (bool, error)
	ElementText(ctx context.Context, selector string) (string, error)
	CurrentURL(ctx context.Context) (string, error)
	ConsoleErrors(ctx context.Context) ([]string, error)
	A11yViolations(ctx context.Context) ([]A11yViolation, error)
	Screenshot(ctx context.Context, label string) ([]byte, error)
	StartTrace(ctx context.Context) error
	StopTrace(ctx context.Context) (string, error)
}

// BrowserWorker executes journeys of KindBrowser through a BrowserDriver.
// All bounds are worker-local: StepTimeout (per step), RunWallClock (per
// run), MaxInFlight (per worker). Zero values fall back to the defaults
// above. Evidence lands in EvidenceStore (NoopEvidenceStore when nil).
type BrowserWorker struct {
	Driver       BrowserDriver
	Evidence     EvidenceStore
	Clock        Clock
	StepTimeout  time.Duration
	RunWallClock time.Duration
	MaxInFlight  int

	semOnce sync.Once
	sem     chan struct{}
}

// NewBrowserWorker wires a browser worker with the given driver and evidence
// store (the store may be nil → NoopEvidenceStore; the driver may not).
func NewBrowserWorker(d BrowserDriver, ev EvidenceStore) *BrowserWorker {
	return &BrowserWorker{Driver: d, Evidence: ev}
}

// Kind implements Worker.
func (w *BrowserWorker) Kind() RunKind { return KindBrowser }

// Healthy implements Worker: the worker is healthy when a driver is wired
// and (when the driver exposes availability, as the Playwright CLI adapter
// does) that driver reports itself available.
func (w *BrowserWorker) Healthy() bool {
	if w == nil || w.Driver == nil {
		return false
	}
	if av, ok := w.Driver.(interface{ Available() bool }); ok && !av.Available() {
		return false
	}
	return true
}

// InFlightCap implements InFlightCap.
func (w *BrowserWorker) InFlightCap() int { return w.inFlightLimit() }

func (w *BrowserWorker) inFlightLimit() int {
	if w.MaxInFlight > 0 {
		return w.MaxInFlight
	}
	return DefaultWorkerInFlight
}

func (w *BrowserWorker) semaphore() chan struct{} {
	w.semOnce.Do(func() { w.sem = make(chan struct{}, w.inFlightLimit()) })
	return w.sem
}

func (w *BrowserWorker) clock() Clock {
	if w.Clock != nil {
		return w.Clock
	}
	return SystemClock
}

func (w *BrowserWorker) evidence() EvidenceStore {
	if w.Evidence != nil {
		return w.Evidence
	}
	return NoopEvidenceStore{}
}

// acquire takes one in-flight slot: non-blocking fast path, then a bounded
// wait that honours cancellation (capacity pressure + ctx kill collapse into
// ErrWorkerBusy — pre-execution, so Execute returns (nil, err)).
func (w *BrowserWorker) acquire(ctx context.Context, kill <-chan struct{}) (func(), error) {
	sem := w.semaphore()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	default:
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w (caller context cancelled while waiting for a slot)", ErrWorkerBusy)
	case <-kill:
		return nil, fmt.Errorf("%w (kill switch fired while waiting for a slot)", ErrWorkerBusy)
	}
}

// browserObservation is the running snapshot browser assertions evaluate
// against (refreshed on every navigation).
type browserObservation struct {
	url    string
	status int
	loadMS int64
	title  string
}

// browserExec carries the mutable per-run execution state. One instance per
// run; all mutation happens on the run's goroutine.
type browserExec struct {
	w        *BrowserWorker
	run      *Run
	sm       RunStateMachine
	clock    Clock
	store    EvidenceStore
	limiter  *rateLimiter
	budget   int
	consumed int
	obs      browserObservation
	assert   []Assertion             // j.Assertions, for inline-assert lookup
	assertIx map[AssertionKind][]int // kind -> indices into assert
	claims   map[int]bool            // assertion indices consumed by inline assert steps
	kill     <-chan struct{}
}

// Execute implements Worker. See the Worker contract for the (run, error)
// protocol. The flow: pending→queued→running, trace start, steps (fail-fast;
// inline assert steps consume journey.Assertions by kind), best-effort
// cleanup, trace stop + final screenshot, terminal transition.
func (w *BrowserWorker) Execute(ctx context.Context, j *Journey, env *Environment, p *Persona, opts RunOptions) (*Run, error) {
	if j == nil {
		return nil, fmt.Errorf("synth: journey is required: %w", ErrInvalid)
	}
	if env == nil {
		return nil, fmt.Errorf("synth: environment is required: %w", ErrInvalid)
	}
	if w.Driver == nil {
		return nil, ErrNoDriver
	}
	release, err := w.acquire(ctx, opts.KillSwitch)
	if err != nil {
		return nil, err
	}
	defer release()

	clock := w.clock()
	sm := RunStateMachine{}
	run := &Run{
		ID:            uuid.NewString(),
		OrgID:         j.OrgID,
		ProjectID:     j.ProjectID,
		JourneyID:     j.ID,
		JourneyVer:    j.Version,
		EnvironmentID: env.ID,
		Kind:          KindBrowser,
		Status:        RunPending,
		AppVersion:    opts.AppVersion,
		CreatedAt:     clock.Now(),
	}
	run.UpdatedAt = run.CreatedAt
	if p != nil {
		run.PersonaID, run.PersonaVer = p.ID, p.Version
	}
	if err := sm.Transition(run, RunQueued, clock.Now()); err != nil {
		return run, err
	}
	if err := sm.Transition(run, RunRunning, clock.Now()); err != nil {
		return run, err
	}

	runCtx, cancel := haltContext(ctx, opts.KillSwitch)
	defer cancel()

	ex := &browserExec{
		w:        w,
		run:      run,
		sm:       sm,
		clock:    clock,
		store:    w.evidence(),
		limiter:  newRateLimiter(env.RateCapPerMin, DefaultRateWindow, clock),
		budget:   effectiveBudget(opts, env),
		assert:   j.Assertions,
		assertIx: groupAssertions(j.Assertions),
		claims:   make(map[int]bool),
		kill:     opts.KillSwitch,
	}

	status, errMsg := ex.runSteps(runCtx, j, env)
	// Post-run evidence only when the run was NOT halted: the halt check is
	// synchronous (unlike the runCtx watcher goroutine) so a pre-closed or
	// mid-run kill switch deterministically skips driver work.
	if _, _, halted := haltStatus(ctx, opts.KillSwitch); !halted {
		ex.cleanup(runCtx, j, env)
		ex.captureTrace(runCtx)
		ex.screenshot(runCtx, "final")
	}
	if err := sm.Transition(run, status, clock.Now()); err != nil {
		// A terminal status was already applied (recovery race); keep it.
		return run, err
	}
	if errMsg != "" {
		run.Error = errMsg
	}
	return run, nil
}

// groupAssertions indexes journey assertions by kind, preserving order.
func groupAssertions(assertions []Assertion) map[AssertionKind][]int {
	ix := make(map[AssertionKind][]int)
	for i, a := range assertions {
		ix[a.Kind] = append(ix[a.Kind], i)
	}
	return ix
}

// runSteps executes the main step loop and returns the terminal status plus the
// run.Error message (empty on completed).
func (ex *browserExec) runSteps(ctx context.Context, j *Journey, env *Environment) (RunStatus, string) {
	wall := ex.w.RunWallClock
	if wall <= 0 {
		wall = DefaultRunWallClock
	}
	deadline := ex.clock.Now().Add(wall)

	// A halt that arrived before the first step (e.g. a pre-closed kill
	// switch) must not touch the driver at all — not even tracing.
	if st, msg, halted := haltStatus(ctx, ex.kill); halted {
		ex.event(-1, EventError, string(st), msg, nil, "")
		return st, msg
	}
	// Tracing is best-effort: a driver that cannot trace never blocks a run.
	_ = ex.w.Driver.StartTrace(ctx)

	for i, step := range j.Steps {
		if st, msg, halted := haltStatus(ctx, ex.kill); halted {
			ex.event(i, EventError, string(st), msg, nil, "")
			return st, msg
		}
		if ex.consumed >= ex.budget {
			msg := ErrBudgetExceeded.Error() + fmt.Sprintf(" (step budget %d exhausted at step %d)", ex.budget, i)
			return ex.guardBlocked(j, i, msg, "budget")
		}
		if now := ex.clock.Now(); now.After(deadline) {
			msg := fmt.Sprintf("synth: run wall-clock cap (%s) exceeded", wall)
			return ex.guardBlocked(j, i, msg, "wall_clock")
		}
		if !ex.limiter.allow() {
			msg := fmt.Sprintf("synth: environment rate cap (%d/min) reached at step %d", env.RateCapPerMin, i)
			return ex.guardBlocked(j, i, msg, "rate_capped")
		}
		if step.WriteOp && env.Mode == ModeProductionReadOnly {
			msg := ErrReadOnlyMode.Error() + fmt.Sprintf(" (step %d: %s %s)", i, step.Action, step.Target)
			return ex.guardBlocked(j, i, msg, "write_denied_readonly")
		}
		if step.Action == "navigate" {
			if reason, ok := ex.navigateScope(step, env); !ok {
				msg := ErrScopeViolation.Error() + " (" + reason + ")"
				return ex.guardBlocked(j, i, msg, "allowlisted_domain")
			}
		}

		outcome, detail := ex.execStep(ctx, i, step)
		switch outcome {
		case StepPassed, StepSkipped, StepBlocked:
			// A blocked single step (e.g. a browser-only assertion handed to
			// the api worker) does not fail the run by itself; only blocked
			// GUARDS (budget, scope, read-only, rate, wall clock) are fatal.
		default: // StepFailed
			if st, msg, halted := haltStatus(ctx, ex.kill); halted {
				ex.event(i, EventError, string(st), msg, nil, "")
				return st, msg
			}
			ex.screenshot(ctx, fmt.Sprintf("step-%d-failure", i))
			ex.skipRest(j, i+1, detail)
			return RunFailed, truncate(detail, maxDetailChars)
		}
		ex.consumed++
	}

	return ex.epilogue(ctx, j)
}

// epilogue evaluates every journey assertion not consumed by an inline assert
// step, in declaration order. Any failure fails the run.
func (ex *browserExec) epilogue(ctx context.Context, j *Journey) (RunStatus, string) {
	for ai, a := range j.Assertions {
		if ex.claims[ai] {
			continue
		}
		status, detail := ex.evalAssertion(ctx, a)
		ex.event(-1, EventAssertion, status, detail, nil, "")
		if status == StepFailed {
			ex.screenshot(ctx, "assertion-failure")
			return RunFailed, "assertion " + string(a.Kind) + " failed: " + detail
		}
	}
	return RunCompleted, ""
}

// guardBlocked records a fatal guard denial (blocked) at step i, marks the
// remaining steps skipped and returns the failed outcome.
func (ex *browserExec) guardBlocked(j *Journey, i int, reason, guard string) (RunStatus, string) {
	ex.event(i, EventAction, StepBlocked, reason, map[string]string{"guard": guard}, "")
	ex.skipRest(j, i+1, reason)
	return RunFailed, truncate(reason, maxDetailChars)
}

// skipRest marks steps [from, len) as skipped with the given reason.
func (ex *browserExec) skipRest(j *Journey, from int, reason string) {
	for i := from; i < len(j.Steps); i++ {
		ex.event(i, EventAction, StepSkipped, "skipped: "+reason, nil, "")
	}
}

// cleanup executes journey.Cleanup best-effort: cleanup events are recorded
// (negative StepIndex: -(i+1)) but never change the run outcome.
func (ex *browserExec) cleanup(ctx context.Context, j *Journey, env *Environment) {
	for i, step := range j.Cleanup {
		if _, _, halted := haltStatus(ctx, ex.kill); halted {
			return
		}
		if step.WriteOp && env.Mode == ModeProductionReadOnly {
			ex.event(-(i + 1), EventAction, StepBlocked, "cleanup blocked: "+ErrReadOnlyMode.Error(), nil, "")
			continue
		}
		ex.execStep(ctx, -(i + 1), step) // outcome intentionally ignored
	}
}

// execStep performs one journey step and records its event. The returned
// status is one of StepPassed / StepFailed / StepSkipped / StepBlocked.
// stepIndex is the journey step position (negative for cleanup steps).
func (ex *browserExec) execStep(ctx context.Context, stepIndex int, step JourneyStep) (string, string) {
	stepTimeout := ex.w.StepTimeout
	if stepTimeout <= 0 {
		stepTimeout = DefaultStepTimeout
	}
	if step.TimeoutMS > 0 {
		stepTimeout = time.Duration(step.TimeoutMS) * time.Millisecond
	}
	sctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	var status, detail string
	switch step.Action {
	case "navigate":
		obs, err := ex.w.Driver.Navigate(sctx, step.Target)
		if err == nil {
			ex.obs = browserObservation{url: obs.URL, status: obs.Status, loadMS: obs.LoadMS, title: obs.Title}
			detail = fmt.Sprintf("navigated to %s (status %d, load %dms)", obs.URL, obs.Status, obs.LoadMS)
			status = StepPassed
			ex.captureConsole(sctx, stepIndex)
		} else {
			status, detail = StepFailed, "navigate failed: "+err.Error()
		}
		ex.event(stepIndex, EventNavigation, status, detail, nil, "")
	case "click":
		err := ex.w.Driver.Click(sctx, step.Target)
		status, detail = actionOutcome(err, "click "+step.Target)
		ex.event(stepIndex, EventAction, status, detail, nil, "")
	case "fill":
		err := ex.w.Driver.Fill(sctx, step.Target, step.Value)
		status, detail = actionOutcome(err, "fill "+step.Target)
		ex.event(stepIndex, EventAction, status, detail, nil, "")
	case "select":
		err := ex.w.Driver.Select(sctx, step.Target, step.Value)
		status, detail = actionOutcome(err, "select "+step.Target)
		ex.event(stepIndex, EventAction, status, detail, nil, "")
	case "wait":
		err := ex.w.Driver.Wait(sctx, step.Target, stepTimeout)
		status, detail = actionOutcome(err, "wait "+step.Target)
		ex.event(stepIndex, EventAction, status, detail, nil, "")
	case "assert":
		status, detail = ex.execInlineAssert(ctx, stepIndex, step)
		ex.event(stepIndex, EventAssertion, status, detail, nil, "")
	case "extract":
		status, detail = StepSkipped, "extract requires the api worker (browser driver has no extraction surface)"
		ex.event(stepIndex, EventAction, status, detail, nil, "")
	default:
		status, detail = StepSkipped, fmt.Sprintf("unsupported browser action %q", step.Action)
		ex.event(stepIndex, EventAction, status, detail, nil, "")
	}

	// Re-classify harness-level outcomes so kill/ctx/timeout never read as
	// app failures.
	if status == StepFailed {
		if st, msg, halted := haltStatus(ctx, ex.kill); halted {
			ex.event(stepIndex, EventError, string(st), msg, nil, "")
			return string(st), msg
		}
		if errors.Is(sctx.Err(), context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			detail = fmt.Sprintf("step timeout after %s: %s", stepTimeout, detail)
			ex.event(stepIndex, EventError, StepFailed, detail, nil, "")
			return StepFailed, detail
		}
	}
	return status, detail
}

// execInlineAssert evaluates the next unconsumed journey assertion of the
// kind named by step.Target (the step/assert pairing convention: a step with
// Action "assert" and Target "<kind>" consumes journey.Assertions entries of
// that kind in order).
func (ex *browserExec) execInlineAssert(ctx context.Context, stepIndex int, step JourneyStep) (string, string) {
	kind := AssertionKind(strings.TrimSpace(step.Target))
	for _, ai := range ex.assertIx[kind] {
		if !ex.claims[ai] {
			ex.claims[ai] = true
			return ex.evalAssertion(ctx, ex.assert[ai])
		}
	}
	return StepFailed, fmt.Sprintf("assert step %d references kind %q but no unconsumed journey assertion of that kind exists", stepIndex, kind)
}

// captureConsole stores console errors observed after a navigation; the
// record is observational (the no_console_error assertion decides failure).
func (ex *browserExec) captureConsole(ctx context.Context, stepIndex int) {
	errs, err := ex.w.Driver.ConsoleErrors(ctx)
	if err != nil || len(errs) == 0 {
		return
	}
	payload := strings.Join(errs, "\n")
	ref, _ := ex.store.Put(ctx, "console", []byte(payload), map[string]string{
		"run_id": ex.run.ID, "step": fmt.Sprintf("%d", stepIndex), "count": fmt.Sprintf("%d", len(errs)),
	})
	ex.run.EvidenceRefs = append(ex.run.EvidenceRefs, ref)
	ex.event(stepIndex, EventConsole, StepPassed,
		fmt.Sprintf("captured %d console messages", len(errs)), nil, ref)
}

// captureTrace stores the stopped trace as evidence (best-effort).
func (ex *browserExec) captureTrace(ctx context.Context) {
	payload, err := ex.w.Driver.StopTrace(ctx)
	if err != nil || payload == "" {
		return
	}
	ref, _ := ex.store.Put(ctx, "trace", []byte(payload), map[string]string{"run_id": ex.run.ID})
	ex.run.EvidenceRefs = append(ex.run.EvidenceRefs, ref)
}

// screenshot stores a driver screenshot as evidence (best-effort).
func (ex *browserExec) screenshot(ctx context.Context, label string) {
	if ctx.Err() != nil {
		return
	}
	shot, err := ex.w.Driver.Screenshot(ctx, label)
	if err != nil || len(shot) == 0 {
		return
	}
	ref, _ := ex.store.Put(ctx, "screenshot", shot, map[string]string{"run_id": ex.run.ID, "label": label})
	ex.run.EvidenceRefs = append(ex.run.EvidenceRefs, ref)
}

// event appends one StepEvent with a monotonic Index and stamped time.
// stepIndex is the journey step position, -1 for journey-level epilogue
// assertions, -(i+1) for cleanup steps.
func (ex *browserExec) event(stepIndex int, typ, status, message string, meta map[string]string, evidenceRef string) {
	ex.run.StepEvents = append(ex.run.StepEvents, StepEvent{
		Index:        len(ex.run.StepEvents),
		StepIndex:    stepIndex,
		Type:         typ,
		Status:       status,
		Message:      truncate(message, maxDetailChars),
		RedactedMeta: meta,
		EvidenceRef:  evidenceRef,
		At:           ex.clock.Now(),
	})
}

// navigateScope enforces the environment allowlist for navigations BEFORE
// any driver call: untrusted page content can never expand authorized scope
// (ScopeDecision semantics from types.go).
func (ex *browserExec) navigateScope(step JourneyStep, env *Environment) (string, bool) {
	u, err := url.Parse(step.Target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "unsupported scheme", false
	}
	if !domainAllowed(u.Hostname(), env.AllowedDomains) {
		return "host outside authorized test scope", false
	}
	return "", true
}

// evalAssertion evaluates one deterministic assertion worker-side against
// driver observations. Returns (status, detail); status is StepPassed,
// StepFailed or StepSkipped.
func (ex *browserExec) evalAssertion(ctx context.Context, a Assertion) (string, string) {
	obs := ex.obs
	switch a.Kind {
	case AssertElementVisible:
		visible, err := ex.w.Driver.ElementVisible(ctx, a.Target)
		if err != nil {
			return StepFailed, "element_visible lookup failed: " + err.Error()
		}
		if visible {
			return StepPassed, "element visible: " + a.Target
		}
		return StepFailed, "element not visible: " + a.Target
	case AssertTextContains:
		text, err := ex.w.Driver.ElementText(ctx, a.Target)
		if err != nil {
			return StepFailed, "text lookup failed: " + err.Error()
		}
		if strings.Contains(text, a.Expected) {
			return StepPassed, fmt.Sprintf("text %q found", a.Expected)
		}
		return StepFailed, fmt.Sprintf("text %q not found in %q", a.Expected, truncate(text, 128))
	case AssertAPIStatus:
		if obs.status == 0 {
			return StepFailed, "api_status: no observed navigation status"
		}
		if matchStatusClass(a.Expected, obs.status) {
			return StepPassed, fmt.Sprintf("status %d matches %s", obs.status, a.Expected)
		}
		return StepFailed, fmt.Sprintf("status %d does not match %s", obs.status, a.Expected)
	case AssertJSONSchema:
		return StepSkipped, "json_schema requires the api worker (browser driver captures no response body)"
	case AssertURLPattern:
		ok, err := matchURLPattern(a.Expected, obs.url)
		if err != nil {
			return StepFailed, "url_pattern: " + err.Error()
		}
		if ok {
			return StepPassed, "url matches pattern " + a.Expected
		}
		return StepFailed, fmt.Sprintf("url %q does not match pattern %q", obs.url, a.Expected)
	case AssertNoConsoleError:
		errs, err := ex.w.Driver.ConsoleErrors(ctx)
		if err != nil {
			return StepFailed, "console capture failed: " + err.Error()
		}
		if len(errs) == 0 {
			return StepPassed, "no console errors"
		}
		return StepFailed, fmt.Sprintf("%d console errors, first: %s", len(errs), truncate(errs[0], 128))
	case AssertA11y:
		bound, err := parseNonNegative(a.Expected)
		if err != nil {
			return StepFailed, "a11y_violations_below: invalid bound " + a.Expected
		}
		violations, err := ex.w.Driver.A11yViolations(ctx)
		if err != nil {
			return StepFailed, "a11y scan failed: " + err.Error()
		}
		if int64(len(violations)) <= bound {
			return StepPassed, fmt.Sprintf("%d a11y violations (bound %d)", len(violations), bound)
		}
		first := ""
		if len(violations) > 0 {
			first = violations[0].Rule
		}
		return StepFailed, fmt.Sprintf("%d a11y violations exceed bound %d (first rule: %s)", len(violations), bound, first)
	case AssertLatencyBelow:
		bound, err := parseNonNegative(a.Expected)
		if err != nil {
			return StepFailed, "latency_below_ms: invalid bound " + a.Expected
		}
		if obs.loadMS < bound {
			return StepPassed, fmt.Sprintf("load %dms below %dms", obs.loadMS, bound)
		}
		return StepFailed, fmt.Sprintf("load %dms exceeds %dms", obs.loadMS, bound)
	default:
		return StepSkipped, fmt.Sprintf("unknown assertion kind %q", a.Kind)
	}
}

// actionOutcome maps a driver action error onto an event status.
func actionOutcome(err error, what string) (string, string) {
	if err == nil {
		return StepPassed, what + " ok"
	}
	return StepFailed, what + " failed: " + err.Error()
}

// ---------------------------------------------------------------------------
// Deterministic matchers shared by both workers.
// ---------------------------------------------------------------------------

// matchStatusClass compares an observed HTTP status against an expected
// literal ("200") or class ("2xx".."5xx").
func matchStatusClass(expected string, got int) bool {
	e := strings.TrimSpace(expected)
	switch {
	case len(e) == 3 && strings.HasSuffix(e, "xx") && e[0] >= '1' && e[0] <= '5':
		want := int(e[0]-'0') * 100
		return got >= want && got < want+100
	default:
		var want int
		if _, err := fmt.Sscanf(e, "%d", &want); err != nil {
			return false
		}
		return got == want
	}
}

// matchURLPattern matches a URL against a regular expression (the pinned
// assertion kind is "url_pattern"; patterns are Go RE2). Compile errors are
// authoring defects and fail the assertion loudly.
func matchURLPattern(pattern, rawURL string) (bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}
	return re.MatchString(rawURL), nil
}

// parseNonNegative parses a non-negative integer bound.
func parseNonNegative(s string) (int64, error) {
	var n int64
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty bound")
	}
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("invalid non-negative integer %q", s)
	}
	return n, nil
}
