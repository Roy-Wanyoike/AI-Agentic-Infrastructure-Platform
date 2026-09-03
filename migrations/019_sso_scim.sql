-- 019_sso_scim.sql
--
-- Issue #29: SSO (OIDC) login + SCIM 2.0 user provisioning.
--
-- Scope of this migration (all statements idempotent, mirrors 017 style):
--   1. organizations.sso_config JSONB  - per-tenant OIDC identity-provider
--        configuration (issuer, client_id, client_secret_ref, default_role,
--        scopes, optional redirect_uri). NULL means "SSO not configured".
--      SECRET HANDLING (documented choice): the client secret is stored by
--        REFERENCE, not by value. The JSONB carries client_secret_ref, the
--        NAME of a row in the existing secrets store (migration 017), and
--        internal/sso resolves it through the secrets Resolve(orgID, name)
--        seam at token-exchange time. This reuses the established
--        AES-256-GCM envelope PATTERN without duplicating crypto or key
--        management in this migration; a plaintext client_secret key is
--        accepted by the in-memory/dev config path only and must never be
--        written to Postgres. If an encrypted column is ever preferred, the
--        same v1:<keyVersion>:<b64(nonce)>:<b64(ct)> envelope layout applies.
--   2. users.sso_subject TEXT NULL     - the IdP's stable subject identifier
--        (the `sub` claim) once a user has logged in via SSO or been linked.
--        NULL = purely local identity. Uniqueness is a PARTIAL unique index
--        restricted to non-NULL rows (many local users have no subject; a
--        plain UNIQUE would collapse them into one).
--   3. users.active BOOLEAN NOT NULL DEFAULT TRUE - SCIM 2.0 lifecycle flag.
--        SCIM PATCH replace active=false (deprovisioning) sets it to FALSE
--        and LoginCtx rejects password logins for disabled accounts. TRUE
--        for all pre-existing rows (backfill via DEFAULT).
--
-- NOTE on table naming: the issue text says "auth_users.sso_subject"; the
-- platform's auth user-identity table is `users` (migration 001, globally
-- UNIQUE email). The columns are applied there.
--
--   4. scim_tokens                     - bearer credentials for the SCIM 2.0
--        API, minted by POST /v1/scim/tokens (OWNER-only). token_hash stores
--        SHA-256 hex of the secret exactly like api_keys.key_hash is checked
--        (internal/apikeys hashAPIKey); the plaintext is shown once at
--        creation and never persisted. UNIQUE(token_hash) because a presented
--        credential must resolve to exactly one tenant row.
--
-- All statements re-runnable: ADD COLUMN IF NOT EXISTS, CREATE TABLE IF NOT
-- EXISTS, DO-block guarded unique index (CREATE INDEX inside a guarded block
-- keeps parity with migrations 006-018 style), CREATE INDEX IF NOT EXISTS.

ALTER TABLE organizations ADD COLUMN IF NOT EXISTS sso_config JSONB;

ALTER TABLE users ADD COLUMN IF NOT EXISTS sso_subject TEXT;

ALTER TABLE users ADD COLUMN IF NOT EXISTS active BOOLEAN NOT NULL DEFAULT TRUE;

-- One IdP subject can map to at most one user; local users (NULL) are exempt.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'uq_users_sso_subject') THEN
        CREATE UNIQUE INDEX uq_users_sso_subject
            ON users(sso_subject)
            WHERE sso_subject IS NOT NULL;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS scim_tokens (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    CONSTRAINT fk_scim_tokens_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_scim_tokens_org_id ON scim_tokens(organization_id);
