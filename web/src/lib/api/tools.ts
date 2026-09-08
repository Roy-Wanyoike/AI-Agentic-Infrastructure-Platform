// Tool registry (issue #18 surface, cmd/api/tools.go):
//
//   GET /tools -> {"tools":[{"name","description"?,"input_schema"?}]}
//
// Read-only catalog of the runtime's registered tools (agents.read — every
// role). description/input_schema are omitted by the backend for tools that
// publish no metadata; this client mirrors that optionality honestly.

import { apiFetch } from './client'
import { asRecord, asString, pickField } from './types'

export type ToolDescriptor = {
  name: string
  description: string
  inputSchema: Record<string, unknown> | null
}

function normalizeTool(raw: unknown): ToolDescriptor {
  return {
    name: asString(pickField(raw, 'name')) ?? '',
    description: asString(pickField(raw, 'description')) ?? '',
    inputSchema: asRecord(pickField(raw, 'input_schema', 'inputSchema')),
  }
}

export async function listTools(): Promise<ToolDescriptor[]> {
  const raw = await apiFetch<unknown>('/tools')
  const list = pickField(raw, 'tools')
  return (Array.isArray(list) ? list : []).map(normalizeTool)
}
