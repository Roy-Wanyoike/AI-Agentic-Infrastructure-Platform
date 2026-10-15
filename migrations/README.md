# Migrations

This directory holds PostgreSQL migration files for the AgentOS schema.

Current migration structure:

- 001_init_schema.sql
- 002_auth_tables.sql
- 003_agents_tables.sql
- 004_runs_and_steps.sql
- 005_persistence_hardening.sql
- 006_workflows_approvals.sql
- 007_versions_deployments.sql
- 008_policies.sql
- 009_evaluations.sql
- 010_events_webhooks.sql
- 011_scheduler.sql
- 012_cost_tracking.sql
- 013_durable_workflows.sql
- 014_memory_knowledge.sql
- 015_canary_deployments.sql
- 016_billing.sql
- 017_secrets.sql
- 018_marketplace.sql
- 019_sso_scim.sql
- 020_connectors.sql
- 021_canary_policy.sql
- 022_marketplace_signing.sql
- 023_auth_lifecycle.sql (issue #76)
- 024_notifications_subscriptions.sql (issue #83)
- 025_mcp_registry.sql (issue #82)
- 027_agent_api_keys.sql (wave-8: agent-scoped API keys with tool allow-lists)
- 028_a2a_agent_cards.sql (issue #136: A2A Agent Card registry — org-scoped publications of org agents)
- 029_approval_oversight.sql (issue #137: approvals enterprise HITL v2 — delegation chain, escalation SLA policy, dual-control columns, oversight event log)
- 030_virtual_keys.sql (wave-9 issue #139: gateway virtual keys with model allow-lists + enforced monthly spend caps)
- 031_residency_policies.sql (issue #140: org data-residency policies + out-of-region evidence)
- 033_pgvector.sql (issue #113: pgvector for knowledge_chunks — embedding_vec vector(1536) + HNSW cosine index; PGVECTOR-OPTIONAL: a pg_available_extensions guard keeps it a no-op NOTICE on plain Postgres)
- 036_rate_limits.sql (issue #124: granular per-scope rate limits — rate_limits(organization_id, scope, NULLABLE subject_id, rpm, rpd, tpm, enabled); uniqueness via the expression index ux_rate_limits_subject (org, scope, COALESCE(subject_id,'')) — the functional PK over a nullable column; subject-specific > org default > unlimited)
- 037_run_checkpoints.sql (issue #115: durable run checkpoints — run_checkpoints keyed PK (run_id, step_index) so end-of-step snapshot saves are idempotent replays; run_id FK cascades on run delete; created_at doubles as the recovery sweep's liveness heartbeat, idx_run_checkpoints_created walks it by recency)

Migration conventions:

- keep each migration idempotent with CREATE TABLE IF NOT EXISTS and CREATE INDEX IF NOT EXISTS
- keep migrations backward-safe and additive where possible
- prefer small, ordered schema changes per version
- use the migration runner in the repository root to apply them in order

Migrations are discovered automatically by filename (`NNN_name.sql`, see
`internal/database.LoadMigrations`); the data-driven `TestApplyMigrations` in
`internal/database/database_test.go` applies every file it finds, so adding a
file requires no registration anywhere else.

A migration tool such as goose or migrate can be adopted later, but the SQL files in this directory remain the source of truth for local and CI schema evolution.
- 034_synth_registry.sql (issue #147 flagship #118: synthetic assurance registry — projects/targets/environments/personas/journeys/runs/findings, org-scoped with safety-posture constraints)
