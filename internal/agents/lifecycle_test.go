package agents

// Issue #106 tests: the lifecycle state machine.
//
// Coverage:
//   - the transition TABLE is pinned against the issue contract (every state,
//     every legal target, archived terminal);
//   - the full 6x6 transition matrix runs against a live service: legal moves
//     persist, every illegal move returns the typed *TransitionError exposing
//     current AND target (wrapping ErrInvalidTransition) and leaves the agent
//     untouched;
//   - store mode (sqlmock): the durable round-trip — org-scoped SELECT, then
//     exactly one guarded UPDATE binding the new status; a rejected
//     transition performs NO write;
//   - concurrency: two racing transitions from the same source state are
//     serialized by the service mutex — exactly one wins, the loser gets the
//     typed error, the final status is never corrupted;
//   - audit: agent.state_changed is emitted through the wired auditor seam
//     (real *audit.Service), with nil/no auditor fully functional.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/audit"
)

// wantTransitions is the issue #106 contract, transcribed verbatim so the
// table test fails loudly if agentTransitions drifts.
var wantTransitions = map[AgentState][]AgentState{
	StateDraft:    {StateBuilding, StateArchived},
	StateBuilding: {StateActive, StateDraft, StateDisabled},
	StateActive:   {StatePaused, StateDisabled, StateArchived},
	StatePaused:   {StateActive, StateDisabled, StateArchived},
	StateDisabled: {StateBuilding, StateDraft, StateArchived},
	StateArchived: {},
}

func TestAgentTransitionsTablePinsIssueContract(t *testing.T) {
	states := AllAgentStates()
	if len(states) != 6 {
		t.Fatalf("expected 6 lifecycle states, got %d: %v", len(states), states)
	}
	for _, state := range states {
		want, ok := wantTransitions[state]
		if !ok {
			t.Fatalf("state %q missing from contract table", state)
		}
		got := AllowedTransitions(state)
		if len(got) != len(want) {
			t.Fatalf("state %q: allowed = %v, want %v", state, got, want)
		}
		for i, target := range want {
			if got[i] != target {
				t.Fatalf("state %q: allowed[%d] = %q, want %q (full: %v vs %v)", state, i, got[i], target, got, want)
			}
		}
		// Every legal move must be recognized and every non-listed move
		// (including self and all other states) must be rejected.
		for _, target := range states {
			legal := false
			for _, wantTarget := range want {
				if target == wantTarget {
					legal = true
				}
			}
			if CanTransition(state, target) != legal {
				t.Fatalf("CanTransition(%s, %s) = %v, want %v", state, target, !legal, legal)
			}
		}
	}
	// archived is terminal: nothing (not even itself) leaves it.
	if got := AllowedTransitions(StateArchived); len(got) != 0 {
		t.Fatalf("archived must be terminal, got allowed targets %v", got)
	}
}

func TestAllowedTransitionsDefensiveCopy(t *testing.T) {
	got := AllowedTransitions(StateDraft)
	got[0] = StateArchived
	if again := AllowedTransitions(StateDraft); again[0] == StateArchived {
		t.Fatal("AllowedTransitions must return a copy; caller mutation leaked into the table")
	}
}

func TestParseAgentState(t *testing.T) {
	cases := []struct {
		name, raw string
		want      AgentState
		wantOK    bool
	}{
		{"canonical uppercase", "DRAFT", StateDraft, true},
		{"lowercase payload", "active", StateActive, true},
		{"mixed case", "Paused", StatePaused, true},
		{"whitespace padded", "  building  ", StateBuilding, true},
		{"legacy row value", "DRAFT", StateDraft, true},
		{"archived", "ARCHIVED", StateArchived, true},
		{"disabled", "disabled", StateDisabled, true},
		{"unknown", "PUBLISHED", "", false},
		{"blank", "", "", false},
		{"blank spaces", "   ", "", false},
	}
	for _, tc := range cases {
		got, ok := ParseAgentState(tc.raw)
		if ok != tc.wantOK || got != tc.want {
			t.Fatalf("%s: ParseAgentState(%q) = (%q, %v), want (%q, %v)", tc.name, tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

// lifecycleMatrixService seeds one DRAFT agent per org state value so every
// matrix case starts from a controlled status.
func lifecycleMatrixService(t *testing.T, orgID, seedStatus string) (*Service, string) {
	t.Helper()
	svc := NewService()
	agent, err := svc.CreateAgentCtx(t.Context(), orgID, "Matrix Agent", "d", "be helpful", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("seed agent failed: %v", err)
	}
	if seedStatus != string(StateDraft) {
		agent.Status = seedStatus
		if err := svc.UpdateAgentCtx(t.Context(), orgID, agent); err != nil {
			t.Fatalf("seed status failed: %v", err)
		}
	}
	return svc, agent.ID
}

func TestTransitionMatrixEveryStatePair(t *testing.T) {
	for _, from := range AllAgentStates() {
		for _, to := range AllAgentStates() {
			legal := CanTransition(from, to)
			t.Run(fmt.Sprintf("%s_to_%s", from, to), func(t *testing.T) {
				orgID := "org-matrix"
				svc, agentID := lifecycleMatrixService(t, orgID, string(from))
				err := svc.TransitionState(t.Context(), orgID, agentID, to)
				if legal {
					if err != nil {
						t.Fatalf("legal transition %s → %s rejected: %v", from, to, err)
					}
					agent, err := svc.GetAgentCtx(t.Context(), orgID, agentID)
					if err != nil {
						t.Fatalf("reload agent failed: %v", err)
					}
					if agent.Status != string(to) {
						t.Fatalf("legal transition %s → %s not persisted: status = %q", from, to, agent.Status)
					}
					return
				}
				// Illegal move: typed error with both endpoints.
				var tErr *TransitionError
				if !errors.As(err, &tErr) {
					t.Fatalf("illegal transition %s → %s: expected *TransitionError, got %v", from, to, err)
				}
				if tErr.Current != from || tErr.Target != to {
					t.Fatalf("typed error endpoints = (%s → %s), want (%s → %s)", tErr.Current, tErr.Target, from, to)
				}
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("TransitionError must wrap ErrInvalidTransition, got %v", err)
				}
				// The agent stays exactly where it was.
				agent, aggErr := svc.GetAgentCtx(t.Context(), orgID, agentID)
				if aggErr != nil {
					t.Fatalf("reload agent failed: %v", aggErr)
				}
				if agent.Status != string(from) {
					t.Fatalf("rejected transition mutated status: %q, want %q", agent.Status, from)
				}
			})
		}
	}
}

func TestTransitionVariantReturnsUpdatedAgent(t *testing.T) {
	orgID := "org-variant"
	svc, agentID := lifecycleMatrixService(t, orgID, string(StateDraft))
	updated, err := svc.TransitionStateWithAgent(t.Context(), orgID, agentID, StateBuilding)
	if err != nil {
		t.Fatalf("transition failed: %v", err)
	}
	if updated.Status != string(StateBuilding) {
		t.Fatalf("variant returned status %q, want BUILDING", updated.Status)
	}
	if updated.ID != agentID || updated.OrganizationID != orgID {
		t.Fatalf("variant returned wrong agent identity: %+v", updated)
	}
}

func TestTransitionTenantGuard(t *testing.T) {
	orgID := "org-owner"
	svc, agentID := lifecycleMatrixService(t, orgID, string(StateDraft))

	cases := []struct {
		name, orgID, agentID string
	}{
		{"cross-org read", "org-other", agentID},
		{"unknown agent", orgID, "agent-does-not-exist"},
		{"blank agent id", orgID, ""},
		{"blank org id", "", agentID},
	}
	for _, tc := range cases {
		if err := svc.TransitionState(t.Context(), tc.orgID, tc.agentID, StateBuilding); !errors.Is(err, ErrAgentNotFound) {
			t.Fatalf("%s: expected ErrAgentNotFound, got %v", tc.name, err)
		}
	}
}

func TestTransitionUnknownTargetState(t *testing.T) {
	orgID := "org-unknown"
	svc, agentID := lifecycleMatrixService(t, orgID, string(StateDraft))
	for _, target := range []AgentState{"", "PUBLISHED", "active-but-typo"} {
		err := svc.TransitionState(t.Context(), orgID, agentID, target)
		if !errors.Is(err, ErrUnknownAgentState) {
			t.Fatalf("target %q: expected ErrUnknownAgentState, got %v", target, err)
		}
	}
	// Unknown targets never touch the agent.
	agent, err := svc.GetAgentCtx(t.Context(), orgID, agentID)
	if err != nil || agent.Status != string(StateDraft) {
		t.Fatalf("unknown target mutated the agent: %+v err=%v", agent, err)
	}
}

func TestTransitionRejectsUnparseableStoredStatus(t *testing.T) {
	orgID := "org-legacy"
	svc, agentID := lifecycleMatrixService(t, orgID, "SOME_LEGACY_VALUE")
	err := svc.TransitionState(t.Context(), orgID, agentID, StateBuilding)
	if err == nil {
		t.Fatal("expected failure for a status outside the state machine")
	}
	if errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unparseable status is not a transition problem, got %v", err)
	}
	agent, err := svc.GetAgentCtx(t.Context(), orgID, agentID)
	if err != nil || agent.Status != "SOME_LEGACY_VALUE" {
		t.Fatalf("failed transition must not rewrite the status: %+v err=%v", agent, err)
	}
}

func TestTransitionAuditEventEmitted(t *testing.T) {
	orgID := "org-audit"
	svc, agentID := lifecycleMatrixService(t, orgID, string(StateDraft))
	auditSvc := audit.NewService()
	svc.SetAuditor(auditSvc)

	if err := svc.TransitionState(t.Context(), orgID, agentID, StateBuilding); err != nil {
		t.Fatalf("transition failed: %v", err)
	}
	entries, err := auditSvc.ListCtx(t.Context(), orgID)
	if err != nil {
		t.Fatalf("audit list failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 audit entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Action != "agent.state_changed" {
		t.Fatalf("audit action = %q, want agent.state_changed", entry.Action)
	}
	if entry.OrganizationID != orgID || entry.Resource != "agents/"+agentID {
		t.Fatalf("audit entry scope mismatch: %+v", entry)
	}
	if entry.Metadata["from"] != string(StateDraft) || entry.Metadata["to"] != string(StateBuilding) {
		t.Fatalf("audit metadata must carry from/to, got %v", entry.Metadata)
	}

	// A rejected transition emits nothing.
	if _, err := svc.TransitionStateWithAgent(t.Context(), orgID, agentID, StateBuilding); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("building → building must be rejected, got %v", err)
	}
	entries, err = auditSvc.ListCtx(t.Context(), orgID)
	if err != nil {
		t.Fatalf("audit list failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("rejected transitions must not audit, got %d entries", len(entries))
	}
}

func TestTransitionWithoutAuditorStillWorks(t *testing.T) {
	orgID := "org-noaudit"
	svc, agentID := lifecycleMatrixService(t, orgID, string(StateDraft))
	svc.SetAuditor(nil) // explicit nil must be a no-op, not a panic
	if err := svc.TransitionState(t.Context(), orgID, agentID, StateArchived); err != nil {
		t.Fatalf("transition without auditor failed: %v", err)
	}
}

// lifecycleSQLAgentRows lists the agent SELECT columns in scanAgent order.
func lifecycleSQLAgentRows() []string {
	return []string{"id", "organization_id", "name", "description", "instructions", "model", "status", "current_version_id", "created_at", "updated_at"}
}

func TestTransitionStoreRoundTrip(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	orgID := "org-store"
	now := time.Now().UTC()
	svc := NewServiceWithStore(NewPostgresStore(db))

	// Org-scoped SELECT behind TransitionStateWithAgent.
	mock.ExpectQuery("FROM agents WHERE id = \\$1 AND organization_id = \\$2").
		WithArgs("agent-1", orgID).
		WillReturnRows(sqlmock.NewRows(lifecycleSQLAgentRows()).
			AddRow("agent-1", orgID, "Support Agent", "d", "p", "gpt-4o-mini", "DRAFT", "v1", now, now))
	// The guarded UPDATE must bind the NEW status + the tenant scope.
	mock.ExpectExec("UPDATE agents SET").
		WithArgs("Support Agent", "d", "p", "gpt-4o-mini", "BUILDING", "v1", sqlmock.AnyArg(), "agent-1", orgID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	updated, err := svc.TransitionStateWithAgent(t.Context(), orgID, "agent-1", StateBuilding)
	if err != nil {
		t.Fatalf("store transition failed: %v", err)
	}
	if updated.Status != string(StateBuilding) {
		t.Fatalf("store transition returned status %q, want BUILDING", updated.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestTransitionStoreRejectedTransitionWritesNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	orgID := "org-store"
	now := time.Now().UTC()
	svc := NewServiceWithStore(NewPostgresStore(db))

	// draft → active is illegal: only the SELECT runs; the missing UPDATE
	// expectation makes any stray write fail the test.
	mock.ExpectQuery("FROM agents WHERE id = \\$1 AND organization_id = \\$2").
		WithArgs("agent-1", orgID).
		WillReturnRows(sqlmock.NewRows(lifecycleSQLAgentRows()).
			AddRow("agent-1", orgID, "Support Agent", "d", "p", "gpt-4o-mini", "DRAFT", "v1", now, now))

	err = svc.TransitionState(t.Context(), orgID, "agent-1", StateActive)
	var tErr *TransitionError
	if !errors.As(err, &tErr) || tErr.Current != StateDraft || tErr.Target != StateActive {
		t.Fatalf("expected typed draft→active rejection, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("rejected store transition must not write: %v", err)
	}
}

func TestTransitionStoreAgentNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	svc := NewServiceWithStore(NewPostgresStore(db))
	// Empty result set → ErrAgentNotFound, no writes.
	mock.ExpectQuery("FROM agents WHERE id").
		WithArgs("agent-x", "org-store").
		WillReturnRows(sqlmock.NewRows(lifecycleSQLAgentRows()))
	if err := svc.TransitionState(t.Context(), "org-store", "agent-x", StateBuilding); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("expected ErrAgentNotFound from store mode, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestTransitionConcurrentRacingMoves(t *testing.T) {
	// draft → building and draft → archived race from the same source state.
	// The mutex-guarded transition serializes them: exactly one wins per
	// round, the loser observes the moved state and receives the typed
	// error, and the final status is always one of the two targets.
	for round := 0; round < 25; round++ {
		orgID := fmt.Sprintf("org-race-%d", round)
		svc, agentID := lifecycleMatrixService(t, orgID, string(StateDraft))

		var wg sync.WaitGroup
		results := make([]error, 2)
		targets := []AgentState{StateBuilding, StateArchived}
		for i, target := range targets {
			wg.Add(1)
			go func(i int, target AgentState) {
				defer wg.Done()
				results[i] = svc.TransitionState(context.Background(), orgID, agentID, target)
			}(i, target)
		}
		wg.Wait()

		wins, losses := 0, 0
		for _, err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrInvalidTransition):
				losses++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if wins != 1 || losses != 1 {
			t.Fatalf("round %d: exactly one racing transition must win (wins=%d losses=%d, errs=%v)", round, wins, losses, results)
		}
		agent, err := svc.GetAgentCtx(t.Context(), orgID, agentID)
		if err != nil {
			t.Fatalf("round %d: reload failed: %v", round, err)
		}
		if agent.Status != string(StateBuilding) && agent.Status != string(StateArchived) {
			t.Fatalf("round %d: corrupted final status %q", round, agent.Status)
		}
	}
}

func TestTransitionConcurrentSameTargetSingleWinner(t *testing.T) {
	// 8 goroutines all attempt draft → building: exactly one may succeed;
	// the rest must observe the typed rejection, never a double write.
	orgID := "org-race-same"
	svc, agentID := lifecycleMatrixService(t, orgID, string(StateDraft))

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = svc.TransitionState(context.Background(), orgID, agentID, StateBuilding)
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("racer rejected with unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one racer may win, got %d wins (errs=%v)", wins, errs)
	}
}
