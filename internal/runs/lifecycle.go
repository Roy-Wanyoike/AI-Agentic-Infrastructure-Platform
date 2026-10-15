package runs

// lifecycle.go — issue #115: the complete run lifecycle state machine.
//
//      pending → queued → running → paused
//                            ↓         ↑ (resume re-enqueues: paused → queued)
//                          cancelling → cancelled
//      terminal: completed | failed | cancelled | timeout
//
// The machine is the enforced contract for every control-plane transition
// (pause/resume/cancel and the worker's terminal writes). It follows the
// internal/agents/lifecycle.go pattern (issue #106):
//
//   - one authoritative transition table, pinned by TestRunTransitionsTable,
//     so a typo cannot silently widen (or close) a legal move;
//   - case-insensitive parsing of wire/storage values — the legacy UPPERCASE
//     worker statuses ("QUEUED"/"RUNNING"/...) written by CreateRunCtx and
//     the lowercase control-plane contract ("pending"/"paused"/...) written
//     by the wave-2 control ops normalize onto the same states;
//   - a typed RunTransitionError carrying BOTH endpoints (From/To), always
//     wrapping the ErrInvalidTransition sentinel so errors.Is classification
//     keeps working for every existing caller;
//   - illegal moves rejected at the service level (table check on the parsed
//     current state) AND at the store level (optimistic WHERE status =
//     expected guard, StatusGuardStore) so two racing transitions cannot
//     corrupt the status column.
//
// Terminal states never transition (a completed/failed/cancelled/timed-out
// run is immutable). Self-transitions (current == target) are idempotent
// no-ops handled by TransitionRunStatus before the table check — they are
// deliberately NOT part of the table.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"agentos/internal/events"
)

// RunState is one node of the run lifecycle state machine (issue #115). The
// canonical persisted values are lowercase; parsing is case-insensitive and
// whitespace tolerant so legacy UPPERCASE rows and API payloads normalize
// onto the same states. The zero value is not a valid state — use
// ParseRunState (API/storage boundary) or the State* constants (Go callers).
type RunState string

// The lifecycle states. pending → queued → running is the execution path;
// paused parks a run (worker halt at the step boundary, resumable);
// cancelling is the graceful-unwind phase of a cancel (current step
// completes, partial results are recorded — issue #107 shutdown.go); the
// remaining four are terminal.
const (
	StatePending         RunState = "pending"
	StateQueued          RunState = "queued"
	StateRunning         RunState = "running"
	StatePaused          RunState = "paused"
	StateWaitingApproval RunState = "waiting_approval"
	StateCancelling      RunState = "cancelling"
	StateCompleted       RunState = "completed"
	StateFailed          RunState = "failed"
	StateCancelled       RunState = "cancelled"
	StateTimeout         RunState = "timeout"
)

// runStateTransitions is the authoritative transition table (issue #115):
//
//	pending          → {queued, running, paused, cancelling, cancelled}
//	queued           → {running, paused, cancelling, cancelled}
//	running          → {paused, cancelling, completed, failed}
//	paused           → {queued, pending, cancelling, cancelled, completed, failed}
//	waiting_approval → {queued, pending, paused, cancelling, cancelled}
//	cancelling       → {cancelled, failed}
//	completed | failed | cancelled | timeout → {} (terminal)
//
// Notes:
//   - cancel of an in-flight (running) run goes through cancelling first:
//     the runtime lets the current step finish and records the partial
//     results (issue #107) before the worker confirms cancelling → cancelled;
//   - paused → completed/failed keeps the pause race honest: a run whose
//     in-flight execution finished before the pause signal landed reports its
//     real outcome instead of being stuck in paused forever;
//   - paused has NO running transition: a paused run is never re-claimed by
//     the worker (the enforced pre-flight check refuses to start one) — only
//     an explicit resume (paused → queued via ResumeRunQueued) re-enters
//     execution;
//   - waiting_approval is only ever left through an operator decision (the
//     approvals flow resumes to pending) or a control op; the resume endpoint
//     must NOT bypass the approval gate (ResumeRunQueued accepts paused only).
//
// Every state must appear exactly once as a key; TestRunTransitionsTable
// pins the table against the issue contract.
var runStateTransitions = map[RunState][]RunState{
	StatePending:         {StateQueued, StateRunning, StatePaused, StateCancelling, StateCancelled},
	StateQueued:          {StateRunning, StatePaused, StateCancelling, StateCancelled},
	StateRunning:         {StatePaused, StateCancelling, StateCompleted, StateFailed},
	StatePaused:          {StateQueued, StatePending, StateCancelling, StateCancelled, StateCompleted, StateFailed},
	StateWaitingApproval: {StateQueued, StatePending, StatePaused, StateCancelling, StateCancelled},
	StateCancelling:      {StateCancelled, StateFailed},
	StateCompleted:       {},
	StateFailed:          {},
	StateCancelled:       {},
	StateTimeout:         {},
}

// AllRunStates returns every lifecycle state in table order (pending,
// queued, running, paused, waiting_approval, cancelling, completed, failed,
// cancelled, timeout). A defensive copy: callers cannot mutate the
// transition table through the returned slice.
func AllRunStates() []RunState {
	return []RunState{
		StatePending, StateQueued, StateRunning, StatePaused, StateWaitingApproval,
		StateCancelling, StateCompleted, StateFailed, StateCancelled, StateTimeout,
	}
}

// ParseRunState normalizes a wire/storage value onto a RunState. Matching is
// case-insensitive and trims surrounding whitespace, so the historical
// UPPERCASE worker rows ("QUEUED", "RUNNING", "COMPLETED", "FAILED"), the
// lowercase control-plane contract ("pending", "paused", ...) and future
// drift all land on the canonical state. The boolean is false for blank or
// unknown values — callers decide fail-open vs fail-closed (the lifecycle
// methods fail closed).
func ParseRunState(raw string) (RunState, bool) {
	normalized := RunState(strings.ToLower(strings.TrimSpace(raw)))
	switch normalized {
	case StatePending, StateQueued, StateRunning, StatePaused, StateWaitingApproval,
		StateCancelling, StateCompleted, StateFailed, StateCancelled, StateTimeout:
		return normalized, true
	default:
		return "", false
	}
}

// CanTransitionRunState reports whether from → to is a legal move of the
// state machine. Unknown states (including "") can never transition.
func CanTransitionRunState(from, to RunState) bool {
	targets, ok := runStateTransitions[from]
	if !ok {
		return false
	}
	for _, target := range targets {
		if target == to {
			return true
		}
	}
	return false
}

// AllowedRunTransitions returns the legal target states from one state, in
// the deterministic table order. Unknown states yield an empty (non-nil)
// slice. A defensive copy: mutating the result cannot corrupt the table.
func AllowedRunTransitions(from RunState) []RunState {
	return append([]RunState(nil), runStateTransitions[from]...)
}

// IsTerminalRunState reports whether the state is final: a terminal run can
// no longer be paused, resumed or cancelled.
func IsTerminalRunState(s RunState) bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateTimeout:
		return true
	}
	return false
}

// timeNowUTC is the clock seam for the lifecycle writes (single formatting
// point: always UTC). A package var so tests can inject a deterministic
// clock (the recovery-sweep staleness math; no flaky sleeps).
var timeNowUTC = func() time.Time { return time.Now().UTC() }

// ErrUnknownRunState is returned when a transition targets (or reads) a
// value that is not one of the lifecycle states (blank payloads, typos,
// hand-edited rows, future drift). Fail-closed by contract.
var ErrUnknownRunState = errors.New("unknown run state")

// ErrStatusConflict is returned when the optimistic store guard rejected a
// transition because the run's status changed concurrently (another control
// op or a worker terminal write won the race). Callers treat it as "the
// other writer won" — never as a corruption.
var ErrStatusConflict = errors.New("run status changed concurrently")

// ErrRecoveryUnsupported is returned by RecoverStaleRuns when the configured
// Store cannot serve the stale-run scan (the optional StaleRunScanner
// contract is not implemented).
var ErrRecoveryUnsupported = errors.New("runs: store does not support stale-run recovery")

// RunTransitionError is the typed rejection for an illegal from → to move.
// It carries BOTH endpoints so API handlers can render precise messages
// ("cannot transition from RUNNING to COMPLETED") and tests can pin the
// offending pair. It always wraps the ErrInvalidTransition sentinel.
type RunTransitionError struct {
	// From is the canonical state the run was actually in when the move was
	// attempted (parsed case-insensitively).
	From RunState
	// To is the requested state (canonical form).
	To RunState
}

// Error implements error with the from → to pair and the allowed targets.
func (e *RunTransitionError) Error() string {
	allowed := AllowedRunTransitions(e.From)
	names := make([]string, 0, len(allowed))
	for _, s := range allowed {
		names = append(names, string(s))
	}
	if len(names) == 0 {
		return fmt.Sprintf("invalid run status transition: %s → %s (allowed: none — %s is terminal)", e.From, e.To, e.From)
	}
	return fmt.Sprintf("invalid run status transition: %s → %s (allowed: %s)", e.From, e.To, strings.Join(names, ", "))
}

// Unwrap exposes the ErrInvalidTransition sentinel for errors.Is.
func (e *RunTransitionError) Unwrap() error { return ErrInvalidTransition }

// ---------------------------------------------------------------------------
// Service-level enforcement
// ---------------------------------------------------------------------------

// validateRunTransition parses the run's current status and checks the move
// against the table. Shared by every enforced transition entry point.
// Fail-closed: an unparseable current status (hand-edited row, future drift)
// is never silently migrated.
func validateRunTransition(currentStatus string, target RunState) (RunState, error) {
	if _, ok := ParseRunState(string(target)); !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownRunState, string(target))
	}
	current, ok := ParseRunState(currentStatus)
	if !ok {
		return "", fmt.Errorf("run status %q is not a lifecycle state; reconcile it before transitioning", currentStatus)
	}
	// Self-transitions are idempotent no-ops BEFORE the table check (the
	// doc contract): replayed control ops and re-confirmed cancels must
	// succeed even when the current state is terminal.
	if current == target {
		return current, nil
	}
	if !CanTransitionRunState(current, target) {
		return current, &RunTransitionError{From: current, To: target}
	}
	return current, nil
}

// TransitionRunStatus moves one run to the target lifecycle state through
// the ENFORCED path (issue #115):
//
//   - Validation: the current status is parsed (fail-closed on unknown
//     values) and the move checked against runStateTransitions BEFORE any
//     write; a rejected move leaves the row untouched and returns a
//     *RunTransitionError (wrapping ErrInvalidTransition). A self-transition
//     (current == target) is an idempotent no-op success.
//   - Tenant guard: when orgID is empty (trusted internal worker path) it is
//     resolved from the run row first, so the write always carries the
//     organization_id scope.
//   - Store guard: the persisted write uses the optimistic WHERE status =
//     expected update (StatusGuardStore) so a racing transition loses with
//     ErrStatusConflict instead of corrupting the status; stores without the
//     guard fall back to the table-checked legacy update.
//   - Streaming + events: the status.changed stream event and the terminal
//     metrics counting fire exactly like UpdateStatusCtx; the control-plane
//     events (run.paused / run.resumed / run.cancelled) are emitted for the
//     corresponding targets, EXCEPT for the cancelling → cancelled
//     confirmation (the runtime already published run.cancelled with the
//     execution details when the graceful unwind landed).
func (s *Service) TransitionRunStatus(ctx context.Context, orgID, id string, target RunState, output string) error {
	if strings.TrimSpace(id) == "" {
		return ErrRunNotFound
	}
	if _, ok := ParseRunState(string(target)); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownRunState, string(target))
	}
	// Tenant resolution (trusted internal worker path): the update always
	// executes with the organization_id guard.
	run, err := s.runForTransition(ctx, orgID, id)
	if err != nil {
		return err
	}
	if orgID == "" {
		orgID = run.OrganizationID
	}
	current, err := validateRunTransition(string(run.Status), target)
	if err != nil {
		return err
	}
	if current == target {
		return nil // idempotent no-op
	}
	if err := s.persistTransition(ctx, orgID, id, target, output); err != nil {
		return err
	}
	s.mirrorTransition(id, target, output)
	// Observability parity with UpdateStatusCtx: terminal counting + the
	// status.changed stream event.
	s.countTerminalRun(id, RunStatus(target))
	s.publishStatus(id, RunStatus(target), output)
	s.emitControlEvent(ctx, orgID, id, current, target)
	return nil
}

// runForTransition resolves the run row for a transition (tenant-scoped when
// orgID is set; trusted PK lookup otherwise).
func (s *Service) runForTransition(ctx context.Context, orgID, id string) (*Run, error) {
	if strings.TrimSpace(orgID) != "" {
		return s.GetRunCtx(ctx, orgID, id)
	}
	return s.GetRunByIDCtx(ctx, id)
}

// persistTransition writes the validated transition. Postgres mode uses the
// optimistic expected-status guard (store-level rejection of racing
// transitions: the WHERE clause re-checks the exact status the validation
// read, so a concurrent writer wins the race and this caller sees
// ErrStatusConflict). In-memory mode the whole re-check-write sequence runs
// under the service mutex, which IS the guard.
func (s *Service) persistTransition(ctx context.Context, orgID, id string, target RunState, output string) error {
	if s.store != nil {
		if guarded, ok := s.store.(StatusGuardStore); ok {
			// The expected value is the run's status AS READ during
			// validation; re-read it here so the guard pins the row this
			// caller actually validated (see runForTransition above — the
			// parse happened on the same read).
			expected := s.expectedStatusForGuard(ctx, orgID, id)
			updated, err := guarded.UpdateRunStatusGuarded(ctx, orgID, id, expected, RunStatus(target), output)
			if err != nil {
				return err
			}
			if !updated {
				return ErrStatusConflict
			}
			return nil
		}
		// Foreign Store without the guard: the service-level table check
		// above still enforced legality; persist through the legacy path.
		return s.store.UpdateRunStatus(ctx, orgID, id, RunStatus(target), output)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	if !ok || (orgID != "" && run.OrganizationID != orgID) {
		return ErrRunNotFound
	}
	current, ok := ParseRunState(string(run.Status))
	if !ok {
		return fmt.Errorf("run status %q is not a lifecycle state; reconcile it before transitioning", run.Status)
	}
	if current == target {
		return nil // racing writer already landed the same state
	}
	if !CanTransitionRunState(current, target) {
		return &RunTransitionError{From: current, To: target}
	}
	run.Status = RunStatus(target)
	if output != "" {
		run.Output = output
	}
	run.UpdatedAt = timeNowUTC()
	return nil
}

// expectedStatusForGuard re-reads the raw stored status for the optimistic
// guard's WHERE clause (the guard must match the STORED spelling, legacy
// uppercase or contract lowercase alike). A failed re-read returns "" which
// matches no row — the transition then surfaces as ErrStatusConflict, the
// conservative outcome.
func (s *Service) expectedStatusForGuard(ctx context.Context, orgID, id string) RunStatus {
	run, err := s.runForTransition(ctx, orgID, id)
	if err != nil {
		return ""
	}
	return run.Status
}

// mirrorTransition refreshes the write-through in-memory cache after a
// store-accepted transition (store mode); memory mode already wrote through
// persistTransition.
func (s *Service) mirrorTransition(id string, target RunState, output string) {
	if s.store == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run, ok := s.runs[id]; ok {
		run.Status = RunStatus(target)
		if output != "" {
			run.Output = output
		}
		run.UpdatedAt = timeNowUTC()
	}
}

// emitControlEvent publishes the issue #115 control-plane event for a landed
// transition (best-effort: no publisher, no tenant or a publish failure
// never affects the transition). The cancelling → cancelled confirmation is
// deliberately silent: the runtime's graceful unwind already published
// run.cancelled with the execution payload (steps, reason).
func (s *Service) emitControlEvent(ctx context.Context, orgID, runID string, from, to RunState) {
	var eventType string
	switch to {
	case StatePaused:
		eventType = events.EventRunPaused
	case StateQueued:
		eventType = events.EventRunResumed
	case StateCancelled:
		if from != StateCancelling {
			eventType = events.EventRunCancelled
		}
	}
	if eventType == "" {
		return
	}
	s.publishRunEvent(ctx, orgID, runID, eventType, map[string]any{
		"run_id": runID,
		"from":   string(from),
		"to":     string(to),
	})
}

// publishRunEvent emits one platform event through the optional event
// publisher seam (SetEventPublisher). Best-effort by contract.
func (s *Service) publishRunEvent(ctx context.Context, orgID, runID, eventType string, payload map[string]any) {
	if s == nil || s.eventPub == nil || strings.TrimSpace(orgID) == "" {
		return
	}
	pubCtx := ctx
	if pubCtx == nil {
		pubCtx = context.Background()
	}
	// A cancelled caller context must not swallow the event (the recovery
	// sweep and cancel unwinds both fire on dying contexts).
	_ = s.eventPub.Publish(context.WithoutCancel(pubCtx), events.NewEvent(eventType, orgID, "run", runID, payload))
}

// ---------------------------------------------------------------------------
// RunControlRegistry — the in-flight control seam (issue #115)
// ---------------------------------------------------------------------------

// RunController is the in-flight execution handle a run executor (the
// worker) registers in the RunControlRegistry while a run executes. The API
// control ops look the controller up and signal it so the in-flight run
// stops at its current step boundary; when the executor runs in another
// process (or already exited) the lookup misses and the control op
// transitions the durable status only — the executor observes the new status
// through its own seam (the worker's control poller / outcome handler).
type RunController interface {
	// Pause asks the in-flight run to stop gracefully at the current step
	// boundary: partial results are preserved (issue #107 machinery) and the
	// run lands in paused. Idempotent.
	Pause(ctx context.Context) error
	// Cancel asks the in-flight run to unwind gracefully: the current step
	// completes, partial results are recorded and the run lands in
	// cancelled (via cancelling). Idempotent.
	Cancel(ctx context.Context) error
}

// RunControlRegistry maps in-flight run ids to their executors' controllers.
// The registry is per-process BY DESIGN: it carries the live handles of the
// executors running inside THIS process (worker tasks, embedded runners,
// tests). Cross-process control rides the durable status transitions plus
// the executor-side observation seam (the worker's control poller).
type RunControlRegistry struct {
	mu          sync.Mutex
	controllers map[string]RunController
}

// NewRunControlRegistry returns an empty registry.
func NewRunControlRegistry() *RunControlRegistry {
	return &RunControlRegistry{controllers: make(map[string]RunController)}
}

// Register attaches the controller for one in-flight run and returns the
// unregister func (safe to defer). Registering over an existing entry
// replaces it (a run id executes at most once per process). Nil-safe: a nil
// receiver/controller yields a no-op unregister.
func (r *RunControlRegistry) Register(runID string, c RunController) (unregister func()) {
	if r == nil || c == nil || strings.TrimSpace(runID) == "" {
		return func() {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.controllers == nil {
		r.controllers = make(map[string]RunController)
	}
	r.controllers[runID] = c
	return func() { r.Unregister(runID) }
}

// Lookup returns the registered controller for the run (in-flight here).
func (r *RunControlRegistry) Lookup(runID string) (RunController, bool) {
	if r == nil || strings.TrimSpace(runID) == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.controllers[runID]
	return c, ok
}

// Unregister removes the controller (executor finished or unwound).
func (r *RunControlRegistry) Unregister(runID string) {
	if r == nil || strings.TrimSpace(runID) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.controllers, runID)
}

// Controls returns the service's control registry (lazily created, safe for
// concurrent use). The worker registers its in-flight executors here; the
// API control ops consult it.
func (s *Service) Controls() *RunControlRegistry {
	if s == nil {
		return nil
	}
	s.controlOnce.Do(func() {
		s.controls = NewRunControlRegistry()
	})
	return s.controls
}

// signalInFlight signals the in-flight controller for the run, if one is
// registered in this process. Best-effort: a missing controller or a
// signalling failure never fails the (already persisted) transition.
func (s *Service) signalInFlight(ctx context.Context, runID string, action func(RunController, context.Context) error) {
	if s == nil {
		return
	}
	if c, ok := s.Controls().Lookup(runID); ok && c != nil {
		_ = action(c, ctx)
	}
}

// ---------------------------------------------------------------------------
// Optional seams (nil-safe DI, wired in cmd/api / cmd/worker)
// ---------------------------------------------------------------------------

// WorkflowCascade is the additive issue #115 cascade hook: implemented by the
// workflows service so pausing/cancelling a run that belongs to a workflow
// node cascades to the owning workflow run and its node executions. The runs
// service invokes it best-effort AFTER the durable transition landed; a nil
// seam or a cascade failure never fails the control op.
type WorkflowCascade interface {
	// CascadeRunPaused pauses the workflow run that owns the given agent run
	// and marks its active node executions paused (scheduling halt).
	CascadeRunPaused(ctx context.Context, orgID, runID string) error
	// CascadeRunCancelled cancels the workflow run that owns the given agent
	// run and marks its active node executions cancelled.
	CascadeRunCancelled(ctx context.Context, orgID, runID string) error
}

// SetWorkflowCascade wires (or replaces, via nil) the workflow cascade seam.
// Nil-safe; wire before control ops are served.
func (s *Service) SetWorkflowCascade(c WorkflowCascade) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cascade = c
}

// SetEventPublisher wires (or replaces, via nil) the platform event
// publisher used for the run.paused / run.resumed / run.cancelled /
// run.recovered emissions. Nil-safe.
func (s *Service) SetEventPublisher(pub events.Publisher) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventPub = pub
}

// cascadeRunPaused invokes the optional cascade seam (best-effort; the
// returned error is informational — the durable transition already landed).
func (s *Service) cascadeRunPaused(ctx context.Context, orgID, runID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	cascade := s.cascade
	s.mu.Unlock()
	if cascade == nil {
		return nil
	}
	return cascade.CascadeRunPaused(ctx, orgID, runID)
}

// cascadeRunCancelled invokes the optional cascade seam for cancels
// (best-effort, same contract as cascadeRunPaused).
func (s *Service) cascadeRunCancelled(ctx context.Context, orgID, runID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	cascade := s.cascade
	s.mu.Unlock()
	if cascade == nil {
		return nil
	}
	return cascade.CascadeRunCancelled(ctx, orgID, runID)
}
