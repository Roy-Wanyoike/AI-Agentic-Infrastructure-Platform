package approvals

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	// Tenant guard: approvals are inserted with their organization_id scope.
	sqlInsertApproval = `INSERT INTO approvals (id, organization_id, run_id, workflow_run_id, resource, action, reason, risk, status, requester, approver, decision_reason, created_at, decided_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`
	// Tenant guard: single-approval reads are scoped to one organization_id.
	sqlSelectApprovalScoped = `SELECT id, organization_id, COALESCE(run_id, ''), COALESCE(workflow_run_id, ''), COALESCE(resource, ''), COALESCE(action, ''), COALESCE(reason, ''), COALESCE(risk, ''), status, COALESCE(requester, ''), COALESCE(approver, ''), COALESCE(decision_reason, ''), created_at, decided_at FROM approvals WHERE id = $1 AND organization_id = $2`
	// Tenant guard: listings filter on organization_id (optional status filter).
	sqlSelectApprovalsByOrg = `SELECT id, organization_id, COALESCE(run_id, ''), COALESCE(workflow_run_id, ''), COALESCE(resource, ''), COALESCE(action, ''), COALESCE(reason, ''), COALESCE(risk, ''), status, COALESCE(requester, ''), COALESCE(approver, ''), COALESCE(decision_reason, ''), created_at, decided_at FROM approvals WHERE organization_id = $1 AND ($2 = '' OR status = $2) ORDER BY created_at DESC`
	// Tenant guard: decision updates require a matching organization_id.
	sqlUpdateApproval = `UPDATE approvals SET status = $1, approver = $2, decision_reason = $3, decided_at = $4 WHERE id = $5 AND organization_id = $6`
)

// Store abstracts durable approval storage. Every tenant-facing query is
// guarded by organization_id.
type Store interface {
	// CreateApproval inserts the approval row with its organization_id scope.
	CreateApproval(ctx context.Context, orgID string, a *Approval) error
	// GetApproval fetches one approval strictly within one tenant.
	GetApproval(ctx context.Context, orgID, id string) (*Approval, error)
	// ListApprovals returns the tenant's approvals, optionally filtered by status.
	ListApprovals(ctx context.Context, orgID, status string) ([]*Approval, error)
	// UpdateApproval applies a decision within one tenant.
	UpdateApproval(ctx context.Context, orgID, id, status, approver, decisionReason string, decidedAt time.Time) error
}

// pgStore is the Postgres-backed Store implementation (lib/pq driver).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB.
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("approvals: database is nil")
	}
	return nil
}

func (s *pgStore) CreateApproval(ctx context.Context, orgID string, a *Approval) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertApproval,
		a.ID, orgID, nullableString(a.RunID), nullableString(a.WorkflowRunID),
		a.Resource, a.Action, a.Reason, a.Risk, a.Status,
		a.Requester, a.Approver, a.DecisionReason, a.CreatedAt, nullableTime(a.DecidedAt))
	return err
}

func (s *pgStore) GetApproval(ctx context.Context, orgID, id string) (*Approval, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanApproval(s.db.QueryRowContext(ctx, sqlSelectApprovalScoped, id, orgID))
}

func (s *pgStore) ListApprovals(ctx context.Context, orgID, status string) ([]*Approval, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectApprovalsByOrg, orgID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Approval, 0)
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateApproval(ctx context.Context, orgID, id, status, approver, decisionReason string, decidedAt time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateApproval, status, approver, decisionReason, decidedAt, id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (wrong org or unknown approval)
		return ErrApprovalNotFound
	}
	return nil
}

func scanApproval(scanner interface{ Scan(dest ...any) error }) (*Approval, error) {
	var a Approval
	var decidedAt sql.NullTime
	if err := scanner.Scan(&a.ID, &a.OrganizationID, &a.RunID, &a.WorkflowRunID, &a.Resource, &a.Action, &a.Reason, &a.Risk, &a.Status, &a.Requester, &a.Approver, &a.DecisionReason, &a.CreatedAt, &decidedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrApprovalNotFound
		}
		return nil, err
	}
	if decidedAt.Valid {
		t := decidedAt.Time
		a.DecidedAt = &t
	}
	return &a, nil
}

// nullableString maps empty strings to SQL NULL for optional FK columns.
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// nullableTime maps a nil/zero timestamp to SQL NULL.
func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}
