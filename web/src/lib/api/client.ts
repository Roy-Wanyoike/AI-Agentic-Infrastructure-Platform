// Typed fetch layer for the AgentOS API.
//
// - Base URL / prefix come from VITE_API_URL / VITE_API_PREFIX (defaults:
//   http://localhost:8080 and api/v1 — the canonical prefix served by the API).
// - Auth: Bearer token when one is stored (localStorage `agentos_token`),
//   otherwise X-API-Key (localStorage `agentos_api_key`).
// - 401 responses clear the stored token and emit `agentos:unauthorized` on
//   window so the app can drop back to the auth gate.

import { normalizeAuthUser, type AuthUser } from './types'

const DEFAULT_API_URL = 'http://localhost:8080'
const DEFAULT_API_PREFIX = 'api/v1'

function readEnv(name: string): string | undefined {
  const value: unknown = import.meta.env[name]
  return value === undefined || value === null ? undefined : String(value)
}

export const API_BASE = (readEnv('VITE_API_URL') || DEFAULT_API_URL).replace(/\/+$/, '')
export const API_PREFIX = (readEnv('VITE_API_PREFIX') ?? DEFAULT_API_PREFIX).replace(/^\/+|\/+$/g, '')
export const APP_NAME = readEnv('VITE_APP_NAME') || 'AgentOS'

/** Builds an absolute API URL, e.g. apiUrl('/runs/1/events'). */
export function apiUrl(path: string, query: Record<string, string | undefined> = {}): string {
  const cleanPath = path.replace(/^\/+/, '')
  const base = API_PREFIX ? `${API_BASE}/${API_PREFIX}/${cleanPath}` : `${API_BASE}/${cleanPath}`
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined && value !== '') params.set(key, value)
  }
  const queryString = params.toString()
  return queryString ? `${base}?${queryString}` : base
}

export class ApiError extends Error {
  status: number
  body: unknown
  requestId?: string

  constructor(status: number, message: string, body: unknown = null, requestId?: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.body = body
    this.requestId = requestId
  }
}

// ---------------------------------------------------------------------------
// Credential storage (localStorage-backed, with safe fallbacks)
// ---------------------------------------------------------------------------

const TOKEN_STORAGE_KEY = 'agentos_token'
const USER_STORAGE_KEY = 'agentos_user'
const API_KEY_STORAGE_KEY = 'agentos_api_key'
const LEGACY_API_KEY_STORAGE_KEY = 'AGENTOS_API_KEY'

function readStorage(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

function writeStorage(key: string, value: string): void {
  try {
    localStorage.setItem(key, value)
  } catch {
    // storage unavailable (private mode etc.) — session stays in-memory
  }
}

function removeStorage(key: string): void {
  try {
    localStorage.removeItem(key)
  } catch {
    // ignore
  }
}

export function getStoredToken(): string | null {
  const value = readStorage(TOKEN_STORAGE_KEY)
  return value && value.trim() ? value : null
}

export function setStoredToken(token: string): void {
  writeStorage(TOKEN_STORAGE_KEY, token)
}

export function clearStoredToken(): void {
  removeStorage(TOKEN_STORAGE_KEY)
}

export function getStoredApiKey(): string | null {
  const current = readStorage(API_KEY_STORAGE_KEY)
  if (current && current.trim()) return current
  const legacy = readStorage(LEGACY_API_KEY_STORAGE_KEY)
  return legacy && legacy.trim() ? legacy : null
}

export function setStoredApiKey(key: string): void {
  writeStorage(API_KEY_STORAGE_KEY, key)
  removeStorage(LEGACY_API_KEY_STORAGE_KEY)
}

export function clearStoredApiKey(): void {
  removeStorage(API_KEY_STORAGE_KEY)
  removeStorage(LEGACY_API_KEY_STORAGE_KEY)
}

export function getStoredUser(): AuthUser | null {
  const raw = readStorage(USER_STORAGE_KEY)
  if (!raw) return null
  try {
    return normalizeAuthUser(JSON.parse(raw))
  } catch {
    return null
  }
}

export function setStoredUser(user: AuthUser): void {
  writeStorage(USER_STORAGE_KEY, JSON.stringify(user))
}

export function clearStoredUser(): void {
  removeStorage(USER_STORAGE_KEY)
}

// ---------------------------------------------------------------------------
// Auth headers
// ---------------------------------------------------------------------------

/** Attaches Authorization: Bearer <token> or falls back to X-API-Key. */
export function applyAuthHeaders(headers: Headers): void {
  const token = getStoredToken()
  if (token) {
    headers.set('Authorization', `Bearer ${token}`)
    return
  }
  const apiKey = getStoredApiKey()
  if (apiKey) headers.set('X-API-Key', apiKey)
}

// ---------------------------------------------------------------------------
// 401 handling
// ---------------------------------------------------------------------------

export const AUTH_UNAUTHORIZED_EVENT = 'agentos:unauthorized'

function emitUnauthorized(): void {
  if (typeof window === 'undefined' || typeof window.dispatchEvent !== 'function') return
  window.dispatchEvent(new CustomEvent<{ status: number }>(AUTH_UNAUTHORIZED_EVENT, { detail: { status: 401 } }))
}

function handleUnauthorized(): void {
  const hadToken = getStoredToken() !== null
  clearStoredToken()
  // If the failing request could only have used the API key, drop it too so
  // the UI does not loop on a dead credential.
  if (!hadToken) clearStoredApiKey()
  emitUnauthorized()
}

// ---------------------------------------------------------------------------
// Core fetch
// ---------------------------------------------------------------------------

export function extractErrorMessage(body: unknown, fallback: string): string {
  if (typeof body === 'string' && body.trim()) return body.trim()
  if (body && typeof body === 'object') {
    const record = body as Record<string, unknown>
    for (const key of ['error', 'message', 'detail']) {
      const value = record[key]
      if (typeof value === 'string' && value.trim()) return value.trim()
    }
  }
  return fallback || 'Request failed'
}

/**
 * Machine-readable code from the shared {"error":{"code","message"}} error
 * envelope (e.g. NO_SUBSCRIPTION, VALIDATION_ERROR). Returns null when the
 * body does not carry the envelope shape.
 */
export function extractErrorCode(body: unknown): string | null {
  if (!body || typeof body !== 'object' || Array.isArray(body)) return null
  const err = (body as Record<string, unknown>)['error']
  if (!err || typeof err !== 'object' || Array.isArray(err)) return null
  const code = (err as Record<string, unknown>)['code']
  return typeof code === 'string' && code.trim() ? code : null
}

export async function apiFetch<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (!headers.has('Accept')) headers.set('Accept', 'application/json')
  if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  applyAuthHeaders(headers)

  let response: Response
  try {
    response = await fetch(apiUrl(path), { ...init, headers })
  } catch {
    throw new ApiError(0, `Cannot reach the AgentOS API at ${API_BASE}`)
  }

  const requestId = response.headers.get('x-request-id') ?? undefined
  const text = await response.text()
  let body: unknown = null
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = text
    }
  }

  if (!response.ok) {
    if (response.status === 401) handleUnauthorized()
    throw new ApiError(response.status, extractErrorMessage(body, response.statusText), body, requestId)
  }

  return body as T
}
