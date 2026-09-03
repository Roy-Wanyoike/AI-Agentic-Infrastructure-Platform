// Metrics + platform health endpoints.
//
// GET /metrics (Prometheus text by default; ?format=json for the JSON shape)
// returns:
//   { "counts": { "<name>": n, ... },
//     "latency": { "<name>": ms, ... },
//     "queue_length": n,
//     "histograms": { "<name>": { count, sum, min, max, p50, p95, p99, buckets } } }
// where the histogram section (wave-2 track 2-h) carries bucketed
// p50/p95/p99 summaries keyed like
// agentos_request_duration_seconds{route="…",method="…",status="…"} (seconds).

import { API_BASE, apiFetch } from './client'
import { normalizeMetrics, type MetricsSnapshot } from './types'

export type PlatformHealth = {
  healthz: string
  readyz: string
}

export async function getMetrics(): Promise<MetricsSnapshot> {
  // ?format=json is a no-op today and keeps JSON responses once the Prometheus
  // text exposition (wave-2 track 2-h) lands on the same path.
  return normalizeMetrics(await apiFetch<unknown>('/metrics?format=json'))
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
