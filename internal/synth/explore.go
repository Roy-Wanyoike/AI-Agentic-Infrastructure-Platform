package synth

// explore.go — issue #155: safety-bounded autonomous exploration and
// candidate journey generation for the Synthetic Assurance subsystem.
//
// ExplorePlanner walks a REGISTERED app (verified Target + pinned
// Environment) by proposing navigations from (a) a deterministic scripted
// explorer built from the allowlisted routes/goals and, optionally, (b) a
// pluggable AI explorer (nil by default). EVERY proposed action is
// re-checked server-side by ScopeEnforcer.CheckScope against the pinned
// Environment before it is applied:
//
//   - allowlisted domains only (env.AllowedDomains; strict host match),
//   - write operations denied in read-only modes (ModeProductionReadOnly
//     and any production mode other than ModeReleaseComparison),
//   - writes under ModeReleaseComparison require an isolated synthetic
//     account (SyntheticAccount on the action),
//   - per-run step budget (env.BudgetPerRun, default 200) and time budget,
//   - rate cap (env.RateCapPerMin),
//   - quiet/test windows (evaluateWindows — shared with schedule.go),
//   - immediate cancellation via ctx and the thread-safe KillSwitch.
//
// Untrusted page-derived content is DATA ONLY: observations carry Content
// strings that CheckScope never reads for decisions (the decision path takes
// only env, url and the action fields; see SanitizeUntrusted for how page
// strings are neutralized before they enter the ledger or journey drafts).
// Page content can propose, never authorize.
//
// Output: draft Journey candidates (Source "exploration") that REMAIN
// drafts until explicit human approval (CandidateJourney.Approve converts
// draft -> reviewed and records ApprovedBy/ApprovedAt), plus a CoverageLedger
// (planned/discovered/tested routes, gaps, uncertainty, flags, cost).
//
// This file owns the #155 surface. schedule.go and compare.go (#156) reuse
// KillSwitch and evaluateWindows from here.

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// Exploration-specific sentinel errors (wrap with %w for context).
var (
	// ErrExplorerExhausted: the explorer has no further proposals.
	ErrExplorerExhausted = errors.New("synth: explorer has no further proposals")
	// ErrRiskyWrite: a proposed action matched a risky-write heuristic and
	// exploration was stopped (the flag is recorded in the ledger).
	ErrRiskyWrite = errors.New("synth: risky write action blocked by exploration heuristics")
	// ErrLoopDetected: repeated visited-state hashes triggered loop stop.
	ErrLoopDetected = errors.New("synth: exploration loop detected")
)

// KillSwitch is the thread-safe global/per-project emergency stop shared by
// exploration (#155) and production scheduling (#156). A tripped switch
// denies every subsequent scope decision immediately; resets are explicit
// operator actions. Global trips dominate project trips.
type KillSwitch struct {
	mu           sync.RWMutex
	globalReason string // "" = not tripped
	projects     map[string]string
}

// NewKillSwitch returns a clean (not tripped) switch.
func NewKillSwitch() *KillSwitch {
	return &KillSwitch{projects: make(map[string]string)}
}

// TripGlobal trips the switch for EVERY project with a human-readable reason.
func (k *KillSwitch) TripGlobal(reason string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if strings.TrimSpace(reason) == "" {
		reason = "unspecified"
	}
	k.globalReason = reason
}

// TripProject trips the switch for one project only.
func (k *KillSwitch) TripProject(projectID, reason string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if strings.TrimSpace(reason) == "" {
		reason = "unspecified"
	}
	if k.projects == nil {
		k.projects = make(map[string]string)
	}
	k.projects[projectID] = reason
}

// ResetGlobal clears a global trip (project trips remain).
func (k *KillSwitch) ResetGlobal() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.globalReason = ""
}

// ResetProject clears a per-project trip.
func (k *KillSwitch) ResetProject(projectID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.projects, projectID)
}

// Tripped reports whether the switch is tripped for the given project and,
// if so, the governing reason (global reasons win).
func (k *KillSwitch) Tripped(projectID string) (bool, string) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.globalReason != "" {
		return true, "global: " + k.globalReason
	}
	if r, ok := k.projects[projectID]; ok {
		return true, "project: " + r
	}
	return false, ""
}

// SanitizeUntrusted neutralizes a page-derived string so it can never
// smuggle control characters or newlines into structured records (ledger
// entries, journey steps, plan metadata). Control characters are stripped,
// whitespace runs collapse to single spaces and the result is truncated to
// max runes (max <= 0 selects the default 512). The output is DATA ONLY:
// callers must never feed it back into policy inputs.
func SanitizeUntrusted(s string, max int) string {
	if max <= 0 {
		max = 512
	}
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		if space {
			if b.Len() > 0 {
				b.WriteRune(' ')
			}
			space = false
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}

// Action is one proposed exploration action (the Explorer seam vocabulary
// mirrors JourneyStep.Action). Fields other than Kind/URL/Target/Value are
// planner-supplied context; explorers fill Kind/URL/Target/Value only.
type Action struct {
	Kind             string // navigate|click|fill|select|wait|api_call|assert|extract
	URL              string // navigate target (absolute, or path resolved against Target.BaseURL)
	Target           string // selector for click/fill/select/extract
	Value            string // input value for fill/select
	WriteOp          bool   // true = state-changing; enforced against Environment.Mode
	SyntheticAccount string // REQUIRED for writes under ModeReleaseComparison (isolation)
	TimeoutMS        int    // 0 = worker default (carried into JourneyStep)
}

// Observation is a page-derived fact captured after an executed action.
// Title/Content/Links are UNTRUSTED DATA: they are carried for explorers and
// the ledger, and are structurally invisible to CheckScope (which only ever
// receives env, url and an Action).
type Observation struct {
	URL     string   // page URL (absolute)
	Title   string   // sanitized page title
	Links   []string // raw hrefs seen on the page (data only; navigation still scope-checked)
	Content string   // sanitized page text excerpt (data only)
	At      time.Time
}

// ExplorationState is the planner-visible session snapshot handed to
// Explorer.ProposeNext. It contains NO policy inputs: only observations.
type ExplorationState struct {
	RunID         string
	EnvironmentID string
	TargetID      string
	CurrentURL    string
	StepIndex     int // executed steps so far
	Elapsed       time.Duration
	VisitedHashes []string      // hashes of executed (kind,url,selector,value) tuples, in order
	Observations  []Observation // most recent observations (newest last, capped)
}

// ExplorationRoute is an allowlisted route the app owner registered for
// exploration. Empty Roles means any persona may walk it.
type ExplorationRoute struct {
	Path        string   // must start with "/"
	Description string   // human description (journey naming)
	Roles       []string // persona RoleHints that may walk this route
}

// ExplorationGoal is a coverage objective: a named set of routes to reach.
type ExplorationGoal struct {
	ID          string
	Description string
	Routes      []string // route paths; must exist among the registered routes
}

// ExplorationPlan is the ordered candidate navigation list built from
// routes/goals/persona role hints. Deterministic: identical inputs produce
// an identical plan (and plan ID).
type ExplorationPlan struct {
	ID            string
	OrgID         string
	ProjectID     string
	EnvironmentID string
	PersonaID     string
	TargetBaseURL string
	GoalIDs       []string
	Steps         []ExplorationPlanStep
	CreatedAt     time.Time
}

// ExplorationPlanStep is one planned navigation. GoalID "seed" marks the
// landing-page navigation that always leads the plan.
type ExplorationPlanStep struct {
	Index  int
	Path   string
	GoalID string
}

// Explorer is the browser-agent seam: given the current exploration state,
// propose the next action. Proposals are ADVISORY — the planner re-checks
// every one server-side via ScopeEnforcer.CheckScope before applying it.
// The deterministic ScriptedExplorer implements this interface; AI explorers
// plug in through AIExplorer.
type Explorer interface {
	ProposeNext(ctx context.Context, state ExplorationState) (Action, error)
}

// AIExplorer is the pluggable model-driven explorer extension (nil by
// default — pure-script mode). It is consulted only after the scripted
// explorer is exhausted, and its proposals are scope-checked exactly like
// scripted ones. Model identifies the implementation for the result record.
type AIExplorer interface {
	Explorer
	Model() string
}

// ScriptedExplorer is the deterministic Explorer implementation: it replays
// a fixed action list in order and returns ErrExplorerExhausted afterwards.
type ScriptedExplorer struct {
	mu     sync.Mutex
	script []Action
	next   int
}

// NewScriptedExplorer returns an explorer replaying script in order.
func NewScriptedExplorer(script []Action) *ScriptedExplorer {
	return &ScriptedExplorer{script: append([]Action(nil), script...)}
}

// Append adds further actions to the script (used to extend plans).
func (s *ScriptedExplorer) Append(actions ...Action) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = append(s.script, actions...)
}

// Remaining reports how many actions are left (test/diagnostic aid).
func (s *ScriptedExplorer) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.script) - s.next
}

// ProposeNext implements Explorer.
func (s *ScriptedExplorer) ProposeNext(ctx context.Context, _ ExplorationState) (Action, error) {
	if err := ctx.Err(); err != nil {
		return Action{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.script) {
		return Action{}, ErrExplorerExhausted
	}
	a := s.script[s.next]
	s.next++
	return a, nil
}

// Observer is the browser-agent observation seam: after each executed
// in-scope action, an optional Observer returns page-derived observations.
// Observer output is UNTRUSTED DATA (see Observation) — it can never expand
// scope because CheckScope never reads it. Nil default: headless planning.
type Observer interface {
	Observe(ctx context.Context, step ExecutedStep) []Observation
}

// ExecutedStep is one applied (scope-allowed) action with its decision.
type ExecutedStep struct {
	Index    int
	Action   Action
	URL      string // absolute URL the action targeted
	Decision ScopeDecision
	GoalID   string // goal attribution (bookkeeping only, never policy)
	At       time.Time
}

// explorationActionKinds is the accepted action vocabulary.
var explorationActionKinds = map[string]bool{
	"navigate": true, "click": true, "fill": true, "select": true,
	"wait": true, "api_call": true, "assert": true, "extract": true,
}

// ScopeEnforcer enforces the pinned Environment policy server-side on every
// proposed action. It is safe for concurrent use; per-run budgets are keyed
// by run id (BeginRun + WithScopeRun). Decision reason precedence:
// kill_switch > context_cancelled > step/time budget > rate_capped >
// quiet_window > invalid_action > invalid_url > allowlisted_domain >
// write policy > allowed.
type ScopeEnforcer struct {
	kill *KillSwitch
	now  func() time.Time

	mu   sync.Mutex
	runs map[string]*scopeRunState

	// envProject associates environment ids with owning project ids so
	// CheckScope can evaluate per-project kill switches (the pinned
	// Environment type carries no project column; Target does).
	envProject map[string]string

	// DefaultStepBudget is the per-run step cap when env.BudgetPerRun == 0.
	DefaultStepBudget int
	// RunTimeBudget caps wall-clock time per registered run (<= 0 selects
	// the 10-minute default; negative values are treated as the default).
	RunTimeBudget time.Duration
}

type scopeRunState struct {
	steps      int
	started    time.Time
	lastAction time.Time
}

type scopeRunCtxKey struct{}

// WithScopeRun binds a run id into ctx so CheckScope can attribute budgets.
func WithScopeRun(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, scopeRunCtxKey{}, runID)
}

// NewScopeEnforcer returns an enforcer bound to kill (nil = fresh switch).
func NewScopeEnforcer(kill *KillSwitch) *ScopeEnforcer {
	if kill == nil {
		kill = NewKillSwitch()
	}
	return &ScopeEnforcer{
		kill:              kill,
		now:               time.Now,
		runs:              make(map[string]*scopeRunState),
		envProject:        make(map[string]string),
		DefaultStepBudget: 200,
		RunTimeBudget:     10 * time.Minute,
	}
}

// RegisterProject associates an environment id with its owning project so
// CheckScope can evaluate per-project kill switches for that environment.
func (e *ScopeEnforcer) RegisterProject(envID, projectID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.envProject[envID] = projectID
}

// projectForEnv resolves the kill-switch key for an environment (the
// environment id itself when no project was registered).
func (e *ScopeEnforcer) projectForEnv(envID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.envProject[envID]; ok && p != "" {
		return p
	}
	return envID
}

// WithClock injects a deterministic clock (tests); returns the enforcer.
func (e *ScopeEnforcer) WithClock(now func() time.Time) *ScopeEnforcer {
	if now != nil {
		e.now = now
	}
	return e
}

// BeginRun registers a run for budget accounting (steps/time/rate).
func (e *ScopeEnforcer) BeginRun(runID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runs[runID] = &scopeRunState{started: e.now().UTC()}
}

// ResetRun drops a run's accounting state.
func (e *ScopeEnforcer) ResetRun(runID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.runs, runID)
}

// CheckScope returns the server-side verdict for one proposed action against
// the pinned Environment. url is the absolute URL the action will load or
// act on (planner-resolved for navigations; the current page otherwise).
//
// Untrusted page content is structurally excluded: this method's inputs are
// the environment, the url string and the action fields — never
// Observation.Content. Content can therefore propose URLs but can never
// grant scope.
//
// On allow, the run's step counter and rate-limiter timestamps advance.
func (e *ScopeEnforcer) CheckScope(ctx context.Context, env Environment, rawURL string, action Action) ScopeDecision {
	now := e.now().UTC()
	deny := func(reason string) ScopeDecision {
		return ScopeDecision{Allowed: false, Reason: reason, At: now}
	}

	runID, _ := ctx.Value(scopeRunCtxKey{}).(string)
	if runID == "" {
		return deny("unregistered_run")
	}
	e.mu.Lock()
	st, ok := e.runs[runID]
	e.mu.Unlock()
	if !ok {
		return deny("unregistered_run")
	}

	if tripped, _ := e.kill.Tripped(e.projectForEnv(env.ID)); tripped {
		return deny("kill_switch")
	}
	if err := ctx.Err(); err != nil {
		return deny("context_cancelled")
	}

	// Per-run step and time budgets.
	maxSteps := env.BudgetPerRun
	if maxSteps <= 0 {
		maxSteps = e.DefaultStepBudget
	}
	timeBudget := e.RunTimeBudget
	if timeBudget <= 0 {
		timeBudget = 10 * time.Minute
	}
	e.mu.Lock()
	if st.steps >= maxSteps {
		e.mu.Unlock()
		return deny("step_budget")
	}
	if now.Sub(st.started) >= timeBudget {
		e.mu.Unlock()
		return deny("time_budget")
	}
	if env.RateCapPerMin > 0 && !st.lastAction.IsZero() {
		if gap := time.Minute / time.Duration(env.RateCapPerMin); now.Sub(st.lastAction) < gap {
			e.mu.Unlock()
			return deny("rate_capped")
		}
	}
	e.mu.Unlock()

	// Quiet/test windows (fail-closed on unparsable policy).
	blocked, _, werr := evaluateWindows(env, now)
	if werr != nil {
		return deny("invalid_window_policy")
	}
	if blocked {
		return deny("quiet_window")
	}

	if !explorationActionKinds[action.Kind] {
		return deny("invalid_action")
	}

	// URL validation + strict allowlisted-domain policy.
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return deny("invalid_url")
	}
	host := strings.ToLower(u.Host) // host[:port]
	if !domainAllowed(env.AllowedDomains, host) {
		return deny("allowlisted_domain")
	}

	// Write policy: read-only default enforced server-side. Production
	// writes are possible ONLY in ModeReleaseComparison AND with an
	// isolated synthetic account on the action.
	if action.WriteOp {
		switch {
		case env.Mode == ModeProductionReadOnly:
			return deny("write_denied_readonly")
		case env.Kind == EnvProduction && env.Mode != ModeReleaseComparison:
			// Misconfigured production environment: deny regardless of mode.
			return deny("write_denied_readonly")
		case env.Mode == ModeReleaseComparison && strings.TrimSpace(action.SyntheticAccount) == "":
			return deny("write_requires_synthetic_account")
		}
	}

	e.mu.Lock()
	st.steps++
	st.lastAction = now
	e.mu.Unlock()
	return ScopeDecision{Allowed: true, Reason: "allowed", At: now}
}

// domainAllowed matches host[:port] against the allowlist (exact match,
// case-insensitive). A port-qualified entry matches only the same port.
func domainAllowed(allowed []string, host string) bool {
	for _, a := range allowed {
		if strings.ToLower(strings.TrimSpace(a)) == host {
			return true
		}
	}
	return false
}

// resolveActionURL resolves action URL references against the target base.
// Absolute URLs pass through (validated later by CheckScope); relative paths
// resolve against base per RFC 3986.
func resolveActionURL(base, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty url")
	}
	bu, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("bad base url: %w", err)
	}
	ru, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("bad action url: %w", err)
	}
	return bu.ResolveReference(ru).String(), nil
}

// riskyWritePatterns are state-change heuristics: a write-ish action whose
// selector/value/path matches one of these stops exploration and is flagged.
// They are intentionally coarse (exploration never NEEDS to perform
// destructive actions — deeper coverage belongs to approved journeys).
var riskyWritePatterns = []string{
	"delete", "remove", "purge", "drop", "destroy", "shutdown",
	"payment", "checkout", "purchase", "billing", "refund", "card",
	"password", "invite", "transfer", "withdraw", "export",
}

// RiskyWriteReason reports "" when the action is safe to consider, or a
// "risky_write: <detail>" reason when a state-change heuristic matched.
// Write-ish means: declared WriteOp, or an input/API action (fill, select,
// api_call) that could mutate state even without the flag.
func RiskyWriteReason(a Action) string {
	writeIsh := a.WriteOp || a.Kind == "fill" || a.Kind == "select" || a.Kind == "api_call"
	if !writeIsh {
		return ""
	}
	hay := strings.ToLower(a.Target + " \x1f " + a.Value + " \x1f " + a.URL)
	for _, p := range riskyWritePatterns {
		if strings.Contains(hay, p) {
			return fmt.Sprintf("risky_write: pattern %q on %s", p, a.Kind)
		}
	}
	return ""
}

// actionHash derives the visited-state hash for repeat/loop detection: a
// stable FNV-1a digest over the action identity (kind, url, selector,
// value). Identical proposals from any origin are repeats.
func actionHash(a Action) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(a.Kind + "\x1f" + a.URL + "\x1f" + a.Target + "\x1f" + a.Value))
	return fmt.Sprintf("%x", h.Sum64())
}

// planHash derives a deterministic plan ID from the plan inputs.
func planHash(parts ...string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.Join(parts, "\x1e")))
	return fmt.Sprintf("%x", h.Sum64())
}

// parseHHMM parses "HH:MM" into (minutes since midnight, error).
func parseHHMM(s string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid HH:MM %q", s)
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0, 0, fmt.Errorf("invalid HH:MM %q", s)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0, 0, fmt.Errorf("invalid HH:MM %q", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid HH:MM %q", s)
	}
	return h*60 + m, h, nil
}

// weekdayIn reports whether the IANA-resolved weekday is listed (0=Sunday).
func weekdayIn(days []int, wd time.Weekday) bool {
	for _, d := range days {
		if d == int(wd) {
			return true
		}
	}
	return false
}

// windowContains reports whether t is inside one QuietWindow and, if so,
// when the current interval ends. Overnight windows (End <= Start) span
// midnight and match either the listing day's evening or the next morning.
// A window whose Location fails to load is an error (fail-closed).
func windowContains(w QuietWindow, t time.Time) (bool, time.Time, error) {
	loc, err := time.LoadLocation(w.Location)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("bad window location %q: %w", w.Location, err)
	}
	startMin, _, err := parseHHMM(w.Start)
	if err != nil {
		return false, time.Time{}, err
	}
	endMin, _, err := parseHHMM(w.End)
	if err != nil {
		return false, time.Time{}, err
	}
	lt := t.In(loc)
	dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)

	contains := func(baseDay time.Time, wd time.Weekday) (bool, time.Time) {
		s := baseDay.Add(time.Duration(startMin) * time.Minute)
		e := baseDay.Add(time.Duration(endMin) * time.Minute)
		if endMin <= startMin { // overnight: ends the NEXT day
			e = e.AddDate(0, 0, 1)
		}
		if !weekdayIn(w.Weekdays, wd) {
			return false, time.Time{}
		}
		return !lt.Before(s) && !lt.After(e), e
	}

	// Same-day interpretation.
	if in, end := contains(dayStart, lt.Weekday()); in {
		return true, end, nil
	}
	// Overnight continuation started yesterday.
	if endMin <= startMin {
		if in, end := contains(dayStart.AddDate(0, 0, -1), lt.AddDate(0, 0, -1).Weekday()); in {
			return true, end, nil
		}
	}
	return false, time.Time{}, nil
}

// nextWindowStart returns the earliest opening of w at or after t.
func nextWindowStart(w QuietWindow, t time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(w.Location)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad window location %q: %w", w.Location, err)
	}
	startMin, _, err := parseHHMM(w.Start)
	if err != nil {
		return time.Time{}, err
	}
	lt := t.In(loc)
	for d := 0; d <= 7; d++ {
		day := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, d)
		if !weekdayIn(w.Weekdays, day.Weekday()) {
			continue
		}
		start := day.Add(time.Duration(startMin) * time.Minute)
		if !start.Before(t) {
			return start, nil
		}
	}
	return time.Time{}, nil
}

// evaluateWindows reports whether t is blocked by env's window policy and
// the advisory earliest time running becomes allowed again (scanned over
// the next 8 days). Fail-closed: an unparsable window (bad IANA location or
// HH:MM) is an error and callers MUST treat the environment as unrunnable.
// Shared by the #155 scope enforcer and the #156 production scheduler.
func evaluateWindows(env Environment, t time.Time) (blocked bool, nextAllowed time.Time, err error) {
	nextAllowed = t
	var quietUntil time.Time
	for _, w := range env.QuietWindows {
		in, end, werr := windowContains(w, t)
		if werr != nil {
			return false, time.Time{}, werr
		}
		if in && (quietUntil.IsZero() || end.Before(quietUntil)) {
			quietUntil = end
		}
	}
	if quietUntil.After(nextAllowed) {
		blocked = true
		nextAllowed = quietUntil
	}
	if len(env.TestWindows) > 0 {
		inAny := false
		nextStart := time.Time{}
		for _, w := range env.TestWindows {
			in, _, werr := windowContains(w, t)
			if werr != nil {
				return false, time.Time{}, werr
			}
			if in {
				inAny = true
				break
			}
			ns, werr := nextWindowStart(w, t)
			if werr != nil {
				return false, time.Time{}, werr
			}
			if !ns.IsZero() && (nextStart.IsZero() || ns.Before(nextStart)) {
				nextStart = ns
			}
		}
		if !inAny {
			blocked = true
			if !nextStart.IsZero() && nextStart.After(nextAllowed) {
				nextAllowed = nextStart
			}
		}
	}
	return blocked, nextAllowed, nil
}

// ExplorationCost is the exploration cost accounting.
type ExplorationCost struct {
	Steps    int           // applied (scope-allowed) actions
	Denied   int           // proposals denied by scope/loop checks
	Duration time.Duration // wall-clock run duration
}

// CoverageLedger is the exploration coverage record: planned vs discovered
// vs tested routes, gaps, uncertainty (denied/unparseable proposals), safety
// flags and cost. All strings derived from page content are sanitized.
type CoverageLedger struct {
	PlannedRoutes    []string
	DiscoveredRoutes []string // unique paths reached by executed navigations
	TestedRoutes     []string // unique paths with at least one executed in-scope action
	Gaps             []string // planned routes never tested
	Uncertainty      []string // denied/unparseable proposals (sanitized detail)
	Flags            []string // risky_write / loop flags (sanitized detail)
	VisitedStates    int      // executed unique state hashes
	Cost             ExplorationCost
}

// CoveragePercent is the share of planned routes with at least one executed
// in-scope action (0 when nothing was planned).
func (l CoverageLedger) CoveragePercent() float64 {
	if len(l.PlannedRoutes) == 0 {
		return 0
	}
	tested := make(map[string]bool, len(l.TestedRoutes))
	for _, p := range l.TestedRoutes {
		tested[p] = true
	}
	covered := 0
	for _, p := range l.PlannedRoutes {
		if tested[p] {
			covered++
		}
	}
	return float64(covered) / float64(len(l.PlannedRoutes)) * 100
}

// CandidateJourney pairs a draft Journey (Source "exploration") with its
// draft approval metadata. The Journey type is the pinned #118 contract and
// carries no approval fields, so approval lives HERE: a candidate stays a
// draft until Approve records the explicit human approval and converts the
// status to reviewed. Without that call the candidate is ineligible for
// scheduling forever.
type CandidateJourney struct {
	Journey     Journey
	GoalID      string // owning exploration goal ("ungrouped" for extras)
	CoverageRef string // originating ExplorationPlan ID
	ApprovedBy  string // "" until explicit approval
	ApprovedAt  *time.Time
}

// Approve records explicit human approval and converts draft -> reviewed.
// It fails with ErrInvalid for non-draft candidates and empty approvers.
func (c *CandidateJourney) Approve(by string, at time.Time) error {
	if strings.TrimSpace(by) == "" {
		return fmt.Errorf("%w: approver identity required", ErrInvalid)
	}
	if c.Journey.Status != JourneyDraft {
		return fmt.Errorf("%w: candidate journey is not a draft", ErrInvalid)
	}
	t := at.UTC()
	c.ApprovedBy = by
	c.ApprovedAt = &t
	c.Journey.Status = JourneyReviewed
	c.Journey.UpdatedAt = t
	return nil
}

// ExplorationResult is the outcome of one exploration run.
type ExplorationResult struct {
	PlanID        string
	RunID         string
	StartedAt     time.Time
	FinishedAt    time.Time
	ExplorerUsed  string // "scripted" or the AI explorer's Model()
	StoppedReason string // completed|explorer_exhausted|explorer_error|budget|kill_switch|context_cancelled|risky_write|loop_detected|scope_denials
	Ledger        CoverageLedger
	Steps         []ExecutedStep
	Candidates    []CandidateJourney
}

// ExplorePlannerConfig carries optional planner dependencies/limits; zero
// values select documented defaults.
type ExplorePlannerConfig struct {
	Enforcer *ScopeEnforcer // nil = NewScopeEnforcer(nil)
	Explorer Explorer       // nil = deterministic ScriptedExplorer over the plan
	AI       AIExplorer     // nil default: pure-script mode
	Observer Observer       // nil default: no observations
	Now      func() time.Time
	// MaxLoopDenials consecutive repeat proposals tolerated before the run
	// stops with loop_detected (default 3).
	MaxLoopDenials int
	// MaxScopeSkips non-hard scope denials tolerated before the run stops
	// with scope_denials (default 5).
	MaxScopeSkips int
}

// ExplorePlanner builds exploration plans from a registered app's permitted
// routes/roles/goals and runs safety-bounded exploration walks over them,
// producing draft candidate journeys and a coverage ledger. Construct via
// NewExplorePlanner (validates the target/environment and builds the plan).
type ExplorePlanner struct {
	Target  Target
	Env     Environment
	Persona Persona
	Routes  []ExplorationRoute
	Goals   []ExplorationGoal

	Explorer Explorer
	AI       AIExplorer
	Observer Observer
	Enforcer *ScopeEnforcer

	MaxLoopDenials int
	MaxScopeSkips  int

	now  func() time.Time
	plan *ExplorationPlan
}

// NewExplorePlanner validates inputs and builds the deterministic plan.
// Fails with ErrTargetUnverified for unverified targets, ErrInvalid for
// empty allowlists, malformed routes/goals or an unusable base URL.
func NewExplorePlanner(target Target, env Environment, persona Persona, routes []ExplorationRoute, goals []ExplorationGoal, cfg ExplorePlannerConfig) (*ExplorePlanner, error) {
	if !target.Verified {
		return nil, fmt.Errorf("%w: target %s", ErrTargetUnverified, target.ID)
	}
	if len(env.AllowedDomains) == 0 {
		return nil, fmt.Errorf("%w: environment must allowlist at least one domain", ErrInvalid)
	}
	if strings.TrimSpace(persona.ID) == "" {
		return nil, fmt.Errorf("%w: persona required", ErrInvalid)
	}
	bu, err := url.Parse(strings.TrimSpace(target.BaseURL))
	if err != nil || bu.Host == "" || (bu.Scheme != "http" && bu.Scheme != "https") {
		return nil, fmt.Errorf("%w: target base URL must be absolute http(s)", ErrInvalid)
	}
	routeByPath := make(map[string]bool, len(routes))
	for _, r := range routes {
		if !strings.HasPrefix(r.Path, "/") {
			return nil, fmt.Errorf("%w: route path %q must start with /", ErrInvalid, r.Path)
		}
		routeByPath[r.Path] = true
	}
	goalIDs := make([]string, 0, len(goals))
	for _, g := range goals {
		if strings.TrimSpace(g.ID) == "" {
			return nil, fmt.Errorf("%w: goal id required", ErrInvalid)
		}
		for _, p := range g.Routes {
			if !routeByPath[p] {
				return nil, fmt.Errorf("%w: goal %s references unknown route %q", ErrInvalid, g.ID, p)
			}
		}
		goalIDs = append(goalIDs, g.ID)
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	enforcer := cfg.Enforcer
	if enforcer == nil {
		enforcer = NewScopeEnforcer(nil)
	}
	enforcer.RegisterProject(env.ID, target.ProjectID)
	maxLoop := cfg.MaxLoopDenials
	if maxLoop <= 0 {
		maxLoop = 3
	}
	maxSkip := cfg.MaxScopeSkips
	if maxSkip <= 0 {
		maxSkip = 5
	}

	p := &ExplorePlanner{
		Target:         target,
		Env:            env,
		Persona:        persona,
		Routes:         routes,
		Goals:          goals,
		Explorer:       cfg.Explorer,
		AI:             cfg.AI,
		Observer:       cfg.Observer,
		Enforcer:       enforcer,
		MaxLoopDenials: maxLoop,
		MaxScopeSkips:  maxSkip,
		now:            now,
	}
	p.plan = p.buildPlan(goalIDs)
	if p.Explorer == nil {
		p.Explorer = NewScriptedExplorer(p.planActions())
	}
	return p, nil
}

// buildPlan assembles the ordered candidate navigations: the seed landing
// navigation, then each goal's persona-permitted routes in registration
// order (dedup by path, first attribution wins).
func (p *ExplorePlanner) buildPlan(goalIDs []string) *ExplorationPlan {
	hints := map[string]bool{}
	for _, h := range p.Persona.RoleHints {
		hints[h] = true
	}
	routeFor := func(path string) (ExplorationRoute, bool) {
		for _, r := range p.Routes {
			if r.Path == path {
				return r, true
			}
		}
		return ExplorationRoute{}, false
	}
	allowed := func(r ExplorationRoute) bool {
		if len(r.Roles) == 0 {
			return true
		}
		for _, role := range r.Roles {
			if hints[role] {
				return true
			}
		}
		return false
	}

	steps := []ExplorationPlanStep{{Index: 0, Path: "/", GoalID: "seed"}}
	seen := map[string]bool{"/": true}
	ix := 1
	for _, g := range p.Goals {
		for _, path := range g.Routes {
			if seen[path] {
				continue
			}
			r, ok := routeFor(path)
			if !ok || !allowed(r) {
				continue
			}
			steps = append(steps, ExplorationPlanStep{Index: ix, Path: path, GoalID: g.ID})
			seen[path] = true
			ix++
		}
	}
	id := "plan-" + planHash(p.Target.OrgID, p.Target.ProjectID, p.Env.ID, p.Persona.ID, p.Target.BaseURL, fmt.Sprint(len(steps)))
	return &ExplorationPlan{
		ID:            id,
		OrgID:         p.Target.OrgID,
		ProjectID:     p.Target.ProjectID,
		EnvironmentID: p.Env.ID,
		PersonaID:     p.Persona.ID,
		TargetBaseURL: p.Target.BaseURL,
		GoalIDs:       goalIDs,
		Steps:         steps,
		CreatedAt:     p.now().UTC(),
	}
}

// Plan returns the deterministic exploration plan.
func (p *ExplorePlanner) Plan() ExplorationPlan { return *p.plan }

// planActions converts plan steps into the scripted navigation actions.
func (p *ExplorePlanner) planActions() []Action {
	acts := make([]Action, 0, len(p.plan.Steps))
	for _, s := range p.plan.Steps {
		acts = append(acts, Action{Kind: "navigate", URL: p.Target.BaseURL + s.Path})
	}
	return acts
}

// goalForPath returns the first plan goal attributed to a path ("ungrouped"
// when the path is not part of the plan — e.g. AI-discovered pages).
func (p *ExplorePlanner) goalForPath(path string) string {
	for _, s := range p.plan.Steps {
		if s.Path == path {
			return s.GoalID
		}
	}
	return "ungrouped"
}

// nextProposal consults the scripted explorer first, then the optional AI
// explorer once the script is exhausted. Errors pass through unchanged.
func (p *ExplorePlanner) nextProposal(ctx context.Context, state ExplorationState) (Action, error) {
	a, err := p.Explorer.ProposeNext(ctx, state)
	if errors.Is(err, ErrExplorerExhausted) && p.AI != nil {
		return p.AI.ProposeNext(ctx, state)
	}
	return a, err
}

var exploreRunSeq atomic.Uint64

// Run executes one safety-bounded exploration walk. Every proposal passes
// RiskyWriteReason heuristics, loop detection (visited-state hashes) and
// ScopeEnforcer.CheckScope before it is applied. Hard denials (kill switch,
// cancellation, budgets) stop the run; soft denials (domain, rate, window,
// write policy, unparseable URLs) are recorded as uncertainty and skipped up
// to MaxScopeSkips times. Candidates and the finalized ledger are returned
// for every stopping reason (candidates always remain drafts).
func (p *ExplorePlanner) Run(ctx context.Context) (ExplorationResult, error) {
	plan := *p.plan
	start := p.now().UTC()
	runID := fmt.Sprintf("explore-%d-%d", start.UnixNano(), exploreRunSeq.Add(1))
	ctx = WithScopeRun(ctx, runID)
	p.Enforcer.BeginRun(runID)

	res := ExplorationResult{
		PlanID:       plan.ID,
		RunID:        runID,
		StartedAt:    start,
		ExplorerUsed: "scripted",
	}
	if p.AI != nil {
		res.ExplorerUsed = p.AI.Model()
	}
	res.Ledger.PlannedRoutes = make([]string, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		res.Ledger.PlannedRoutes = append(res.Ledger.PlannedRoutes, s.Path)
	}

	state := ExplorationState{
		RunID:         runID,
		EnvironmentID: p.Env.ID,
		TargetID:      p.Target.ID,
		CurrentURL:    p.Target.BaseURL,
	}
	visited := map[string]bool{}
	tested := map[string]bool{}
	discovered := map[string]bool{}
	loopDenials, scopeSkips := 0, 0
	currentGoal := "ungrouped"

walk:
	for {
		if tripped, _ := p.Enforcer.kill.Tripped(p.Target.ProjectID); tripped {
			res.StoppedReason = "kill_switch"
			break walk
		}
		if err := ctx.Err(); err != nil {
			res.StoppedReason = "context_cancelled"
			break walk
		}

		a, err := p.nextProposal(ctx, state)
		if err != nil {
			if errors.Is(err, ErrExplorerExhausted) {
				res.StoppedReason = "explorer_exhausted"
			} else {
				res.StoppedReason = "explorer_error"
			}
			break walk
		}

		// Risky-write heuristics: stop + flag (never execute).
		if reason := RiskyWriteReason(a); reason != "" {
			res.Ledger.Flags = append(res.Ledger.Flags,
				SanitizeUntrusted(fmt.Sprintf("%s target=%q", reason, a.Target), 256))
			res.StoppedReason = "risky_write"
			break walk
		}

		// Resolve the absolute target URL for scope checking.
		raw := a.URL
		if a.Kind != "navigate" {
			raw = state.CurrentURL
		}
		targetURL, uerr := resolveActionURL(p.Target.BaseURL, raw)
		if uerr != nil {
			res.Ledger.Uncertainty = append(res.Ledger.Uncertainty,
				SanitizeUntrusted("unparseable_url "+raw, 256))
			res.Ledger.Cost.Denied++
			continue
		}

		// Loop/repeat detection on visited-state hashes.
		h := actionHash(Action{Kind: a.Kind, URL: targetURL, Target: a.Target, Value: a.Value})
		if visited[h] {
			loopDenials++
			res.Ledger.Cost.Denied++
			res.Ledger.Flags = append(res.Ledger.Flags,
				SanitizeUntrusted(fmt.Sprintf("loop_detected: repeat %s %s", a.Kind, targetURL), 256))
			if loopDenials > p.MaxLoopDenials {
				res.StoppedReason = "loop_detected"
				break walk
			}
			continue
		}

		dec := p.Enforcer.CheckScope(ctx, p.Env, targetURL, a)
		if !dec.Allowed {
			res.Ledger.Cost.Denied++
			res.Ledger.Uncertainty = append(res.Ledger.Uncertainty,
				SanitizeUntrusted(fmt.Sprintf("denied:%s %s", dec.Reason, targetURL), 256))
			switch dec.Reason {
			case "kill_switch":
				res.StoppedReason = "kill_switch"
				break walk
			case "context_cancelled":
				res.StoppedReason = "context_cancelled"
				break walk
			case "step_budget", "time_budget":
				res.StoppedReason = dec.Reason
				break walk
			default:
				scopeSkips++
				if scopeSkips > p.MaxScopeSkips {
					res.StoppedReason = "scope_denials"
					break walk
				}
				continue
			}
		}

		// Apply the action.
		visited[h] = true
		step := ExecutedStep{
			Index:    len(res.Steps),
			Action:   a,
			URL:      targetURL,
			Decision: dec,
			GoalID:   currentGoal,
			At:       p.now().UTC(),
		}
		if a.Kind == "navigate" {
			step.GoalID = p.goalForPath(pathOf(targetURL))
			currentGoal = step.GoalID
			discovered[pathOf(targetURL)] = true
			state.CurrentURL = targetURL
		}
		tested[pathOf(targetURL)] = true
		res.Steps = append(res.Steps, step)
		res.Ledger.VisitedStates = len(visited)

		// Observation seam: page-derived data only, sanitized, capped.
		if p.Observer != nil {
			for _, o := range p.Observer.Observe(ctx, step) {
				state.Observations = append(state.Observations, Observation{
					URL:     o.URL,
					Title:   SanitizeUntrusted(o.Title, 256),
					Links:   o.Links, // raw hrefs remain data; navigation is re-checked
					Content: SanitizeUntrusted(o.Content, 512),
					At:      p.now().UTC(),
				})
			}
			if len(state.Observations) > 8 {
				state.Observations = state.Observations[len(state.Observations)-8:]
			}
		}

		state.StepIndex = len(res.Steps)
		state.Elapsed = p.now().UTC().Sub(start)
		state.VisitedHashes = append(state.VisitedHashes, h)
	}

	res.FinishedAt = p.now().UTC()
	res.Ledger.Cost.Steps = len(res.Steps)
	res.Ledger.Cost.Duration = res.FinishedAt.Sub(res.StartedAt)
	for path := range discovered {
		res.Ledger.DiscoveredRoutes = append(res.Ledger.DiscoveredRoutes, path)
	}
	for path := range tested {
		res.Ledger.TestedRoutes = append(res.Ledger.TestedRoutes, path)
	}
	// Deterministic ordering (maps are unordered).
	sortStrings(res.Ledger.DiscoveredRoutes)
	sortStrings(res.Ledger.TestedRoutes)
	for _, s := range plan.Steps {
		if !tested[s.Path] {
			res.Ledger.Gaps = append(res.Ledger.Gaps, s.Path)
		}
	}
	res.Candidates = p.candidates(plan, res)
	return res, nil
}

// pathOf extracts the (possibly empty) path of an absolute URL.
func pathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

// sortStrings sorts in place (small slices; deterministic output).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// candidates converts the executed walk into draft Journey specs grouped by
// goal attribution. Groups keep plan goal order (extras last); every journey
// is Status draft, Source "exploration", Version 1, with deterministic
// assertions (final URL pattern + first element) and sanitized steps.
func (p *ExplorePlanner) candidates(plan ExplorationPlan, res ExplorationResult) []CandidateJourney {
	type group struct {
		goalID  string
		desc    string
		steps   []JourneyStep
		lastNav string
		clicked string
		write   bool
	}
	goalDesc := map[string]string{}
	for _, g := range p.Goals {
		goalDesc[g.ID] = g.Description
	}
	order := append([]string(nil), plan.GoalIDs...)
	order = append(order, "seed", "ungrouped") // seed landing + extras last
	groups := map[string]*group{}

	for _, es := range res.Steps {
		g := groups[es.GoalID]
		if g == nil {
			g = &group{goalID: es.GoalID, desc: goalDesc[es.GoalID]}
			groups[es.GoalID] = g
		}
		js := JourneyStep{
			Index:     len(g.steps),
			Action:    es.Action.Kind,
			Target:    SanitizeUntrusted(firstNonEmpty(es.Action.URL, es.Action.Target), 512),
			Value:     SanitizeUntrusted(es.Action.Value, 512),
			WriteOp:   es.Action.WriteOp,
			TimeoutMS: es.Action.TimeoutMS,
		}
		if es.Action.Kind == "navigate" {
			g.lastNav = pathOf(es.URL)
		}
		if es.Action.Kind == "click" && g.clicked == "" {
			g.clicked = js.Target
		}
		if es.Action.WriteOp {
			g.write = true
		}
		g.steps = append(g.steps, js)
	}

	out := make([]CandidateJourney, 0, len(groups))
	now := p.now().UTC()
	for _, id := range order {
		g, ok := groups[id]
		if !ok {
			continue
		}
		name := "exploration: " + SanitizeUntrusted(firstNonEmpty(g.desc, id), 120)
		assertions := make([]Assertion, 0, 2)
		if g.lastNav != "" {
			assertions = append(assertions, Assertion{
				Kind: AssertURLPattern, Target: g.lastNav, Expected: "prefix", Severity: RiskLow,
			})
		}
		if g.clicked != "" {
			assertions = append(assertions, Assertion{
				Kind: AssertElementVisible, Target: g.clicked, Expected: "visible", Severity: RiskLow,
			})
		}
		risk := RiskLow
		if g.write {
			risk = RiskMedium
		}
		j := Journey{
			ID:            fmt.Sprintf("j-%s-%s", plan.ID, id),
			OrgID:         plan.OrgID,
			ProjectID:     plan.ProjectID,
			EnvironmentID: plan.EnvironmentID,
			PersonaID:     plan.PersonaID,
			Name:          name,
			Status:        JourneyDraft,
			Risk:          risk,
			Version:       1,
			Preconditions: []string{
				"target verified: " + p.Target.ID,
				"environment mode: " + string(p.Env.Mode),
				"persona: " + p.Persona.ID,
			},
			Steps:      g.steps,
			Assertions: assertions,
			Tags:       []string{"exploration", "goal:" + id},
			Source:     "exploration",
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		out = append(out, CandidateJourney{Journey: j, GoalID: id, CoverageRef: plan.ID})
	}
	return out
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
