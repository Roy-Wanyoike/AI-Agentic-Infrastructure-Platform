// Identity admin (issue #81 Settings surface): SSO (OIDC) status + SCIM
// token mint/revoke. Backend contracts: cmd/api/sso.go, cmd/api/scim.go.
//
//   GET  /auth/sso/{org_slug}/login  -> 302 to the IdP when configured,
//                                       404 {"error":{code:ORG_NOT_FOUND|
//                                       SSO_NOT_CONFIGURED}} otherwise.
//                                       Unauthenticated by design; probed
//                                       (never followed) via probeApiEndpoint.
//   POST /scim/tokens                -> 201 {"token":{id,organization_id,
//                                       created_by,created_at},"secret":"scim_…"}
//                                       (organization.manage — OWNER only;
//                                       the plaintext secret appears ONCE)
//   DELETE /scim/tokens/{id}         -> 204 revoked | 404 unknown/foreign id
//                                       (parallel backend build, pinned by
//                                       issue #81: DELETE with 204/404)
//
// NOTE (honest limitation): the API exposes SCIM token MINT + REVOKE only —
// there is no list endpoint for issued tokens. The dashboard therefore keeps
// a session-local record of tokens minted in THIS browser session (real
// mint-response metadata, nothing invented) and offers revoke-by-ID for
// tokens minted elsewhere; it is documented in the view.

import { apiFetch, probeApiEndpoint, type ApiProbeResult } from './client'
import { asRecord, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type ScimTokenMetadata = {
  id: string
  organizationId: string
  createdBy: string
  createdAt?: string
}

/** POST /scim/tokens response — the ONLY shape that carries the scim_ secret. */
export type MintedScimToken = {
  token: ScimTokenMetadata
  secret: string
}

/**
 * SSO configured-state classification derived from the login-endpoint probe:
 * the backend's documented contract is 302 (configured) vs 404
 * ORG_NOT_FOUND / SSO_NOT_CONFIGURED (unauthenticated endpoint).
 */
export type SsoProbeOutcome =
  | { state: 'configured'; loginUrl: string }
  | { state: 'not-configured'; loginUrl: string }
  | { state: 'unknown-org'; loginUrl: string }
  | { state: 'unreachable' }
  | { state: 'unexpected'; loginUrl: string; status: number }

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeTokenMetadata(raw: unknown): ScimTokenMetadata {
  const record = asRecord(raw) ?? {}
  return {
    id: asString(pickField(record, 'id')) ?? '',
    organizationId: asString(pickField(record, 'organization_id', 'organizationId')) ?? '',
    createdBy: asString(pickField(record, 'created_by', 'createdBy')) ?? '',
    createdAt: asString(pickField(record, 'created_at', 'createdAt')),
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

/** Owner-only mint: the plaintext scim_ secret is returned exactly once. */
export async function mintScimToken(): Promise<MintedScimToken> {
  const raw = await apiFetch<unknown>('/scim/tokens', { method: 'POST' })
  return {
    token: normalizeTokenMetadata(pickField(raw, 'token') ?? raw),
    secret: asString(pickField(raw, 'secret')) ?? '',
  }
}

/** Revoke one credential by id. 204 = revoked; 404 = unknown or foreign id. */
export async function revokeScimToken(id: string): Promise<void> {
  await apiFetch<unknown>(`/scim/tokens/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/**
 * Probes GET /auth/sso/{org_slug}/login without following the redirect and
 * classifies the documented outcomes. The slug is the backend's slugified
 * organization name (lowercase, non-alphanumeric runs collapsed to '-').
 */
export async function probeSsoLogin(orgSlug: string): Promise<SsoProbeOutcome> {
  const slug = orgSlug.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '')
  const path = `/auth/sso/${encodeURIComponent(slug)}/login`
  const result: ApiProbeResult = await probeApiEndpoint(path)
  if (result.redirected) return { state: 'configured', loginUrl: path }
  if (result.unreachable) return { state: 'unreachable' }
  const code = extractProbeErrorCode(result.body)
  if (result.status === 404) {
    if (code === 'ORG_NOT_FOUND') return { state: 'unknown-org', loginUrl: path }
    if (code === 'SSO_NOT_CONFIGURED') return { state: 'not-configured', loginUrl: path }
  }
  return { state: 'unexpected', loginUrl: path, status: result.status }
}

function extractProbeErrorCode(body: unknown): string | null {
  const record = asRecord(body)
  const error = asRecord(record?.error)
  const code = error ? asString(pickField(error, 'code')) : undefined
  return code ?? null
}

// ---------------------------------------------------------------------------
// Display helpers
// ---------------------------------------------------------------------------

/**
 * Backend slugify (internal/sso/config.go): lowercase, runs of
 * non-alphanumerics collapse to a single '-', trimmed of edge dashes.
 * Mirrored so the probe input can pre-fill from the session's org name.
 */
export function slugifyOrgName(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
}
