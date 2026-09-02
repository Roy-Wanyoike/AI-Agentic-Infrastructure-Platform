// Shared building blocks for all dashboard views.
//
// Extracted from App.tsx so the per-resource view modules (workflows,
// approvals, evaluations, …) can reuse the exact same design language
// (App.css tokens) without circular imports.

import type { ReactNode } from 'react'
import { describeError } from './uiHelpers'

export function PageHeader({ eyebrow, title, badge, actions }: { eyebrow: string; title: string; badge?: ReactNode; actions?: ReactNode }) {
  return (
    <header className="topbar">
      <div>
        <p className="eyebrow">{eyebrow}</p>
        <h2>{title}</h2>
      </div>
      {badge || actions ? (
        <div className="topbar-actions">
          {badge}
          {actions}
        </div>
      ) : null}
    </header>
  )
}

export function SummaryStat({ label, value, accent = 'default' }: { label: string; value: string; accent?: 'default' | 'info' | 'success' | 'warning' }) {
  return (
    <article className={`mini-stat accent-${accent}`}>
      <span>{label}</span>
      <strong>{value}</strong>
    </article>
  )
}

export function StatusPill({ status }: { status: string }) {
  return <span className={`status-badge ${status.toLowerCase().replace(/\s+/g, '-')}`}>{status}</span>
}

export function Skeleton({ width, height = 14, style }: { width?: number | string; height?: number | string; style?: Record<string, string | number> }) {
  return <span className="skeleton" style={{ display: 'block', width: width ?? '100%', height, ...style }} aria-hidden="true" />
}

export function ErrorBanner({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  return (
    <div className="error-banner" role="alert">
      <div>
        <strong>Could not load live data</strong>
        <span>{describeError(error)}</span>
      </div>
      <button type="button" className="ghost-button small" onClick={onRetry}>
        Retry
      </button>
    </div>
  )
}

export function EmptyState({ title, hint, action }: { title: string; hint?: string; action?: ReactNode }) {
  return (
    <div className="empty-state">
      <strong>{title}</strong>
      {hint ? <span>{hint}</span> : null}
      {action}
    </div>
  )
}

export function DemoBadge() {
  return (
    <span className="demo-badge" title="Demo content — this section is not backed by a live API endpoint yet">
      Demo data
    </span>
  )
}

export function DemoStrip({ note }: { note: string }) {
  return (
    <div className="demo-strip" role="note">
      <DemoBadge />
      <span>{note}</span>
    </div>
  )
}

