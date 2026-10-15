package approvals

// oversight_store.go — issue #137: the OversightStore implementation on the
// Postgres store. Every statement is guarded by organization_id (same tenant
// guard convention as store.go) and every table is created by migration 029.
//
// The dual-mode contract: the service treats the store as OPTIONAL — when the
// store handed to NewServiceWithStore implements OversightStore the oversight
// features persist; otherwise the in-memory caches carry them. The in-memory
// map remains the write-through cache in both modes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	// Tenant guard: delegation chain rows carry their organization_id scope.
	sqlInsertDelegation = `INSERT INTO approval_delegations (id, organization_id, approval_id, from_user, to_user, note, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	// Tenant guard: chain reads filter on organization_id AND approval_id.
	sqlSelectDelegations = `SELECT id, organization_id, approval_id, COALESCE(from_user, ''), COALESCE(to_user, ''), COALESCE(note, ''), created_at, expires_at FROM approval_delegations WHERE organization_id = $1 AND approval_id = $2 ORDER BY created_at ASC, id ASC`
	// One policy row per org: upsert keeps it a single row forever.
	sqlUpsertEscalationPolicy = `INSERT INTO approval_escalation_policies (organization_id, target_status, sla_minutes, updated_by, updated_at) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (organization_id) DO UPDATE SET target_status = EXCLUDED.target_status, sla_minutes = EXCLUDED.sla_minutes, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`
	// Tenant guard: policy reads are scoped to one organization_id.
	sqlSelectEscalationPolicy = `SELECT organization_id, target_status, sla_minutes, COALESCE(updated_by, ''), updated_at FROM approval_escalation_policies WHERE organization_id = $1`
	// Tenant guard: oversight column writes require a matching organization_id.
	// UpdateApproval (decisions) and this statement write DISJOINT column sets
	// so the decision path never clobbers the oversight pair (and vice versa).
	sqlUpdateApprovalOversight = `UPDATE approvals SET first_approver = $1, first_decision = $2, first_decided_at = $3, delegated_to = $4, delegated_by = $5, delegation_note = $6, delegation_expires_at = $7, escalated_at = $8, escalation_reason = $9, escalation_deadline = $10 WHERE id = $11 AND organization_id = $12`
	// Tenant guard: oversight events are inserted with their organization_id.
	sqlInsertApprovalEvent = `INSERT INTO approval_events (id, organization_id, approval_id, type, actor, detail, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`
	// Tenant guard: evidence reads filter on organization_id and the window;
	// canonical order (created_at ASC, id ASC) is fixed in the query so the
	// export is byte-deterministic per data set.
	sqlSelectApprovalEvents = `SELECT id, organization_id, approval_id, type, COALESCE(actor, ''), COALESCE(detail, '{}'::jsonb)::text, created_at FROM approval_events WHERE organization_id = $1 AND created_at >= $2 AND created_at <= $3 ORDER BY created_at ASC, id ASC`
)

// Compile-time proof the Postgres store satisfies the additive contract.
var _ OversightStore = (*pgStore)(nil)

func (s *pgStore) CreateDelegation(ctx context.Context, orgID string, d *Delegation) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertDelegation,
		d.ID, orgID, d.ApprovalID, d.FromUser, d.ToUser, d.Note, d.CreatedAt, nullableTime(d.ExpiresAt))
	return err
}

func (s *pgStore) ListDelegations(ctx context.Context, orgID, approvalID string) ([]*Delegation, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectDelegations, orgID, approvalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Delegation, 0)
	for rows.Next() {
		var d Delegation
		var expiresAt sql.NullTime
		if err := rows.Scan(&d.ID, &d.OrganizationID, &d.ApprovalID, &d.FromUser, &d.ToUser, &d.Note, &d.CreatedAt, &expiresAt); err != nil {
			return nil, err
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			d.ExpiresAt = &t
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

func (s *pgStore) UpsertEscalationPolicy(ctx context.Context, orgID string, p *EscalationPolicy) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlUpsertEscalationPolicy,
		orgID, p.TargetStatus, p.SLAMinutes, p.UpdatedBy, p.UpdatedAt)
	return err
}

func (s *pgStore) GetEscalationPolicy(ctx context.Context, orgID string) (*EscalationPolicy, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var p EscalationPolicy
	err := s.db.QueryRowContext(ctx, sqlSelectEscalationPolicy, orgID).
		Scan(&p.OrganizationID, &p.TargetStatus, &p.SLAMinutes, &p.UpdatedBy, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Unset policy: not an error — the caller falls back to the
			// disabled default (nil).
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (s *pgStore) UpdateApprovalOversight(ctx context.Context, orgID, id string, a *Approval) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateApprovalOversight,
		a.FirstApprover, a.FirstDecision, nullableTime(a.FirstDecidedAt),
		a.DelegatedTo, a.DelegatedBy, a.DelegationNote, nullableTime(a.DelegationExpiresAt),
		nullableTime(a.EscalatedAt), a.EscalationReason, nullableTime(a.EscalationDeadline),
		id, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// tenant guard rejected the update (wrong org or unknown approval)
		return ErrApprovalNotFound
	}
	return nil
}

func (s *pgStore) InsertApprovalEvent(ctx context.Context, orgID string, e *Event) error {
	if err := s.guard(); err != nil {
		return err
	}
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	if len(detail) == 0 || string(detail) == "null" {
		detail = []byte("{}")
	}
	_, err = s.db.ExecContext(ctx, sqlInsertApprovalEvent,
		e.ID, orgID, e.ApprovalID, e.Type, e.Actor, string(detail), e.CreatedAt)
	return err
}

func (s *pgStore) ListApprovalEvents(ctx context.Context, orgID string, from, to time.Time) ([]*Event, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Zero bounds are open: time.Time{} sorts before every real timestamp in
	// Postgres, and the service resolves a zero `to` before calling.
	if to.IsZero() {
		to = time.Now().UTC()
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectApprovalEvents, orgID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Event, 0)
	for rows.Next() {
		var e Event
		var detail []byte
		if err := rows.Scan(&e.ID, &e.OrganizationID, &e.ApprovalID, &e.Type, &e.Actor, &detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &e.Detail) // best-effort: absent detail stays nil
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
