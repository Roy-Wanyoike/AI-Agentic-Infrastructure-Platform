package synth

// worker_test.go — issue #148: deterministic tests for the worker contract
// and BrowserWorker. Everything runs against the driverContractFake below
// (the DriverContract test fake named by the issue): no network, no sleeps —
// synchronization is channel-based and time is injected (ManualClock). The
// real Playwright runtime is exercised (honestly, with t.Skip detection) in
// TestPlaywrightAdapterHonestDetection only.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test doubles.
// ---------------------------------------------------------------------------

// driverContractFake is the deterministic BrowserDriver used by every worker
// test. It scripts observations, records calls, and exposes channels so
// tests can synchronize with the worker without sleeping.
type driverContractFake struct {
	mu       sync.Mutex
	navs     []NavigationObservation // popped per Navigate (last repeats)
	texts    map[string]string       // selector -> text
	visible  map[string]bool
	console  []string
	a11y     []A11yViolation
	failOn   map[string]error // navigate|click|fill|select|wait -> error
	calls    []string
	shots    []string
	traceOn  bool
	navCount int

	// waitGate, when non-nil, blocks Wait until closed (or ctx done).
	waitGate chan struct{}
	// waitStarted is closed (once) when Wait begins blocking. The field is
	// never reassigned; waitStartedClosed guards the close.
	waitStarted       chan struct{}
	waitStartedClosed bool
	// onAction fires after each successful action ("navigate", "click", ...)
	// so tests can drive the ManualClock deterministically.
	onAction func(action string)
}

func (d *driverContractFake) record(call string) {
	d.mu.Lock()
	d.calls = append(d.calls, call)
	d.mu.Unlock()
}

func (d *driverContractFake) after(action string) {
	if d.onAction != nil {
		d.onAction(action)
	}
}

func (d *driverContractFake) Navigate(ctx context.Context, url string) (NavigationObservation, error) {
	d.record("navigate " + url)
	if err := d.failOn["navigate"]; err != nil {
		return NavigationObservation{}, err
	}
	d.mu.Lock()
	navCount := d.navCount
	d.navCount++
	var obs NavigationObservation
	if len(d.navs) == 0 {
		obs = NavigationObservation{URL: url, Status: 200, LoadMS: 10, Title: "page"}
	} else if navCount < len(d.navs) {
		obs = d.navs[navCount]
	} else {
		obs = d.navs[len(d.navs)-1]
	}
	d.mu.Unlock()
	d.after("navigate")
	return obs, nil
}

func (d *driverContractFake) Click(ctx context.Context, selector string) error {
	d.record("click " + selector)
	if err := d.failOn["click"]; err != nil {
		return err
	}
	d.after("click")
	return nil
}

func (d *driverContractFake) Fill(ctx context.Context, selector, value string) error {
	d.record(fmt.Sprintf("fill %s=%s", selector, value))
	if err := d.failOn["fill"]; err != nil {
		return err
	}
	d.after("fill")
	return nil
}

func (d *driverContractFake) Select(ctx context.Context, selector, value string) error {
	d.record(fmt.Sprintf("select %s=%s", selector, value))
	if err := d.failOn["select"]; err != nil {
		return err
	}
	d.after("select")
	return nil
}

func (d *driverContractFake) Wait(ctx context.Context, selector string, timeout time.Duration) error {
	d.record("wait " + selector)
	if err := d.failOn["wait"]; err != nil {
		return err
	}
	if d.waitGate == nil {
		d.after("wait")
		return nil
	}
	// Close waitStarted exactly once; the channel value itself is never
	// reassigned so tests can select on it without additional locking.
	d.mu.Lock()
	if d.waitStarted != nil && !d.waitStartedClosed {
		close(d.waitStarted)
		d.waitStartedClosed = true
	}
	d.mu.Unlock()
	select {
	case <-d.waitGate:
		d.after("wait")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *driverContractFake) ElementVisible(ctx context.Context, selector string) (bool, error) {
	d.record("visible " + selector)
	return d.visible[selector], nil
}

func (d *driverContractFake) ElementText(ctx context.Context, selector string) (string, error) {
	d.record("text " + selector)
	return d.texts[selector], nil
}

func (d *driverContractFake) CurrentURL(ctx context.Context) (string, error) {
	d.record("current_url")
	if len(d.navs) == 0 {
		return "", nil
	}
	return d.navs[len(d.navs)-1].URL, nil
}

func (d *driverContractFake) ConsoleErrors(ctx context.Context) ([]string, error) {
	d.record("console_errors")
	return d.console, nil
}

func (d *driverContractFake) A11yViolations(ctx context.Context) ([]A11yViolation, error) {
	d.record("a11y")
	return d.a11y, nil
}

func (d *driverContractFake) Screenshot(ctx context.Context, label string) ([]byte, error) {
	d.mu.Lock()
	d.shots = append(d.shots, label)
	d.mu.Unlock()
	d.record("screenshot " + label)
	return []byte("png:" + label), nil
}

func (d *driverContractFake) StartTrace(ctx context.Context) error {
	d.mu.Lock()
	d.traceOn = true
	d.mu.Unlock()
	d.record("start_trace")
	return nil
}

func (d *driverContractFake) StopTrace(ctx context.Context) (string, error) {
	d.record("stop_trace")
	d.mu.Lock()
	on := d.traceOn
	d.traceOn = false
	d.mu.Unlock()
	if !on {
		return "", nil
	}
	return "trace-archive-bytes", nil
}

// callCount returns how many times the driver was called in total.
func (d *driverContractFake) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// ---------------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------------

func browserTestEnv() *Environment {
	return &Environment{
		ID:             "env-1",
		OrgID:          "org-1",
		TargetID:       "target-1",
		Kind:           EnvStaging,
		Mode:           ModePreProduction,
		AllowedDomains: []string{"app.test"},
	}
}

func browserJourney() *Journey {
	return &Journey{
		ID:            "j-1",
		OrgID:         "org-1",
		ProjectID:     "proj-1",
		EnvironmentID: "env-1",
		PersonaID:     "pers-1",
		Name:          "login-dashboard",
		Status:        JourneyScheduled,
		Risk:          RiskMedium,
		Version:       3,
		Steps: []JourneyStep{
			{Index: 0, Action: "navigate", Target: "https://app.test/login"},
			{Index: 1, Action: "fill", Target: "#email", Value: "synth@example.test"},
			{Index: 2, Action: "click", Target: "#submit"},
			{Index: 3, Action: "wait", Target: "#dashboard"},
			{Index: 4, Action: "select", Target: "#plan", Value: "pro"},
		},
		Assertions: []Assertion{
			{Kind: AssertElementVisible, Target: "#dashboard", Severity: RiskHigh},
			{Kind: AssertTextContains, Target: "", Expected: "Welcome", Severity: RiskHigh},
			{Kind: AssertURLPattern, Expected: `^https://app\.test/dashboard`, Severity: RiskMedium},
			{Kind: AssertAPIStatus, Expected: "2xx", Severity: RiskMedium},
			{Kind: AssertNoConsoleError, Severity: RiskMedium},
			{Kind: AssertA11y, Expected: "2", Severity: RiskMedium},
			{Kind: AssertLatencyBelow, Expected: "5000", Severity: RiskLow},
			{Kind: AssertJSONSchema, Expected: "{}", Severity: RiskLow},
		},
		Cleanup: []JourneyStep{{Index: 0, Action: "click", Target: "#logout"}},
	}
}

func browserTestPersona() *Persona {
	return &Persona{ID: "pers-1", OrgID: "org-1", Name: "standard_customer", BuiltIn: "standard_customer", Version: 7}
}

func happyFake() *driverContractFake {
	return &driverContractFake{
		navs: []NavigationObservation{{URL: "https://app.test/dashboard", Status: 200, LoadMS: 120, Title: "Dashboard"}},
		texts: map[string]string{
			"": "Welcome to the dashboard",
		},
		visible: map[string]bool{"#dashboard": true},
		a11y:    []A11yViolation{{Rule: "color-contrast", Impact: "minor", Selector: "body"}},
	}
}

// ---------------------------------------------------------------------------
// Event inspection helpers.
// ---------------------------------------------------------------------------

func findEvents(t *testing.T, run *Run, status string) []StepEvent {
	t.Helper()
	var out []StepEvent
	for _, ev := range run.StepEvents {
		if ev.Status == status {
			out = append(out, ev)
		}
	}
	return out
}

func requireOneEvent(t *testing.T, run *Run, typ, status, needle string) StepEvent {
	t.Helper()
	for _, ev := range run.StepEvents {
		if ev.Type == typ && ev.Status == status && strings.Contains(ev.Message, needle) {
			return ev
		}
	}
	t.Fatalf("no %s/%s event containing %q in run %+v", typ, status, needle, run.StepEvents)
	return StepEvent{}
}

func requireNoEvent(t *testing.T, run *Run, status string) {
	t.Helper()
	if got := findEvents(t, run, status); len(got) > 0 {
		t.Fatalf("unexpected %s events: %+v", status, got)
	}
}

func requireMonotonicIndexes(t *testing.T, run *Run) {
	t.Helper()
	for i, ev := range run.StepEvents {
		if ev.Index != i {
			t.Fatalf("event %d has Index %d (not monotonic)", i, ev.Index)
		}
	}
}

// ---------------------------------------------------------------------------
// RunStateMachine.
// ---------------------------------------------------------------------------

func TestRunStateMachineLegalTransitions(t *testing.T) {
	sm := RunStateMachine{}
	legal := [][2]RunStatus{
		{RunPending, RunQueued}, {RunPending, RunCancelled}, {RunPending, RunFailed},
		{RunQueued, RunRunning}, {RunQueued, RunCancelled}, {RunQueued, RunInconclusive},
		{RunRunning, RunCompleted}, {RunRunning, RunFailed}, {RunRunning, RunCancelled},
		{RunRunning, RunCancelling}, {RunRunning, RunPaused}, {RunRunning, RunInconclusive},
		{RunPaused, RunRunning}, {RunPaused, RunCancelled}, {RunPaused, RunInconclusive},
		{RunCancelling, RunCancelled}, {RunCancelling, RunInconclusive},
	}
	for _, pair := range legal {
		if !sm.CanTransition(pair[0], pair[1]) {
			t.Errorf("expected %s -> %s to be legal", pair[0], pair[1])
		}
	}
}

func TestRunStateMachineIllegalTransitionsAndTerminal(t *testing.T) {
	sm := RunStateMachine{}
	terminal := []RunStatus{RunCompleted, RunFailed, RunCancelled, RunInconclusive}
	for _, from := range terminal {
		if !sm.Terminal(from) {
			t.Errorf("%s should be terminal", from)
		}
		for _, to := range []RunStatus{RunPending, RunQueued, RunRunning, RunCompleted, RunFailed, RunCancelled, RunInconclusive, RunPaused, RunCancelling} {
			if sm.CanTransition(from, to) {
				t.Errorf("terminal %s -> %s must be illegal", from, to)
			}
		}
	}
	// Pending must never jump straight to running.
	if sm.CanTransition(RunPending, RunRunning) {
		t.Error("pending -> running must go through queued")
	}
	if sm.Terminal(RunRunning) || sm.Terminal(RunPending) {
		t.Error("non-terminal statuses reported terminal")
	}
}

func TestRunStateMachineTransitionStampsAndErrors(t *testing.T) {
	sm := RunStateMachine{}
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := NewManualClock(base)
	run := &Run{ID: "r-1", Status: RunPending, CreatedAt: base}

	err := sm.Transition(run, RunRunning, clock.Now())
	if err == nil {
		t.Fatal("pending -> running must be rejected")
	}
	var terr *TransitionError
	if !errors.As(err, &terr) || terr.From != RunPending || terr.To != RunRunning {
		t.Fatalf("expected *TransitionError pending->running, got %v", err)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("TransitionError must wrap ErrInvalid")
	}
	if run.Status != RunPending {
		t.Fatal("rejected transition must not mutate the run")
	}

	if err := sm.Transition(run, RunQueued, clock.Now()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := sm.Transition(run, RunRunning, clock.Now()); err != nil {
		t.Fatal(err)
	}
	if run.StartedAt == nil || !run.StartedAt.Equal(base.Add(time.Second)) {
		t.Fatalf("StartedAt not stamped on first running: %+v", run.StartedAt)
	}
	clock.Advance(time.Second)
	if err := sm.Transition(run, RunCompleted, clock.Now()); err != nil {
		t.Fatal(err)
	}
	if run.FinishedAt == nil || !run.FinishedAt.Equal(base.Add(2*time.Second)) {
		t.Fatalf("FinishedAt not stamped on terminal: %+v", run.FinishedAt)
	}
	if run.UpdatedAt.Equal(base) {
		t.Fatal("UpdatedAt not advanced")
	}
}

// ---------------------------------------------------------------------------
// BrowserWorker — happy paths.
// ---------------------------------------------------------------------------

func TestBrowserWorkerHappyPathAllAssertions(t *testing.T) {
	fake := happyFake()
	store := NewInMemoryEvidenceStore()
	w := NewBrowserWorker(fake, store)
	j := browserJourney()
	env := browserTestEnv()
	p := browserTestPersona()

	run, err := w.Execute(context.Background(), j, env, p, RunOptions{AppVersion: "2025.1"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("status = %s, want completed (error: %s)", run.Status, run.Error)
	}
	if run.Kind != KindBrowser || run.JourneyID != "j-1" || run.JourneyVer != 3 {
		t.Fatalf("run identity wrong: %+v", run)
	}
	if run.PersonaID != "pers-1" || run.PersonaVer != 7 {
		t.Fatalf("persona not projected: %+v", run)
	}
	if run.AppVersion != "2025.1" {
		t.Fatalf("AppVersion not projected: %q", run.AppVersion)
	}
	if run.StartedAt == nil || run.FinishedAt == nil {
		t.Fatal("timestamps missing on completed run")
	}
	if !run.FinishedAt.After(*run.StartedAt) && !run.FinishedAt.Equal(*run.StartedAt) {
		t.Fatal("FinishedAt before StartedAt")
	}

	// Five action events passed; epilogue: 7 passed (element_visible,
	// text_contains, url_pattern, api_status, no_console_error, a11y,
	// latency) + 1 skipped (json_schema is browser-inapplicable).
	if got := len(findEvents(t, run, StepPassed)); got != 5+7+1 { // +1 cleanup click
		t.Fatalf("passed events = %d, want 13", got)
	}
	skip := findEvents(t, run, StepSkipped)
	if len(skip) != 1 || !strings.Contains(skip[0].Message, "json_schema") {
		t.Fatalf("expected exactly one skipped json_schema event, got %+v", skip)
	}
	requireNoEvent(t, run, StepFailed)
	requireNoEvent(t, run, StepBlocked)
	requireNoEvent(t, run, StepInconclusive)
	requireMonotonicIndexes(t, run)

	// Evidence: final screenshot + trace must be stored refs.
	if len(run.EvidenceRefs) < 2 {
		t.Fatalf("expected >=2 evidence refs, got %v", run.EvidenceRefs)
	}
	for _, ref := range run.EvidenceRefs {
		if _, ok, _ := store.Get(context.Background(), ref); !ok {
			t.Fatalf("evidence ref %q not resolvable", ref)
		}
	}
	foundTrace := false
	for _, ref := range run.EvidenceRefs {
		payload, ok, _ := store.Get(context.Background(), ref)
		if ok && strings.Contains(string(payload), "trace-archive-bytes") {
			foundTrace = true
		}
	}
	if !foundTrace {
		t.Fatal("trace payload not captured as evidence")
	}
	// Cleanup executed after the main steps and before trace stop + final
	// screenshot.
	tail := fake.calls[len(fake.calls)-3:]
	if len(tail) != 3 || !strings.HasPrefix(tail[0], "click #logout") || tail[1] != "stop_trace" || tail[2] != "screenshot final" {
		t.Fatalf("cleanup ordering wrong, tail = %v", tail)
	}
}

func TestBrowserWorkerInlineAssertSteps(t *testing.T) {
	fake := happyFake()
	w := NewBrowserWorker(fake, nil)
	j := &Journey{
		ID: "j-inline", OrgID: "org-1", ProjectID: "proj-1", EnvironmentID: "env-1", Version: 1,
		Steps: []JourneyStep{
			{Index: 0, Action: "navigate", Target: "https://app.test/dashboard"},
			{Index: 1, Action: "assert", Target: "element_visible", Value: "#dashboard"},
			{Index: 2, Action: "assert", Target: "text_contains", Value: "Welcome"},
		},
		Assertions: []Assertion{
			{Kind: AssertElementVisible, Target: "#dashboard"},
			{Kind: AssertTextContains, Expected: "Welcome"},
		},
	}
	run, err := w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("status = %s (%s)", run.Status, run.Error)
	}
	// Two inline assertion events, one per consumed journey assertion.
	asserts := []StepEvent{}
	for _, ev := range run.StepEvents {
		if ev.Type == EventAssertion && ev.Status == StepPassed {
			asserts = append(asserts, ev)
		}
	}
	if len(asserts) != 2 || !strings.Contains(asserts[0].Message, "#dashboard") || !strings.Contains(asserts[1].Message, "Welcome") {
		t.Fatalf("inline assert events wrong: %+v", asserts)
	}
	// Both assertions were consumed inline; nothing left for the epilogue.
	for _, ev := range run.StepEvents {
		if ev.StepIndex == -1 {
			t.Fatalf("unexpected epilogue event: %+v", ev)
		}
	}
}

func TestBrowserWorkerInFlightCapAndKind(t *testing.T) {
	w := NewBrowserWorker(happyFake(), nil)
	if w.Kind() != KindBrowser {
		t.Fatalf("Kind = %s", w.Kind())
	}
	if !w.Healthy() {
		t.Fatal("worker with wired driver must be healthy")
	}
	if w.InFlightCap() != DefaultWorkerInFlight {
		t.Fatalf("default in-flight cap = %d", w.InFlightCap())
	}
	w.MaxInFlight = 2
	if w.InFlightCap() != 2 {
		t.Fatal("MaxInFlight override ignored")
	}
	if (*BrowserWorker)(nil).Healthy() {
		t.Fatal("nil worker must not be healthy")
	}
	if NewBrowserWorker(nil, nil).Healthy() {
		t.Fatal("worker without driver must not be healthy")
	}
}

// ---------------------------------------------------------------------------
// BrowserWorker — failures, guards, and DISTINCT event statuses.
// ---------------------------------------------------------------------------

func TestBrowserWorkerAssertionFailureFailFast(t *testing.T) {
	fake := happyFake()
	fake.texts[""] = "Access denied" // text_contains("Welcome") will fail
	w := NewBrowserWorker(fake, NewInMemoryEvidenceStore())
	j := browserJourney()

	run, err := w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed {
		t.Fatalf("status = %s, want failed", run.Status)
	}
	if !strings.Contains(run.Error, "Welcome") {
		t.Fatalf("run.Error missing assertion detail: %q", run.Error)
	}
	// The failing assertion is the inline-epilogue text_contains; after it,
	// later epilogue assertions never evaluate.
	failed := findEvents(t, run, StepFailed)
	if len(failed) != 1 || !strings.Contains(failed[0].Message, "Welcome") {
		t.Fatalf("expected one failed assertion event, got %+v", failed)
	}
	// Failure screenshot evidence captured.
	foundShot := false
	for _, label := range fake.shots {
		if strings.HasPrefix(label, "assertion-failure") || strings.HasPrefix(label, "step-") {
			foundShot = true
		}
	}
	if !foundShot {
		t.Fatalf("no failure screenshot captured: %v", fake.shots)
	}
}

func TestBrowserWorkerDriverFailureSkipsRest(t *testing.T) {
	fake := happyFake()
	fake.failOn = map[string]error{"click": errors.New("element detached")}
	w := NewBrowserWorker(fake, nil)
	j := browserJourney()

	run, err := w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed || !strings.Contains(run.Error, "element detached") {
		t.Fatalf("status=%s error=%q", run.Status, run.Error)
	}
	requireOneEvent(t, run, EventAction, StepFailed, "element detached")
	// wait + select (steps 3,4) recorded as skipped, never executed.
	skips := findEvents(t, run, StepSkipped)
	if len(skips) != 2 {
		t.Fatalf("expected 2 skipped events, got %+v", skips)
	}
	for _, ev := range skips {
		if ev.StepIndex != 3 && ev.StepIndex != 4 {
			t.Fatalf("unexpected skipped step index %d", ev.StepIndex)
		}
	}
}

func TestBrowserWorkerWriteBlockedInReadOnly(t *testing.T) {
	fake := happyFake()
	w := NewBrowserWorker(fake, nil)
	j := browserJourney()
	j.Steps[1].WriteOp = true // fill #email is a state-changing op
	env := browserTestEnv()
	env.Mode = ModeProductionReadOnly

	run, err := w.Execute(context.Background(), j, env, nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed {
		t.Fatalf("status = %s, want failed", run.Status)
	}
	blocked := requireOneEvent(t, run, EventAction, StepBlocked, "read-only mode")
	if blocked.StepIndex != 1 {
		t.Fatalf("write guard should trip at fill step (index 1), got %d", blocked.StepIndex)
	}
	if !strings.Contains(blocked.Message, "fill #email") {
		t.Fatalf("blocked message lacks step context: %q", blocked.Message)
	}
	for _, c := range fake.calls {
		if strings.HasPrefix(c, "fill") {
			t.Fatalf("write op reached the driver despite read-only mode: %v", fake.calls)
		}
	}
}

func TestBrowserWorkerNavigateScopeViolation(t *testing.T) {
	fake := happyFake()
	w := NewBrowserWorker(fake, nil)
	j := browserJourney()
	j.Steps = []JourneyStep{{Index: 0, Action: "navigate", Target: "https://evil.test/login"}}
	env := browserTestEnv() // allowlist: app.test only

	run, err := w.Execute(context.Background(), j, env, nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed {
		t.Fatalf("status = %s, want failed", run.Status)
	}
	requireOneEvent(t, run, EventAction, StepBlocked, "outside authorized test scope")
	if fake.callCount() > 1 { // start_trace happened, navigate must not
		for _, c := range fake.calls {
			if strings.HasPrefix(c, "navigate") {
				t.Fatalf("driver navigated outside scope: %v", fake.calls)
			}
		}
	}
}

func TestBrowserWorkerBudgetExhausted(t *testing.T) {
	fake := happyFake()
	w := NewBrowserWorker(fake, nil)
	j := browserJourney()

	run, err := w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed || !strings.Contains(run.Error, "budget") {
		t.Fatalf("status=%s error=%q", run.Status, run.Error)
	}
	requireOneEvent(t, run, EventAction, StepBlocked, ErrBudgetExceeded.Error())
	// Exactly 2 steps executed; steps 2..4 skipped.
	skips := findEvents(t, run, StepSkipped)
	if len(skips) != 2 { // steps 2,3 (blocked consumed slot 2 of budget)
		t.Fatalf("expected 2 skipped events after budget block, got %+v", skips)
	}
}

func TestBrowserWorkerDefaultBudgetFromEnvironment(t *testing.T) {
	if got := effectiveBudget(RunOptions{}, nil); got != DefaultBudgetPerRun {
		t.Fatalf("default budget = %d", got)
	}
	env := &Environment{BudgetPerRun: 50}
	if got := effectiveBudget(RunOptions{}, env); got != 50 {
		t.Fatalf("env budget = %d", got)
	}
	if got := effectiveBudget(RunOptions{MaxSteps: 7}, env); got != 7 {
		t.Fatalf("MaxSteps must win, got %d", got)
	}
}

func TestBrowserWorkerRateCap(t *testing.T) {
	fake := happyFake()
	clock := NewManualClock(time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))
	w := NewBrowserWorker(fake, nil)
	w.Clock = clock
	j := browserJourney()
	j.Steps = j.Steps[:3] // navigate, fill, click
	env := browserTestEnv()
	env.RateCapPerMin = 2

	run, err := w.Execute(context.Background(), j, env, nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed || !strings.Contains(run.Error, "rate cap") {
		t.Fatalf("status=%s error=%q", run.Status, run.Error)
	}
	requireOneEvent(t, run, EventAction, StepBlocked, "rate cap")
}

func TestBrowserWorkerWallClockCap(t *testing.T) {
	fake := happyFake()
	clock := NewManualClock(time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))
	fake.onAction = func(action string) { clock.Advance(2 * time.Hour) }
	w := NewBrowserWorker(fake, nil)
	w.Clock = clock
	w.RunWallClock = time.Hour
	j := browserJourney()

	run, err := w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed || !strings.Contains(run.Error, "wall-clock cap") {
		t.Fatalf("status=%s error=%q", run.Status, run.Error)
	}
	requireOneEvent(t, run, EventAction, StepBlocked, "wall-clock cap")
}

func TestBrowserWorkerStepTimeout(t *testing.T) {
	fake := happyFake()
	fake.waitGate = make(chan struct{}) // Wait blocks until ctx deadline
	w := NewBrowserWorker(fake, nil)
	w.StepTimeout = 5 * time.Millisecond
	j := browserJourney()
	j.Steps = []JourneyStep{
		{Index: 0, Action: "navigate", Target: "https://app.test/login"},
		{Index: 1, Action: "wait", Target: "#never-appears", TimeoutMS: 5},
		{Index: 2, Action: "click", Target: "#after"},
	}

	run, err := w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed || !strings.Contains(run.Error, "step timeout") {
		t.Fatalf("status=%s error=%q", run.Status, run.Error)
	}
	requireOneEvent(t, run, EventError, StepFailed, "step timeout")
	if len(findEvents(t, run, StepSkipped)) != 1 {
		t.Fatalf("post-timeout step not skipped: %+v", run.StepEvents)
	}
}

// ---------------------------------------------------------------------------
// BrowserWorker — cancellation semantics (kill switch vs interrupted).
// ---------------------------------------------------------------------------

func TestBrowserWorkerKillSwitchPreClosed(t *testing.T) {
	fake := happyFake()
	w := NewBrowserWorker(fake, nil)
	j := browserJourney()
	kill := make(chan struct{})
	close(kill)

	run, err := w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{KillSwitch: kill})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunCancelled || !strings.Contains(run.Error, ErrKilled.Error()) {
		t.Fatalf("status=%s error=%q", run.Status, run.Error)
	}
	for _, c := range fake.calls {
		if !strings.HasPrefix(c, "start_trace") {
			t.Fatalf("driver work happened despite pre-closed kill switch: %v", fake.calls)
		}
	}
	requireOneEvent(t, run, EventError, string(RunCancelled), "kill switch")
}

func TestBrowserWorkerKillSwitchMidRun(t *testing.T) {
	fake := happyFake()
	fake.waitGate = make(chan struct{})
	fake.waitStarted = make(chan struct{})
	w := NewBrowserWorker(fake, nil)
	j := browserJourney()
	j.Steps = []JourneyStep{
		{Index: 0, Action: "navigate", Target: "https://app.test/login"},
		{Index: 1, Action: "wait", Target: "#dashboard"},
		{Index: 2, Action: "click", Target: "#after"},
	}
	kill := make(chan struct{})

	done := make(chan struct{})
	var run *Run
	var execErr error
	go func() {
		defer close(done)
		run, execErr = w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{KillSwitch: kill})
	}()

	select {
	case <-fake.waitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never reached the wait step")
	}
	close(kill) // operator kill while the run is mid-step
	<-done

	if execErr != nil {
		t.Fatalf("execute error: %v", execErr)
	}
	if run.Status != RunCancelled || !strings.Contains(run.Error, ErrKilled.Error()) {
		t.Fatalf("status=%s error=%q", run.Status, run.Error)
	}
	// The interrupted wait is an error event mirroring the cancellation;
	// the never-reached step 2 must be absent (no silent skip pollution).
	requireOneEvent(t, run, EventError, string(RunCancelled), "kill switch")
	for _, ev := range run.StepEvents {
		if ev.StepIndex == 2 {
			t.Fatalf("step 2 must not be recorded on kill: %+v", ev)
		}
	}
}

func TestBrowserWorkerParentCancelInconclusive(t *testing.T) {
	fake := happyFake()
	fake.waitGate = make(chan struct{})
	fake.waitStarted = make(chan struct{})
	w := NewBrowserWorker(fake, nil)
	j := browserJourney()
	j.Steps = []JourneyStep{
		{Index: 0, Action: "navigate", Target: "https://app.test/login"},
		{Index: 1, Action: "wait", Target: "#dashboard"},
	}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var run *Run
	go func() {
		defer close(done)
		run, _ = w.Execute(ctx, j, browserTestEnv(), nil, RunOptions{})
	}()

	select {
	case <-fake.waitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never reached the wait step")
	}
	cancel() // harness death — never silently lost
	<-done

	if run.Status != RunInconclusive {
		t.Fatalf("status = %s, want inconclusive (error: %s)", run.Status, run.Error)
	}
	if !strings.Contains(run.Error, "interrupted") {
		t.Fatalf("error should explain the interruption: %q", run.Error)
	}
	requireOneEvent(t, run, EventError, string(RunInconclusive), "interrupted")
}

func TestBrowserWorkerConcurrencyCap(t *testing.T) {
	fake := happyFake()
	gate := make(chan struct{})
	fake.waitGate = gate
	fake.waitStarted = make(chan struct{})
	w := NewBrowserWorker(fake, nil)
	w.MaxInFlight = 1

	j := browserJourney()
	j.Steps = []JourneyStep{
		{Index: 0, Action: "navigate", Target: "https://app.test/login"},
		{Index: 1, Action: "wait", Target: "#dashboard"},
	}

	done := make(chan *Run)
	go func() {
		run, _ := w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{})
		done <- run
	}()
	select {
	case <-fake.waitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first run never started")
	}

	kill := make(chan struct{})
	close(kill) // deterministic: second Execute must not block on the slot
	if _, err := w.Execute(context.Background(), j, browserTestEnv(), nil, RunOptions{KillSwitch: kill}); !errors.Is(err, ErrWorkerBusy) {
		t.Fatalf("expected ErrWorkerBusy, got %v", err)
	}

	close(gate)
	first := <-done
	if first.Status != RunCompleted {
		t.Fatalf("first run status = %s (%s)", first.Status, first.Error)
	}
}

// ---------------------------------------------------------------------------
// Shared helpers.
// ---------------------------------------------------------------------------

func TestRedactHeaders(t *testing.T) {
	in := map[string]string{
		"Authorization":       "Bearer secret-token",
		"authorization":       "Bearer lower",
		"Cookie":              "session=abc",
		"Set-Cookie":          "sid=xyz; HttpOnly",
		"Proxy-Authorization": "Basic aaa",
		"X-Request-Id":        "req-1",
		"Content-Type":        "application/json",
	}
	out := RedactHeaders(in)
	for _, k := range []string{"Authorization", "authorization", "Cookie", "Set-Cookie", "Proxy-Authorization"} {
		if out[k] != RedactedPlaceholder {
			t.Fatalf("header %q not redacted: %q", k, out[k])
		}
	}
	if out["X-Request-Id"] != "req-1" || out["Content-Type"] != "application/json" {
		t.Fatalf("benign headers must pass through: %+v", out)
	}
	if in["Authorization"] == RedactedPlaceholder {
		t.Fatal("input map must not be mutated")
	}
	mv := RedactHeader(map[string][]string{"Set-Cookie": {"a=1", "b=2"}, "Accept": {"*/*"}})
	if mv["Set-Cookie"] != RedactedPlaceholder || mv["Accept"] != "*/*" {
		t.Fatalf("multi-value redaction wrong: %+v", mv)
	}
}

func TestHostInAllowlist(t *testing.T) {
	allow := []string{"app.test", "*.staging.example.com"}
	cases := []struct {
		host string
		want bool
	}{
		{"app.test", true},
		{"APP.TEST:443", true},
		{"api.app.test", false},
		{"x.staging.example.com", true},
		{"staging.example.com", false}, // wildcard matches only subdomains
		{"evil.test", false},
	}
	for _, c := range cases {
		if got := hostInAllowlist(c.host, allow); got != c.want {
			t.Errorf("hostInAllowlist(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	if !hostInAllowlist("anything.test", nil) {
		t.Error("empty allowlist must disable the fence")
	}
	if !hostInAllowlist("app.test", []string{"*"}) {
		t.Error("* must allow any host")
	}
}

func TestRateLimiterWindow(t *testing.T) {
	clock := NewManualClock(time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
	l := newRateLimiter(2, time.Minute, clock)
	if !l.allow() || !l.allow() {
		t.Fatal("first two hits must pass")
	}
	if l.allow() {
		t.Fatal("third hit inside window must be capped")
	}
	clock.Advance(61 * time.Second)
	if !l.allow() {
		t.Fatal("hit after window must pass")
	}
	if newRateLimiter(0, time.Minute, clock).allow() == false {
		t.Fatal("cap 0 means unlimited")
	}
}

func TestMatchStatusClassAndURLPattern(t *testing.T) {
	if !matchStatusClass("200", 200) || matchStatusClass("200", 201) {
		t.Fatal("literal status match broken")
	}
	if !matchStatusClass("2xx", 204) || !matchStatusClass("4xx", 404) || !matchStatusClass("5xx", 500) {
		t.Fatal("class match broken")
	}
	if matchStatusClass("3xx", 200) || matchStatusClass("3xx", 299) || matchStatusClass("2xx", 300) {
		t.Fatal("class boundaries wrong")
	}
	ok, err := matchURLPattern(`^https://app\.test/dash.*`, "https://app.test/dashboard?x=1")
	if err != nil || !ok {
		t.Fatalf("url_pattern match failed: %v %v", ok, err)
	}
	if _, err := matchURLPattern("([", "x"); err == nil {
		t.Fatal("invalid pattern must error")
	}
	if n, err := parseNonNegative("42"); err != nil || n != 42 {
		t.Fatalf("parseNonNegative = %d, %v", n, err)
	}
	if _, err := parseNonNegative("-1"); err == nil {
		t.Fatal("negative bounds must be rejected")
	}
}

func TestSanitizeURL(t *testing.T) {
	u := mustParseURL(t, "https://user:pass@app.test/p?token=secret#frag")
	if got := sanitizeURL(u); got != "https://app.test/p" {
		t.Fatalf("sanitizeURL = %q", got)
	}
	if sanitizeURL(nil) != "" {
		t.Fatal("nil URL should render empty")
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// ---------------------------------------------------------------------------
// Evidence store.
// ---------------------------------------------------------------------------

func TestInMemoryEvidenceStoreRoundTrip(t *testing.T) {
	store := NewInMemoryEvidenceStore()
	ctx := context.Background()
	ref1, err := store.Put(ctx, "screenshot", []byte("png-1"), map[string]string{"label": "final"})
	if err != nil {
		t.Fatal(err)
	}
	ref2, _ := store.Put(ctx, "trace", []byte("trace-1"), nil)
	if ref1 == ref2 {
		t.Fatal("refs must be unique")
	}
	if !strings.HasPrefix(ref1, "mem://evidence/screenshot-") {
		t.Fatalf("unexpected ref shape %q", ref1)
	}
	payload, ok, err := store.Get(ctx, ref1)
	if err != nil || !ok || string(payload) != "png-1" {
		t.Fatalf("get = %q, %v, %v", payload, ok, err)
	}
	if _, ok, _ := store.Get(ctx, "mem://evidence/nope"); ok {
		t.Fatal("unknown ref must report missing")
	}
	// Stored payloads are copies — mutating a returned payload must not
	// corrupt the store.
	payload[0] = 'X'
	again, _, _ := store.Get(ctx, ref1)
	if string(again) != "png-1" {
		t.Fatal("store payload was aliased")
	}
	if store.Len() != 2 {
		t.Fatalf("Len = %d", store.Len())
	}
	var noop NoopEvidenceStore
	if ref, _ := noop.Put(ctx, "screenshot", []byte("x"), nil); ref != "" {
		t.Fatal("noop store must return empty ref")
	}
	if _, ok, _ := noop.Get(ctx, "any"); ok {
		t.Fatal("noop store must report missing")
	}
}

// ---------------------------------------------------------------------------
// Playwright adapter — honest runtime detection (never faked).
// ---------------------------------------------------------------------------

func TestPlaywrightAdapterHonestDetection(t *testing.T) {
	a := NewPlaywrightCLIAdapter()
	version, ok := a.Detect()
	if !ok {
		// Without the playwright runtime every operation must fail with
		// ErrPlaywrightUnavailable — never a fake success.
		if _, err := a.Navigate(context.Background(), "https://app.test/"); !errors.Is(err, ErrPlaywrightUnavailable) {
			t.Fatalf("Navigate without runtime = %v, want ErrPlaywrightUnavailable", err)
		}
		if err := a.Click(context.Background(), "#x"); !errors.Is(err, ErrPlaywrightUnavailable) {
			t.Fatalf("Click without runtime = %v, want ErrPlaywrightUnavailable", err)
		}
		if _, err := a.ElementText(context.Background(), "#x"); !errors.Is(err, ErrPlaywrightUnavailable) {
			t.Fatalf("ElementText without runtime = %v, want ErrPlaywrightUnavailable", err)
		}
		if _, err := a.StopTrace(context.Background()); !errors.Is(err, ErrPlaywrightUnavailable) {
			t.Fatalf("StopTrace without runtime = %v, want ErrPlaywrightUnavailable", err)
		}
		t.Skip("playwright runtime not installed (node/npx/@playwright) — adapter honestly unavailable")
	}
	t.Logf("playwright runtime detected: %s", version)
}
