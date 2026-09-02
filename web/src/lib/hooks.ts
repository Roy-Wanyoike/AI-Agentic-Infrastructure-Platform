// React Query hooks over the typed API client layer.
//
// Query keys:
//   ['agents']          — agent list
//   ['agents', id]      — agent detail
//   ['runs']            — run list
//   ['runs', id]        — run detail (shared by SSE subscription + polling)
//   ['metrics']         — platform metrics snapshot
//   ['health']          — healthz/readyz probes

import { useEffect, useRef } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError } from './api/client'
import { createAgent, deleteAgent, getAgent, listAgents, updateAgent } from './api/agents'
import { getMetrics, getPlatformHealth } from './api/metrics'
import { createRun, getRun, listRunSteps, listRuns, subscribeRunEvents } from './api/runs'
import type { CreateAgentInput, UpdateAgentInput } from './api/agents'
import type { CreateRunInput } from './api/runs'
import { eventOutput, eventStatus, isTerminalRunStatus, type Run, type RunEvent } from './api/types'
import {
  createWorkflow,
  executeWorkflow,
  getWorkflow,
  getWorkflowRun,
  isTerminalWorkflowRunStatus,
  listWorkflows,
  publishWorkflow,
} from './api/workflows'
import type { CreateWorkflowInput } from './api/workflows'
import { decideApproval, listApprovals, type ApprovalDecision } from './api/approvals'
import {
  compareEvalRuns,
  createEvalDataset,
  getEvalDataset,
  getEvalRun,
  listEvalDatasets,
  runEvalDataset,
} from './api/evaluations'
import type { CreateEvalDatasetInput } from './api/evaluations'

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

export function useRunSteps(runId: string | null | undefined) {
  return useQuery({
    queryKey: ['runs', runId, 'steps'],
    queryFn: () => listRunSteps(runId as string),
    enabled: Boolean(runId),
    // Keep the trace fresh while the run is still executing; SSE events below
    // trigger targeted invalidations, this only covers dropped streams.
    refetchInterval: (query) => {
      const steps = query.state.data
      const stillActive = (steps ?? []).some((step) => step.status === 'pending' || step.status === 'running')
      return stillActive ? 5000 : false
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
 *
 * An optional `onEvent` lets a view piggyback on the same connection (e.g. the
 * run timeline refreshing its steps) without opening a second stream.
 */
export function useRunEvents(runId: string | null | undefined, onEvent?: (event: RunEvent) => void) {
  const queryClient = useQueryClient()
  const onEventRef = useRef(onEvent)
  // Keep the latest callback without re-subscribing the stream on re-renders.
  useEffect(() => {
    onEventRef.current = onEvent
  }, [onEvent])

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
      onEventRef.current?.(event)
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

// ---------------------------------------------------------------------------
// Workflows (track 2-a contract)
// ---------------------------------------------------------------------------

export function useWorkflows() {
  return useQuery({ queryKey: ['workflows'], queryFn: listWorkflows })
}

export function useWorkflow(id: string | null | undefined) {
  return useQuery({
    queryKey: ['workflows', id],
    queryFn: () => getWorkflow(id as string),
    enabled: Boolean(id),
  })
}

export function useCreateWorkflow() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateWorkflowInput) => createWorkflow(input),
    onSuccess: (workflow) => {
      void queryClient.invalidateQueries({ queryKey: ['workflows'] })
      if (workflow.id) queryClient.setQueryData(['workflows', workflow.id], workflow)
    },
  })
}

export function usePublishWorkflow() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => publishWorkflow(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['workflows'] })
    },
  })
}

export function useExecuteWorkflow() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: string }) => executeWorkflow(id, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['workflows'] })
      void queryClient.invalidateQueries({ queryKey: ['workflowRuns'] })
      void queryClient.invalidateQueries({ queryKey: ['runs'] })
      void queryClient.invalidateQueries({ queryKey: ['metrics'] })
    },
  })
}

export function useWorkflowRun(id: string | null | undefined) {
  return useQuery({
    queryKey: ['workflowRuns', id],
    queryFn: () => getWorkflowRun(id as string),
    enabled: Boolean(id),
    // Workflow runs fan out to child runs; poll until terminal for a live view.
    refetchInterval: (query) => {
      const status = query.state.data?.status
      return status && !isTerminalWorkflowRunStatus(status) ? 4000 : false
    },
  })
}

// ---------------------------------------------------------------------------
// Approvals (track 2-a contract)
// ---------------------------------------------------------------------------

export function useApprovals(status?: string) {
  return useQuery({
    queryKey: ['approvals', status ?? 'all'],
    queryFn: () => listApprovals(status),
    // Pending queues benefit from a light refresh; React Query dedupes.
    refetchInterval: status === undefined || status === 'pending' ? 15000 : false,
  })
}

export function useDecideApproval() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, decision, reason }: { id: string; decision: ApprovalDecision; reason?: string }) =>
      decideApproval(id, decision, reason),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['approvals'] })
      void queryClient.invalidateQueries({ queryKey: ['runs'] })
      void queryClient.invalidateQueries({ queryKey: ['workflowRuns'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Evaluations (track 2-d contract)
// ---------------------------------------------------------------------------

export function useEvalDatasets() {
  return useQuery({ queryKey: ['evalDatasets'], queryFn: listEvalDatasets })
}

export function useEvalDataset(id: string | null | undefined) {
  return useQuery({
    queryKey: ['evalDatasets', id],
    queryFn: () => getEvalDataset(id as string),
    enabled: Boolean(id),
  })
}

export function useCreateEvalDataset() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateEvalDatasetInput) => createEvalDataset(input),
    onSuccess: (dataset) => {
      void queryClient.invalidateQueries({ queryKey: ['evalDatasets'] })
      if (dataset.id) queryClient.setQueryData(['evalDatasets', dataset.id], dataset)
    },
  })
}

export function useRunEvalDataset() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ datasetId, agentId }: { datasetId: string; agentId: string }) => runEvalDataset(datasetId, agentId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['evalDatasets'] })
      void queryClient.invalidateQueries({ queryKey: ['runs'] })
    },
  })
}

export function useEvalRun(id: string | null | undefined) {
  return useQuery({
    queryKey: ['evalRuns', id],
    queryFn: () => getEvalRun(id as string),
    enabled: Boolean(id),
  })
}

export function useCompareEvalRuns() {
  return useMutation({
    mutationFn: ({ baselineRunId, candidateRunId }: { baselineRunId: string; candidateRunId: string }) =>
      compareEvalRuns(baselineRunId, candidateRunId),
  })
}
