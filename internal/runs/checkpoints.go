package runs

// checkpoints.go — issue #115: durable run checkpoints.
//
// A checkpoint is the serializable snapshot the runtime saves after each
// completed step (conversation history + token accounting + step index), so
// a paused/interrupted run RESUMES from the latest checkpoint instead of
// starting over. The storage seam (CheckpointStore) is intentionally runtime
//-agnostic: internal/runtime defines the Checkpointer execution seam
// (no-op default; see internal/runtime/checkpoint.go) and the worker adapts
// this store onto it — the runs package owns the DURABLE half.
//
// Two implementations ship here:
//   - PostgresCheckpointStore (NewPostgresCheckpointStore): the durable
//     store backed by the run_checkpoints table (migration 037). Every write
//     is tenant-guarded through the runs join (a checkpoint can never be
//     attached to a foreign run), keyed idempotently by
//     PK (run_id, step_index) so replays overwrite in place.
//   - InMemoryCheckpointStore (NewInMemoryCheckpointStore): the
//     zero-infrastructure counterpart (mutex-guarded map), used when no DSN
//     is configured so resume semantics work identically in memory mode.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrCheckpointNotFound is returned by LatestCheckpoint when the run has no
// checkpoint yet (fresh run) — callers start from the beginning.
var ErrCheckpointNotFound = errors.New("runs: checkpoint not found")

// RunCheckpoint is one persisted mid-run snapshot.
type RunCheckpoint struct {
	RunID     string
	StepIndex int
	// State is the opaque runtime snapshot document (JSONB column; the runs
	// package never interprets it — the runtime owns the schema).
	State     json.RawMessage
	CreatedAt time.Time
}

// CheckpointStore persists run checkpoints (issue #115). Implementations
// must be safe for concurrent use.
type CheckpointStore interface {
	// SaveCheckpoint upserts the checkpoint for (runID, stepIndex). The
	// write is tenant-guarded: implementations must reject (ErrRunNotFound)
	// checkpoints for runs outside orgID.
	SaveCheckpoint(ctx context.Context, orgID, runID string, stepIndex int, state json.RawMessage) error
	// LatestCheckpoint returns the highest step_index checkpoint of the run
	// within one tenant (organization_id guard), or ErrCheckpointNotFound.
	LatestCheckpoint(ctx context.Context, orgID, runID string) (*RunCheckpoint, error)
	// HasCheckpoint reports whether the run holds at least one checkpoint
	// (the recovery sweep's paused-vs-failed decision; issue #115).
	HasCheckpoint(ctx context.Context, runID string) bool
}

// sqlInsertRunCheckpoint upserts one checkpoint row. Tenant guard: the
// INSERT ... SELECT ... WHERE EXISTS form only accepts rows for runs inside
// the caller's organization (the same guard shape as sqlInsertRunStep).
// ON CONFLICT keeps the checkpoint idempotent per (run_id, step_index): a
// replayed save overwrites in place instead of erroring.
const sqlUpsertRunCheckpoint = `INSERT INTO run_checkpoints (run_id, step_index, state, created_at)
SELECT $1, $2, $3::jsonb, $4
WHERE EXISTS (SELECT 1 FROM runs WHERE id = $1 AND organization_id = $5)
ON CONFLICT (run_id, step_index) DO UPDATE SET state = EXCLUDED.state, created_at = EXCLUDED.created_at`

// sqlSelectLatestRunCheckpoint returns the newest checkpoint of one run
// within one tenant (organization_id guard via the runs join).
const sqlSelectLatestRunCheckpoint = `SELECT c.run_id, c.step_index, COALESCE(c.state::text, '{}'), c.created_at
FROM run_checkpoints c JOIN runs r ON r.id = c.run_id
WHERE r.organization_id = $1 AND c.run_id = $2
ORDER BY c.step_index DESC LIMIT 1`

// postgresCheckpoints is the Postgres-backed CheckpointStore.
type postgresCheckpoints struct {
	db *sql.DB
}

// NewPostgresCheckpointStore returns a CheckpointStore backed by *sql.DB
// (run_checkpoints table, migration 037).
func NewPostgresCheckpointStore(db *sql.DB) CheckpointStore {
	return &postgresCheckpoints{db: db}
}

func (s *postgresCheckpoints) guard() error {
	if s == nil || s.db == nil {
		return errors.New("runs: database is nil")
	}
	return nil
}

// SaveCheckpoint implements CheckpointStore. affected == 0 means the tenant
// guard rejected the write (unknown or foreign run).
func (s *postgresCheckpoints) SaveCheckpoint(ctx context.Context, orgID, runID string, stepIndex int, state json.RawMessage) error {
	if err := s.guard(); err != nil {
		return err
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(runID) == "" {
		return ErrRunNotFound
	}
	res, err := s.db.ExecContext(ctx, sqlUpsertRunCheckpoint,
		runID, stepIndex, string(state), time.Now().UTC(), orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrRunNotFound
	}
	return nil
}

// LatestCheckpoint implements CheckpointStore. The JSONB column is selected
// as text (driver-agnostic: no database/sql driver-specific []byte
// semantics) and converted to the opaque json.RawMessage afterwards.
func (s *postgresCheckpoints) LatestCheckpoint(ctx context.Context, orgID, runID string) (*RunCheckpoint, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var cp RunCheckpoint
	var state string
	if err := s.db.QueryRowContext(ctx, sqlSelectLatestRunCheckpoint, orgID, runID).
		Scan(&cp.RunID, &cp.StepIndex, &state, &cp.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCheckpointNotFound
		}
		return nil, err
	}
	cp.State = json.RawMessage(state)
	return &cp, nil
}

// sqlExistsRunCheckpoint answers the recovery sweep's paused-vs-failed
// decision cheaply (any checkpoint row for the run).
const sqlExistsRunCheckpoint = `SELECT EXISTS (SELECT 1 FROM run_checkpoints WHERE run_id = $1)`

// HasCheckpoint implements CheckpointStore (any checkpoint for the run; the
// recovery sweep is a trusted internal path, not tenant-facing).
func (s *postgresCheckpoints) HasCheckpoint(ctx context.Context, runID string) bool {
	if err := s.guard(); err != nil || strings.TrimSpace(runID) == "" {
		return false
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, sqlExistsRunCheckpoint, runID).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// inMemoryCheckpoints is the zero-infrastructure CheckpointStore: a
// mutex-guarded map with the same idempotency and not-found semantics as the
// Postgres store.
type inMemoryCheckpoints struct {
	mu   sync.Mutex
	data map[string]map[int]RunCheckpoint // runID -> stepIndex -> checkpoint
}

// NewInMemoryCheckpointStore returns an empty in-memory CheckpointStore
// (memory mode: identical resume semantics without Postgres).
func NewInMemoryCheckpointStore() CheckpointStore {
	return &inMemoryCheckpoints{data: make(map[string]map[int]RunCheckpoint)}
}

// SaveCheckpoint implements CheckpointStore. The tenant guard requires a
// prior runs-service record in memory mode: SaveCheckpoint is always called
// with the org the run was created under, so the guard here is orgID
// presence (the durable store cross-checks the runs table instead).
func (s *inMemoryCheckpoints) SaveCheckpoint(_ context.Context, orgID, runID string, stepIndex int, state json.RawMessage) error {
	if s == nil {
		return errors.New("runs: memory checkpoint store is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(runID) == "" {
		return ErrRunNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = make(map[string]map[int]RunCheckpoint)
	}
	if s.data[runID] == nil {
		s.data[runID] = make(map[int]RunCheckpoint)
	}
	s.data[runID][stepIndex] = RunCheckpoint{
		RunID:     runID,
		StepIndex: stepIndex,
		State:     append(json.RawMessage(nil), state...),
		CreatedAt: timeNowUTC(),
	}
	return nil
}

// LatestCheckpoint implements CheckpointStore.
func (s *inMemoryCheckpoints) LatestCheckpoint(_ context.Context, orgID, runID string) (*RunCheckpoint, error) {
	if s == nil {
		return nil, errors.New("runs: memory checkpoint store is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(runID) == "" {
		return nil, ErrRunNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byStep, ok := s.data[runID]
	if !ok || len(byStep) == 0 {
		return nil, ErrCheckpointNotFound
	}
	best := -1
	for stepIndex := range byStep {
		if stepIndex > best {
			best = stepIndex
		}
	}
	cp := byStep[best]
	cp.State = append(json.RawMessage(nil), cp.State...)
	return &cp, nil
}

// HasCheckpoint implements CheckpointStore.
func (s *inMemoryCheckpoints) HasCheckpoint(_ context.Context, runID string) bool {
	if s == nil || strings.TrimSpace(runID) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data[runID]) > 0
}

// SetCheckpointStore wires (or replaces, via nil) the optional checkpoint
// store on the service. Used by the IN-MEMORY recovery sweep to distinguish
// recoverable (checkpoint present → paused) from failed stale runs; the
// Postgres sweep reads run_checkpoints directly in SQL. Nil-safe.
func (s *Service) SetCheckpointStore(store CheckpointStore) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoints = store
}
