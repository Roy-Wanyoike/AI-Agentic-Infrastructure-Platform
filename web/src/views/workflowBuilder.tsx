// Visual Workflow Builder (issue #120) — hand-rolled SVG DAG editor.
//
// Deliberately dependency-free (no reactflow): the canvas is plain SVG with
// pointer-event gestures (pan, zoom, node drag, port-to-port linking), a
// per-type inspector that speaks the exact workflow DSL config shapes the
// backend validates (internal/workflows/dsl.go), client-side validation that
// mirrors the server rules (cycles via DFS, missing config, dangling refs,
// disconnected nodes), server validation through POST /workflows/validate
// (issue #109), save through POST /workflows/create and a JSON DSL
// import/export round-trip plus a dependency-free SVG serializer export.
//
// Layout positions are canvas-only state and never leak into the exported
// DSL — the JSON document stays byte-compatible with the API contract.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { DragEvent as ReactDragEvent, MouseEvent as ReactMouseEvent, PointerEvent as ReactPointerEvent } from 'react'
import { useCreateWorkflow } from '../lib/hooks'
import {
  extractValidationIssues,
  validateWorkflowDsl,
  type WorkflowDsl,
  type WorkflowValidationIssue,
} from '../lib/api/workflows'
import { PageHeader } from './shared'
import { describeError } from './uiHelpers'
import './workflowBuilder.css'

// ---------------------------------------------------------------------------
// Constants + types
// ---------------------------------------------------------------------------

const NODE_W = 180
const NODE_H = 56
const GRID = 24
const MINI_W = 176
const MINI_H = 108
const ZOOM_MIN = 0.35
const ZOOM_MAX = 2.5

const NODE_KINDS = [
  'agent',
  'tool',
  'condition',
  'parallel',
  'approval',
  'delay',
  'webhook',
  'switch',
  'loop',
  'retry',
  'wait_until',
  'end',
] as const

type NodeKind = (typeof NODE_KINDS)[number]

const EDGE_CONDITIONS = ['on_success', 'on_failure', 'always'] as const
type EdgeCondition = (typeof EDGE_CONDITIONS)[number]

const PALETTE_GROUPS: Array<{ label: string; kinds: NodeKind[] }> = [
  { label: 'Work', kinds: ['agent', 'tool', 'webhook'] },
  { label: 'Control flow', kinds: ['condition', 'switch', 'parallel', 'loop', 'retry', 'wait_until'] },
  { label: 'Governance', kinds: ['approval', 'delay', 'end'] },
]

type KindMeta = { label: string; hint: string; fill: string; stroke: string }

const KIND_META: Record<NodeKind, KindMeta> = {
  agent: { label: 'Agent', hint: 'Run an agent', fill: 'rgba(129,140,248,0.16)', stroke: '#818cf8' },
  tool: { label: 'Tool', hint: 'Call a tool', fill: 'rgba(45,212,191,0.13)', stroke: '#2dd4bf' },
  webhook: { label: 'Webhook', hint: 'HTTP callback', fill: 'rgba(56,189,248,0.13)', stroke: '#38bdf8' },
  condition: { label: 'Condition', hint: 'If / else branch', fill: 'rgba(251,191,36,0.12)', stroke: '#fbbf24' },
  switch: { label: 'Switch', hint: 'Multi-way branch', fill: 'rgba(251,146,60,0.12)', stroke: '#fb923c' },
  parallel: { label: 'Parallel', hint: 'Fan-out branches', fill: 'rgba(167,139,250,0.13)', stroke: '#a78bfa' },
  loop: { label: 'Loop', hint: 'Iterate a body', fill: 'rgba(244,114,182,0.12)', stroke: '#f472b6' },
  retry: { label: 'Retry', hint: 'Retry a node', fill: 'rgba(251,113,133,0.12)', stroke: '#fb7185' },
  wait_until: { label: 'Wait until', hint: 'Poll until true', fill: 'rgba(94,234,212,0.11)', stroke: '#5eead4' },
  approval: { label: 'Approval', hint: 'Human gate', fill: 'rgba(248,113,113,0.12)', stroke: '#f87171' },
  delay: { label: 'Delay', hint: 'Wait N seconds', fill: 'rgba(148,163,184,0.13)', stroke: '#94a3b8' },
  end: { label: 'End', hint: 'Terminal marker', fill: 'rgba(203,213,225,0.10)', stroke: '#cbd5e1' },
}

const FALLBACK_META: KindMeta = { label: 'Unknown', hint: 'Unknown DSL type', fill: 'rgba(148,163,184,0.12)', stroke: '#94a3b8' }

function isNodeKind(kind: string): kind is NodeKind {
  return (NODE_KINDS as readonly string[]).includes(kind)
}

function kindMeta(kind: string): KindMeta {
  return isNodeKind(kind) ? KIND_META[kind] : { ...FALLBACK_META, label: kind || 'Unknown' }
}

function isLoopOwnable(kind: string): boolean {
  return ['agent', 'tool', 'delay', 'webhook', 'end'].includes(kind)
}

type BuilderNode = {
  id: string
  kind: string
  name: string
  config: Record<string, unknown>
  x: number
  y: number
}

type BuilderEdge = {
  key: string
  from: string
  to: string
  condition: string
}

type Viewport = { x: number; y: number; zoom: number }

type Selection = { kind: 'node'; id: string } | { kind: 'edge'; key: string }

type BuilderIssue = {
  code: string
  message: string
  nodeId?: string
  edgeKey?: string
  severity: 'error' | 'warning'
  source: 'client' | 'server'
}

type Gesture =
  | { mode: 'pan'; startClientX: number; startClientY: number; startPanX: number; startPanY: number; moved: boolean }
  | { mode: 'drag'; startWorldX: number; startWorldY: number; originX: number; originY: number; nodeId: string; moved: boolean }

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value))
}

function snap(value: number): number {
  return Math.round(value / 12) * 12
}

function truncate(text: string, max: number): string {
  return text.length > max ? `${text.slice(0, max - 1)}…` : text
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function displayName(node: BuilderNode): string {
  return node.name.trim() || node.id
}

function configString(config: Record<string, unknown>, key: string): string {
  const value = config[key]
  return typeof value === 'string' ? value.trim() : ''
}

function configRawString(config: Record<string, unknown>, key: string): string {
  const value = config[key]
  return typeof value === 'string' ? value : ''
}

function configNumberText(config: Record<string, unknown>, key: string): string {
  const value = config[key]
  return typeof value === 'number' && Number.isFinite(value) ? String(value) : ''
}

function edgeKeyOf(from: string, to: string, condition: string): string {
  return `${from}=>${to}:${condition || 'on_success'}`
}

function nodePort(node: BuilderNode, side: 'in' | 'out'): { x: number; y: number } {
  return { x: side === 'out' ? node.x + NODE_W : node.x, y: node.y + NODE_H / 2 }
}

function edgePath(from: BuilderNode, to: BuilderNode): string {
  const a = nodePort(from, 'out')
  const b = nodePort(to, 'in')
  const dx = Math.max(44, Math.abs(b.x - a.x) / 2)
  return `M ${a.x} ${a.y} C ${a.x + dx} ${a.y}, ${b.x - dx} ${b.y}, ${b.x} ${b.y}`
}

function worldBounds(nodes: BuilderNode[]): { minX: number; minY: number; maxX: number; maxY: number } {
  const xs = nodes.flatMap((n) => [n.x, n.x + NODE_W])
  const ys = nodes.flatMap((n) => [n.y, n.y + NODE_H])
  return { minX: Math.min(...xs) - 60, minY: Math.min(...ys) - 60, maxX: Math.max(...xs) + 60, maxY: Math.max(...ys) + 60 }
}

/** Canvas → standalone .svg file: clone, strip interactive layers, set viewBox. */
function serializeCanvasSvg(svg: SVGSVGElement, nodes: BuilderNode[]): string | null {
  if (nodes.length === 0) return null
  const clone = svg.cloneNode(true) as SVGSVGElement
  clone.querySelectorAll('[data-wfb-interactive]').forEach((element) => element.remove())
  clone.removeAttribute('class')
  const bounds = worldBounds(nodes)
  const width = Math.max(1, Math.round(bounds.maxX - bounds.minX))
  const height = Math.max(1, Math.round(bounds.maxY - bounds.minY))
  clone.setAttribute('xmlns', 'http://www.w3.org/2000/svg')
  clone.setAttribute('viewBox', `${bounds.minX} ${bounds.minY} ${width} ${height}`)
  clone.setAttribute('width', String(width))
  clone.setAttribute('height', String(height))
  const background = document.createElementNS('http://www.w3.org/2000/svg', 'rect')
  background.setAttribute('x', String(bounds.minX))
  background.setAttribute('y', String(bounds.minY))
  background.setAttribute('width', String(width))
  background.setAttribute('height', String(height))
  background.setAttribute('fill', '#0c1220')
  clone.insertBefore(background, clone.firstChild)
  return `<?xml version="1.0" encoding="UTF-8"?>\n${new XMLSerializer().serializeToString(clone)}`
}

function downloadText(filename: string, mime: string, text: string): void {
  const blob = new Blob([text], { type: mime })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  URL.revokeObjectURL(url)
}

/** Canvas graph → wire DSL (positions intentionally dropped). */
function toDsl(nodes: BuilderNode[], edges: BuilderEdge[]): WorkflowDsl {
  return {
    nodes: nodes.map((node) => ({
      id: node.id,
      type: node.kind,
      name: node.name.trim() ? node.name.trim() : undefined,
      config: Object.keys(node.config).length > 0 ? { ...node.config } : undefined,
    })),
    edges: edges.map((edge) => ({ from: edge.from, to: edge.to, condition: edge.condition || undefined })),
  }
}

/** Wire DSL → canvas graph with an automatic layered layout. */
function fromDsl(dsl: WorkflowDsl): { builderNodes: BuilderNode[]; builderEdges: BuilderEdge[] } {
  const positions = layoutPositions(dsl.nodes, dsl.edges)
  const builderNodes: BuilderNode[] = dsl.nodes.map((node) => ({
    id: node.id,
    kind: node.type,
    name: node.name ?? '',
    config: { ...(node.config ?? {}) },
    x: positions.get(node.id)?.x ?? 80,
    y: positions.get(node.id)?.y ?? 80,
  }))
  const builderEdges: BuilderEdge[] = dsl.edges.map((edge) => ({
    key: edgeKeyOf(edge.from, edge.to, edge.condition ?? 'on_success'),
    from: edge.from,
    to: edge.to,
    condition: edge.condition ?? 'on_success',
  }))
  return { builderNodes, builderEdges }
}

/** Layered layout (Kahn topological layers; edges into loop nodes are
 * loop-back exempt so loop-heavy graphs still lay out). Cycle remnants and
 * orphans land in the last layer. */
function layoutPositions(
  nodes: Array<{ id: string; type: string }>,
  edges: Array<{ from: string; to: string }>,
): Map<string, { x: number; y: number }> {
  const ids = nodes.map((n) => n.id).filter((id) => id.trim() !== '')
  const idSet = new Set(ids)
  const loopTargets = new Set(nodes.filter((n) => idSet.has(n.id) && n.type === 'loop').map((n) => n.id))
  const indegree = new Map<string, number>(ids.map((id) => [id, 0]))
  const adjacency = new Map<string, string[]>(ids.map((id) => [id, []]))
  for (const edge of edges) {
    if (!idSet.has(edge.from) || !idSet.has(edge.to) || loopTargets.has(edge.to) || edge.from === edge.to) continue
    adjacency.get(edge.from)?.push(edge.to)
    indegree.set(edge.to, (indegree.get(edge.to) ?? 0) + 1)
  }
  const layerOf = new Map<string, number>()
  let frontier = ids.filter((id) => (indegree.get(id) ?? 0) === 0)
  let layer = 0
  while (frontier.length > 0) {
    const next: string[] = []
    for (const id of frontier) if (!layerOf.has(id)) layerOf.set(id, layer)
    for (const id of frontier) {
      for (const neighbor of adjacency.get(id) ?? []) {
        const remaining = (indegree.get(neighbor) ?? 1) - 1
        indegree.set(neighbor, remaining)
        if (remaining === 0 && !layerOf.has(neighbor)) next.push(neighbor)
      }
    }
    frontier = next
    layer += 1
  }
  for (const id of ids) if (!layerOf.has(id)) layerOf.set(id, layer)
  const byLayer = new Map<number, string[]>()
  for (const id of ids) {
    const target = layerOf.get(id) ?? 0
    const bucket = byLayer.get(target) ?? []
    bucket.push(id)
    byLayer.set(target, bucket)
  }
  const positions = new Map<string, { x: number; y: number }>()
  for (const [layerIndex, members] of byLayer) {
    members.forEach((id, row) => {
      positions.set(id, { x: 60 + layerIndex * 250, y: 60 + row * 96 })
    })
  }
  return positions
}

function defaultConfig(kind: NodeKind): Record<string, unknown> {
  switch (kind) {
    case 'agent':
      return { agent_id: '', input: '{{input}}' }
    case 'tool':
      return { tool_id: '', input: '{{input}}' }
    case 'webhook':
      return { url: '' }
    case 'condition':
      return { expression: '' }
    case 'switch':
      return { expression: '' }
    case 'loop':
      return { expression: '', max_iterations: 100 }
    case 'retry':
      return { target: '', attempts: 3 }
    case 'wait_until':
      return { expression: '' }
    case 'parallel':
      return { strategy: 'all' }
    case 'delay':
      return { seconds: 30 }
    default:
      return {}
  }
}

// --- config readers --------------------------------------------------------

function readBranches(config: Record<string, unknown>): { true: string; false: string } {
  const raw = config['branches']
  if (!isRecord(raw)) return { true: '', false: '' }
  return { true: configString(raw, 'true'), false: configString(raw, 'false') }
}

function readCases(config: Record<string, unknown>): Array<{ value: string; target: string }> {
  const raw = config['cases']
  if (!isRecord(raw)) return []
  return Object.entries(raw)
    .filter(([, target]) => typeof target === 'string')
    .map(([value, target]) => ({ value, target: target as string }))
}

function readBody(config: Record<string, unknown>): string[] {
  const raw = config['body']
  return Array.isArray(raw) ? raw.filter((item): item is string => typeof item === 'string') : []
}

function readBackoff(config: Record<string, unknown>): { base: string; multiplier: string; max: string } {
  const raw = config['backoff']
  if (!isRecord(raw)) return { base: '', multiplier: '', max: '' }
  return {
    base: configNumberText(raw, 'base_delay_ms'),
    multiplier: configNumberText(raw, 'multiplier'),
    max: configNumberText(raw, 'max_delay_ms'),
  }
}

function branchTargetsOf(config: Record<string, unknown>): string[] {
  const branches = readBranches(config)
  return [branches.true, branches.false].filter((target) => target !== '')
}

function collectRoutingRefs(config: Record<string, unknown>, out: Set<string>): void {
  for (const target of branchTargetsOf(config)) out.add(target)
  for (const row of readCases(config)) if (row.target) out.add(row.target)
  const fallback = configString(config, 'default')
  if (fallback) out.add(fallback)
  for (const bodyId of readBody(config)) out.add(bodyId)
  const retryTarget = configString(config, 'target')
  if (retryTarget) out.add(retryTarget)
}

/** Rewires config references (branches/cases/default/body/target) after an id rename. */
function rewriteConfigRefs(config: Record<string, unknown>, from: string, to: string): Record<string, unknown> {
  const out: Record<string, unknown> = { ...config }
  if (configString(out, 'target') === from) out['target'] = to
  else if (configRawString(out, 'target') === from) out['target'] = to
  const branches = out['branches']
  if (isRecord(branches)) {
    const next = { ...branches }
    for (const side of ['true', 'false'] as const) {
      if (typeof next[side] === 'string' && (next[side] as string) === from) next[side] = to
    }
    out['branches'] = next
  }
  const cases = out['cases']
  if (isRecord(cases)) {
    const next: Record<string, unknown> = {}
    for (const [value, target] of Object.entries(cases)) next[value] = target === from ? to : target
    out['cases'] = next
  }
  if (configRawString(out, 'default') === from) out['default'] = to
  if (Array.isArray(out['body'])) out['body'] = (out['body'] as unknown[]).map((item) => (item === from ? to : item))
  return out
}

// --- client-side validation (mirrors internal/workflows/dsl.go) ------------

function missingConfigMessage(node: BuilderNode): string {
  const config = node.config
  switch (node.kind) {
    case 'agent':
      return configString(config, 'agent_id') ? '' : 'agent node requires config.agent_id'
    case 'tool':
      return configString(config, 'tool_id') ? '' : 'tool node requires config.tool_id'
    case 'condition':
      return configString(config, 'expression') ? '' : 'condition node requires config.expression'
    case 'switch': {
      if (!configString(config, 'expression')) return 'switch node requires config.expression'
      const hasCases = readCases(config).length > 0
      const hasDefault = configString(config, 'default') !== ''
      return hasCases || hasDefault ? '' : 'switch node requires config.cases and/or config.default'
    }
    case 'loop': {
      if (!configString(config, 'expression')) return 'loop node requires config.expression'
      return readBody(config).length > 0 ? '' : 'loop node requires a non-empty config.body node id list'
    }
    case 'retry':
      return configString(config, 'target') ? '' : 'retry node requires config.target'
    case 'wait_until':
      return configString(config, 'expression') ? '' : 'wait_until node requires config.expression'
    case 'delay': {
      const seconds = Number(config['seconds'])
      return Number.isFinite(seconds) && seconds > 0 ? '' : 'delay node requires config.seconds > 0'
    }
    case 'webhook':
      return configString(config, 'url') ? '' : 'webhook node requires config.url'
    default:
      return ''
  }
}

/** DFS cycle detection — edges into loop nodes are structural loop-backs and
 * stay exempt, exactly like the backend's findCycle. */
function findCycleNode(nodes: BuilderNode[], edges: BuilderEdge[], ids: Set<string>): string {
  const loopTargets = new Set(nodes.filter((n) => ids.has(n.id) && n.kind === 'loop').map((n) => n.id))
  const adjacency = new Map<string, string[]>([...ids].map((id) => [id, []]))
  for (const edge of edges) {
    if (!ids.has(edge.from) || !ids.has(edge.to) || loopTargets.has(edge.to)) continue
    adjacency.get(edge.from)?.push(edge.to)
  }
  const color = new Map<string, 0 | 1 | 2>()
  let found = ''
  const visit = (id: string): boolean => {
    color.set(id, 1)
    for (const next of adjacency.get(id) ?? []) {
      const state = color.get(next) ?? 0
      if (state === 1) {
        found = next
        return true
      }
      if (state === 0 && visit(next)) return true
    }
    color.set(id, 2)
    return false
  }
  for (const id of ids) {
    if ((color.get(id) ?? 0) === 0 && visit(id)) break
  }
  return found
}

function validateControlFlow(nodes: BuilderNode[], ids: Set<string>): BuilderIssue[] {
  const issues: BuilderIssue[] = []
  const kinds = new Map(nodes.map((n) => [n.id, n.kind]))
  const push = (code: string, message: string, nodeId: string) =>
    issues.push({ code, message, nodeId, severity: 'error', source: 'client' })

  for (const node of nodes) {
    if (!node.id.trim() || !ids.has(node.id)) continue
    switch (node.kind) {
      case 'condition': {
        for (const target of branchTargetsOf(node.config)) {
          if (!ids.has(target)) push('dangling_branch_target', `condition branch references unknown node "${target}".`, node.id)
        }
        break
      }
      case 'switch': {
        for (const row of readCases(node.config)) {
          if (!row.target) push('missing_config', `switch case "${row.value}" has no target node id.`, node.id)
          else if (!ids.has(row.target)) push('dangling_branch_target', `switch case "${row.value}" references unknown node "${row.target}".`, node.id)
        }
        const fallback = configString(node.config, 'default')
        if (fallback && !ids.has(fallback)) push('dangling_branch_target', `switch default references unknown node "${fallback}".`, node.id)
        break
      }
      case 'loop': {
        const bodySet = new Set<string>()
        for (const bodyId of readBody(node.config)) {
          if (bodySet.has(bodyId)) {
            push('duplicate_loop_body', `loop body lists node "${bodyId}" twice.`, node.id)
            continue
          }
          bodySet.add(bodyId)
          if (!ids.has(bodyId)) push('dangling_loop_body', `loop body references unknown node "${bodyId}".`, node.id)
          else if (!isLoopOwnable(kinds.get(bodyId) ?? ''))
            push('loop_body_invalid_node', `loop body node "${bodyId}" has type "${kinds.get(bodyId)}"; bodies may only contain agent, tool, delay, webhook or end nodes.`, node.id)
        }
        break
      }
      case 'retry': {
        const target = configString(node.config, 'target')
        if (target && ids.has(target) && !isLoopOwnable(kinds.get(target) ?? ''))
          push('invalid_retry_target', `retry target "${target}" has type "${kinds.get(target)}"; only agent, tool, delay, webhook or end nodes can be retried.`, node.id)
        break
      }
    }
  }

  const owner = new Map<string, string>()
  for (const node of nodes) {
    if (node.kind !== 'loop' || !ids.has(node.id)) continue
    for (const bodyId of readBody(node.config)) {
      if (!ids.has(bodyId)) continue
      if (owner.has(bodyId)) push('duplicate_loop_body', `node "${bodyId}" is already owned by "${owner.get(bodyId)}".`, node.id)
      else owner.set(bodyId, node.id)
    }
  }
  for (const node of nodes) {
    if (node.kind !== 'retry') continue
    const target = configString(node.config, 'target')
    if (!target || !ids.has(target)) continue
    if (owner.has(target)) push('duplicate_retry_target', `node "${target}" is already owned by "${owner.get(target)}".`, node.id)
    else owner.set(target, node.id)
  }
  return issues
}

function validateCanvas(nodes: BuilderNode[], edges: BuilderEdge[]): BuilderIssue[] {
  if (nodes.length === 0) {
    return [{ code: 'empty_graph', message: 'The canvas is empty — add at least one node before saving.', severity: 'error', source: 'client' }]
  }
  const issues: BuilderIssue[] = []
  const ids = new Set<string>()
  for (const node of nodes) {
    const id = node.id.trim()
    if (!id) {
      issues.push({ code: 'missing_node_id', message: 'A node is missing an id.', severity: 'error', source: 'client' })
    } else if (ids.has(id)) {
      issues.push({ code: 'duplicate_node_id', message: `Duplicate node id "${id}".`, nodeId: id, severity: 'error', source: 'client' })
    } else {
      ids.add(id)
    }
    if (!isNodeKind(node.kind)) {
      issues.push({ code: 'unknown_node_type', message: `Unknown node type "${node.kind}" — not part of the workflow DSL.`, nodeId: node.id, severity: 'error', source: 'client' })
    }
    const configIssue = missingConfigMessage(node)
    if (configIssue) issues.push({ code: 'missing_config', message: configIssue, nodeId: node.id, severity: 'error', source: 'client' })
  }

  for (const edge of edges) {
    if (!edge.from || !ids.has(edge.from)) {
      issues.push({ code: 'missing_node_ref', message: `Edge references unknown source "${edge.from}".`, edgeKey: edge.key, severity: 'error', source: 'client' })
    }
    if (!edge.to || !ids.has(edge.to)) {
      issues.push({ code: 'missing_node_ref', message: `Edge references unknown target "${edge.to}".`, edgeKey: edge.key, severity: 'error', source: 'client' })
    }
    if (!(EDGE_CONDITIONS as readonly string[]).includes(edge.condition)) {
      issues.push({ code: 'invalid_edge_condition', message: `Edge condition "${edge.condition}" must be on_success, on_failure or always.`, edgeKey: edge.key, severity: 'error', source: 'client' })
    }
  }

  const cycleNode = findCycleNode(nodes, edges, ids)
  if (cycleNode) {
    issues.push({ code: 'cycle_detected', message: `The graph contains a cycle through "${cycleNode}".`, nodeId: cycleNode, severity: 'error', source: 'client' })
  }

  issues.push(...validateControlFlow(nodes, ids))

  if (nodes.length > 1) {
    const connected = new Set<string>()
    for (const edge of edges) {
      connected.add(edge.from)
      connected.add(edge.to)
    }
    for (const node of nodes) collectRoutingRefs(node.config, connected)
    for (const node of nodes) {
      if (node.id.trim() && !connected.has(node.id)) {
        issues.push({ code: 'isolated_node', message: `"${displayName(node)}" has no connections.`, nodeId: node.id, severity: 'warning', source: 'client' })
      }
    }
  }
  return issues
}

// ---------------------------------------------------------------------------
// Inspector components
// ---------------------------------------------------------------------------

type NodeInspectorProps = {
  node: BuilderNode
  allNodes: BuilderNode[]
  onNameChange: (name: string) => void
  onRename: (nextId: string) => string | null
  onConfigChange: (config: Record<string, unknown>, routingTargets?: string[]) => void
  onDelete: () => void
}

function NodeInspector({ node, allNodes, onNameChange, onRename, onConfigChange, onDelete }: NodeInspectorProps) {
  const [nextId, setNextId] = useState(node.id)
  const [idError, setIdError] = useState<string | null>(null)
  const [caseRows, setCaseRows] = useState<Array<{ value: string; target: string }>>(() => readCases(node.config))

  const setConfigKey = (key: string, value: unknown) => {
    const config = { ...node.config }
    if (value === '' || value === undefined) delete config[key]
    else config[key] = value
    onConfigChange(config)
  }

  const setNumberKey = (key: string, raw: string) => {
    const trimmed = raw.trim()
    const config = { ...node.config }
    if (trimmed === '') delete config[key]
    else {
      const parsed = Number(trimmed)
      if (Number.isFinite(parsed)) config[key] = parsed
    }
    onConfigChange(config)
  }

  const setBranch = (side: 'true' | 'false', target: string) => {
    const branches = { ...readBranches(node.config), [side]: target }
    const config = { ...node.config }
    if (!branches.true && !branches.false) delete config['branches']
    else config['branches'] = branches
    onConfigChange(config, branchTargetsOf(config))
  }

  const writeSwitch = (rows: Array<{ value: string; target: string }>, fallback: string) => {
    const config = { ...node.config }
    const cases: Record<string, string> = {}
    for (const row of rows) if (row.value.trim() !== '') cases[row.value] = row.target
    if (Object.keys(cases).length === 0) delete config['cases']
    else config['cases'] = cases
    if (fallback.trim() === '') delete config['default']
    else config['default'] = fallback.trim()
    const targets = [...Object.values(cases).filter(Boolean), ...(fallback.trim() ? [fallback.trim()] : [])]
    onConfigChange(config, targets)
  }

  const setBody = (bodyId: string, checked: boolean) => {
    const current = readBody(node.config)
    const body = checked ? [...current, bodyId] : current.filter((id) => id !== bodyId)
    const config = { ...node.config }
    if (body.length === 0) delete config['body']
    else config['body'] = body
    onConfigChange(config)
  }

  const setBackoff = (key: 'base' | 'multiplier' | 'max', raw: string) => {
    const trimmed = raw.trim()
    const config = { ...node.config }
    const backoff = { ...readBackoff(config) }
    backoff[key] = trimmed
    if (!backoff.base && !backoff.multiplier && !backoff.max) delete config['backoff']
    else {
      const nested: Record<string, number> = {}
      const base = Number(backoff.base)
      const multiplier = Number(backoff.multiplier)
      const max = Number(backoff.max)
      if (backoff.base !== '' && Number.isFinite(base)) nested['base_delay_ms'] = base
      if (backoff.multiplier !== '' && Number.isFinite(multiplier)) nested['multiplier'] = multiplier
      if (backoff.max !== '' && Number.isFinite(max)) nested['max_delay_ms'] = max
      config['backoff'] = nested
    }
    onConfigChange(config)
  }

  const loopOwnableNodes = allNodes.filter((candidate) => isLoopOwnable(candidate.kind) && candidate.id !== node.id)
  const otherNodes = allNodes.filter((candidate) => candidate.id !== node.id)
  const meta = kindMeta(node.kind)

  return (
    <>
      <div className="inspector-title">
        <h3>Node</h3>
        <code>{truncate(node.id, 18)}</code>
      </div>
      <div className="field">
        <label htmlFor="wfb-node-kind">Type</label>
        <input id="wfb-node-kind" value={meta.label} readOnly />
      </div>
      <div className="field">
        <label htmlFor="wfb-node-id">Node id</label>
        <input
          id="wfb-node-id"
          value={nextId}
          onChange={(event) => setNextId(event.target.value)}
          onBlur={() => {
            const error = onRename(nextId)
            setIdError(error)
            if (!error) setNextId(nextId.trim())
          }}
          spellCheck={false}
        />
        {idError ? <span className="form-note" style={{ color: '#fca5a5' }}>{idError}</span> : null}
      </div>
      <div className="field">
        <label htmlFor="wfb-node-name">Name</label>
        <input id="wfb-node-name" value={node.name} onChange={(event) => onNameChange(event.target.value)} placeholder={meta.label} />
      </div>

      {node.kind === 'agent' ? (
        <>
          <div className="field">
            <label htmlFor="wfb-agent-id">Agent id (config.agent_id)</label>
            <input id="wfb-agent-id" value={configRawString(node.config, 'agent_id')} onChange={(event) => setConfigKey('agent_id', event.target.value)} placeholder="agent uuid" spellCheck={false} />
          </div>
          <div className="field">
            <label htmlFor="wfb-agent-input">Input template (config.input)</label>
            <textarea id="wfb-agent-input" rows={3} value={configRawString(node.config, 'input')} onChange={(event) => setConfigKey('input', event.target.value)} placeholder="{{input}}" spellCheck={false} />
            <span className="form-note">{"Placeholders: {{input}} (workflow input), {{item}} (inside loop bodies)."}</span>
          </div>
        </>
      ) : null}

      {node.kind === 'tool' ? (
        <>
          <div className="field">
            <label htmlFor="wfb-tool-id">Tool id (config.tool_id)</label>
            <input id="wfb-tool-id" value={configRawString(node.config, 'tool_id')} onChange={(event) => setConfigKey('tool_id', event.target.value)} placeholder="tool uuid" spellCheck={false} />
          </div>
          <div className="field">
            <label htmlFor="wfb-tool-timeout">Timeout seconds (config.timeout_seconds)</label>
            <input id="wfb-tool-timeout" type="number" min={0} value={configNumberText(node.config, 'timeout_seconds')} onChange={(event) => setNumberKey('timeout_seconds', event.target.value)} placeholder="backend default" />
          </div>
          <div className="field">
            <label htmlFor="wfb-tool-input">Input template (config.input)</label>
            <textarea id="wfb-tool-input" rows={3} value={configRawString(node.config, 'input')} onChange={(event) => setConfigKey('input', event.target.value)} placeholder="{{input}}" spellCheck={false} />
          </div>
        </>
      ) : null}

      {node.kind === 'webhook' ? (
        <div className="field">
          <label htmlFor="wfb-webhook-url">URL (config.url)</label>
          <input id="wfb-webhook-url" value={configRawString(node.config, 'url')} onChange={(event) => setConfigKey('url', event.target.value)} placeholder="https://example.invalid/hook" spellCheck={false} />
        </div>
      ) : null}

      {node.kind === 'delay' ? (
        <div className="field">
          <label htmlFor="wfb-delay-seconds">Seconds (config.seconds)</label>
          <input id="wfb-delay-seconds" type="number" min={0} step={0.5} value={configNumberText(node.config, 'seconds')} onChange={(event) => setNumberKey('seconds', event.target.value)} />
        </div>
      ) : null}

      {node.kind === 'approval' ? (
        <div className="field">
          <label htmlFor="wfb-approval-reason">Reason (config.reason)</label>
          <input id="wfb-approval-reason" value={configRawString(node.config, 'reason')} onChange={(event) => setConfigKey('reason', event.target.value)} placeholder="Why does this need a human?" />
        </div>
      ) : null}

      {node.kind === 'condition' ? (
        <>
          <div className="field">
            <label htmlFor="wfb-condition-expr">Expression (config.expression)</label>
            <input id="wfb-condition-expr" value={configRawString(node.config, 'expression')} onChange={(event) => setConfigKey('expression', event.target.value)} placeholder="$.nodes.n1.output.ok == true" spellCheck={false} />
            <span className="form-note">{"Roots: $.nodes.<id>.output, $.input, $.item, $.loop, $.context.<key>."}</span>
          </div>
          <div className="field">
            <label htmlFor="wfb-branch-true">True branch → (config.branches.true)</label>
            <select id="wfb-branch-true" value={readBranches(node.config).true} onChange={(event) => setBranch('true', event.target.value)}>
              <option value="">— none —</option>
              {otherNodes.map((candidate) => (
                <option key={candidate.id} value={candidate.id}>{candidate.id}{candidate.name.trim() ? ` · ${truncate(candidate.name, 16)}` : ''}</option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="wfb-branch-false">False branch → (config.branches.false)</label>
            <select id="wfb-branch-false" value={readBranches(node.config).false} onChange={(event) => setBranch('false', event.target.value)}>
              <option value="">— none —</option>
              {otherNodes.map((candidate) => (
                <option key={candidate.id} value={candidate.id}>{candidate.id}{candidate.name.trim() ? ` · ${truncate(candidate.name, 16)}` : ''}</option>
              ))}
            </select>
          </div>
          <p className="inspector-note">Branch targets are kept in sync as on_success edges on the canvas.</p>
        </>
      ) : null}

      {node.kind === 'switch' ? (
        <>
          <div className="field">
            <label htmlFor="wfb-switch-expr">Expression (config.expression)</label>
            <input id="wfb-switch-expr" value={configRawString(node.config, 'expression')} onChange={(event) => setConfigKey('expression', event.target.value)} placeholder="$.context.tier" spellCheck={false} />
          </div>
          <div className="field">
            <label>Cases (config.cases: value → target)</label>
            {caseRows.map((row, index) => (
              <div className="case-row" key={index}>
                <input
                  value={row.value}
                  onChange={(event) => {
                    const rows = caseRows.map((item, i) => (i === index ? { ...item, value: event.target.value } : item))
                    setCaseRows(rows)
                    writeSwitch(rows, configRawString(node.config, 'default'))
                  }}
                  placeholder="value"
                  aria-label={`Case ${index + 1} value`}
                />
                <select
                  value={row.target}
                  onChange={(event) => {
                    const rows = caseRows.map((item, i) => (i === index ? { ...item, target: event.target.value } : item))
                    setCaseRows(rows)
                    writeSwitch(rows, configRawString(node.config, 'default'))
                  }}
                  aria-label={`Case ${index + 1} target`}
                >
                  <option value="">— none —</option>
                  {otherNodes.map((candidate) => (
                    <option key={candidate.id} value={candidate.id}>{candidate.id}</option>
                  ))}
                </select>
                <button
                  type="button"
                  className="remove-case"
                  aria-label={`Remove case ${index + 1}`}
                  onClick={() => {
                    const rows = caseRows.filter((_, i) => i !== index)
                    setCaseRows(rows)
                    writeSwitch(rows, configRawString(node.config, 'default'))
                  }}
                >
                  ×
                </button>
              </div>
            ))}
            <button
              type="button"
              className="ghost-button small"
              onClick={() => {
                const rows = [...caseRows, { value: '', target: '' }]
                setCaseRows(rows)
              }}
            >
              + Add case
            </button>
          </div>
          <div className="field">
            <label htmlFor="wfb-switch-default">Default target (config.default)</label>
            <select
              id="wfb-switch-default"
              value={configRawString(node.config, 'default')}
              onChange={(event) => {
                writeSwitch(caseRows, event.target.value)
              }}
            >
              <option value="">— none —</option>
              {otherNodes.map((candidate) => (
                <option key={candidate.id} value={candidate.id}>{candidate.id}</option>
              ))}
            </select>
          </div>
        </>
      ) : null}

      {node.kind === 'loop' ? (
        <>
          <div className="field">
            <label htmlFor="wfb-loop-expr">Expression over array (config.expression)</label>
            <input id="wfb-loop-expr" value={configRawString(node.config, 'expression')} onChange={(event) => setConfigKey('expression', event.target.value)} placeholder="$.input.items" spellCheck={false} />
          </div>
          <div className="field">
            <label htmlFor="wfb-loop-max">Max iterations (config.max_iterations)</label>
            <input id="wfb-loop-max" type="number" min={1} value={configNumberText(node.config, 'max_iterations')} onChange={(event) => setNumberKey('max_iterations', event.target.value)} placeholder="100" />
          </div>
          <div className="field">
            <label>Body nodes (config.body)</label>
            <div className="body-list">
              {loopOwnableNodes.length === 0 ? <span className="form-note">No agent/tool/delay/webhook/end nodes yet.</span> : null}
              {loopOwnableNodes.map((candidate) => (
                <label key={candidate.id}>
                  <input
                    type="checkbox"
                    checked={readBody(node.config).includes(candidate.id)}
                    onChange={(event) => setBody(candidate.id, event.target.checked)}
                  />
                  {candidate.id}{candidate.name.trim() ? ` · ${truncate(candidate.name, 14)}` : ''}
                </label>
              ))}
            </div>
            <span className="form-note">Edges INTO the loop node are its loop-back edges; body order follows config.body.</span>
          </div>
        </>
      ) : null}

      {node.kind === 'retry' ? (
        <>
          <div className="field">
            <label htmlFor="wfb-retry-target">Retry target (config.target)</label>
            <select id="wfb-retry-target" value={configRawString(node.config, 'target')} onChange={(event) => setConfigKey('target', event.target.value)}>
              <option value="">— none —</option>
              {loopOwnableNodes.map((candidate) => (
                <option key={candidate.id} value={candidate.id}>{candidate.id}{candidate.kind ? ` (${candidate.kind})` : ''}</option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="wfb-retry-attempts">Attempts (config.attempts)</label>
            <input id="wfb-retry-attempts" type="number" min={1} value={configNumberText(node.config, 'attempts')} onChange={(event) => setNumberKey('attempts', event.target.value)} placeholder="3" />
          </div>
          <div className="field">
            <label>Backoff (config.backoff)</label>
            <div className="inspector-row">
              <input type="number" min={0} aria-label="Base delay ms" placeholder="base ms" value={readBackoff(node.config).base} onChange={(event) => setBackoff('base', event.target.value)} />
              <input type="number" min={1} step={0.1} aria-label="Multiplier" placeholder="× multiplier" value={readBackoff(node.config).multiplier} onChange={(event) => setBackoff('multiplier', event.target.value)} />
            </div>
            <div className="inspector-row" style={{ marginTop: 6 }}>
              <input type="number" min={0} aria-label="Max delay ms" placeholder="max ms" value={readBackoff(node.config).max} onChange={(event) => setBackoff('max', event.target.value)} />
            </div>
            <span className="form-note">Defaults: 3 attempts · 100ms base · ×2 · 30s cap.</span>
          </div>
        </>
      ) : null}

      {node.kind === 'wait_until' ? (
        <>
          <div className="field">
            <label htmlFor="wfb-wait-expr">Expression (config.expression)</label>
            <input id="wfb-wait-expr" value={configRawString(node.config, 'expression')} onChange={(event) => setConfigKey('expression', event.target.value)} placeholder="$.nodes.n2.output.ready == true" spellCheck={false} />
          </div>
          <div className="inspector-row">
            <div className="field">
              <label htmlFor="wfb-wait-timeout">Timeout s</label>
              <input id="wfb-wait-timeout" type="number" min={0} value={configNumberText(node.config, 'timeout_seconds')} onChange={(event) => setNumberKey('timeout_seconds', event.target.value)} placeholder="60" />
            </div>
            <div className="field">
              <label htmlFor="wfb-wait-poll">Poll ms</label>
              <input id="wfb-wait-poll" type="number" min={0} value={configNumberText(node.config, 'poll_interval_ms')} onChange={(event) => setNumberKey('poll_interval_ms', event.target.value)} placeholder="1000" />
            </div>
          </div>
        </>
      ) : null}

      {node.kind === 'parallel' ? (
        <>
          <div className="field">
            <label htmlFor="wfb-parallel-strategy">Aggregation (config.strategy)</label>
            <select id="wfb-parallel-strategy" value={configRawString(node.config, 'strategy')} onChange={(event) => setConfigKey('strategy', event.target.value)}>
              <option value="">— backend default —</option>
              <option value="all">all</option>
              <option value="any">any</option>
              <option value="race">race</option>
              <option value="majority">majority</option>
            </select>
            <span className="form-note">Branches fan out over the node's outgoing edges.</span>
          </div>
          <div className="inspector-row">
            <div className="field">
              <label htmlFor="wfb-parallel-conc">Max concurrency</label>
              <input id="wfb-parallel-conc" type="number" min={1} value={configNumberText(node.config, 'max_concurrency')} onChange={(event) => setNumberKey('max_concurrency', event.target.value)} />
            </div>
            <div className="field">
              <label htmlFor="wfb-parallel-timeout">Branch timeout s</label>
              <input id="wfb-parallel-timeout" type="number" min={0} value={configNumberText(node.config, 'branch_timeout_seconds')} onChange={(event) => setNumberKey('branch_timeout_seconds', event.target.value)} />
            </div>
          </div>
        </>
      ) : null}

      {node.kind === 'end' ? <p className="inspector-note">Terminal marker — no configuration.</p> : null}

      {!isNodeKind(node.kind) ? (
        <>
          <p className="inspector-note">
            Type "{node.kind}" is not part of the DSL vocabulary. It round-trips through import/export, but the backend will reject it until the type is fixed.
          </p>
          <p className="preserved-keys">Preserved config keys: {Object.keys(node.config).join(', ') || '—'}</p>
        </>
      ) : null}

      <button type="button" className="ghost-button small" onClick={onDelete} style={{ color: '#fca5a5' }}>
        Delete node
      </button>
    </>
  )
}

type EdgeInspectorProps = {
  edge: BuilderEdge
  nodes: BuilderNode[]
  onConditionChange: (condition: string) => void
  onDelete: () => void
}

function EdgeInspector({ edge, nodes, onConditionChange, onDelete }: EdgeInspectorProps) {
  const source = nodes.find((n) => n.id === edge.from)
  const target = nodes.find((n) => n.id === edge.to)
  return (
    <>
      <div className="inspector-title">
        <h3>Edge</h3>
      </div>
      <div className="field">
        <label htmlFor="wfb-edge-from">From</label>
        <input id="wfb-edge-from" value={source ? `${source.id}${source.name.trim() ? ` · ${source.name}` : ''}` : edge.from} readOnly />
      </div>
      <div className="field">
        <label htmlFor="wfb-edge-to">To</label>
        <input id="wfb-edge-to" value={target ? `${target.id}${target.name.trim() ? ` · ${target.name}` : ''}` : edge.to} readOnly />
      </div>
      <div className="field">
        <label htmlFor="wfb-edge-condition">Condition (edge.condition)</label>
        <select id="wfb-edge-condition" value={edge.condition} onChange={(event) => onConditionChange(event.target.value)}>
          {EDGE_CONDITIONS.map((condition) => (
            <option key={condition} value={condition}>{condition}</option>
          ))}
        </select>
        <span className="form-note">on_failure / always matter after failing nodes; on_success is the default path.</span>
      </div>
      <button type="button" className="ghost-button small" onClick={onDelete} style={{ color: '#fca5a5' }}>
        Delete edge
      </button>
    </>
  )
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

export function WorkflowBuilderView({ canWrite }: { canWrite: boolean }) {
  const [nodes, setNodes] = useState<BuilderNode[]>([])
  const [edges, setEdges] = useState<BuilderEdge[]>([])
  const [viewport, setViewport] = useState<Viewport>({ x: 40, y: 40, zoom: 1 })
  const [selection, setSelection] = useState<Selection | null>(null)
  const [pendingLink, setPendingLink] = useState<string | null>(null)
  const [linkCursor, setLinkCursor] = useState<{ x: number; y: number } | null>(null)
  const [panning, setPanning] = useState(false)
  const [wfName, setWfName] = useState('')
  const [wfDescription, setWfDescription] = useState('')
  const [dslOpen, setDslOpen] = useState(false)
  const [dslText, setDslText] = useState('')
  const [dslNote, setDslNote] = useState<{ kind: 'ok' | 'error'; text: string } | null>(null)
  const [serverIssues, setServerIssues] = useState<WorkflowValidationIssue[] | null>(null)
  const [serverNote, setServerNote] = useState<{ kind: 'ok' | 'error'; text: string } | null>(null)
  const [validating, setValidating] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [savedWorkflow, setSavedWorkflow] = useState<{ id: string; name: string } | null>(null)

  const svgRef = useRef<SVGSVGElement | null>(null)
  const wrapRef = useRef<HTMLDivElement | null>(null)
  const gestureRef = useRef<Gesture | null>(null)
  const linkStartRef = useRef<{ x: number; y: number } | null>(null)
  const [wrapSize, setWrapSize] = useState({ width: 860, height: 560 })

  const createWorkflow = useCreateWorkflow()

  // --- geometry ------------------------------------------------------------
  // Geometry helpers close over the current viewport/wrap size instead of
  // mirror refs: they are only called from event handlers on the latest render.
  const toWorld = useCallback(
    (clientX: number, clientY: number): { x: number; y: number } => {
      const rect = svgRef.current?.getBoundingClientRect()
      if (!rect) return { x: 0, y: 0 }
      return { x: (clientX - rect.left - viewport.x) / viewport.zoom, y: (clientY - rect.top - viewport.y) / viewport.zoom }
    },
    [viewport],
  )

  const centerWorld = useCallback((): { x: number; y: number } => {
    return { x: (wrapSize.width / 2 - viewport.x) / viewport.zoom, y: (wrapSize.height / 2 - viewport.y) / viewport.zoom }
  }, [viewport, wrapSize])

  const fitTo = useCallback((target: BuilderNode[]) => {
    const rect = wrapRef.current?.getBoundingClientRect()
    if (!rect) return
    if (target.length === 0) {
      setViewport({ x: 40, y: 40, zoom: 1 })
      return
    }
    const bounds = worldBounds(target)
    const width = Math.max(1, bounds.maxX - bounds.minX)
    const height = Math.max(1, bounds.maxY - bounds.minY)
    const zoom = clamp(Math.min((rect.width - 90) / width, (rect.height - 90) / height), ZOOM_MIN, 1.5)
    setViewport({
      zoom,
      x: (rect.width - width * zoom) / 2 - bounds.minX * zoom,
      y: (rect.height - height * zoom) / 2 - bounds.minY * zoom,
    })
  }, [])

  useEffect(() => {
    const element = wrapRef.current
    if (!element) return
    const update = () => setWrapSize({ width: element.clientWidth, height: element.clientHeight })
    update()
    const observer = new ResizeObserver(update)
    observer.observe(element)
    return () => observer.disconnect()
  }, [])

  // Non-passive wheel handler: React's onWheel is passive, so zoom must
  // preventDefault through a native listener. Re-registered per viewport
  // change so the zoom anchor always uses the live transform.
  useEffect(() => {
    const svg = svgRef.current
    if (!svg) return
    const onWheel = (event: WheelEvent) => {
      event.preventDefault()
      const rect = svg.getBoundingClientRect()
      const next = clamp(viewport.zoom * Math.exp(-event.deltaY * 0.0015), ZOOM_MIN, ZOOM_MAX)
      if (next === viewport.zoom) return
      const px = event.clientX - rect.left
      const py = event.clientY - rect.top
      const wx = (px - viewport.x) / viewport.zoom
      const wy = (py - viewport.y) / viewport.zoom
      setViewport({ zoom: next, x: px - wx * next, y: py - wy * next })
    }
    svg.addEventListener('wheel', onWheel, { passive: false })
    return () => svg.removeEventListener('wheel', onWheel)
  }, [viewport])

  // --- graph mutations ------------------------------------------------------
  const addNode = (kind: NodeKind, world?: { x: number; y: number }) => {
    let maxSuffix = 0
    for (const node of nodes) {
      const match = /^n(\d+)$/.exec(node.id)
      if (match) maxSuffix = Math.max(maxSuffix, Number(match[1]))
    }
    const id = `n${maxSuffix + 1}`
    const position = world ?? centerWorld()
    const jitter = (nodes.length % 5) * 28
    setNodes((current) => [
      ...current,
      {
        id,
        kind,
        name: KIND_META[kind].label,
        config: defaultConfig(kind),
        x: snap(position.x - NODE_W / 2 + jitter),
        y: snap(position.y - NODE_H / 2 + jitter),
      },
    ])
    setSelection({ kind: 'node', id })
  }

  const deleteNode = useCallback((id: string) => {
    setNodes((current) => current.filter((node) => node.id !== id))
    setEdges((current) => current.filter((edge) => edge.from !== id && edge.to !== id))
  }, [])

  const deleteEdge = useCallback((key: string) => {
    setEdges((current) => current.filter((edge) => edge.key !== key))
  }, [])

  const addEdge = useCallback((from: string, to: string, condition: EdgeCondition = 'on_success') => {
    if (!from || !to || from === to) return
    const key = edgeKeyOf(from, to, condition)
    setEdges((current) => (current.some((edge) => edge.key === key) ? current : [...current, { key, from, to, condition }]))
  }, [])

  /** Keeps a condition/switch node's on_success edges equal to its configured routing targets. */
  const syncRoutingEdges = useCallback((from: string, targets: string[]) => {
    const wanted = new Set(targets.filter((target) => target && target !== from))
    setEdges((current) => {
      const kept = current.filter((edge) => !(edge.from === from && edge.condition === 'on_success' && !wanted.has(edge.to)))
      const missing = [...wanted].filter((target) => !kept.some((edge) => edge.from === from && edge.to === target))
      if (missing.length === 0) return kept
      return [...kept, ...missing.map((target) => ({ key: edgeKeyOf(from, target, 'on_success'), from, to: target, condition: 'on_success' }))]
    })
  }, [])

  const renameNode = useCallback(
    (from: string, nextIdRaw: string): string | null => {
      const nextId = nextIdRaw.trim()
      if (!nextId) return 'Node id is required.'
      if (nextId === from) return null
      if (nodes.some((node) => node.id === nextId)) return `Node id "${nextId}" is already taken.`
      setNodes((current) =>
        current.map((node) => (node.id === from ? { ...node, id: nextId, config: rewriteConfigRefs(node.config, from, nextId) } : { ...node, config: rewriteConfigRefs(node.config, from, nextId) })),
      )
      setEdges((current) =>
        current.map((edge) => {
          const fromId = edge.from === from ? nextId : edge.from
          const toId = edge.to === from ? nextId : edge.to
          return { ...edge, from: fromId, to: toId, key: edgeKeyOf(fromId, toId, edge.condition) }
        }),
      )
      setSelection((current) => (current?.kind === 'node' && current.id === from ? { kind: 'node', id: nextId } : current))
      return null
    },
    [nodes],
  )

  // --- pointer gestures ------------------------------------------------------

  const onCanvasPointerDown = (event: ReactPointerEvent<SVGSVGElement>) => {
    if (event.button !== 0) return
    svgRef.current?.setPointerCapture(event.pointerId)
    if (pendingLink) {
      setPendingLink(null)
      setLinkCursor(null)
      return
    }
    gestureRef.current = {
      mode: 'pan',
      startClientX: event.clientX,
      startClientY: event.clientY,
      startPanX: viewport.x,
      startPanY: viewport.y,
      moved: false,
    }
    setPanning(true)
  }

  const onNodePointerDown = (event: ReactPointerEvent<SVGGElement>, node: BuilderNode) => {
    if (event.button !== 0) return
    event.stopPropagation()
    svgRef.current?.setPointerCapture(event.pointerId)
    if (pendingLink && pendingLink !== node.id) {
      addEdge(pendingLink, node.id, 'on_success')
      setPendingLink(null)
      setLinkCursor(null)
      return
    }
    if (pendingLink === node.id) return
    const world = toWorld(event.clientX, event.clientY)
    gestureRef.current = { mode: 'drag', startWorldX: world.x, startWorldY: world.y, originX: node.x, originY: node.y, nodeId: node.id, moved: false }
    setSelection({ kind: 'node', id: node.id })
  }

  const onPortPointerDown = (event: ReactPointerEvent<SVGCircleElement>, node: BuilderNode) => {
    if (event.button !== 0) return
    event.stopPropagation()
    svgRef.current?.setPointerCapture(event.pointerId)
    if (pendingLink && pendingLink !== node.id) {
      addEdge(pendingLink, node.id, 'on_success')
      setPendingLink(null)
      setLinkCursor(null)
      return
    }
    if (pendingLink === node.id) return
    setPendingLink(node.id)
    setLinkCursor(toWorld(event.clientX, event.clientY))
    linkStartRef.current = { x: event.clientX, y: event.clientY }
  }

  const onEdgePointerDown = (event: ReactPointerEvent<SVGPathElement>, edge: BuilderEdge) => {
    if (event.button !== 0) return
    event.stopPropagation()
    setSelection({ kind: 'edge', key: edge.key })
  }

  const onCanvasPointerMove = (event: ReactPointerEvent<SVGSVGElement>) => {
    if (pendingLink) setLinkCursor(toWorld(event.clientX, event.clientY))
    const gesture = gestureRef.current
    if (!gesture) return
    if (gesture.mode === 'pan') {
      const dx = event.clientX - gesture.startClientX
      const dy = event.clientY - gesture.startClientY
      if (Math.abs(dx) + Math.abs(dy) > 3) gesture.moved = true
      setViewport((vp) => ({ ...vp, x: gesture.startPanX + dx, y: gesture.startPanY + dy }))
      return
    }
    const world = toWorld(event.clientX, event.clientY)
    const dx = world.x - gesture.startWorldX
    const dy = world.y - gesture.startWorldY
    if (Math.abs(dx) + Math.abs(dy) > 2) gesture.moved = true
    setNodes((current) =>
      current.map((node) => (node.id === gesture.nodeId ? { ...node, x: gesture.originX + dx, y: gesture.originY + dy } : node)),
    )
  }

  const onCanvasPointerUp = (event: ReactPointerEvent<SVGSVGElement>) => {
    const gesture = gestureRef.current
    gestureRef.current = null
    setPanning(false)
    if (gesture?.mode === 'pan' && !gesture.moved && !pendingLink) setSelection(null)
    if (!pendingLink) return
    const under = document.elementFromPoint(event.clientX, event.clientY)
    const targetId = under?.closest('[data-wfb-node]')?.getAttribute('data-wfb-node') ?? null
    if (targetId && targetId !== pendingLink) {
      addEdge(pendingLink, targetId, 'on_success')
      setPendingLink(null)
      setLinkCursor(null)
      return
    }
    if (!targetId) {
      const start = linkStartRef.current
      const distance = start ? Math.hypot(event.clientX - start.x, event.clientY - start.y) : Number.POSITIVE_INFINITY
      if (distance > 5) {
        // drag released over empty canvas — cancel the pending link
        setPendingLink(null)
        setLinkCursor(null)
      }
      // else: a plain click on the port keeps the link armed (click → click mode)
    }
  }

  // --- keyboard --------------------------------------------------------------

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null
      if (target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.tagName === 'SELECT' || target.isContentEditable)) return
      if (event.key === 'Escape') {
        setPendingLink(null)
        setLinkCursor(null)
        setSelection(null)
        return
      }
      if (event.key !== 'Delete' && event.key !== 'Backspace') return
      if (pendingLink) return
      if (selection?.kind === 'node') {
        deleteNode(selection.id)
        setSelection(null)
      } else if (selection?.kind === 'edge') {
        deleteEdge(selection.key)
        setSelection(null)
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [selection, pendingLink, deleteNode, deleteEdge])

  // --- palette + toolbar actions ----------------------------------------------

  const onPaletteDrop = (event: ReactDragEvent<HTMLDivElement>) => {
    const kind = event.dataTransfer.getData('application/x-agentos-node-kind')
    if (!isNodeKind(kind)) return
    event.preventDefault()
    addNode(kind, toWorld(event.clientX, event.clientY))
  }

  const autoLayout = () => {
    const positions = layoutPositions(nodes.map((n) => ({ id: n.id, type: n.kind })), edges)
    setNodes((current) => current.map((node) => ({ ...node, x: positions.get(node.id)?.x ?? node.x, y: positions.get(node.id)?.y ?? node.y })))
  }

  const zoomBy = (factor: number) => {
    const next = clamp(viewport.zoom * factor, ZOOM_MIN, ZOOM_MAX)
    const cx = wrapSize.width / 2
    const cy = wrapSize.height / 2
    const wx = (cx - viewport.x) / viewport.zoom
    const wy = (cy - viewport.y) / viewport.zoom
    setViewport({ zoom: next, x: cx - wx * next, y: cy - wy * next })
  }

  // --- validation --------------------------------------------------------------

  const clientIssues = useMemo(() => validateCanvas(nodes, edges), [nodes, edges])

  const serverBuilderIssues = useMemo<BuilderIssue[]>(
    () => (serverIssues ?? []).map((issue) => ({ code: issue.code, message: issue.message, nodeId: issue.nodeId, severity: 'error', source: 'server' })),
    [serverIssues],
  )
  const allIssues = useMemo(() => [...clientIssues, ...serverBuilderIssues], [clientIssues, serverBuilderIssues])
  const errorCount = allIssues.filter((issue) => issue.severity === 'error').length
  const errorNodeIds = useMemo(() => new Set(allIssues.filter((issue) => issue.severity === 'error' && issue.nodeId).map((issue) => issue.nodeId as string)), [allIssues])
  const errorEdgeKeys = useMemo(() => new Set(allIssues.filter((issue) => issue.severity === 'error' && issue.edgeKey).map((issue) => issue.edgeKey as string)), [allIssues])

  const focusIssue = (issue: BuilderIssue) => {
    if (issue.nodeId && nodes.some((node) => node.id === issue.nodeId)) setSelection({ kind: 'node', id: issue.nodeId })
    else if (issue.edgeKey && edges.some((edge) => edge.key === issue.edgeKey)) setSelection({ kind: 'edge', key: issue.edgeKey })
  }

  const runServerValidation = async () => {
    if (validating) return
    setValidating(true)
    setServerNote(null)
    try {
      await validateWorkflowDsl(toDsl(nodes, edges))
      setServerIssues([])
      setServerNote({ kind: 'ok', text: 'Server validation passed — POST /workflows/validate accepted this definition.' })
    } catch (error) {
      const issues = extractValidationIssues(error)
      if (issues.length > 0) {
        setServerIssues(issues)
        setServerNote(null)
      } else {
        setServerIssues(null)
        setServerNote({ kind: 'error', text: `Server validation failed: ${describeError(error)}` })
      }
    } finally {
      setValidating(false)
    }
  }

  // --- save ---------------------------------------------------------------

  const saveDraft = () => {
    if (!canWrite || createWorkflow.isPending) return
    setSaveError(null)
    const blocking = clientIssues.filter((issue) => issue.severity === 'error')
    if (blocking.length > 0) {
      setSaveError(`Fix ${blocking.length} validation ${blocking.length === 1 ? 'error' : 'errors'} before saving — see the list on the right.`)
      return
    }
    if (!wfName.trim()) {
      setSaveError('Workflow name is required.')
      return
    }
    createWorkflow.mutate(
      { name: wfName.trim(), description: wfDescription.trim(), dsl: toDsl(nodes, edges) },
      {
        onSuccess: (workflow) => {
          setSavedWorkflow({ id: workflow.id, name: workflow.name })
          setSaveError(null)
        },
        onError: (error) => {
          const issues = extractValidationIssues(error)
          if (issues.length > 0) {
            setServerIssues(issues)
            setServerNote(null)
            setSaveError('The backend rejected this definition — see the server errors in the list.')
          } else {
            setSaveError(describeError(error))
          }
        },
      },
    )
  }

  // --- DSL round-trip --------------------------------------------------------

  const openDslDrawer = () => {
    setDslText(JSON.stringify(toDsl(nodes, edges), null, 2))
    setDslNote(null)
    setDslOpen(true)
  }

  const applyDsl = () => {
    let parsed: unknown
    try {
      parsed = JSON.parse(dslText)
    } catch (error) {
      setDslNote({ kind: 'error', text: `Invalid JSON: ${error instanceof Error ? error.message : 'parse failed'}` })
      return
    }
    const record = isRecord(parsed) ? parsed : null
    const raw = record && isRecord(record['dsl']) ? record['dsl'] : parsed
    if (!isRecord(raw) || !Array.isArray(raw['nodes'])) {
      setDslNote({ kind: 'error', text: 'Expected a DSL object with "nodes" and "edges" arrays (or {"dsl": {...}}).' })
      return
    }
    const nodesRaw = (raw['nodes'] as unknown[]).filter(isRecord)
    const edgesRaw = Array.isArray(raw['edges']) ? (raw['edges'] as unknown[]).filter(isRecord) : []
    const dsl: WorkflowDsl = {
      nodes: nodesRaw.map((node) => ({
        id: typeof node['id'] === 'string' ? node['id'] : '',
        type: typeof node['type'] === 'string' ? node['type'].toLowerCase() : '',
        name: typeof node['name'] === 'string' ? node['name'] : undefined,
        config: isRecord(node['config']) ? { ...node['config'] } : undefined,
      })),
      edges: edgesRaw.map((edge) => ({
        from: typeof edge['from'] === 'string' ? edge['from'] : typeof edge['source'] === 'string' ? (edge['source'] as string) : '',
        to: typeof edge['to'] === 'string' ? edge['to'] : typeof edge['target'] === 'string' ? (edge['target'] as string) : '',
        condition: typeof edge['condition'] === 'string' ? edge['condition'] : undefined,
      })),
    }
    const { builderNodes, builderEdges } = fromDsl(dsl)
    setNodes(builderNodes)
    setEdges(builderEdges)
    setSelection(null)
    setServerIssues(null)
    setServerNote(null)
    setDslNote({ kind: 'ok', text: `Loaded ${builderNodes.length} nodes and ${builderEdges.length} edges from the DSL.` })
    fitTo(builderNodes)
  }

  const copyDsl = async () => {
    try {
      await navigator.clipboard.writeText(dslText)
      setDslNote({ kind: 'ok', text: 'DSL copied to the clipboard.' })
    } catch {
      setDslNote({ kind: 'error', text: 'Clipboard unavailable — select the text and copy manually.' })
    }
  }

  const downloadDsl = () => {
    downloadText(`${(wfName.trim() || 'workflow').toLowerCase().replace(/[^a-z0-9-_]+/g, '-')}.dsl.json`, 'application/json', JSON.stringify(toDsl(nodes, edges), null, 2))
    setDslNote({ kind: 'ok', text: 'DSL downloaded.' })
  }

  const loadDslFile = async (file: File) => {
    const text = await file.text()
    setDslText(text)
    setDslNote({ kind: 'ok', text: `Loaded "${file.name}" into the editor — press Apply to replace the canvas.` })
  }

  const exportSvg = () => {
    const svg = svgRef.current
    if (!svg) return
    const content = serializeCanvasSvg(svg, nodes)
    if (!content) {
      setDslNote({ kind: 'error', text: 'Nothing to export — add nodes first.' })
      return
    }
    downloadText(`${(wfName.trim() || 'workflow').toLowerCase().replace(/[^a-z0-9-_]+/g, '-')}.svg`, 'image/svg+xml', content)
    setDslNote({ kind: 'ok', text: 'Canvas exported as SVG.' })
  }

  // --- minimap ---------------------------------------------------------------

  const minimap = useMemo(() => {
    const pad = 10
    const bounds = nodes.length > 0 ? worldBounds(nodes) : { minX: 0, minY: 0, maxX: 240, maxY: 160 }
    const viewX = -viewport.x / viewport.zoom
    const viewY = -viewport.y / viewport.zoom
    const viewW = wrapSize.width / viewport.zoom
    const viewH = wrapSize.height / viewport.zoom
    const minX = Math.min(bounds.minX, viewX) - pad
    const minY = Math.min(bounds.minY, viewY) - pad
    const maxX = Math.max(bounds.maxX, viewX + viewW) + pad
    const maxY = Math.max(bounds.maxY, viewY + viewH) + pad
    const scale = Math.min(MINI_W / (maxX - minX), MINI_H / (maxY - minY))
    return {
      minX,
      minY,
      scale,
      offsetX: (MINI_W - (maxX - minX) * scale) / 2,
      offsetY: (MINI_H - (maxY - minY) * scale) / 2,
      view: { x: viewX, y: viewY, w: viewW, h: viewH },
    }
  }, [nodes, viewport, wrapSize])

  const onMinimapClick = (event: ReactMouseEvent<SVGSVGElement>) => {
    const rect = event.currentTarget.getBoundingClientRect()
    const wx = (event.clientX - rect.left - minimap.offsetX) / minimap.scale + minimap.minX
    const wy = (event.clientY - rect.top - minimap.offsetY) / minimap.scale + minimap.minY
    setViewport((vp) => ({ ...vp, x: wrapSize.width / 2 - wx * vp.zoom, y: wrapSize.height / 2 - wy * vp.zoom }))
  }

  const toMini = (wx: number, wy: number) => ({
    x: (wx - minimap.minX) * minimap.scale + minimap.offsetX,
    y: (wy - minimap.minY) * minimap.scale + minimap.offsetY,
  })

  // --- render ---------------------------------------------------------------

  const selectedNode = selection?.kind === 'node' ? nodes.find((node) => node.id === selection.id) ?? null : null
  const selectedEdge = selection?.kind === 'edge' ? edges.find((edge) => edge.key === selection.key) ?? null : null
  const linkSource = pendingLink ? nodes.find((node) => node.id === pendingLink) ?? null : null

  return (
    <>
      <PageHeader
        eyebrow="Automation"
        title="Workflow Builder"
        actions={
          <span className="form-note">
            Drag-to-connect ports · scroll to zoom · Del removes the selection
          </span>
        }
      />

      <div className="builder-grid">
        {/* ---- palette ---- */}
        <aside className="builder-panel builder-palette">
          <h3>Node palette</h3>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12, overflowY: 'auto' }}>
            {PALETTE_GROUPS.map((group) => (
              <div className="builder-palette-group" key={group.label}>
                <p>{group.label}</p>
                {group.kinds.map((kind) => {
                  const meta = KIND_META[kind]
                  return (
                    <button
                      key={kind}
                      type="button"
                      className="palette-item"
                      draggable
                      onDragStart={(event: ReactDragEvent<HTMLButtonElement>) => {
                        event.dataTransfer.setData('application/x-agentos-node-kind', kind)
                        event.dataTransfer.effectAllowed = 'copy'
                      }}
                      onClick={() => addNode(kind)}
                      title={`Click to add at viewport center, or drag onto the canvas`}
                    >
                      <span className="palette-dot" style={{ background: meta.stroke }} aria-hidden="true" />
                      <span className="palette-text">
                        <strong>{meta.label}</strong>
                        <span>{meta.hint}</span>
                      </span>
                    </button>
                  )
                })}
              </div>
            ))}
          </div>
          <div className="builder-identity">
            <h3>Definition</h3>
            <div className="field">
              <label htmlFor="wfb-name">Workflow name</label>
              <input id="wfb-name" value={wfName} onChange={(event) => setWfName(event.target.value)} placeholder="Support triage pipeline" />
            </div>
            <div className="field">
              <label htmlFor="wfb-desc">Description</label>
              <input id="wfb-desc" value={wfDescription} onChange={(event) => setWfDescription(event.target.value)} placeholder="What does this workflow automate?" />
            </div>
            {!canWrite ? <p className="builder-hint">Viewer role — saving workflows needs MEMBER and above.</p> : null}
            <p className="builder-hint">
              Node types follow the backend DSL: agent · tool · webhook · condition · switch · parallel · loop · retry · wait_until · approval · delay · end.
            </p>
          </div>
        </aside>

        {/* ---- canvas ---- */}
        <section className="builder-panel builder-canvas-panel">
          <div className="builder-toolbar">
            <button type="button" className="ghost-button small" onClick={() => zoomBy(1 / 1.2)} aria-label="Zoom out">−</button>
            <span className="builder-stat">{Math.round(viewport.zoom * 100)}%</span>
            <button type="button" className="ghost-button small" onClick={() => zoomBy(1.2)} aria-label="Zoom in">+</button>
            <button type="button" className="ghost-button small" onClick={() => fitTo(nodes)}>Fit</button>
            <button type="button" className="ghost-button small" onClick={autoLayout}>Auto layout</button>
            <span className="spacer" />
            <span className="builder-stat">
              {nodes.length} nodes · {edges.length} edges ·{' '}
              {errorCount === 0 ? <span style={{ color: '#6ee7b7' }}>valid</span> : <span style={{ color: '#fca5a5' }}>{errorCount} error{errorCount === 1 ? '' : 's'}</span>}
            </span>
            <button type="button" className="ghost-button small" onClick={() => void runServerValidation()} disabled={validating}>
              {validating ? 'Validating…' : 'Validate on server'}
            </button>
            <button type="button" className="ghost-button small" onClick={dslOpen ? () => setDslOpen(false) : openDslDrawer}>
              {dslOpen ? 'Hide DSL' : 'DSL / Import'}
            </button>
            <button type="button" className="ghost-button small" onClick={exportSvg}>Export SVG</button>
            <button
              type="button"
              className="primary-button small"
              onClick={saveDraft}
              disabled={!canWrite || createWorkflow.isPending}
              title={canWrite ? 'Create this workflow as a draft (POST /workflows/create)' : 'Viewer role — saving needs MEMBER and above'}
            >
              {createWorkflow.isPending ? 'Saving…' : 'Save draft'}
            </button>
          </div>

          {dslOpen ? (
            <div className="builder-drawer">
              <div className="row">
                <strong style={{ fontSize: '0.8rem' }}>Workflow DSL (JSON)</strong>
                <span className="spacer" style={{ flex: 1 }} />
                <button type="button" className="link-button" onClick={() => setDslText(JSON.stringify(toDsl(nodes, edges), null, 2))}>Refresh from canvas</button>
                <button type="button" className="link-button" onClick={applyDsl}>Apply to canvas</button>
                <button type="button" className="link-button" onClick={() => void copyDsl()}>Copy</button>
                <button type="button" className="link-button" onClick={downloadDsl}>Download .json</button>
                <label className="builder-file-label">
                  Load file
                  <input
                    type="file"
                    accept="application/json,.json"
                    onChange={(event) => {
                      const file = event.target.files?.[0]
                      if (file) void loadDslFile(file)
                      event.target.value = ''
                    }}
                  />
                </label>
              </div>
              <textarea
                rows={10}
                value={dslText}
                onChange={(event) => setDslText(event.target.value)}
                spellCheck={false}
                aria-label="Workflow DSL JSON"
              />
              {dslNote ? <span className="form-note" style={{ color: dslNote.kind === 'ok' ? '#6ee7b7' : '#fca5a5' }}>{dslNote.text}</span> : null}
            </div>
          ) : null}

          {saveError ? <div className="form-error">{saveError}</div> : null}
          {savedWorkflow ? (
            <div className="form-error" style={{ borderColor: 'rgba(110,231,183,0.4)', background: 'rgba(16,185,129,0.1)', color: '#6ee7b7' }}>
              Saved "{savedWorkflow.name}" as draft <code>{savedWorkflow.id}</code> — manage versions and executions in the Workflows view.
            </div>
          ) : null}
          {serverNote ? (
            <div className="form-error" style={serverNote.kind === 'ok' ? { borderColor: 'rgba(110,231,183,0.4)', background: 'rgba(16,185,129,0.1)', color: '#6ee7b7' } : undefined}>
              {serverNote.text}
            </div>
          ) : null}

          <div
            className="builder-canvas-wrap"
            ref={wrapRef}
            onDragOver={(event: ReactDragEvent<HTMLDivElement>) => {
              event.preventDefault()
              event.dataTransfer.dropEffect = 'copy'
            }}
            onDrop={onPaletteDrop}
          >
            <svg
              ref={svgRef}
              className={`builder-canvas${panning ? ' panning' : ''}${pendingLink ? ' linking' : ''}`}
              role="application"
              aria-label="Workflow canvas"
              onPointerDown={onCanvasPointerDown}
              onPointerMove={onCanvasPointerMove}
              onPointerUp={onCanvasPointerUp}
            >
              <defs>
                <pattern id="wfb-grid" width={GRID} height={GRID} patternUnits="userSpaceOnUse" patternTransform={`translate(${viewport.x} ${viewport.y}) scale(${viewport.zoom})`}>
                  <circle cx={1.1} cy={1.1} r={1.1} fill="rgba(148,163,184,0.22)" />
                </pattern>
                {(['default', 'selected', 'error'] as const).map((variant) => (
                  <marker key={variant} id={`wfb-arrow-${variant}`} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                    <path
                      d="M 0 1 L 9 5 L 0 9 z"
                      fill={variant === 'selected' ? '#7dd3fc' : variant === 'error' ? '#f87171' : 'rgba(148,163,184,0.75)'}
                    />
                  </marker>
                ))}
              </defs>

              <rect width="100%" height="100%" fill="url(#wfb-grid)" />

              <g transform={`translate(${viewport.x} ${viewport.y}) scale(${viewport.zoom})`}>
                {edges.map((edge) => {
                  const from = nodes.find((node) => node.id === edge.from)
                  const to = nodes.find((node) => node.id === edge.to)
                  if (!from || !to) return null
                  const selected = selection?.kind === 'edge' && selection.key === edge.key
                  const errored = errorEdgeKeys.has(edge.key)
                  const stroke = selected ? '#7dd3fc' : errored ? '#f87171' : 'rgba(148,163,184,0.6)'
                  return (
                    <g key={edge.key}>
                      <path
                        d={edgePath(from, to)}
                        fill="none"
                        stroke={stroke}
                        strokeWidth={selected || errored ? 2.2 : 1.6}
                        markerEnd={`url(#wfb-arrow-${errored ? 'error' : selected ? 'selected' : 'default'})`}
                      />
                      {edge.condition !== 'on_success' ? (
                        <text
                          x={(nodePort(from, 'out').x + nodePort(to, 'in').x) / 2}
                          y={(nodePort(from, 'out').y + nodePort(to, 'in').y) / 2 - 6}
                          textAnchor="middle"
                          fontSize={9}
                          fill={stroke}
                          style={{ pointerEvents: 'none' }}
                        >
                          {edge.condition}
                        </text>
                      ) : null}
                      <path
                        d={edgePath(from, to)}
                        fill="none"
                        stroke="transparent"
                        strokeWidth={14}
                        className="builder-edge-hit"
                        data-wfb-interactive="true"
                        onPointerDown={(event) => onEdgePointerDown(event, edge)}
                      />
                    </g>
                  )
                })}

                {pendingLink && linkSource ? (
                  <path
                    d={(() => {
                      const start = nodePort(linkSource, 'out')
                      const end = linkCursor ?? start
                      const dx = Math.max(44, Math.abs(end.x - start.x) / 2)
                      return `M ${start.x} ${start.y} C ${start.x + dx} ${start.y}, ${end.x - dx} ${end.y}, ${end.x} ${end.y}`
                    })()}
                    fill="none"
                    stroke="#7dd3fc"
                    strokeWidth={2}
                    strokeDasharray="6 4"
                    data-wfb-interactive="true"
                    style={{ pointerEvents: 'none' }}
                  />
                ) : null}

                {nodes.map((node) => {
                  const meta = kindMeta(node.kind)
                  const selected = selectedNode?.id === node.id
                  const errored = errorNodeIds.has(node.id)
                  const out = nodePort(node, 'out')
                  const inset = nodePort(node, 'in')
                  return (
                    <g
                      key={node.id}
                      className={`builder-node${selected ? ' selected' : ''}`}
                      data-wfb-node={node.id}
                      onPointerDown={(event) => onNodePointerDown(event, node)}
                    >
                      <rect
                        className="node-body"
                        x={node.x}
                        y={node.y}
                        width={NODE_W}
                        height={NODE_H}
                        rx={12}
                        fill={meta.fill}
                        stroke={errored ? '#f87171' : selected ? '#7dd3fc' : meta.stroke}
                        strokeWidth={errored || selected ? 2 : 1.3}
                      />
                      <text x={node.x + 12} y={node.y + 19} fontSize={9} letterSpacing={1.4} fill={meta.stroke} style={{ textTransform: 'uppercase' }}>
                        {truncate(meta.label.toUpperCase(), 18)}
                      </text>
                      <text x={node.x + 12} y={node.y + 38} fontSize={12.5} fill="#e5ecff">
                        {truncate(displayName(node), 22)}
                      </text>
                      <circle cx={inset.x} cy={inset.y} r={4.5} fill={pendingLink === node.id ? '#7dd3fc' : '#94a3b8'} stroke="#0c1220" strokeWidth={1.5} />
                      <circle cx={out.x} cy={out.y} r={4.5} fill="#94a3b8" stroke="#0c1220" strokeWidth={1.5} />
                      <circle
                        cx={inset.x}
                        cy={inset.y}
                        r={11}
                        fill="transparent"
                        className="builder-port"
                        data-wfb-interactive="true"
                        onPointerDown={(event) => onPortPointerDown(event, node)}
                      />
                      <circle
                        cx={out.x}
                        cy={out.y}
                        r={11}
                        fill="transparent"
                        className="builder-port"
                        data-wfb-interactive="true"
                        onPointerDown={(event) => onPortPointerDown(event, node)}
                      />
                    </g>
                  )
                })}
              </g>
            </svg>

            {nodes.length === 0 ? (
              <div className="builder-canvas-empty">
                <div style={{ textAlign: 'center' }}>
                  <p style={{ margin: '0 0 6px', fontWeight: 600, color: '#c7d2fe' }}>Canvas is empty</p>
                  <p style={{ margin: 0, fontSize: '0.8rem' }}>Click a palette node to add it, or drag one onto the canvas. Connect ports to build the DAG.</p>
                </div>
              </div>
            ) : null}

            {nodes.length > 0 ? (
              <svg className="builder-minimap" onClick={onMinimapClick} aria-label="Minimap">
                {nodes.map((node) => {
                  const p = toMini(node.x, node.y)
                  const meta = kindMeta(node.kind)
                  return (
                    <rect
                      key={node.id}
                      x={p.x}
                      y={p.y}
                      width={Math.max(3, NODE_W * minimap.scale)}
                      height={Math.max(2, NODE_H * minimap.scale)}
                      rx={1.5}
                      fill={errorNodeIds.has(node.id) ? '#f87171' : meta.stroke}
                      opacity={0.75}
                    />
                  )
                })}
                <rect
                  x={(minimap.view.x - minimap.minX) * minimap.scale + minimap.offsetX}
                  y={(minimap.view.y - minimap.minY) * minimap.scale + minimap.offsetY}
                  width={minimap.view.w * minimap.scale}
                  height={minimap.view.h * minimap.scale}
                  fill="rgba(125,211,252,0.08)"
                  stroke="#7dd3fc"
                  strokeWidth={1}
                  rx={2}
                />
              </svg>
            ) : null}
          </div>
        </section>

        {/* ---- inspector ---- */}
        <aside className="builder-panel builder-inspector">
          {selectedNode ? (
            <NodeInspector
              key={selectedNode.id}
              node={selectedNode}
              allNodes={nodes}
              onNameChange={(name) => setNodes((current) => current.map((node) => (node.id === selectedNode.id ? { ...node, name } : node)))}
              onRename={(nextId) => renameNode(selectedNode.id, nextId)}
              onConfigChange={(config, routingTargets) => {
                setNodes((current) => current.map((node) => (node.id === selectedNode.id ? { ...node, config } : node)))
                if (routingTargets) syncRoutingEdges(selectedNode.id, routingTargets)
              }}
              onDelete={() => {
                deleteNode(selectedNode.id)
                setSelection(null)
              }}
            />
          ) : selectedEdge ? (
            <EdgeInspector
              edge={selectedEdge}
              nodes={nodes}
              onConditionChange={(condition) => {
                if (!(EDGE_CONDITIONS as readonly string[]).includes(condition)) return
                setEdges((current) =>
                  current.map((edge) => (edge.key === selectedEdge.key ? { ...edge, condition, key: edgeKeyOf(edge.from, edge.to, condition) } : edge)),
                )
                setSelection({ kind: 'edge', key: edgeKeyOf(selectedEdge.from, selectedEdge.to, condition) })
              }}
              onDelete={() => {
                deleteEdge(selectedEdge.key)
                setSelection(null)
              }}
            />
          ) : (
            <>
              <h3>Inspector</h3>
              <p className="builder-hint">
                Select a node or an edge to edit its DSL configuration. Click a source port then a target (or drag between ports) to connect nodes; Delete/Backspace removes the selection.
              </p>
            </>
          )}

          <div className="builder-validation" style={{ marginTop: 'auto', paddingTop: 12, borderTop: '1px solid rgba(148,163,184,0.14)' }}>
            <h3>Validation</h3>
            {allIssues.length === 0 ? (
              <div className="validation-item ok">No issues — the graph satisfies the DSL rules.</div>
            ) : (
              allIssues.map((issue, index) => (
                <button
                  key={`${issue.source}-${issue.code}-${issue.nodeId ?? issue.edgeKey ?? 'issue'}-${index}`}
                  type="button"
                  className={`validation-item ${issue.severity}`}
                  onClick={() => focusIssue(issue)}
                >
                  <em>{issue.source}</em>
                  <code>{issue.nodeId ?? issue.edgeKey ?? issue.code}</code>
                  <span>{issue.message}</span>
                </button>
              ))
            )}
          </div>
        </aside>
      </div>
    </>
  )
}
