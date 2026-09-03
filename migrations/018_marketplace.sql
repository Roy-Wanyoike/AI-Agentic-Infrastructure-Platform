-- 018_marketplace.sql
--
-- Issue #28: agent marketplace — publish, discover and install agent
-- templates across organizations.
--
-- One row per marketplace listing. A listing is a VETTED TEMPLATE distilled
-- from an existing agent: version_snapshot JSONB is a self-contained copy of
-- the agent's configuration at publish time (the same config-only document
-- shape the wave-2 immutable config versions store — see
-- internal/agents/versions.go AgentSnapshot). The snapshot is denormalized on
-- purpose:
--   * installing NEVER reads the source agent or its versions again, so a
--     listing keeps working after the source agent is edited or deleted;
--   * source_agent_id is provenance metadata WITHOUT a foreign key by design:
--     deleting the source agent must not cascade-delete a public catalog
--     entry (ON DELETE SET NULL would force a nullable column; ON DELETE
--     CASCADE would silently remove published listings).
-- publisher_org_id keeps a real FK so tenant deletion cleans up its listings.
--
--   status          draft|published|unlisted (service-enforced transitions;
--                   only published listings appear in the global catalog and
--                   are installable; draft/unlisted stay visible to their
--                   publisher org via point lookups)
--   slug            GLOBAL UNIQUE across every organization and status —
--                   the catalog is global-read, so slugs are a global
--                   namespace (conflicts surface as SQLSTATE 23505 and map to
--                   ErrDuplicateSlug -> HTTP 409 in internal/marketplace).
--   tags            TEXT[] for the browse tag filter (overlap query `&&`);
--                   the GIN index keeps array-overlap lookups indexed.
--   download_count  cumulative installs; bumped by the install path with
--                   UPDATE ... RETURNING (no read-modify-write race).
--
-- Snapshots contain CONFIG ONLY (name/description/instructions/model/status)
-- and never secrets: the marketplace service builds the snapshot exclusively
-- from the agents domain (live config or an immutable ConfigVersion), the
-- publish request body never carries snapshot JSON, and the install path
-- only ever feeds the four config fields to the agents service.
--
-- All statements are idempotent (CREATE TABLE IF NOT EXISTS plus DO-block
-- guards, migrations 006-017 pattern) so cmd/migrate can be re-run safely.

CREATE TABLE IF NOT EXISTS marketplace_listings (
    id TEXT PRIMARY KEY,
    publisher_org_id TEXT NOT NULL,
    publisher_user_id TEXT NOT NULL,
    source_agent_id TEXT NOT NULL,
    version_snapshot JSONB NOT NULL,
    name TEXT NOT NULL,
    slug TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    tags TEXT[] NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'draft',
    download_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_marketplace_listings_org FOREIGN KEY (publisher_org_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT ck_marketplace_listings_status CHECK (status IN ('draft', 'published', 'unlisted')),
    CONSTRAINT ck_marketplace_listings_downloads CHECK (download_count >= 0)
);

-- Global catalog browse: newest-first keyset pagination over published rows
-- (created_at DESC, id DESC — matches the service/store ordering exactly, so
-- the index also serves the keyset predicate).
CREATE INDEX IF NOT EXISTS idx_marketplace_listings_browse
    ON marketplace_listings(status, created_at DESC, id DESC);

-- Publisher-scoped lookups ("our listings") and org cleanup on tenant delete.
CREATE INDEX IF NOT EXISTS idx_marketplace_listings_publisher
    ON marketplace_listings(publisher_org_id, created_at DESC);

-- Tag-overlap filter (tags && $n) for the browse/search path.
CREATE INDEX IF NOT EXISTS idx_marketplace_listings_tags
    ON marketplace_listings USING GIN (tags);
