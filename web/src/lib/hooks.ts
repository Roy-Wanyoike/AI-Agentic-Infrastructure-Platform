// React Query hooks over the typed API client layer.
//
// Query keys:
//   ['agents']          — agent list
//   ['agents', id]      — agent detail
//   ['runs']            — run list
//   ['runs', id]        — run detail (shared by SSE subscription + polling)
//   ['metrics']         — platform metrics snapshot
//   ['health']          — healthz/readyz probes
//   ['knowledge']       — knowledge document list
//   ['memory', scope]   — memory snippets (scope = agent id or 'all')
//   ['usageCosts', …]   — usage cost report (from/to/group_by)

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
import {
  createAgentVersion,
  createDeployment,
  diffAgentVersions,
  listAgentVersions,
  listDeployments,
  promoteDeployment,
  publishAgentVersion,
  rollbackAgent,
  rollbackDeployment,
} from './api/versions'
import { createPolicy, evaluatePolicy, listPolicies } from './api/policies'
import { createSchedule, listSchedules, pauseSchedule, resumeSchedule } from './api/schedules'
import { createWebhook, deleteWebhook, listWebhookDeliveries, listWebhooks } from './api/webhooks'
import {
  createKnowledgeDocument,
  listKnowledgeDocuments,
  searchKnowledge,
} from './api/knowledge'
import { listMemorySnippets, putMemorySnippets } from './api/memory'
import { getUsageCosts } from './api/usage'
import type { CreateKnowledgeDocumentInput } from './api/knowledge'
import type { PutMemoryInput } from './api/memory'
import type { UsageCostWindow } from './api/usage'
import type { CreateScheduleInput } from './api/schedules'
import type { CreatePolicyInput } from './api/policies'

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

// ---------------------------------------------------------------------------
// Versions + deployments (track 2-b contract + track 3-e diff)
// ---------------------------------------------------------------------------

export function useAgentVersions(agentId: string | null | undefined) {
  return useQuery({
    queryKey: ['agentVersions', agentId],
    queryFn: () => listAgentVersions(agentId as string),
    enabled: Boolean(agentId),
  })
}

export function useVersionDiff(agentId: string | null | undefined, from: number | null, to: number | null) {
  return useQuery({
    queryKey: ['versionDiff', agentId, from, to],
    queryFn: () => diffAgentVersions(agentId as string, from as number, to as number),
    enabled: Boolean(agentId) && from !== null && to !== null,
    retry: false, // 404 VERSION_NOT_FOUND should surface immediately
  })
}

export function useCreateAgentVersion(agentId: string | null | undefined) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => createAgentVersion(agentId as string),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['agentVersions', agentId] })
      void queryClient.invalidateQueries({ queryKey: ['agents'] })
    },
  })
}

export function usePublishAgentVersion(agentId: string | null | undefined) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (version: number) => publishAgentVersion(agentId as string, version),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['agentVersions', agentId] })
      void queryClient.invalidateQueries({ queryKey: ['agents'] })
      void queryClient.invalidateQueries({ queryKey: ['versionDiff'] })
    },
  })
}

export function useRollbackAgent(agentId: string | null | undefined) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (targetVersion: number) => rollbackAgent(agentId as string, targetVersion),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['agentVersions', agentId] })
      void queryClient.invalidateQueries({ queryKey: ['agents'] })
      void queryClient.invalidateQueries({ queryKey: ['agent', agentId] })
    },
  })
}

export function useDeployments(agentId?: string | null) {
  return useQuery({
    queryKey: ['deployments', agentId ?? 'all'],
    queryFn: () => listDeployments(agentId ?? undefined),
  })
}

export function useCreateDeployment() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: { agentId: string; version: number; environment: string }) => createDeployment(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['deployments'] })
    },
  })
}

export function usePromoteDeployment() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => promoteDeployment(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['deployments'] })
    },
  })
}

export function useRollbackDeployment() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => rollbackDeployment(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['deployments'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Policies (track 2-c contract)
// ---------------------------------------------------------------------------

export function usePolicies() {
  return useQuery({ queryKey: ['policies'], queryFn: listPolicies })
}

export function useCreatePolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreatePolicyInput) => createPolicy(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['policies'] })
    },
  })
}

export function useEvaluatePolicy() {
  return useMutation({ mutationFn: (request: unknown) => evaluatePolicy(request) })
}

// ---------------------------------------------------------------------------
// Schedules (track 2-f contract)
// ---------------------------------------------------------------------------

export function useSchedules() {
  return useQuery({ queryKey: ['schedules'], queryFn: listSchedules })
}

export function useCreateSchedule() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateScheduleInput) => createSchedule(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['schedules'] })
    },
  })
}

export function useScheduleTransition() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, action }: { id: string; action: 'pause' | 'resume' }) =>
      action === 'pause' ? pauseSchedule(id) : resumeSchedule(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['schedules'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Webhooks (track 2-e contract)
// ---------------------------------------------------------------------------

export function useWebhooks() {
  return useQuery({ queryKey: ['webhooks'], queryFn: listWebhooks })
}

export function useCreateWebhook() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: { url: string; events: string[] }) => createWebhook(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['webhooks'] })
    },
  })
}

export function useDeleteWebhook() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => deleteWebhook(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['webhooks'] })
      void queryClient.invalidateQueries({ queryKey: ['webhookDeliveries'] })
    },
  })
}

export function useWebhookDeliveries(webhookId: string | null | undefined, limit = 50) {
  return useQuery({
    queryKey: ['webhookDeliveries', webhookId, limit],
    queryFn: () => listWebhookDeliveries(webhookId as string, limit),
    enabled: Boolean(webhookId),
    // Deliveries land asynchronously while events fire; keep them fresh.
    refetchInterval: 10000,
  })
}

// ---------------------------------------------------------------------------
// Knowledge / RAG (wave-3 knowledge endpoints; backend reuses agents.read /
// agents.write — the views gate writes with the canWrite capability)
// ---------------------------------------------------------------------------

export function useKnowledgeDocuments() {
  return useQuery({ queryKey: ['knowledge'], queryFn: listKnowledgeDocuments })
}

export function useCreateKnowledgeDocument() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateKnowledgeDocumentInput) => createKnowledgeDocument(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['knowledge'] })
    },
  })
}

export function useSearchKnowledge() {
  return useMutation({
    mutationFn: ({ query, k }: { query: string; k?: number }) => searchKnowledge(query, k),
  })
}

// ---------------------------------------------------------------------------
// Memory (wave-3 memory endpoints; same agents.read/agents.write fallback).
// agentId === undefined/'' lists the whole organization; a specific id filters.
// ---------------------------------------------------------------------------

export function useMemorySnippets(agentId?: string | null) {
  return useQuery({
    queryKey: ['memory', agentId ?? 'all'],
    queryFn: () => listMemorySnippets(agentId ?? undefined),
  })
}

export function usePutMemory() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: PutMemoryInput) => putMemorySnippets(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['memory'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Usage costs (wave-3 cost report, GET /usage/costs, usage.read)
// ---------------------------------------------------------------------------

export function useUsageCosts(window: UsageCostWindow = {}) {
  const { from = '', to = '', groupBy = 'day' } = window
  return useQuery({
    queryKey: ['usageCosts', from, to, groupBy],
    queryFn: () => getUsageCosts(window),
  })
}
