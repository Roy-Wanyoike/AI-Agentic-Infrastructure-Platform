// Secrets resource (wave-5 secrets track, issue #25, cmd/api/secrets.go):
//
//   GET    /secrets               -> {"secrets":[…]}              metadata ONLY (runs.execute — MEMBER+)
//   POST   /secrets               -> {"secret":{…}}               create (agents.write — OWNER/ADMIN)
//   DELETE /secrets/{name}        -> {"deleted":true}             soft delete (agents.write — OWNER/ADMIN)
//   POST   /secrets/{name}/reveal -> {"secret":{…,"value":…}}     ONE-TIME reveal (organization.manage — OWNER)
//
// SECURITY (mirrors the backend contract): secret values exist in exactly two
// places — the POST /secrets request body and the one-time reveal response.
// The list projection carries no value field at all (name, key_version,
// created_by, created_at, updated_at), and this client never persists a
// revealed value anywhere.
//
// Name rules (internal/secrets/service.go): ^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$.

import { apiFetch } from './client'
import { asNumber, asString, pickField } from './types'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type SecretMetadata = {
  name: string
  keyVersion: number
  createdBy: string
  createdAt?: string
  updatedAt?: string
}

/** Reveal response: the ONLY shape that ever carries the plaintext value. */
export type RevealedSecret = SecretMetadata & { value: string }

export type CreateSecretInput = {
  name: string
  value: string
}

/** Backend name regex, duplicated so the form can pre-validate honestly. */
export const SECRET_NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$/

// ---------------------------------------------------------------------------
// Normalizers
// ---------------------------------------------------------------------------

function normalizeSecretMetadata(raw: unknown): SecretMetadata {
  return {
    name: asString(pickField(raw, 'name')) ?? '',
    keyVersion: asNumber(pickField(raw, 'keyVersion', 'key_version')) ?? 0,
    createdBy: asString(pickField(raw, 'createdBy', 'created_by')) ?? '',
    createdAt: asString(pickField(raw, 'createdAt', 'created_at')),
    updatedAt: asString(pickField(raw, 'updatedAt', 'updated_at')),
  }
}

// ---------------------------------------------------------------------------
// Fetchers
// ---------------------------------------------------------------------------

function extractSecretList(raw: unknown): SecretMetadata[] {
  const list = pickField(raw, 'secrets')
  return (Array.isArray(list) ? list : []).map(normalizeSecretMetadata)
}

export async function listSecrets(): Promise<SecretMetadata[]> {
  return extractSecretList(await apiFetch<unknown>('/secrets'))
}

export async function createSecret(input: CreateSecretInput): Promise<SecretMetadata> {
  const raw = await apiFetch<unknown>('/secrets', {
    method: 'POST',
    body: JSON.stringify({ name: input.name, value: input.value }),
  })
  return normalizeSecretMetadata(pickField(raw, 'secret') ?? raw)
}

export async function deleteSecret(name: string): Promise<void> {
  await apiFetch<unknown>(`/secrets/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

/** One-time reveal: the API returns the plaintext exactly once (OWNER only). */
export async function revealSecret(name: string): Promise<RevealedSecret> {
  const raw = await apiFetch<unknown>(`/secrets/${encodeURIComponent(name)}/reveal`, { method: 'POST' })
  const secret = pickField(raw, 'secret') ?? raw
  return {
    ...normalizeSecretMetadata(secret),
    value: asString(pickField(secret, 'value')) ?? '',
  }
}
