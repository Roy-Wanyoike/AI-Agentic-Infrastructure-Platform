// Tools view (issue #81): read-only registry listing over GET /tools
// (cmd/api/tools.go — agents.read, every role; the catalog is a process-wide
// runtime surface, so entries carry no organization fields).
//
// Each entry renders name, description and the JSON input schema exactly as
// the backend publishes it; description/input_schema are optional (omitted by
// the backend for tools without Described metadata) and render as "—".

import { useTools } from '../lib/hooks'
import type { ToolDescriptor } from '../lib/api/tools'
import { EmptyState, ErrorBanner, PageHeader, Skeleton } from './shared'

function formatSchema(schema: Record<string, unknown> | null): string {
  if (!schema) return '—'
  try {
    return JSON.stringify(schema, null, 2)
  } catch {
    return '—'
  }
}

function ToolCard({ tool }: { tool: ToolDescriptor }) {
  return (
    <article className="panel tool-card">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Tool</p>
          <h3>{tool.name || '—'}</h3>
        </div>
      </div>
      <p className="detail-copy">{tool.description || 'No description published for this tool.'}</p>
      <details className="tool-schema">
        <summary>Input schema</summary>
        <code className="payload-block">{formatSchema(tool.inputSchema)}</code>
      </details>
    </article>
  )
}

export function ToolsView() {
  const toolsQuery = useTools()
  const tools = toolsQuery.data ?? []

  return (
    <>
      <PageHeader eyebrow="Runtime" title="Tool registry" actions={<span className="form-note">GET /tools · read-only</span>} />

      <p className="form-note">
        The tools the runtime exposes to agents (and through the MCP gateway). The registry is process-wide: every tenant
        sees the same catalog, and tool execution is gated by the runs.execute permission.
      </p>

      {toolsQuery.isError ? <ErrorBanner error={toolsQuery.error} onRetry={() => void toolsQuery.refetch()} /> : null}

      {toolsQuery.isPending ? (
        <div className="tool-grid">
          {[0, 1].map((index) => (
            <article key={index} className="panel">
              <Skeleton height={18} width="40%" />
              <Skeleton height={54} style={{ marginTop: 14 }} />
            </article>
          ))}
        </div>
      ) : tools.length === 0 ? (
        <EmptyState
          title="No tools registered"
          hint="The runtime registry is empty on this deployment. Built-in tools (calculator, http_request) register at boot — check the API process logs."
        />
      ) : (
        <div className="tool-grid">
          {tools.map((tool, index) => (
            <ToolCard key={tool.name || `tool-${index}`} tool={tool} />
          ))}
        </div>
      )}
    </>
  )
}
