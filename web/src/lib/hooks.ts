// React Query hooks over the typed API client layer.
//
// Query keys:
//   ['agents']          — agent list
//   ['agents', id]      — agent detail
//   ['runs']            — run list
//   ['runs', id]        — run detail (shared by SSE subscription + polling)
//   ['metrics']         — platform metrics snapshot
//   ['health']          — healthz/readyz probes

import { useEffect } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError } from './api/client'
import { createAgent, deleteAgent, getAgent, listAgents, updateAgent } from './api/agents'
import { getMetrics, getPlatformHealth } from './api/metrics'
import { createRun, getRun, listRuns, subscribeRunEvents } from './api/runs'
import type { CreateAgentInput, UpdateAgentInput } from './api/agents'
import type { CreateRunInput } from './api/runs'
import { eventOutput, eventStatus, isTerminalRunStatus, type Run, type RunEvent } from './api/types'

export function useAgents() {
  return useQuery({ queryKey: ['agents'], queryFn: listAgents })
}

export function useAgent(id: string | null | undefined) {
  return useQuery({
    queryKey: ['agents', id],
    queryFn: () => getAgent(id as string),
    enabled: Boolean(id),
  })
}

export function useCreateAgent() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateAgentInput) => createAgent(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['agents'] })
    },
  })
}

export function useUpdateAgent() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: UpdateAgentInput }) => updateAgent(id, input),
    onSuccess: (agent) => {
      void queryClient.invalidateQueries({ queryKey: ['agents'] })
      if (agent.id) queryClient.setQueryData(['agents', agent.id], agent)
    },
  })
}

export function useDeleteAgent() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => deleteAgent(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['agents'] })
    },
  })
}

export function useRuns() {
  return useQuery({ queryKey: ['runs'], queryFn: listRuns })
}

export function useRun(id: string | null | undefined) {
  return useQuery({
    queryKey: ['runs', id],
    queryFn: () => getRun(id as string),
    enabled: Boolean(id),
    // Polling fallback for the SSE stream: the API server's WriteTimeout can
    // close long-lived streams, so keep non-terminal runs fresh either way.
    refetchInterval: (query) => {
      const status = query.state.data?.status
      return status && !isTerminalRunStatus(status) ? 4000 : false
    },
  })
}

export function useRunAgent() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateRunInput) => createRun(input),
    onSuccess: (run) => {
      void queryClient.invalidateQueries({ queryKey: ['runs'] })
      void queryClient.invalidateQueries({ queryKey: ['metrics'] })
      if (run.id) queryClient.setQueryData(['runs', run.id], run)
    },
  })
}

export function useMetrics() {
  return useQuery({ queryKey: ['metrics'], queryFn: getMetrics, refetchInterval: 15000 })
}

export function useHealth() {
  return useQuery({ queryKey: ['health'], queryFn: getPlatformHealth, refetchInterval: 20000 })
}

/**
 * Live run updates over SSE (fetch + ReadableStream — EventSource cannot send
 * auth headers). Events patch the ['runs', id] cache entry; if the stream ends
 * (e.g. the server's write timeout) we reconnect with backoff until the run
 * reaches a terminal state. useRun's polling covers the same cache key, so a
 * dropped stream degrades gracefully instead of freezing the UI.
 */
export function useRunEvents(runId: string | null | undefined) {
  const queryClient = useQueryClient()

  useEffect(() => {
    if (!runId) return
    let cancelled = false
    let controller: AbortController | null = null
    let retryHandle: number | undefined
    let attempt = 0

    const patchRun = (patch: Partial<Run>) => {
      queryClient.setQueryData<Run>(['runs', runId], (current) => (current ? { ...current, ...patch } : current))
    }

    const handleEvent = (event: RunEvent) => {
      const status = eventStatus(event)
      if (status) patchRun({ status })
      const output = eventOutput(event)
      if (output) patchRun({ output })
      if (status && isTerminalRunStatus(status)) {
        void queryClient.invalidateQueries({ queryKey: ['runs'] })
      }
    }

    const scheduleRetry = () => {
      if (cancelled) return
      const current = queryClient.getQueryData<Run>(['runs', runId])
      if (current && isTerminalRunStatus(current.status)) return
      attempt += 1
      const delay = Math.min(1000 * 2 ** Math.min(attempt - 1, 3), 8000)
      retryHandle = window.setTimeout(connect, delay)
    }

    const connect = () => {
      if (cancelled) return
      controller = new AbortController()
      subscribeRunEvents(runId, handleEvent, { signal: controller.signal })
        .then(() => scheduleRetry())
        .catch((error: unknown) => {
          if (cancelled) return
          // Auth problems are surfaced by the 401 handler; missing runs (404)
          // will not heal by retrying — stop instead of hammering the API.
          if (error instanceof ApiError && (error.status === 401 || error.status === 404)) return
          scheduleRetry()
        })
    }

    connect()

    return () => {
      cancelled = true
      controller?.abort()
      if (retryHandle !== undefined) window.clearTimeout(retryHandle)
    }
  }, [runId, queryClient])
}
