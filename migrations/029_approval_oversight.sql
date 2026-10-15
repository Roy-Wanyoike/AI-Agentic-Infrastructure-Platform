-- 029_approval_oversight.sql
--
-- Wave-9 issue #137: approvals enterprise HITL v2 — delegation, escalation
-- SLAs, dual control (two-person rule), evidence export.
--
-- Idempotent (CREATE TABLE IF NOT EXISTS / ADD COLUMN IF NOT EXISTS pattern
-- of migrations 001-027) so the migration runner can be re-run safely.
--
-- Model (mirrors internal/approvals oversight.go / oversight_store.go):
--   * approvals gains ADDITIVE oversight columns (all defaulted / nullable so
--     existing rows and queries are untouched):
--       require_dual            — two-person rule flag (RequestInput.RequireDual)
--       first_approver          — dual control: the FIRST approver
--       first_decision          — dual control: the FIRST decision (approved|rejected)
--       first_decided_at        — dual control: the FIRST decision timestamp
--       delegated_to/by/note    — live delegation assignment (chain history
--                                 lives in approval_delegations)
--       delegation_expires_at   — expired delegations lazily return the
--                                 approval to the original queue on read
--       escalated_at            — SLA breach stamp (lazy or sweep)
--       escalation_reason       — 'sla_breach'
--       escalation_deadline     — created_at + policy.sla_minutes
--     The decision columns (status/approver/decision_reason/decided_at) are
--     written by the existing UpdateApproval path; the oversight columns are
--     written by UpdateApprovalOversight — disjoint sets, no clobbering.
--   * approval_delegations — append-only delegation chain (from, to, note,
--     at, expires_at) per approval.
--   * approval_escalation_policies — ONE row per org (target_status marker +
--     SLA minutes; sla_minutes = 0 disables).
--   * approval_events — immutable oversight event log (requested, delegated,
--     delegation_expired, escalated, first_decision, decided incl. dual-control
--     pairs); source for the byte-deterministic evidence export.

-- Additive oversight columns on the wave-2 approvals table (006).
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS require_dual BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS first_approver TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS first_decision TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS first_decided_at TIMESTAMPTZ;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS delegated_to TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS delegated_by TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS delegation_note TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS delegation_expires_at TIMESTAMPTZ;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS escalated_at TIMESTAMPTZ;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS escalation_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS escalation_deadline TIMESTAMPTZ;

-- Delegation chain: one row per hand-off (never mutated; expired delegations
-- are superseded lazily, history is evidence).
CREATE TABLE IF NOT EXISTS approval_delegations (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    approval_id TEXT NOT NULL,
    from_user TEXT NOT NULL DEFAULT '',
    to_user TEXT NOT NULL DEFAULT '',
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ,
    CONSTRAINT fk_approval_delegations_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_approval_delegations_approval FOREIGN KEY (approval_id) REFERENCES approvals(id) ON DELETE CASCADE
);

-- Tenant-scoped hot path: chain reads by (org, approval), oldest first.
CREATE INDEX IF NOT EXISTS idx_approval_delegations_org_approval
    ON approval_delegations(organization_id, approval_id, created_at);

-- Per-org escalation SLA: target_status is the pending-state marker the SLA
-- clock applies to (pending | partially_approved); sla_minutes = 0 disables.
CREATE TABLE IF NOT EXISTS approval_escalation_policies (
    organization_id TEXT PRIMARY KEY,
    target_status TEXT NOT NULL DEFAULT 'pending',
    sla_minutes INTEGER NOT NULL DEFAULT 0,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_approval_escalation_policies_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT ck_approval_escalation_policies_target CHECK (target_status IN ('pending', 'partially_approved')),
    CONSTRAINT ck_approval_escalation_policies_sla CHECK (sla_minutes >= 0)
);

-- Immutable oversight event log — the source for the evidence export (NDJSON,
-- trailing {"type":"evidence_head","count":N} line). Rows are written once
-- and never mutated; the export orders by (created_at, id).
CREATE TABLE IF NOT EXISTS approval_events (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    approval_id TEXT NOT NULL,
    type TEXT NOT NULL,
    actor TEXT NOT NULL DEFAULT '',
    detail JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_approval_events_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT ck_approval_events_type CHECK (type IN ('requested', 'delegated', 'delegation_expired', 'escalated', 'first_decision', 'decided'))
);

-- Tenant-scoped hot paths: window reads for the export + per-approval chains.
CREATE INDEX IF NOT EXISTS idx_approval_events_org_created
    ON approval_events(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_approval_events_org_approval
    ON approval_events(organization_id, approval_id, created_at);
