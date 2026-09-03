-- 017_secrets.sql
--
-- Issue #25: org-scoped secrets management with encryption at rest.
--
-- One row per (organization_id, name). The service layer (internal/secrets)
-- writes ONLY sealed material; no plaintext column exists by design and the
-- metadata projections never select the encrypted columns:
--   ciphertext  BYTEA   - AES-256-GCM output including the 16-byte auth tag
--                         (stdlib crypto/aes + crypto/cipher in Go)
--   nonce       BYTEA   - the per-secret random 12-byte GCM nonce
--   key_version INTEGER - which master key (AGENTOS_SECRETS_MASTER_KEY,
--                         base64 of 32 bytes) encrypted the payload. The v1
--                         envelope "v1:<keyVersion>:<b64(nonce)>:<b64(ct)>"
--                         maps 1:1 onto (key_version, nonce, ciphertext), so
--                         key rotation only registers a new version number and
--                         old rows stay readable without a rewrite.
--
-- Soft delete: deleted_at is the tombstone (NULL = live). The uniqueness
-- contract is a PARTIAL unique index over live rows (uq_secrets_org_name_live):
-- a full UNIQUE(organization_id, name) constraint would also cover tombstoned
-- rows and make a deleted name permanently un-recreatable, which breaks the
-- delete-then-recreate flow pinned by the service tests. A violating insert
-- surfaces as SQLSTATE 23505 and maps to ErrDuplicate in internal/secrets.
--
-- All statements are idempotent (CREATE TABLE IF NOT EXISTS plus DO-block
-- guards for the index, Postgres has no CREATE INDEX IF NOT EXISTS for partial
-- predicates pre-9.5 style parity with migrations 006-016), so cmd/migrate can
-- be re-run safely.
--
-- NOTE: migration 016 is reserved by the billing track; this file jumps to 017.

CREATE TABLE IF NOT EXISTS secrets (
    organization_id TEXT NOT NULL,
    name TEXT NOT NULL,
    ciphertext BYTEA NOT NULL,
    nonce BYTEA NOT NULL,
    key_version INTEGER NOT NULL DEFAULT 1,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,
    CONSTRAINT fk_secrets_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Live-row uniqueness for (organization_id, name): tombstoned rows are exempt
-- so a soft-deleted name can be recreated fresh. Guarded DO block keeps the
-- migration re-runnable.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'uq_secrets_org_name_live') THEN
        CREATE UNIQUE INDEX uq_secrets_org_name_live
            ON secrets(organization_id, name)
            WHERE deleted_at IS NULL;
    END IF;
END $$;

-- Tenant-scoped index: every hot query (list/get/resolve) filters on
-- organization_id (and name for point lookups).
CREATE INDEX IF NOT EXISTS idx_secrets_org_name ON secrets(organization_id, name);
