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
