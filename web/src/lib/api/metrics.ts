// Metrics + platform health endpoints.
//
// GET /metrics (confirmed against cmd/api/handlers.go metricsHandler) returns:
//   { "counts": { "<name>": n, ... }, "latency": { "<name>": ms, ... }, "queue_length": n }

import { API_BASE, apiFetch } from './client'
import { normalizeMetrics, type MetricsSnapshot } from './types'

export type PlatformHealth = {
  healthz: string
  readyz: string
}

export async function getMetrics(): Promise<MetricsSnapshot> {
  return normalizeMetrics(await apiFetch<unknown>('/metrics'))
}

async function fetchHealthText(path: string): Promise<string> {
  try {
    const response = await fetch(`${API_BASE}/${path}`)
    const text = (await response.text()).trim()
    return text || `HTTP ${response.status}`
  } catch {
    return 'unreachable'
  }
}

/** /healthz and /readyz return plain text and need no auth. */
export async function getPlatformHealth(): Promise<PlatformHealth> {
  const [healthz, readyz] = await Promise.all([fetchHealthText('healthz'), fetchHealthText('readyz')])
  return { healthz, readyz }
}
