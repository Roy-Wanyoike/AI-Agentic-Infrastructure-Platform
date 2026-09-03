// Non-component helpers shared across dashboard views (kept separate from
// shared.tsx so react-refresh stays happy: that file only exports components).

import { ApiError } from '../lib/api/client'
import type { Run } from '../lib/api/types'

export const navItems = [
  'Overview',
  'Agents',
  'Runs',
  'Workflows',
  'Approvals',
  'Evaluations',
  'Versions',
  'Usage',
  'Knowledge',
  'Memory',
  'Analytics',
  'Policies',
  'Schedules',
  'Webhooks',
  'Settings',
] as const

export type ViewName = (typeof navItems)[number]

/** Platform-aware shortcut hint for the ⌘K command palette. */
export function paletteShortcutLabel(): string {
  if (typeof navigator === 'undefined') return 'Ctrl K'
  const platform = `${navigator.platform ?? ''} ${navigator.userAgent}`.toLowerCase()
  return /mac|iphone|ipad/.test(platform) ? '⌘K' : 'Ctrl K'
}

export function describeError(error: unknown): string {
  if (error instanceof ApiError) {
    const suffix = error.status ? ` (HTTP ${error.status})` : ''
    return `${error.message}${suffix}`
  }
  if (error instanceof Error) return error.message
  return typeof error === 'string' ? error : 'Unexpected error'
}

export function statusAccent(status: string): 'default' | 'info' | 'success' | 'warning' {
  switch (status) {
    case 'COMPLETED':
      return 'success'
    case 'FAILED':
      return 'warning'
    case 'RUNNING':
      return 'info'
    default:
      return 'default'
  }
}

export function sortRunsDesc(runs: Run[]): Run[] {
  return [...runs].sort((a, b) => {
    const aTime = Date.parse(a.updatedAt ?? a.createdAt ?? '') || 0
    const bTime = Date.parse(b.updatedAt ?? b.createdAt ?? '') || 0
    return bTime - aTime
  })
}

export type StatusCounts = Record<'QUEUED' | 'RUNNING' | 'COMPLETED' | 'FAILED', number>

export function countByStatus(runs: Run[]): StatusCounts {
  const counts: StatusCounts = { QUEUED: 0, RUNNING: 0, COMPLETED: 0, FAILED: 0 }
  for (const run of runs) {
    const status = run.status?.toUpperCase()
    if (status === 'QUEUED' || status === 'RUNNING' || status === 'COMPLETED' || status === 'FAILED') {
      counts[status] += 1
    }
  }
  return counts
}
