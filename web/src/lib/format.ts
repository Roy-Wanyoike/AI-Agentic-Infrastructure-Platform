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
