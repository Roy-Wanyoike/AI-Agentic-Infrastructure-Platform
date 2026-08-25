# Migrations

This directory holds PostgreSQL migration files for the AgentOS schema.

Current migration structure:

- 001_init_schema.sql
- 002_auth_tables.sql
- 003_agents_tables.sql
- 004_runs_and_steps.sql

Migration conventions:

- keep each migration idempotent with CREATE TABLE IF NOT EXISTS and CREATE INDEX IF NOT EXISTS
- keep migrations backward-safe and additive where possible
- prefer small, ordered schema changes per version
- use the migration runner in the repository root to apply them in order

A migration tool such as goose or migrate can be adopted later, but the SQL files in this directory remain the source of truth for local and CI schema evolution.
