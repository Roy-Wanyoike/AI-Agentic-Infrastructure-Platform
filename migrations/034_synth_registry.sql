-- 034_synth_registry.sql — issue #147 (flagship #118): synthetic assurance
-- registry — projects, targets (with ownership verification), environments
-- (safety posture), personas, journeys (versioned specs), runs, findings.
--
-- IDEMPOTENT: safe to re-apply. All tables org-scoped (tenant isolation).
-- Later synth tracks (#148-#156) add their own tables/columns via SEPARATE
-- pre-assigned migrations — this file owns the registry vocabulary only.

CREATE TABLE IF NOT EXISTS synth_projects (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_synth_projects_org ON synth_projects(org_id);

CREATE TABLE IF NOT EXISTS synth_targets (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id   TEXT NOT NULL REFERENCES synth_projects(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    base_url     TEXT NOT NULL,
    verified     BOOLEAN NOT NULL DEFAULT FALSE,
    verified_at  TIMESTAMPTZ,
    verify_token TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_synth_targets_org_project ON synth_targets(org_id, project_id);

CREATE TABLE IF NOT EXISTS synth_environments (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    target_id       TEXT NOT NULL REFERENCES synth_targets(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('local','dev','staging','production')),
    mode            TEXT NOT NULL CHECK (mode IN ('pre_production','production_read_only','release_comparison')),
    allowed_domains JSONB NOT NULL DEFAULT '[]',
    rate_cap_per_min INTEGER NOT NULL DEFAULT 0,
    budget_per_run  INTEGER NOT NULL DEFAULT 0,
    quiet_windows   JSONB NOT NULL DEFAULT '[]',
    test_windows    JSONB NOT NULL DEFAULT '[]',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_synth_env_target ON synth_environments(org_id, target_id);

CREATE TABLE IF NOT EXISTS synth_personas (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    built_in    TEXT NOT NULL DEFAULT '',
    role_hints  JSONB NOT NULL DEFAULT '[]',
    constraints JSONB NOT NULL DEFAULT '{}',
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_synth_personas_org ON synth_personas(org_id);

CREATE TABLE IF NOT EXISTS synth_journeys (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id     TEXT NOT NULL REFERENCES synth_projects(id) ON DELETE CASCADE,
    environment_id TEXT NOT NULL REFERENCES synth_environments(id) ON DELETE CASCADE,
    persona_id     TEXT NOT NULL REFERENCES synth_personas(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','reviewed','scheduled','retired')),
    risk           TEXT NOT NULL DEFAULT 'low' CHECK (risk IN ('low','medium','high','critical')),
    version        INTEGER NOT NULL DEFAULT 1,
    spec           JSONB NOT NULL DEFAULT '{}', -- preconditions/steps/assertions/cleanup/tags/source
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_synth_journeys_org_project ON synth_journeys(org_id, project_id);
CREATE INDEX IF NOT EXISTS idx_synth_journeys_status ON synth_journeys(org_id, status);

CREATE TABLE IF NOT EXISTS synth_runs (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id     TEXT NOT NULL REFERENCES synth_projects(id) ON DELETE CASCADE,
    journey_id     TEXT NOT NULL REFERENCES synth_journeys(id) ON DELETE CASCADE,
    journey_ver    INTEGER NOT NULL DEFAULT 1,
    environment_id TEXT NOT NULL REFERENCES synth_environments(id) ON DELETE CASCADE,
    persona_id     TEXT NOT NULL REFERENCES synth_personas(id) ON DELETE CASCADE,
    persona_ver    INTEGER NOT NULL DEFAULT 1,
    kind           TEXT NOT NULL CHECK (kind IN ('browser','api')),
    status         TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','queued','running','paused','cancelling','completed','failed','cancelled','inconclusive')),
    app_version    TEXT NOT NULL DEFAULT '',
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    step_events    JSONB NOT NULL DEFAULT '[]',
    evidence_refs  JSONB NOT NULL DEFAULT '[]',
    error          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_synth_runs_org_status ON synth_runs(org_id, status);
CREATE INDEX IF NOT EXISTS idx_synth_runs_journey ON synth_runs(org_id, journey_id);

CREATE TABLE IF NOT EXISTS synth_findings (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    run_id        TEXT NOT NULL REFERENCES synth_runs(id) ON DELETE CASCADE,
    journey_id    TEXT NOT NULL REFERENCES synth_journeys(id) ON DELETE CASCADE,
    assertion_ix  INTEGER NOT NULL DEFAULT -1,
    status        TEXT NOT NULL DEFAULT 'inconclusive' CHECK (status IN ('confirmed','likely','flaky','harness_failure','environmental_failure','inconclusive')),
    severity      TEXT NOT NULL DEFAULT 'medium' CHECK (severity IN ('info','low','medium','high','critical')),
    title         TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL DEFAULT '',
    fingerprint   TEXT NOT NULL,
    repro_steps   TEXT NOT NULL DEFAULT '',
    evidence_refs JSONB NOT NULL DEFAULT '[]',
    repro_run_id  TEXT,
    reproducible  BOOLEAN,
    confidence    REAL NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_synth_findings_fp ON synth_findings(org_id, fingerprint, run_id);
CREATE INDEX IF NOT EXISTS idx_synth_findings_org_status ON synth_findings(org_id, status);
