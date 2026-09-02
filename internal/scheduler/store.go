package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// pgStore is the Postgres-backed Store implementation (lib/pq driver, same
// pattern as internal/runs/store.go).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB.
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("scheduler: database is nil")
	}
	return nil
}

const (
	// scheduleColumns is the shared projection; NULLable columns are scanned
	// into sql.Null* values by scanSchedule.
	scheduleColumns = `id, organization_id, agent_id, COALESCE(input, ''), kind, run_at, interval_seconds, COALESCE(cron_expr, ''), COALESCE(timezone, 'UTC'), status, next_run_at, COALESCE(last_run_id, ''), last_fired_at, created_at, updated_at`

	// Tenant guard: rows are inserted with their organization_id scope.
	sqlInsertSchedule = `INSERT INTO schedules
		(id, organization_id, agent_id, input, kind, run_at, interval_seconds, cron_expr, timezone, status, next_run_at, last_run_id, last_fired_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

	// Tenant guard: single-schedule reads are scoped to one organization_id.
	sqlSelectScheduleScoped = `SELECT ` + scheduleColumns + ` FROM schedules WHERE id = $1 AND organization_id = $2`

	// Trusted internal PK lookup (scheduler worker path); the row carries
	// organization_id so fired runs are created for the right tenant.
	sqlSelectScheduleByID = `SELECT ` + scheduleColumns + ` FROM schedules WHERE id = $1`

	// Tenant guard: listings filter on organization_id.
	sqlSelectSchedulesByOrg = `SELECT ` + scheduleColumns + ` FROM schedules WHERE organization_id = $1 ORDER BY created_at ASC`

	// Trusted worker path: due schedules across tenants, hottest query first
	// via idx_schedules_status_next_run (status, next_run_at).
	sqlSelectDueSchedules = `SELECT ` + scheduleColumns + ` FROM schedules
		WHERE status = 'active' AND next_run_at IS NOT NULL AND next_run_at <= $1
		ORDER BY next_run_at ASC`

	// Catch-up protection: the WHERE clause re-checks that the slot is still
	// due, so concurrent workers/restarts can never consume the same firing
	// twice (single conditional UPDATE = atomic claim).
	sqlClaimForFire = `UPDATE schedules SET
		status = $2,
		next_run_at = $3,
		last_fired_at = $4,
		updated_at = $4
		WHERE id = $1 AND status = 'active' AND next_run_at IS NOT NULL AND next_run_at <= $4`

	// Trusted worker path: attach the run created by the claimed firing.
	sqlSetLastRun = `UPDATE schedules SET last_run_id = $2, updated_at = $3 WHERE id = $1`

	// Tenant guard: status transitions require a matching organization_id;
	// completed schedules are terminal and can never be re-activated.
	sqlUpdateScheduleStatus = `UPDATE schedules SET status = $1, updated_at = $2
		WHERE id = $3 AND organization_id = $4 AND status <> 'completed'`

	// Tenant guard: deletes require a matching organization_id.
	sqlDeleteSchedule = `DELETE FROM schedules WHERE id = $1 AND organization_id = $2`
)

func (s *pgStore) Create(ctx context.Context, sched *Schedule) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertSchedule,
		sched.ID, sched.OrganizationID, sched.AgentID, sched.Input, sched.Kind,
		nullableTime(sched.RunAt), nullableInt(sched.IntervalSeconds), sched.CronExpr, sched.Timezone,
		sched.Status, nullableTime(sched.NextRunAt), sched.LastRunID,
		nullableTime(sched.LastFiredAt), sched.CreatedAt, sched.UpdatedAt)
	return err
}

func (s *pgStore) Get(ctx context.Context, orgID, id string) (*Schedule, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2
	return scanSchedule(s.db.QueryRowContext(ctx, sqlSelectScheduleScoped, id, orgID))
}

func (s *pgStore) GetByID(ctx context.Context, id string) (*Schedule, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanSchedule(s.db.QueryRowContext(ctx, sqlSelectScheduleByID, id))
}

func (s *pgStore) List(ctx context.Context, orgID string) ([]*Schedule, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectSchedulesByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Schedule, 0)
	for rows.Next() {
		sched, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sched)
	}
	return out, rows.Err()
}

func (s *pgStore) Due(ctx context.Context, now time.Time) ([]*Schedule, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectDueSchedules, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Schedule, 0)
	for rows.Next() {
		sched, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sched)
	}
	return out, rows.Err()
}

func (s *pgStore) ClaimForFire(ctx context.Context, id string, firedAt time.Time, newStatus string, nextRunAt *time.Time) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, sqlClaimForFire, id, newStatus, nullableTime(nextRunAt), firedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *pgStore) SetLastRun(ctx context.Context, id, runID string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlSetLastRun, id, runID, time.Now().UTC())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

func (s *pgStore) UpdateStatus(ctx context.Context, orgID, id string, status string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateScheduleStatus, status, time.Now().UTC(), id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

func (s *pgStore) Delete(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteSchedule, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

func scanSchedule(scanner interface{ Scan(dest ...any) error }) (*Schedule, error) {
	var sched Schedule
	var runAt, nextRunAt, lastFiredAt sql.NullTime
	var intervalSeconds sql.NullInt64
	if err := scanner.Scan(
		&sched.ID, &sched.OrganizationID, &sched.AgentID, &sched.Input, &sched.Kind,
		&runAt, &intervalSeconds, &sched.CronExpr, &sched.Timezone,
		&sched.Status, &nextRunAt, &sched.LastRunID, &lastFiredAt,
		&sched.CreatedAt, &sched.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrScheduleNotFound
		}
		return nil, err
	}
	if runAt.Valid {
		t := runAt.Time.UTC()
		sched.RunAt = &t
	}
	if intervalSeconds.Valid {
		sched.IntervalSeconds = int(intervalSeconds.Int64)
	}
	if nextRunAt.Valid {
		t := nextRunAt.Time.UTC()
		sched.NextRunAt = &t
	}
	if lastFiredAt.Valid {
		t := lastFiredAt.Time.UTC()
		sched.LastFiredAt = &t
	}
	return &sched, nil
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
