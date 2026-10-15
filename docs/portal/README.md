# AgentOS Developer Portal

A self-service, fully static developer portal for the AgentOS API: generated
API reference, in-browser playground, SDK/CLI guides, and quickstart
tutorials. **No build step, no external assets** — it works offline from any
static file server.

| File | Role |
|---|---|
| `index.html` | The portal page. Self-contained CSS + vanilla JS; loads only `./api-data.js`. |
| `api-data.js` | **Generated** from `api/openapi.yaml` by the script below. Do not edit by hand. |

## Regenerating the API data

```bash
python3 scripts/gen_portal_docs.py
# → docs/portal/api-data.js (paths, operations, params, permissions,
#   request/response schema names — counts are quoted verbatim from the spec)
```

The generator is Python **stdlib-only** (it embeds a small YAML-subset parser
— no PyYAML dependency). Re-run it after every change to `api/openapi.yaml`
and commit the refreshed `api-data.js` together with the spec change. The
current output covers **179 paths, 236 operations, 496 schemas** in 44
tag/domain groups.

## Hosting

The portal is two static files. Any of these works:

### 1. Any static file server

```bash
# Caddy
caddy file_server --root docs/portal --listen :8090

# nginx
#   location /portal/ { alias /srv/agentos/docs/portal/; index index.html; }

# quick local preview
python3 -m http.server 8090 --directory docs/portal
```

### 2. Ship it inside the dashboard build (same origin as the web app)

Copy the directory into the web app's public dir before building — Vite
copies `web/public/**` verbatim into `dist/`:

```bash
cp -r docs/portal web/public/portal
npm --prefix web run build   # → web/dist/portal/index.html (served at /portal/)
```

Do this in CI or a Makefile target at deploy time (the copy is not checked
into `web/public/`); regenerate after spec changes. Because the portal then
shares the web app's origin, the playground can call the API whenever the
reverse proxy in front of both forwards `/api/v1` — no CORS needed.

### 3. Behind the API's own proxy

If you front the Go API with nginx/Caddy, serve the portal from the same
origin and proxy `/api/v1` next to it:

```nginx
server {
  listen 443 ssl;
  root /srv/agentos/portal;        # docs/portal content
  location /api/v1/ { proxy_pass http://127.0.0.1:8080/api/v1/; }
}
```

## Playground & CORS (read this)

The in-browser playground issues real `fetch()` calls against the base URL
you configure (persisted in `localStorage`, credential fields included —
stored only in your browser). Browsers enforce the same-origin policy, so:

- **Same origin** (option 2/3 above): works out of the box.
- **Different origin**: the API must return
  `Access-Control-Allow-Origin` for the portal's origin and include
  `Authorization`, `X-API-Key`, and `Content-Type` in
  `Access-Control-Allow-Headers`. The Go API does not enable cross-origin
  browser access by default; without it the playground will report a CORS
  failure (the page says so honestly). Use `agentosctl`/`curl` or a
  same-origin proxy in that case.

## Auth quick facts

- Bearer: `Authorization: Bearer <token>` — token from `POST /api/v1/auth/login`
  (register creates the first OWNER user).
- API key: `X-API-Key: ak_…` — self-service keys (`POST /api/v1/api-keys`)
  or agent-scoped keys with tool allow-lists (`POST /api/v1/agents/{id}/keys`).
- Browser `EventSource` (SSE) clients that cannot set headers may pass
  `?api_key=<key>` (legacy compatibility, documented in the spec).
