# Migrations

This directory will hold PostgreSQL migration files for the AgentOS schema.

Planned structure:

- 001_init_schema.sql
- 002_auth_tables.sql
- 003_agents_tables.sql
- 004_runs_and_steps.sql

Keep migration files idempotent and backward-safe. Use a migration tool such as goose or migrate in later phases.
