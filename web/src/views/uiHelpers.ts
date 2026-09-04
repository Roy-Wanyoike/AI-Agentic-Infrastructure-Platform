// Non-component helpers shared across dashboard views (kept separate from
// shared.tsx so react-refresh stays happy: that file only exports components).

import { ApiError, extractErrorCode } from '../lib/api/client'
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
  'Billing',
  'Knowledge',
  'Memory',
  'Analytics',
  'Policies',
  'Schedules',
  'Webhooks',
  'Marketplace',
  'Connectors',
  'Secrets',
  'Settings',
] as const

export type ViewName = (typeof navItems)[number]

/**
 * Sidebar glyphs (issue #53 added Billing/Marketplace/Connectors/Secrets).
 * Monochrome symbols matching the existing text-first design language.
 */
export const navIcons: Record<ViewName, string> = {
  Overview: '◎',
  Agents: '⬡',
  Runs: '▷',
  Workflows: '⑂',
  Approvals: '✓',
  Evaluations: '⚗',
  Versions: '⎌',
  Usage: '◔',
  Billing: '¤',
  Knowledge: '❖',
  Memory: '≡',
  Analytics: '◫',
  Policies: '⛨',
  Schedules: '⏱',
  Webhooks: '⇄',
  Marketplace: '⇪',
  Connectors: '⇋',
  Secrets: '✷',
  Settings: '⚙',
}

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

/**
 * Machine-readable code from the shared {"error":{"code",…}} envelope
 * (e.g. NO_SUBSCRIPTION), when the error came from an API response.
 */
export function apiErrorCode(error: unknown): string | null {
  return error instanceof ApiError ? extractErrorCode(error.body) : null
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
