# Security Policy

AgentOS is a multi-tenant platform that stores secrets, API keys, and tenant data. Reports about tenant isolation, secret handling, or auth bypass are treated as critical.

## Supported versions

| Version | Supported |
|---|---|
| `main` | ✅ |

The project ships from `main` only — there are no long-lived release branches. Always run the latest `main` (or a release cut from it) before reporting an issue.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

1. Open the repository's **Security** tab.
2. Click **"Report a vulnerability"** — this creates a private GitHub Security Advisory visible only to you and the maintainers.
3. Include:
   - affected surface (endpoint, CLI command, or component),
   - minimal reproduction steps,
   - mode you ran in (Postgres or in-memory),
   - your assessment of impact/exploitability,
   - relevant logs or screenshots (redact credentials).

## Response SLA

| Stage | Target |
|---|---|
| Acknowledgement | **72 hours** |
| Triage (severity, exploitability, fix plan) | **7 days** |
| Fix / mitigation | P0: immediate · P1: next PR cycle · P2: backlog |

## Scope

**In scope:**

- **API server** (`cmd/api`) — authn/authz, RBAC, tenant isolation, every `/v1` and `/api/v1` route
- **Worker** (`cmd/worker`) — run execution, scheduler loop, webhook delivery
- **sandbox-exec** (`cmd/sandbox-exec` + `internal/sandbox`) — process-isolated tool execution: rlimits, output caps, env scrubbing
- **Secrets management** (`internal/secrets`) — encryption at rest, key-versioned envelopes, one-time reveal
- **SSO / SCIM** (`internal/sso`, `internal/scim`) — OIDC login, SCIM 2.0 provisioning, token handling
- **CLI** (`cmd/agentosctl`) — credential storage, token handling

**Out of scope:** denial-of-service by volume, issues requiring access to the deployment host, and reports from accounts that are already compromised.

## Hardening guarantees (shipped)

- **Secrets**: AES-256-GCM encryption at rest with key-versioned envelopes; plaintext is never persisted; one-time reveal is OWNER-gated and audit-logged.
- **API keys**: stored as hashes only — plaintext is shown once at creation and is unrecoverable afterwards.
- **SCIM bearer plane separation**: SCIM provisioning tokens are hashed and authenticated on their own middleware plane, separate from user sessions and API keys — a leaked provisioning token cannot be replayed against user routes.
- **Baseline**: bcrypt password hashing, HMAC-signed session tokens, every Postgres query carries an `organization_id` guard, and tenant identity always comes from signed claims — never from client payloads.
