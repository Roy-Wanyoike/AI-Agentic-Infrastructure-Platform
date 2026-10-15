package main

// checkpoints.go — issue #115: worker-side run lifecycle plumbing.
//
// Three small pieces adapt the durable runs machinery onto the worker's
// task-execution loop:
//
//  1. runsCheckpointAdapter — adapts runs.CheckpointStore onto the
//     runtime.Checkpointer seam (org-bound: the per-org runners are built per
//     organization, so the tenant guard of the checkpoint store is satisfied
//     without changing the runtime seam's signature).
//  2. runControlHandle — the RunController an in-flight execution registers
//     in runs.RunControlRegistry while it executes: an API pause/cancel lands
//     here and cancels the run's context; the runtime's graceful-unwind
//     semantics (issue #107 shutdown.go) let the current step complete and
//     record the partial results before the loop exits.
//  3. controlledStopStatus — the outcome classifier: a controlled stop is NOT
//     a failure. A pause keeps the run paused (resumable from the latest
//     checkpoint); a cancel confirms cancelling → cancelled through the
//     API's terminal-status relay.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"

	"agentos/internal/runs"
	"agentos/internal/runtime"
)

// ErrNoCheckpointForRun is returned by the adapter when no store is wired
// (nil-safe DI): the runtime treats it as "nothing to resume from".
var ErrNoCheckpointForRun = errors.New("worker: no checkpoint store wired")

// runsCheckpointAdapter persists runtime checkpoint snapshots through the
// durable runs.CheckpointStore (Postgres in postgres mode, in-memory
// otherwise). The org is bound at construction (per-org runner); the runtime
// only ever passes the run id.
type runsCheckpointAdapter struct {
	store runs.CheckpointStore
	orgID string
}

// SaveCheckpoint implements runtime.Checkpointer: the CheckpointState document
// is marshaled to JSON and stored under (org, run, step_index) — the upsert
// keeps replays idempotent.
func (a runsCheckpointAdapter) SaveCheckpoint(ctx context.Context, runID string, state runtime.CheckpointState) error {
	if a.store == nil {
		return nil
	}
	blob, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return a.store.SaveCheckpoint(ctx, a.orgID, runID, state.StepIndex, blob)
}

// LatestCheckpoint implements runtime.Checkpointer. Store errors (including
// the "no checkpoint yet" case) propagate verbatim: the resume path only
// distinguishes "none" from "store failure" by error presence.
func (a runsCheckpointAdapter) LatestCheckpoint(ctx context.Context, runID string) (*runtime.Checkpoint, error) {
	if a.store == nil {
		return nil, ErrNoCheckpointForRun
	}
	cp, err := a.store.LatestCheckpoint(ctx, a.orgID, runID)
	if err != nil {
		return nil, err
	}
	return &runtime.Checkpoint{
		RunID:     cp.RunID,
		StepIndex: cp.StepIndex,
		State:     cp.State,
		CreatedAt: cp.CreatedAt,
	}, nil
}

// runControlAction is the last control signal an in-flight run received.
type runControlAction int32

const (
	controlNone runControlAction = iota
	controlPause
	controlCancel
)

// runControlHandle is the runs.RunController of one in-flight execution.
// Pause and Cancel both cancel the run context — the runtime treats caller
// cancellation as a GRACEFUL unwind (the current step completes, the partial
// results are recorded, ErrRunCancelled is returned); the difference is
// bookkeeping: the action decides the post-run outcome handling
// (controlledStopStatus) and the API already wrote the durable status.
type runControlHandle struct {
	cancel context.CancelFunc
	action atomic.Int32
}

// newRunControlHandle binds the handle to the per-run cancel func.
func newRunControlHandle(cancel context.CancelFunc) *runControlHandle {
	return &runControlHandle{cancel: cancel}
}

// Pause implements runs.RunController: halt at the current step boundary.
// Idempotent; the first signal wins.
func (h *runControlHandle) Pause(context.Context) error {
	h.stop(controlPause)
	return nil
}

// Cancel implements runs.RunController: unwind gracefully (two-phase cancel:
// the API transitioned the run to cancelling; the worker confirms cancelled
// after the unwind).
func (h *runControlHandle) Cancel(context.Context) error {
	h.stop(controlCancel)
	return nil
}

func (h *runControlHandle) stop(a runControlAction) {
	if h == nil || h.cancel == nil {
		return
	}
	h.action.CompareAndSwap(int32(controlNone), int32(a))
	h.cancel()
}

// stopped reports the last control signal (controlNone when the run finished
// on its own).
func (h *runControlHandle) stopped() runControlAction {
	if h == nil {
		return controlNone
	}
	return runControlAction(h.action.Load())
}

// controlledStopStatus maps a run outcome onto the status relay payload of a
// CONTROLLED stop. Empty means "not a controlled stop": regular failure
// handling applies. A pause relays the NON-terminal "paused" (the API's
// terminal-status relay ignores it — the run row already says paused — but
// streaming consumers see the halt); a cancel relays "cancelled", which the
// relay applies onto the cancelling row (cancelling → cancelled is a legal
// transition of the enforced machine).
func controlledStopStatus(h *runControlHandle, rerr error) string {
	if rerr == nil || h.stopped() == controlNone {
		return ""
	}
	switch h.stopped() {
	case controlPause:
		return string(runs.StatePaused)
	case controlCancel:
		return string(runs.StateCancelled)
	}
	return ""
}
