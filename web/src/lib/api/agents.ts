// Agents resource: list / create / get / update / delete.
//
// Backend notes (confirmed against cmd/api/agents.go, read-only):
// - GET  /agents          → bare JSON array (PascalCase Agent structs)
// - POST /agents/create   → bare agent object (201)
// - GET/PATCH/DELETE /agents/{id} → agent detail handler

import { apiFetch } from './client'
import { normalizeAgent, pickField, type Agent } from './types'

export type CreateAgentInput = {
  name: string
  description?: string
  instructions: string
  model: string
}

export type UpdateAgentInput = Partial<CreateAgentInput> & { status?: string }

function extractAgentList(raw: unknown): Agent[] {
  if (Array.isArray(raw)) return raw.map(normalizeAgent)
  // Compatibility in case the endpoint is ever wrapped: {"agents": [...]}
  const wrapped = pickField(raw, 'agents', 'items', 'data')
  return Array.isArray(wrapped) ? wrapped.map(normalizeAgent) : []
}

export async function listAgents(): Promise<Agent[]> {
  return extractAgentList(await apiFetch<unknown>('/agents'))
}

export async function getAgent(id: string): Promise<Agent> {
  return normalizeAgent(await apiFetch<unknown>(`/agents/${encodeURIComponent(id)}`))
}

export async function createAgent(input: CreateAgentInput): Promise<Agent> {
  const raw = await apiFetch<unknown>('/agents/create', {
    method: 'POST',
    body: JSON.stringify({
      name: input.name,
      description: input.description ?? '',
      instructions: input.instructions,
      model: input.model,
    }),
  })
  // Accept either a bare agent object or {"agent": {...}}.
  return normalizeAgent(pickField(raw, 'agent') ?? raw)
}

export async function updateAgent(id: string, input: UpdateAgentInput): Promise<Agent> {
  return normalizeAgent(
    await apiFetch<unknown>(`/agents/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(input),
    }),
  )
}

export async function deleteAgent(id: string): Promise<void> {
  await apiFetch<unknown>(`/agents/${encodeURIComponent(id)}`, { method: 'DELETE' })
}
