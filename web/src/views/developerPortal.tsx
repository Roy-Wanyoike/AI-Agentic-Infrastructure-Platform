// Developer portal view (issue #119): the dashboard's on-ramp to the
// self-service portal served from /portal/ (docs/portal — static, generated
// reference data + playground + SDK/CLI guides).
//
// Deliberately minimal per the issue: the quickstart is inlined here so a
// developer can go from zero to a run without leaving the dashboard, and the
// full portal (API reference browser, playground, schema index, changelog)
// is linked out. Endpoint paths below are quoted from api/openapi.yaml —
// nothing invented: register/login (auth group), POST /agents/create,
// POST /runs, GET /runs/{id}/events.

import { EmptyState, PageHeader, SummaryStat } from './shared'

const PORTAL_HREF = '/portal/'

const QUICKSTART: { step: string; method: string; path: string; note: string }[] = [
  {
    step: 'Register the first OWNER user',
    method: 'POST',
    path: '/auth/register',
    note: '{"organization":"Acme AI","email":"owner@acme.dev","password":"…"} — creates the org and its first OWNER user.',
  },
  {
    step: 'Exchange credentials for a bearer token',
    method: 'POST',
    path: '/auth/login',
    note: '24h HMAC token → Authorization: Bearer <token>. Self-service API keys (X-API-Key) are minted at POST /api-keys.',
  },
  {
    step: 'Create an agent',
    method: 'POST',
    path: '/agents/create',
    note: 'name, instructions and model are required (server-validated, 400 otherwise); the agent starts DRAFT with version 1.',
  },
  {
    step: 'Create and enqueue a run',
    method: 'POST',
    path: '/runs',
    note: '{"agent_id":"…","input":"…"} → 201 {"run_id","status":"queued"}; a worker executes the run.',
  },
  {
    step: 'Stream execution events',
    method: 'GET',
    path: '/runs/{id}/events',
    note: 'text/event-stream until a terminal status; GET /runs/{id}/steps returns the per-step timeline.',
  },
]

const CLI_STEPS = [
  'agentosctl register -org "Acme AI" -email owner@acme.dev -password s3cret',
  'agentosctl login -email owner@acme.dev -password s3cret',
  'agentosctl agents create -name helper -instructions "be brief" -model gpt-4o-mini',
  'agentosctl agents run AGENT_ID "what is 2+2?"',
  'agentosctl runs watch RUN_ID',
]

export function DeveloperPortalView() {
  return (
    <>
      <PageHeader
        eyebrow="Developer portal"
        title="Build on the AgentOS API"
        actions={
          <a className="primary-button" href={PORTAL_HREF} target="_blank" rel="noreferrer">
            Open the full portal ↗
          </a>
        }
      />

      <p className="form-note">
        The full portal — generated API reference browser, in-browser playground, schema index and SDK/CLI guides — is
        a static page served from <code>/portal/</code> (sources in <code>docs/portal/</code>, zero external assets).
        The reference data is generated straight from <code>api/openapi.yaml</code> by{' '}
        <code>scripts/gen_portal_docs.py</code>: 179 paths · 236 operations · 496 schemas.
      </p>

      <div className="summary-grid" style={{ gridTemplateColumns: 'repeat(3, minmax(0, 1fr))' }}>
        <SummaryStat label="Portal" value="/portal/" accent="info" />
        <SummaryStat label="Quickstart" value="5 commands to a run" />
        <SummaryStat label="Playground" value="browser + CORS caveat" accent="warning" />
      </div>

      <section className="panel">
        <div className="panel-header">
          <div>
            <p className="eyebrow">5-minute quickstart</p>
            <h3>Register → create agent → run → watch</h3>
          </div>
        </div>
        <ol>
          {QUICKSTART.map((s) => (
            <li key={s.method + s.path} style={{ marginBottom: 10 }}>
              <code>
                {s.method} {s.path}
              </code>
              <p className="detail-copy">{s.note}</p>
            </li>
          ))}
        </ol>
        <p className="detail-copy">
          Serve the portal locally with <code>python3 -m http.server --directory docs/portal</code>, or copy it into
          the web public dir to host it at <code>/portal/</code> (see <code>docs/portal/README.md</code>). The
          playground issues real fetch() calls and needs CORS enabled server-side when hosted on a different origin —
          the portal states this honestly instead of pretending otherwise.
        </p>
      </section>

      <section className="panel">
        <div className="panel-header">
          <div>
            <p className="eyebrow">CLI quickstart</p>
            <h3>agentosctl</h3>
          </div>
        </div>
        <pre className="payload-block">
          {CLI_STEPS.join('\n')}
        </pre>
        <p className="detail-copy">
          SDKs: a stdlib-only typed Go client lives at <code>internal/sdk</code> (built on by{' '}
          <code>cmd/agentosctl</code>); Python/TypeScript clients are generated from the spec with openapi-generator —
          recipes in the portal's SDK section. Generated clients are documented, never vendored.
        </p>
      </section>

      <EmptyState
        title="Everything is offline-first"
        hint="The portal ships zero external assets and renders its reference from data generated out of the OpenAPI contract — regenerate api-data.js whenever the spec changes."
      />
    </>
  )
}
