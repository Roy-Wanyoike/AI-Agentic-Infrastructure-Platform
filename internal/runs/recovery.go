package runs

// recovery.go — issue #115: the stale-run recovery sweep.
//
// A worker crash (or a lost worker) leaves runs stuck in running/cancelling
// forever. The recovery sweep reconciles them:
//
//   - candidates: runs whose status is running (or cancelling) whose
//     updated_at AND latest checkpoint are BOTH older than the staleness
//     threshold (AGENTOS_RUN_STALE_SECONDS, default 300) — a run with a
//     fresh checkpoint is making progress and is never swept;
//   - a stale RUNNING run WITH a checkpoint is marked paused (recoverable:
//     an operator resume re-enqueues it and the runtime continues from the
//     latest checkpoint);
//   - a stale RUNNING run WITHOUT a checkpoint is marked failed (nothing to
//     resume from);
//   - a stale CANCELLING run is marked cancelled (the cancel intent was
//     recorded; the confirming worker never came back);
//   - every reconciled run emits a run.recovered event through the event
//     publisher seam.
//
// Concurrency: the Postgres sweep writes through the optimistic
// StatusGuardStore update (WHERE status = expected), so two sweeping workers
// (or a worker that came back mid-sweep) can never double-apply — the loser
// gets ErrStatusConflict and skips the run. The sweep is driven by the
// background sweeper in cmd/worker (AGENTOS_RUN_RECOVERY, default on in
// postgres mode only).

import (
	"context"
	"errors"
	"time"
)

// StaleRunRecord is one stale-run candidate resolved by the store scan.
type StaleRunRecord struct {
	ID             string
	OrganizationID string
	// Status is the raw stored status ("running"/"cancelling"/legacy spell).
	Status string
	// HasCheckpoint reports whether the run has at least one checkpoint row
	// (recoverable → paused) or none (failed).
	HasCheckpoint bool
}

// RunRecovery is the outcome of one reconciled stale run.
type RunRecovery struct {
	RunID          string
	OrganizationID string
	From           RunState
	To             RunState
}

// StaleRunScanner is the optional Store contract serving the recovery
// sweep. Implemented by pgStore (Postgres mode); the in-memory service scans
// its own map instead.
type StaleRunScanner interface {
	// StaleControlRuns returns up to limit non-terminal control-plane runs
	// (running/cancelling) whose updated_at AND latest checkpoint are both
	// older than staleBefore, oldest first. An empty orgID sweeps every
	// tenant (internal worker path; never exposed via HTTP).
	StaleControlRuns(ctx context.Context, staleBefore time.Time, orgID string, limit int) ([]StaleRunRecord, error)
}

// sqlSelectStaleControlRuns: candidates = running/cancelling rows whose
// updated_at is older than the staleness cutoff AND with no checkpoint
// created since the cutoff (a fresh checkpoint means the run is alive and
// making progress). has_checkpoint (ANY checkpoint row) decides paused vs
// failed. Status is compared lower() so the legacy UPPERCASE spellings are
// covered; the limit keeps one sweep bounded.
const sqlSelectStaleControlRuns = `SELECT r.id, r.organization_id, lower(r.status),
       EXISTS (SELECT 1 FROM run_checkpoints c WHERE c.run_id = r.id) AS has_checkpoint
FROM runs r
WHERE lower(r.status) IN ('running', 'cancelling')
  AND r.updated_at < $1
  AND NOT EXISTS (SELECT 1 FROM run_checkpoints rc WHERE rc.run_id = r.id AND rc.created_at >= $1)
  AND ($2 = '' OR r.organization_id = $2)
ORDER BY r.updated_at ASC, r.id ASC
LIMIT $3`

// StaleControlRuns implements StaleRunScanner (pgStore).
func (s *pgStore) StaleControlRuns(ctx context.Context, staleBefore time.Time, orgID string, limit int) ([]StaleRunRecord, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectStaleControlRuns, staleBefore, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]StaleRunRecord, 0)
	for rows.Next() {
		var rec StaleRunRecord
		if err := rows.Scan(&rec.ID, &rec.OrganizationID, &rec.Status, &rec.HasCheckpoint); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// recoveryTargets maps one stale candidate onto its reconciled state:
//   - cancelling → cancelled (the cancel intent is confirmed);
//   - running with a checkpoint → paused (recoverable);
//   - running without a checkpoint → failed (nothing to resume from).
func recoveryTarget(rec StaleRunRecord) (RunState, RunState, bool) {
	status, ok := ParseRunState(rec.Status)
	if !ok {
		return "", "", false
	}
	switch status {
	case StateCancelling:
		return status, StateCancelled, true
	case StateRunning:
		if rec.HasCheckpoint {
			return status, StatePaused, true
		}
		return status, StateFailed, true
	}
	return "", "", false
}

// EventRunRecoveredType is the recovery event type (canonical constant:
// internal/events.EventRunRecovered).
const EventRunRecoveredType = "run.recovered"

// DefaultRunStaleAfter is the default staleness threshold (issue #115):
// a running run with no status change and no checkpoint for this long is
// considered lost. Overridable with AGENTOS_RUN_STALE_SECONDS (worker).
const DefaultRunStaleAfter = 300 * time.Second

// RecoverStaleRuns runs one recovery pass and returns the reconciliations.
// The pass is additive and idempotent: already-reconciled runs disappear
// from the candidate query (their status moved) and racing writers only ever
// lose with ErrStatusConflict (skipped, not fatal). The failure/pause
// annotation is written to the run output column so the recovery decision is
// visible on the run itself. Events: every reconciliation emits run.recovered
// (best-effort through the event publisher seam) — the generic control-plane
// emissions are suppressed for the sweep to keep one event per recovery.
func (s *Service) RecoverStaleRuns(ctx context.Context, staleAfter time.Duration, limit int) ([]RunRecovery, error) {
	if s == nil {
		return nil, errors.New("runs: service is nil")
	}
	if staleAfter <= 0 {
		staleAfter = DefaultRunStaleAfter
	}
	staleBefore := timeNowUTC().Add(-staleAfter)

	var candidates []StaleRunRecord
	if s.store != nil {
		scanner, ok := s.store.(StaleRunScanner)
		if !ok {
			return nil, ErrRecoveryUnsupported
		}
		var err error
		candidates, err = scanner.StaleControlRuns(ctx, staleBefore, "", limit)
		if err != nil {
			return nil, err
		}
	} else {
		candidates = s.staleCandidatesInMemory(staleBefore, limit)
	}

	out := make([]RunRecovery, 0, len(candidates))
	for _, rec := range candidates {
		from, to, ok := recoveryTarget(rec)
		if !ok {
			continue
		}
		if err := s.persistRecovery(ctx, rec.OrganizationID, rec.ID, from, to, recoveryOutcomeReason(to, rec.HasCheckpoint)); err != nil {
			if errors.Is(err, ErrStatusConflict) || errors.Is(err, ErrInvalidTransition) {
				continue // another writer won; the run is no longer stale-stuck
			}
			return out, err
		}
		s.publishRunEvent(ctx, rec.OrganizationID, rec.ID, EventRunRecoveredType, map[string]any{
			"run_id":         rec.ID,
			"from":           string(from),
			"to":             string(to),
			"had_checkpoint": rec.HasCheckpoint,
			"stale_after_s":  int(staleAfter.Seconds()),
		})
		out = append(out, RunRecovery{RunID: rec.ID, OrganizationID: rec.OrganizationID, From: from, To: to})
	}
	return out, nil
}

// persistRecovery applies one reconciliation through the same guarded write
// path as TransitionRunStatus but WITHOUT the generic control-plane event
// (the sweep emits run.recovered instead).
func (s *Service) persistRecovery(ctx context.Context, orgID, id string, from, to RunState, output string) error {
	if _, err := validateRunTransition(string(from), to); err != nil {
		return err
	}
	if err := s.persistTransition(ctx, orgID, id, to, output); err != nil {
		return err
	}
	s.mirrorTransition(id, to, output)
	s.countTerminalRun(id, RunStatus(to))
	s.publishStatus(id, RunStatus(to), output)
	return nil
}

// recoveryOutcomeReason is the failure/pause annotation written to the run
// output column so the recovery decision is visible on the run itself.
func recoveryOutcomeReason(to RunState, hadCheckpoint bool) string {
	switch to {
	case StatePaused:
		return "recovered: worker lost; paused at latest checkpoint (resumable)"
	case StateFailed:
		return "recovered: worker lost; no checkpoint to resume from"
	case StateCancelled:
		return "recovered: cancel was requested; run never confirmed (worker lost)"
	}
	return ""
}

// staleCandidatesInMemory is the zero-infrastructure candidate scan (the
// service map is the point of truth). HasCheckpoint consults the optional
// checkpoint store wired via SetCheckpointStore.
func (s *Service) staleCandidatesInMemory(staleBefore time.Time, limit int) []StaleRunRecord {
	if limit <= 0 {
		limit = 50
	}
	s.mu.Lock()
	store := s.checkpoints
	// Snapshot the candidates under the lock; the existence check happens
	// outside it (the store carries its own lock and never re-enters the
	// service mutex).
	type pending struct {
		rec StaleRunRecord
	}
	candidates := make([]pending, 0)
	for _, run := range s.runs {
		if len(candidates) >= limit {
			break
		}
		status, ok := ParseRunState(string(run.Status))
		if !ok || (status != StateRunning && status != StateCancelling) {
			continue
		}
		if !run.UpdatedAt.Before(staleBefore) {
			continue
		}
		candidates = append(candidates, pending{rec: StaleRunRecord{
			ID:             run.ID,
			OrganizationID: run.OrganizationID,
			Status:         string(run.Status),
		}})
	}
	s.mu.Unlock()

	out := make([]StaleRunRecord, 0, len(candidates))
	for _, c := range candidates {
		if store != nil {
			c.rec.HasCheckpoint = store.HasCheckpoint(context.WithoutCancel(context.Background()), c.rec.ID)
		}
		out = append(out, c.rec)
	}
	return out
}
