// checkpoint.go — issue #115: the durable-checkpoint seam of the agent
// runtime.
//
// A checkpoint is the serializable snapshot saved AFTER each completed step
// of the provider loop (conversation history + cumulative token usage + the
// step index it reached). A run that pauses, cancels or loses its worker
// RESUMES from the latest checkpoint instead of starting over: on the next
// execution the loop restores the history and token accounting and continues
// with the following step, still bounded by the same max-steps budget.
//
// Contract:
//   - the seam is OPTIONAL (nil-safe DI): a runner without a checkpointer
//     behaves exactly as before (the no-op default keeps every existing
//     test green);
//   - checkpoint failures NEVER break a run — the seam is durability
//     bookkeeping, not control flow (same posture as the step recorder);
//   - the STATE document schema is owned by this package (CheckpointState);
//     the storage half lives outside (the runs package ships the Postgres
//     and in-memory CheckpointStore implementations; cmd/worker adapts one
//     onto this seam), so the runtime stays storage-agnostic.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"agentos/internal/models"
)

// CheckpointState is the runtime-owned snapshot document persisted after
// each completed step (and loaded on resume). JSON is the storage shape:
// the runs package stores it as an opaque JSONB blob.
type CheckpointState struct {
	// StepIndex is the number of completed loop iterations this snapshot
	// reflects (resume continues with StepIndex+1).
	StepIndex int `json:"step_index"`
	// Messages is the FULL conversation so far (user input, assistant
	// replies, tool observations) — the resume seeds ConversationHistory
	// with it verbatim.
	Messages []models.Message `json:"messages"`
	// Usage is the cumulative provider-reported token usage (the resume
	// re-seeds the token budget with it so a budget already exceeded before
	// the interruption still hard-stops).
	Usage models.Usage `json:"usage"`
}

// Checkpoint is one persisted snapshot as returned by the seam.
type Checkpoint struct {
	RunID     string
	StepIndex int
	State     json.RawMessage
	CreatedAt time.Time
}

// Checkpointer persists one checkpoint per (run, step index). Implementations
// must be safe for concurrent use; SaveCheckpoint is an UPSERT (the latest
// save for a step index wins), LatestCheckpoint returns the highest
// step-index snapshot or an ErrCheckpointNotFound-class error when the run
// has none (the resume then starts from the beginning).
type Checkpointer interface {
	SaveCheckpoint(ctx context.Context, runID string, state CheckpointState) error
	LatestCheckpoint(ctx context.Context, runID string) (*Checkpoint, error)
}

// errNoCheckpoint is the sentinel implementations return when the run has no
// checkpoint yet (structural match: any error carrying this text classifies;
// the seam contract only distinguishes "none" from "store failure" by the
// LatestCheckpoint error being non-nil with a nil checkpoint — implementations
// should return an error here, never a nil checkpoint with nil error).
var errNoCheckpoint = errors.New("runtime: no checkpoint")

// WithCheckpointer attaches the durable-checkpoint seam (issue #115): after
// every completed step the runner saves a snapshot and a re-executed run
// resumes from the latest one. Passing nil keeps the no-op default
// (nil-safe DI).
func WithCheckpointer(c Checkpointer) Option {
	return func(r *Runner) { r.checkpointer = c }
}

// SetCheckpointer attaches (or clears, via nil) the checkpointer after
// construction. Nil-safe on the receiver; call before runs are in flight
// (the Runner is not lock-protected).
func (r *Runner) SetCheckpointer(c Checkpointer) {
	if r == nil {
		return
	}
	r.checkpointer = c
}

// saveCheckpoint persists one end-of-step snapshot (best-effort: a failing
// checkpointer never breaks the run — durability bookkeeping, not control
// flow).
func (r *Runner) saveCheckpoint(ctx context.Context, runID string, state CheckpointState) {
	if r == nil || r.checkpointer == nil {
		return
	}
	_ = r.checkpointer.SaveCheckpoint(ctx, runID, state)
}

// resumeFromCheckpoint loads the latest checkpoint of the run and returns
// the restore plan: (state, true) when the run continues from the snapshot,
// (zero, false) when there is nothing (or nothing usable) to resume from.
// A corrupt snapshot degrades to a fresh start — bad durability data must
// never prevent execution (same posture as the runtime-config source).
func (r *Runner) resumeFromCheckpoint(ctx context.Context, runID string) (CheckpointState, bool) {
	if r == nil || r.checkpointer == nil {
		return CheckpointState{}, false
	}
	cp, err := r.checkpointer.LatestCheckpoint(ctx, runID)
	if err != nil || cp == nil {
		return CheckpointState{}, false
	}
	var state CheckpointState
	if err := json.Unmarshal(cp.State, &state); err != nil {
		return CheckpointState{}, false
	}
	if state.StepIndex <= 0 || len(state.Messages) == 0 {
		return CheckpointState{}, false
	}
	return state, true
}
