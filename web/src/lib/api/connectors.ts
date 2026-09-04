// Connectors resource (issue #30, cmd/api/connectors.go):
//
//   POST   /connectors            -> {"connector":{…}}   create (connectors.write — OWNER/ADMIN)
//   GET    /connectors            -> {"connectors":[…]}  list   (connectors.read — MEMBER+)
//   GET    /connectors/{id}       -> {"connector":{…}}   get    (connectors.read — MEMBER+)
//   DELETE /connectors/{id}       -> {"deleted":true}    delete (connectors.write — OWNER/ADMIN)
//   POST   /connectors/{id}/test  -> {"test":{…}}        live health check (connectors.write — OWNER/ADMIN)
//
// NOTE on "U" in CRUD: the connectors service has an Update method, but the
// HTTP surface exposes NO PUT/PATCH route (cmd/api/marketplace.go sibling
// audit, wave-5). Editing an existing connector is therefore not possible via
// the API and this client does not fake it — the create form doubles as a
// fresh-configuration entry point.
//
// SECURITY: secret VALUES never appear anywhere in this surface. secret_ref is
// a NAME reference into the secrets store; config carries header TEMPLATES and
// auth-style parameters only.

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type ConnectorConfig = {
  authStyle: string
  headers: Record<string, string>
  apiKeyHeader: string
  apiKeyPrefix: string
  username: string
}

export type Connector = {
  id: string
  name: string
  type: string
  baseUrl: string
  secretRef: string
  status: string
  config: ConnectorConfig
  createdBy: string
  lastCheckAt: string | null
  /** "" (never checked) | ok | error */
  lastCheckStatus: string
  createdAt?: string
  updatedAt?: string
}

export type ConnectorTestResult = {
  connectorId: string
  /** ok | error */
  status: string
  statusCode: number
  latencyMs: number
  error?: string
  checkedAt: string
}

export type CreateConnectorInput = {
  name: string
  /** webhook | http */
  type: string
  base_url: string
  /** none | bearer | basic | api_key_header */
  auth_style: string
  headers?: Record<string, string>
  api_key_header?: string
  api_key_prefix?: string
  username?: string
  secret_ref?: string
  /** active | disabled */
  status?: string
}

export const CONNECTOR_TYPES = ['http', 'webhook'] as const
export const CONNECTOR_AUTH_STYLES = ['none', 'bearer', 'basic', 'api_key_header'] as const
export const CONNECTOR_STATUSES = ['active', 'disabled'] as const

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeStringMap(value: unknown): Record<string, string> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return {}
  const out: Record<string, string> = {}
  for (const [key, raw] of Object.entries(value as Record<string, unknown>)) {
    const str = asString(raw)
    if (str !== undefined) out[key] = str
  }
  return out
}

function normalizeConnector(raw: unknown): Connector {
  const config = pickField(raw, 'config')
  const lastCheckAt = asString(pickField(raw, 'lastCheckAt', 'last_check_at'))
  return {
    id: asString(pickField(raw, 'id')) ?? '',
    name: asString(pickField(raw, 'name')) ?? 'Unnamed connector',
    type: (asString(pickField(raw, 'type')) ?? 'unknown').toLowerCase(),
    baseUrl: asString(pickField(raw, 'baseUrl', 'base_url')) ?? '',
    secretRef: asString(pickField(raw, 'secretRef', 'secret_ref')) ?? '',
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    config: {
      authStyle: (asString(pickField(config, 'authStyle', 'auth_style')) ?? 'none').toLowerCase(),
      headers: normalizeStringMap(pickField(config, 'headers')),
      apiKeyHeader: asString(pickField(config, 'apiKeyHeader', 'api_key_header')) ?? '',
      apiKeyPrefix: asString(pickField(config, 'apiKeyPrefix', 'api_key_prefix')) ?? '',
      username: asString(pickField(config, 'username')) ?? '',
    },
    createdBy: asString(pickField(raw, 'createdBy', 'created_by')) ?? '',
    lastCheckAt: lastCheckAt ?? null,
    lastCheckStatus: (asString(pickField(raw, 'lastCheckStatus', 'last_check_status')) ?? '').toLowerCase(),
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

function normalizeTestResult(raw: unknown): ConnectorTestResult {
  return {
    connectorId: asString(pickField(raw, 'connectorId', 'connector_id')) ?? '',
    status: (asString(pickField(raw, 'status')) ?? 'unknown').toLowerCase(),
    statusCode: asNumber(pickField(raw, 'statusCode', 'status_code')) ?? 0,
    latencyMs: asNumber(pickField(raw, 'latencyMs', 'latency_ms')) ?? 0,
    error: asString(pickField(raw, 'error')),
    checkedAt: asString(pickField(raw, 'checkedAt', 'checked_at')) ?? '',
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

function extractConnectorList(raw: unknown): Connector[] {
  const list = pickField(raw, 'connectors')
  return (Array.isArray(list) ? list : []).map(normalizeConnector)
}

export async function listConnectors(): Promise<Connector[]> {
  return extractConnectorList(await apiFetch<unknown>('/connectors'))
}

export async function getConnector(id: string): Promise<Connector> {
  const raw = await apiFetch<unknown>(`/connectors/${encodeURIComponent(id)}`)
  return normalizeConnector(pickField(raw, 'connector') ?? raw)
}

export async function createConnector(input: CreateConnectorInput): Promise<Connector> {
  const raw = await apiFetch<unknown>('/connectors', {
    method: 'POST',
    body: JSON.stringify(input),
  })
  return normalizeConnector(pickField(raw, 'connector') ?? raw)
}

export async function deleteConnector(id: string): Promise<void> {
  await apiFetch<unknown>(`/connectors/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** Live health check; the outcome is also recorded on the connector row. */
export async function testConnector(id: string): Promise<ConnectorTestResult> {
  const raw = await apiFetch<unknown>(`/connectors/${encodeURIComponent(id)}/test`, { method: 'POST' })
  return normalizeTestResult(pickField(raw, 'test') ?? raw)
}
