// Display helpers. Every formatter is defensive: missing or malformed backend
// values render as "—", never "undefined" or "NaN".

export function parseTimestamp(value?: string | null): number | null {
  if (!value) return null
  const direct = Date.parse(value)
  if (!Number.isNaN(direct)) return direct
  // Go marshals RFC3339Nano; some engines reject >millisecond precision.
  const trimmed = value.replace(/(\.\d{3})\d+/, '$1')
  const reparsed = Date.parse(trimmed)
  return Number.isNaN(reparsed) ? null : reparsed
}

export function formatNumber(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—'
  return value.toLocaleString()
}

export function formatRelativeTime(value?: string | null): string {
  const timestamp = parseTimestamp(value)
  if (timestamp === null) return '—'
  const diffSeconds = Math.round((Date.now() - timestamp) / 1000)
  if (diffSeconds < 45) return 'just now'
  const minutes = Math.round(diffSeconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.round(hours / 24)
  return `${days}d ago`
}

export function formatDateTime(value?: string | null): string {
  const timestamp = parseTimestamp(value)
  if (timestamp === null) return '—'
  return new Date(timestamp).toLocaleString()
}

export function shortenId(value?: string | null): string {
  if (!value) return '—'
  return value.length <= 14 ? value : `${value.slice(0, 11)}…`
}

export function formatDurationMs(ms?: number | null): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  const seconds = ms / 1000
  if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 2 : 1)}s`
  const minutes = Math.floor(seconds / 60)
  const rest = Math.round(seconds % 60)
  return `${minutes}m ${rest}s`
}

/**
 * Cost formatting. The API contract exposes `Cost` as a plain double without
 * pinning the unit; the platform prices in cents elsewhere (max_cost_cents),
 * so a value is treated as USD cents when < 1000 and as USD otherwise.
 * Displayed honestly with the assumed unit, never invented precision.
 */
export function formatCost(value?: number | null): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—'
  if (value === 0) return '$0.00'
  if (Math.abs(value) < 1000) return `${value.toFixed(value < 10 ? 4 : 2)}¢`
  return `$${(value / 100).toFixed(2)}`
}

export function formatCents(value?: number | null): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—'
  return `$${(value / 100).toFixed(value === 0 ? 2 : Math.abs(value) < 1 ? 4 : 2)}`
}
