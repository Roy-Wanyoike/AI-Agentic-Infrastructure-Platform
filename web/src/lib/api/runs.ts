// Runs resource: list / create / get + live event subscription.
//
// Backend notes (confirmed against cmd/api/runs.go + handlers.go, read-only):
// - POST /runs           → 201 {"run_id": "...", "status": "queued"}
// - GET  /runs           → {"runs": [...]}
// - GET  /runs/{id}      → bare run object
// - GET  /runs/{id}/events → SSE stream when Accept: text/event-stream;
//   frames are `data: {"run_id":...,"type":"status","name":"status.changed",
//   "payload":{"status":"RUNNING"|"COMPLETED"|"FAILED","output":"..."}}\n\n
//
// EventSource cannot send Authorization headers, so live subscription is
// implemented with fetch + ReadableStream parsing of the text/event-stream
// body. That works with Bearer tokens and X-API-Key headers alike.

import { ApiError, apiFetch, apiUrl, applyAuthHeaders, extractErrorMessage } from './client'
import { normalizeRun, normalizeRunEvent, normalizeRunStep, pickField, type Run, type RunEvent, type RunStep } from './types'

export type CreateRunInput = {
  agentId: string
  input: string
  organizationId?: string
}

function extractRunList(raw: unknown): Run[] {
  if (Array.isArray(raw)) return raw.map(normalizeRun)
  const wrapped = pickField(raw, 'runs', 'items', 'data')
  return Array.isArray(wrapped) ? wrapped.map(normalizeRun) : []
}

export async function listRuns(): Promise<Run[]> {
  return extractRunList(await apiFetch<unknown>('/runs'))
}

export async function getRun(id: string): Promise<Run> {
  return normalizeRun(await apiFetch<unknown>(`/runs/${encodeURIComponent(id)}`))
}

/**
 * GET /runs/{id}/steps → {"run_id": "...", "steps": [...]} (RunStepsResponse).
 * Steps may be empty until the worker records the first model/tool call.
 */
export async function listRunSteps(runId: string): Promise<RunStep[]> {
  const raw = await apiFetch<unknown>(`/runs/${encodeURIComponent(runId)}/steps`)
  const wrapped = pickField(raw, 'steps', 'items', 'data')
  const list = Array.isArray(raw) ? raw : Array.isArray(wrapped) ? wrapped : []
  return list.map((step, index) => normalizeRunStep(step, index))
}

export async function createRun(payload: CreateRunInput): Promise<Run> {
  const body: Record<string, string> = {
    agent_id: payload.agentId,
    input: payload.input,
  }
  if (payload.organizationId) body.organization_id = payload.organizationId
  const raw = await apiFetch<unknown>('/runs', { method: 'POST', body: JSON.stringify(body) })
  return normalizeRun(raw)
}

export type SubscribeRunEventsOptions = {
  signal?: AbortSignal
}

/**
 * Subscribes to the live event stream of a run by consuming the SSE response
 * body incrementally. Resolves when the server closes the stream (callers
 * should re-subscribe if the run has not reached a terminal state yet).
 */
export async function subscribeRunEvents(
  runId: string,
  onEvent: (event: RunEvent) => void,
  options: SubscribeRunEventsOptions = {},
): Promise<void> {
  const headers = new Headers({ Accept: 'text/event-stream' })
  applyAuthHeaders(headers)

  const response = await fetch(apiUrl(`/runs/${encodeURIComponent(runId)}/events`), {
    headers,
    signal: options.signal,
  })

  if (!response.ok) {
    const text = await response.text().catch(() => '')
    let body: unknown = null
    if (text) {
      try {
        body = JSON.parse(text)
      } catch {
        body = text
      }
    }
    throw new ApiError(response.status, extractErrorMessage(body, 'Failed to open the run event stream'), body)
  }
  if (!response.body) {
    throw new ApiError(response.status, 'Event streaming is not supported by this connection')
  }

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  const processFrame = (frame: string) => {
    const event = parseSseFrame(frame)
    if (event) onEvent(event)
  }

  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n')
    let boundary = buffer.indexOf('\n\n')
    while (boundary !== -1) {
      processFrame(buffer.slice(0, boundary))
      buffer = buffer.slice(boundary + 2)
      boundary = buffer.indexOf('\n\n')
    }
  }
  if (buffer.trim()) processFrame(buffer)
}

function parseSseFrame(frame: string): RunEvent | null {
  const dataLines: string[] = []
  for (const rawLine of frame.split('\n')) {
    const line = rawLine.replace(/\r$/, '')
    if (!line || line.startsWith(':')) continue
    if (line.startsWith('data:')) dataLines.push(line.slice(5).replace(/^ /, ''))
    // 'event:' / 'id:' fields are ignored — the backend sends JSON on data lines only.
  }
  if (dataLines.length === 0) return null
  try {
    return normalizeRunEvent(JSON.parse(dataLines.join('\n')))
  } catch {
    return null
  }
}
