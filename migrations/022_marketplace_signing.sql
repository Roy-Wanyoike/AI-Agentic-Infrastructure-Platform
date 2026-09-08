-- 022_marketplace_signing.sql
--
-- Issue #78: marketplace publisher provenance — Ed25519 publisher keys,
-- detached signatures over the canonical snapshot manifest, tamper evidence
-- and the org-level allow_unsigned install policy.
--
-- publisher_keys — one row per registered publisher public key. The PRIVATE
-- half never touches the server: only the base64 (std) raw 32-byte
-- ed25519.PublicKey is stored.
--   public_key  TEXT - base64 (std) of the raw 32-byte Ed25519 public key;
--                  validated by the service (internal/marketplace).
--   alg         TEXT - 'ed25519' (CHECK-constrained; the only supported
--                  algorithm).
--   status      TEXT - 'active' | 'revoked' (CHECK). Revocation blocks future
--                  installs of listings signed with the key; agents already
--                  installed stay untouched.
--   revoked_at  TIMESTAMPTZ - stamped by the FIRST revoke (COALESCE keeps it
--                  across idempotent re-revokes).
--   Uniqueness: UNIQUE(organization_id, public_key) — an org cannot register
--   the same key twice (SQLSTATE 23505 -> ErrDuplicateKey).
--
-- marketplace_listings — additive signature columns (issue #78):
--   signature      TEXT - base64 (std) detached Ed25519 signature over the
--                   canonical manifest (deterministic JSON: sorted keys, no
--                   HTML escaping, no insignificant whitespace — pinned by the
--                   golden test in internal/marketplace/signing_test.go).
--   signing_key_id TEXT - publisher_keys.id of the signing key. Provenance
--                   metadata WITHOUT a foreign key by design (migration 018
--                   source_agent_id precedent): revocation is the governance
--                   switch, never a hard delete, so no dangling reference can
--                   occur and deleting an org must cascade its keys cleanly.
--   sig_alg        TEXT - 'ed25519' for signed listings.
--   signed_at      TIMESTAMPTZ - server clock at publish-time verification.
--   manifest_hash  TEXT - sha256 over the canonical manifest bytes, lowercase
--                   hex (content address; ANY post-publish snapshot mutation
--                   invalidates the listing at install time).
--   legacy_listing BOOLEAN NOT NULL DEFAULT TRUE - pre-signing listings are
--                   GRANDFATHERED: every row that exists before this migration
--                   reads TRUE and stays installable without a signature; the
--                   service writes FALSE for every new listing, which is
--                   therefore subject to signature verification / the
--                   allow_unsigned policy.
--   A signed row always carries ALL of signature+signing_key_id+sig_alg+
--   signed_at+manifest_hash; an unsigned NEW row carries none of them (the
--   service writes all-or-nothing).
--
-- marketplace_org_settings — one row per org (created on first write):
--   allow_unsigned BOOLEAN NOT NULL DEFAULT FALSE - the org's INSTALL policy
--                   for unsigned NEW listings. FALSE (default) = verify:
--                   unsigned new listings are blocked (unsigned_listing).
--                   Legacy (pre-signing) listings are always installable
--                   regardless of this flag. A missing row IS the default
--                   policy, never an error.
--
-- All statements are idempotent (CREATE TABLE IF NOT EXISTS,
-- ADD COLUMN IF NOT EXISTS, migrations 006-021 pattern) so cmd/migrate can be
-- re-run safely.

CREATE TABLE IF NOT EXISTS publisher_keys (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    public_key TEXT NOT NULL,
    alg TEXT NOT NULL DEFAULT 'ed25519' CHECK (alg IN ('ed25519')),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    CONSTRAINT uq_publisher_keys_org_public_key UNIQUE (organization_id, public_key),
    CONSTRAINT fk_publisher_keys_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);

-- Org-scoped key listing (management UI, key inventory).
CREATE INDEX IF NOT EXISTS idx_publisher_keys_org ON publisher_keys(organization_id, created_at);

ALTER TABLE marketplace_listings ADD COLUMN IF NOT EXISTS signature TEXT;
ALTER TABLE marketplace_listings ADD COLUMN IF NOT EXISTS signing_key_id TEXT;
ALTER TABLE marketplace_listings ADD COLUMN IF NOT EXISTS sig_alg TEXT;
ALTER TABLE marketplace_listings ADD COLUMN IF NOT EXISTS signed_at TIMESTAMPTZ;
ALTER TABLE marketplace_listings ADD COLUMN IF NOT EXISTS manifest_hash TEXT;
ALTER TABLE marketplace_listings ADD COLUMN IF NOT EXISTS legacy_listing BOOLEAN NOT NULL DEFAULT TRUE;

-- Signed-listing key attribution lookups (provenance joins, forensics).
CREATE INDEX IF NOT EXISTS idx_marketplace_listings_signing_key
    ON marketplace_listings(signing_key_id) WHERE signing_key_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS marketplace_org_settings (
    organization_id TEXT PRIMARY KEY,
    allow_unsigned BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_marketplace_org_settings_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);
