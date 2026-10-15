package synth

// store_findings.go — issue #150 (track 10-r): Postgres-backed
// implementation of FindingStore writing to synth_findings (migration 034).
//
// Semantics are ROW-PER-OCCURRENCE: one row per
// (org_id, fingerprint, run_id) — exactly the ux_synth_findings_fp unique
// index — so occurrence history IS the row set and dedup never loses
// history. The canonical finding for a fingerprint is its latest row
// (ORDER BY updated_at DESC). All queries are org-scoped: a fingerprint is
// never resolved across tenants (tenant isolation is non-negotiable).
//
// The caller supplies a *sql.DB (lib/pq or pgx stdlib). This store performs
// no schema management — migration 034 owns the table.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PostgresFindingStore persists findings into synth_findings.
type PostgresFindingStore struct {
	db *sql.DB
}

// NewPostgresFindingStore returns a store over db. db must have migration
// 034 applied.
func NewPostgresFindingStore(db *sql.DB) *PostgresFindingStore {
	return &PostgresFindingStore{db: db}
}

// pgFindingColumns is the projected column list, shared by every SELECT so
// the scanner stays in sync with the schema.
const pgFindingColumns = `id, org_id, run_id, journey_id, assertion_ix, status, severity, ` +
	`title, description, fingerprint, repro_steps, evidence_refs, repro_run_id, ` +
	`reproducible, confidence, created_at, updated_at`

// pgRecordFindingSQL upserts one occurrence row keyed by
// (org_id, fingerprint, run_id) — the ux_synth_findings_fp unique index.
// created_at is preserved on conflict; everything else reflects the latest
// observation.
const pgRecordFindingSQL = `INSERT INTO synth_findings (` + pgFindingColumns + `) ` +
	`VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17) ` +
	`ON CONFLICT (org_id, fingerprint, run_id) DO UPDATE SET ` +
	`id = EXCLUDED.id, journey_id = EXCLUDED.journey_id, assertion_ix = EXCLUDED.assertion_ix, ` +
	`status = EXCLUDED.status, severity = EXCLUDED.severity, title = EXCLUDED.title, ` +
	`description = EXCLUDED.description, repro_steps = EXCLUDED.repro_steps, ` +
	`evidence_refs = EXCLUDED.evidence_refs, repro_run_id = EXCLUDED.repro_run_id, ` +
	`reproducible = EXCLUDED.reproducible, confidence = EXCLUDED.confidence, ` +
	`updated_at = EXCLUDED.updated_at`

// refsToJSON marshals evidence refs deterministically; nil becomes "[]"
// (never SQL NULL — the column is NOT NULL DEFAULT '[]').
func refsToJSON(refs []string) string {
	if refs == nil {
		refs = []string{}
	}
	b, err := json.Marshal(refs)
	if err != nil {
		// Marshaling []string cannot fail; kept defensive.
		return "[]"
	}
	return string(b)
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullBool(b *bool) sql.NullBool {
	if b == nil {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: *b, Valid: true}
}

// Record upserts one occurrence row.
func (s *PostgresFindingStore) Record(ctx context.Context, f Finding) error {
	if f.OrgID == "" || f.ID == "" {
		return fmt.Errorf("%w: finding requires org and id", ErrInvalid)
	}
	_, err := s.db.ExecContext(ctx, pgRecordFindingSQL,
		f.ID, f.OrgID, f.RunID, f.JourneyID, f.AssertionIx,
		string(f.Status), string(f.Severity), f.Title, f.Description,
		f.Fingerprint, f.ReproSteps, refsToJSON(f.EvidenceRefs),
		nullString(f.ReproRunID), nullBool(f.Reproducible), f.Confidence,
		f.CreatedAt.UTC(), f.UpdatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("synth: record finding %s: %w", f.ID, err)
	}
	return nil
}

// Get returns one finding by id within org.
func (s *PostgresFindingStore) Get(ctx context.Context, orgID, id string) (Finding, error) {
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings WHERE id = $1 AND org_id = $2`
	row := s.db.QueryRowContext(ctx, q, id, orgID)
	f, err := scanFinding(row.Scan)
	if err != nil {
		return Finding{}, err
	}
	return f, nil
}

// GetByFingerprint returns the canonical (latest) occurrence row for the
// fingerprint within org.
func (s *PostgresFindingStore) GetByFingerprint(ctx context.Context, orgID, fingerprint string) (Finding, error) {
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings ` +
		`WHERE org_id = $1 AND fingerprint = $2 ` +
		`ORDER BY updated_at DESC, run_id DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, orgID, fingerprint)
	return scanFinding(row.Scan)
}

// ListByFingerprint returns the full occurrence history for a fingerprint
// within org, oldest first (the append-only occurrence log).
func (s *PostgresFindingStore) ListByFingerprint(ctx context.Context, orgID, fingerprint string) ([]Finding, error) {
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings ` +
		`WHERE org_id = $1 AND fingerprint = $2 ` +
		`ORDER BY created_at ASC, run_id ASC`
	rows, err := s.db.QueryContext(ctx, q, orgID, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("synth: list findings by fingerprint: %w", err)
	}
	defer rows.Close()
	out := []Finding{}
	for rows.Next() {
		f, err := scanFinding(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("synth: iterate findings: %w", err)
	}
	return out, nil
}

// List returns findings within org matching filter, newest first.
func (s *PostgresFindingStore) List(ctx context.Context, orgID string, filter FindingFilter) ([]Finding, error) {
	where := []string{"org_id = $1"}
	args := []any{orgID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf("%s = $%d", cond, len(args)))
	}
	if filter.Status != "" {
		add("status", string(filter.Status))
	}
	if filter.Fingerprint != "" {
		add("fingerprint", filter.Fingerprint)
	}
	if filter.JourneyID != "" {
		add("journey_id", filter.JourneyID)
	}
	q := `SELECT ` + pgFindingColumns + ` FROM synth_findings WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY updated_at DESC, id DESC`
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("synth: list findings: %w", err)
	}
	defer rows.Close()
	out := []Finding{}
	for rows.Next() {
		f, err := scanFinding(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("synth: iterate findings: %w", err)
	}
	return out, nil
}

// scanFinding adapts a row scanner into a Finding. Nullable columns
// (repro_run_id, reproducible) map to nil pointers; evidence_refs JSON
// deserializes to a slice (missing/empty -> empty, never nil-nil confusion).
func scanFinding(scan func(dest ...any) error) (Finding, error) {
	var (
		f            Finding
		status       string
		severity     string
		refsJSON     []byte
		reproRunID   sql.NullString
		reproducible sql.NullBool
		createdAt    time.Time
		updatedAt    time.Time
	)
	if err := scan(&f.ID, &f.OrgID, &f.RunID, &f.JourneyID, &f.AssertionIx,
		&status, &severity, &f.Title, &f.Description, &f.Fingerprint,
		&f.ReproSteps, &refsJSON, &reproRunID, &reproducible, &f.Confidence,
		&createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return Finding{}, fmt.Errorf("%w: finding", ErrNotFound)
		}
		return Finding{}, fmt.Errorf("synth: scan finding: %w", err)
	}
	f.Status = FindingStatus(status)
	f.Severity = Severity(severity)
	refs := []string{}
	if len(refsJSON) > 0 {
		if err := json.Unmarshal(refsJSON, &refs); err != nil {
			return Finding{}, fmt.Errorf("synth: scan finding refs: %w", err)
		}
	}
	f.EvidenceRefs = refs
	if reproRunID.Valid {
		v := reproRunID.String
		f.ReproRunID = v
	}
	if reproducible.Valid {
		v := reproducible.Bool
		f.Reproducible = &v
	}
	f.CreatedAt = createdAt
	f.UpdatedAt = updatedAt
	return f, nil
}

// Compile-time interface check.
var _ FindingStore = (*PostgresFindingStore)(nil)
