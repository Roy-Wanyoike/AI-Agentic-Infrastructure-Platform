-- 023_auth_lifecycle.sql
--
-- Issue #76: server-side token revocation (logout / refresh rotation).
--
--   auth_revocations - the durable deny list consulted by auth.ValidateToken
--     after the signature check. One row per revoked token:
--
--       key         TEXT PRIMARY KEY
--                   The token's revocation identity (auth.TokenReference):
--                   "jti:<jti>" for tokens minted after issue #76, or
--                   "tok:<sha256 hex of the full token string>" for legacy
--                   tokens without a jti claim. The derivation is pure, so
--                   every later presentation of the same token resolves the
--                   same key.
--       expires_at  TIMESTAMPTZ NOT NULL
--                   Revoked tokens are only denied until their natural
--                   expiry: the API writes (remaining lifetime) as the TTL
--                   and the lookup filters on expires_at > NOW() (lazy
--                   expiry), so the deny list never grows beyond the set of
--                   tokens revoked but not yet naturally expired.
--       revoked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
--                   Operational timestamp of the logout/rotation.
--
-- Additive and idempotent (IF NOT EXISTS everywhere): legacy tokens keep
-- validating unchanged, and before this migration runs the API degrades to
-- its in-memory deny list (single-replica revocation) instead of failing.
-- The expires_at index serves both the lazy-expiry lookup and the
-- opportunistic sweep of dead rows the API runs alongside every revoke.
-- Rows are intentionally NOT deleted on read: expiry is a query predicate,
-- so a revoked token can never be resurrected by a lost delete.

CREATE TABLE IF NOT EXISTS auth_revocations (
    key TEXT PRIMARY KEY,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_auth_revocations_expires_at ON auth_revocations (expires_at);
