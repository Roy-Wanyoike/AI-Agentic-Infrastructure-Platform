# Wiring — Track 3-e (Agent version diff + frontend views)

The diff endpoint lives entirely inside files track 3-e owns, so
**`cmd/api/main.go` needs NO edit**: the route is registered from inside the
existing `registerVersionsRoutes`, which `main.go` already calls at
`cmd/api/main.go:205` (line unchanged, shown for orientation only).

## 1. Files added/changed by this track

| File | Purpose |
|------|---------|
| `internal/agents/versions.go` | + `DiffVersionsCtx` service method, `VersionDiff`/`VersionDiffField` types, `comparableSnapshotFields` table. Existing 2-b methods untouched. |
| `cmd/api/versions.go` | + `versionDiffHandler` + route line inside `registerVersionsRoutes`. All other 2-b handlers untouched. |
| `cmd/api/versions_test.go`, `internal/agents/versions_test.go` | + table-driven diff tests (handler + service). |
| `api/fragments/versions-diff.yaml` | Standalone OpenAPI 3.1 fragment, components prefixed `Vdf*`, `x-required-permission: agents.read`, all local `$ref`s resolve inside the file. |
| `web/src/lib/api/versions.ts` (+ `policies/schedules/webhooks.ts`) | Typed fetchers + normalizers (real endpoints only, no mocks). |
| `web/src/lib/hooks.ts` | React Query hooks (`useVersionDiff`, `useAgentVersions`, `useDeployments`, `usePolicies`, `useSchedules`, `useWebhooks`, …). |
| `web/src/views/{versions,policies,schedules,webhooks}.tsx` | The four new dashboard views (versions view includes the side-by-side diff viewer). |
| `web/src/lib/rbac.ts`, `web/src/App.tsx`, `web/src/views/uiHelpers.ts`, `web/src/App.css` | Nav entries + RBAC capability props (same pattern as wave-2 2-g) + diff/decision/secret-banner styles. |

## 2. Route registration lines

### BEFORE (wave-2 2-b, `cmd/api/versions.go` `registerVersionsRoutes`)

```go
apiMux.Handle("GET /agents/{id}/versions", wrap(auth.PermissionAgentsRead, listAgentVersionsHandler(versionsSvc)))
apiMux.Handle("POST /agents/{id}/versions/create", wrap(auth.PermissionAgentsWrite, createAgentVersionHandler(versionsSvc)))
apiMux.Handle("POST /agents/{id}/versions/{version}/publish", wrap(auth.PermissionAgentsWrite, publishAgentVersionHandler(versionsSvc)))
apiMux.Handle("POST /agents/{id}/rollback", wrap(auth.PermissionAgentsWrite, rollbackAgentHandler(versionsSvc)))
```

### AFTER (3-e adds exactly one line)

```go
apiMux.Handle("GET /agents/{id}/versions", wrap(auth.PermissionAgentsRead, listAgentVersionsHandler(versionsSvc)))
// Wave-3 track 3-e: field-level diff (agents.read — a read-only view).
apiMux.Handle("GET /agents/{id}/versions/diff", wrap(auth.PermissionAgentsRead, versionDiffHandler(versionsSvc)))
apiMux.Handle("POST /agents/{id}/versions/create", wrap(auth.PermissionAgentsWrite, createAgentVersionHandler(versionsSvc)))
apiMux.Handle("POST /agents/{id}/versions/{version}/publish", wrap(auth.PermissionAgentsWrite, publishAgentVersionHandler(versionsSvc)))
apiMux.Handle("POST /agents/{id}/rollback", wrap(auth.PermissionAgentsWrite, rollbackAgentHandler(versionsSvc)))
```

Nothing to add to `routes()` in `main.go` — the existing
`registerVersionsRoutes(apiMux, a.versionsSvc, a.authSvc, a.apiKeysSvc)` line
mounts the new route with the rest. Served under BOTH `/v1` and `/api/v1`.
(Registration order is irrelevant: Go 1.22+ ServeMux prefers the most specific
pattern over the `/agents/` catch-all.)

## 3. Endpoint contract

`GET /agents/{id}/versions/diff?from={n}&to={m}` — permission `agents.read`
(all roles; auth failures keep the middleware's plain-text 401/403).

Response body IS the diff document (snake_case per the wave-3 contract):

```json
{
  "agent_id": "…",
  "from": 2,
  "to": 3,
  "fields": [
    {"field": "model",         "from": "gpt-4o",      "to": "gpt-4o-mini", "changed": true},
    {"field": "system_prompt", "from": "…",           "to": "…",           "changed": true},
    {"field": "temperature",   "from": null,          "to": null,          "changed": false},
    {"field": "params",        "from": null,          "to": null,          "changed": false},
    {"field": "tools",         "from": ["a"],         "to": ["a","b"],     "changed": true},
    {"field": "description",   "from": "…",           "to": "…",           "changed": true}
  ]
}
```

Frontend notes (what the dashboard relies on):

- One entry per comparable field ALWAYS, in stable order
  (model, system_prompt, temperature, params, tools, description); `from`/`to`
  are the raw JSON values and are `null` when that side's snapshot lacks the
  field — the UI renders `—` for null and `JSON.stringify` for arrays/objects.
- `system_prompt` compares the legacy `instructions` snapshot key against
  `system_prompt` (first present key wins per side) so old snapshots stay
  diffable; `temperature`/`params`/`tools` are null on today's snapshots until
  the snapshot schema grows those fields.
- Extra snapshot keys (e.g. `name`, `status` today) are appended SORTED so the
  viewer never hides a recorded change; treat unknown field names as display
  strings, not enum members.
- `from`/`to` may be EQUAL (same-version diff → every field `changed: false`);
  the UI defaults the selectors to the two most recent versions.
- Errors use `{"error":{"code","message"}}`: 400 `INVALID_REQUEST` (missing /
  non-integer / non-positive `from`/`to`), 404 `AGENT_NOT_FOUND` /
  `VERSION_NOT_FOUND` (unknown version, cross-agent number mismatch, or
  foreign-tenant row — tenant scope always comes from auth claims), 500
  `INTERNAL_ERROR`. The UI surfaces 404 immediately (`retry: false`).

## 4. RBAC capability mapping (frontend)

Mirrors the API grants; the API still enforces real permissions (UI only hides
preemptively, per the wave-2 discipline):

| View | Props | Backend permission |
|---|---|---|
| Versions & deployments | `canManageVersions` / `canWrite` / `canDeploy` | `agents.write` (snapshot/publish/restore), `deployments.write` (request deployment), `deployments.deploy` (promote/rollback) |
| Policies | `canManage` | `policies.write` for create; list/evaluate stay `policies.read` (all roles) |
| Schedules | `canWrite` | `schedules.write` (create/pause/resume) |
| Webhooks | `canWrite` | `webhooks.write` (create/delete); list/deliveries stay `webhooks.read` |

`lib/rbac.ts` adds `canManageVersions`/`canDeploy`/`canManagePolicies`
(OWNER/ADMIN) alongside the existing `canWrite` (MEMBER+). Nav entries
(Versions, Policies, Schedules, Webhooks) are regular (non-demo) items — no
"Demo data" badge, honest empty states everywhere.

## 5. Fragment merge notes

`api/fragments/versions-diff.yaml` contributes:

- Path `GET /agents/{id}/versions/diff` (operationId `diffAgentVersions`,
  `x-required-permission: agents.read`).
- Components (all `Vdf*`-prefixed, no collisions with the merged 2-b
  fragment's unprefixed names): `VdfAgentID`, `VdfFromVersion`, `VdfToVersion`,
  `VdfUnauthorized`, `VdfForbidden`, `VdfDiffValue`, `VdfVersionDiffField`,
  `VdfVersionDiffResponse`, `VdfErrorEnvelope`.
- Security scheme names `bearerToken`/`apiKey` resolve against the main
  `openapi.yaml` `securitySchemes` (same convention as every other fragment).

## 6. Judgement calls / deviations (per contract "note it, don't deviate silently")

1. **Permission**: the contract pins no dedicated diff permission; diff is
   read-only agent configuration, so it reuses `agents.read` (consistent with
   2-b's documented `agents.read`/`agents.write` mapping).
2. **Cross-agent/cross-tenant both 404 `VERSION_NOT_FOUND`**: version numbers
   are only meaningful within one agent; foreign-tenant rows already surface as
   404 via the tenant guard, so the same envelope is used (no tenant-existence
   oracle).
3. **Diff includes extra snapshot keys** (sorted) beyond the contract's six
   comparable fields — additive only; consumers filtering by the contract field
   names see identical behavior.
4. **`changed` uses JSON value semantics** (DeepEqual on decoded values), so
   `["a","b"]` vs `["b","a"]` is `changed: true` (order matters — tools order
   is part of the config).
5. **Diff works on any version pair** (draft/published/archived alike): the
   contract says "between two published agent versions", but snapshots exist
   for drafts too and limiting to published would break diffing a draft against
   the last published version — the main UI use case before publishing.
6. **Webhook delete is exposed** (`DELETE /webhooks/{id}`) and surfaced in the
   webhooks view; it shipped with wave-2 2-e and the view treats it as part of
   the resource lifecycle.
7. **No migrations** in this track (diff is computed from existing
   `agent_versions.snapshot` documents; deployments/policies/schedules/
   webhooks views consume the wave-2 endpoints as-is).
