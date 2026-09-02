// Auth flows: register / login / logout / API-key sessions.
//
// Credentials live in localStorage:
//   agentos_token  — bearer token (preferred auth path)
//   agentos_user   — normalized user profile (credential material stripped)
//   agentos_api_key — optional X-API-Key (dev convenience)
//
// The store below is a tiny external store usable from React via
// useSyncExternalStore(subscribeAuth, getAuthSnapshot). It reacts to 401s
// emitted by the fetch layer so the UI can drop back to the auth gate.

import {
  AUTH_UNAUTHORIZED_EVENT,
  apiFetch,
  clearStoredApiKey,
  clearStoredToken,
  clearStoredUser,
  getStoredApiKey,
  getStoredToken,
  getStoredUser,
  setStoredApiKey,
  setStoredToken,
  setStoredUser,
} from './client'
import { asString, normalizeAuthUser, pickField, type AuthUser } from './types'

export type AuthState = {
  token: string | null
  apiKey: string | null
  user: AuthUser | null
}

function buildInitialSnapshot(): AuthState {
  return { token: getStoredToken(), apiKey: getStoredApiKey(), user: getStoredUser() }
}

let snapshot: AuthState = buildInitialSnapshot()
const listeners = new Set<() => void>()

function commit(next: AuthState): void {
  snapshot = next
  if (next.token) setStoredToken(next.token)
  else clearStoredToken()
  if (next.user) setStoredUser(next.user)
  else clearStoredUser()
  if (next.apiKey) setStoredApiKey(next.apiKey)
  else clearStoredApiKey()
  for (const listener of listeners) listener()
}

export function subscribeAuth(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

export function getAuthSnapshot(): AuthState {
  return snapshot
}

// React to 401s raised by the fetch layer: drop the dead token but keep a
// still-usable API key session (the fetch layer already removed an invalid key).
if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
  window.addEventListener(AUTH_UNAUTHORIZED_EVENT, () => {
    if (!snapshot.token && !snapshot.user) return
    commit({ token: null, apiKey: snapshot.apiKey, user: null })
  })
}

// ---------------------------------------------------------------------------
// Token payload decoding (best-effort user profile from the issued token)
// ---------------------------------------------------------------------------

function decodeJwtPayload(token: string): Record<string, unknown> | null {
  const parts = token.split('.')
  if (parts.length < 2) return null
  try {
    const base64 = parts[1].replace(/-/g, '+').replace(/_/g, '/')
    const padded = base64 + '='.repeat((4 - (base64.length % 4)) % 4)
    const decoded = atob(padded)
    const parsed: unknown = JSON.parse(decoded)
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : null
  } catch {
    return null
  }
}

function userFromToken(token: string, fallbackEmail: string): AuthUser {
  const claims = decodeJwtPayload(token)
  if (!claims) return { id: '', email: fallbackEmail }
  return {
    id: asString(pickField(claims, 'userId')) ?? '',
    email: asString(pickField(claims, 'email')) ?? fallbackEmail,
    organizationId: asString(pickField(claims, 'organizationId')),
    role: asString(pickField(claims, 'role')),
  }
}

function defaultOrganization(email: string): string {
  const local = email.split('@')[0]?.trim()
  return local ? `${local}'s org` : 'My organization'
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

export type LoginInput = { email: string; password: string }

export async function login(input: LoginInput): Promise<AuthUser> {
  const raw = await apiFetch<unknown>('/auth/login', {
    method: 'POST',
    body: JSON.stringify({ email: input.email, password: input.password }),
  })
  const token = asString(pickField(raw, 'token', 'accessToken'))
  if (!token) throw new Error('Login succeeded but the API response did not include a token')
  const rawUser = pickField(raw, 'user')
  const user = normalizeAuthUser(rawUser ?? userFromToken(token, input.email))
  commit({ token, apiKey: snapshot.apiKey, user })
  return user
}

export type RegisterInput = { organization?: string; email: string; password: string }

export async function register(input: RegisterInput): Promise<AuthUser> {
  const organization = input.organization?.trim() || defaultOrganization(input.email)
  const raw = await apiFetch<unknown>('/auth/register', {
    method: 'POST',
    body: JSON.stringify({ organization, email: input.email, password: input.password }),
  })

  // The contract calls for { token, user }; today's backend returns
  // { organization, user } without a token, so fall back to an immediate login.
  const token = asString(pickField(raw, 'token', 'accessToken'))
  const rawUser = pickField(raw, 'user')
  const organizationRecord = pickField(raw, 'organization', 'org')
  const organizationName = asString(pickField(organizationRecord, 'name'))
  const user = normalizeAuthUser(rawUser ?? userFromToken(token ?? '', input.email))
  if (organizationName) user.organizationName = organizationName

  if (token) {
    commit({ token, apiKey: snapshot.apiKey, user })
    return user
  }

  await login({ email: input.email, password: input.password })
  const mergedUser: AuthUser = {
    ...(snapshot.user ?? user),
    organizationName: organizationName ?? snapshot.user?.organizationName,
  }
  commit({ token: snapshot.token, apiKey: snapshot.apiKey, user: mergedUser })
  return mergedUser
}

export function loginWithApiKey(key: string): void {
  const trimmed = key.trim()
  if (!trimmed) throw new Error('API key is required')
  commit({ token: null, apiKey: trimmed, user: snapshot.user })
}

export function logout(): void {
  commit({ token: null, apiKey: null, user: null })
}
