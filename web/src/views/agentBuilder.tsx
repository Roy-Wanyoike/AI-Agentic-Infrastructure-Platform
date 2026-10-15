// Visual Agent Builder (issue #121) — guided 8-step creation wizard with a
// template library, a JSON-aware config diff view, and a live test-run
// preview that streams POST /runs events through the existing SSE hooks.
//
// Honesty notes (no-silent-mock rule):
// - POST /agents/create persists exactly name, description, instructions,
//   model. The wizard's tool / memory / policy / limit sections build a
//   "blueprint" that is reviewed side-by-side at deploy time and labelled as
//   design-time configuration (the API does not accept those fields yet).
// - The backend has no model-catalog endpoint, so Model Selection combines
//   free text + known presets + models already in use in the org (derived
//   from GET /agents); the API validates the id server-side (UNKNOWN_MODEL).

import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import {
  useAgents,
  useCreateAgent,
  useKnowledgeDocuments,
  useRun,
  useRunAgent,
  useRunEvents,
  useTools,
} from '../lib/hooks'
import { eventOutput, eventStatus, type Agent, type RunEvent } from '../lib/api/types'
import { formatNumber, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError, type ViewName } from './uiHelpers'

// ---------------------------------------------------------------------------
// Wizard draft model + templates
// ---------------------------------------------------------------------------

type AgentDraft = {
  name: string
  description: string
  model: string
  instructions: string
  /** Registry tool names picked on the Tool Selection step. */
  tools: string[]
  /** Execution allowlist — subset of `tools` (Permissions step). */
  toolAllowlist: string[]
  memoryEnabled: boolean
  /** Knowledge document ids the agent may read (Memory/Knowledge step). */
  knowledgeDocIds: string[]
  requireApproval: boolean
  budgetCentsPerRun: number
  monthlyBudgetCents: number
  maxSteps: number
  stepTimeoutSeconds: number
  runTimeoutSeconds: number
}

const EMPTY_DRAFT: AgentDraft = {
  name: '',
  description: '',
  model: '',
  instructions: '',
  tools: [],
  toolAllowlist: [],
  memoryEnabled: false,
  knowledgeDocIds: [],
  requireApproval: false,
  budgetCentsPerRun: 0,
  monthlyBudgetCents: 0,
  maxSteps: 12,
  stepTimeoutSeconds: 60,
  runTimeoutSeconds: 600,
}

/** Known-good model ids (mirrors the platform catalog examples in the API). */
const MODEL_PRESETS = ['gpt-4o-mini', 'gpt-4o', 'claude-3-7-sonnet', 'llama-3.1-70b']

type Template = {
  id: string
  name: string
  glyph: string
  blurb: string
  draft: Partial<AgentDraft>
}

const TEMPLATES: Template[] = [
  {
    id: 'research',
    name: 'Research Analyst',
    glyph: '❖',
    blurb: 'Deep-dives a topic, cites sources, returns structured briefings.',
    draft: {
      name: 'Research analyst',
      description: 'Researches a topic and returns a structured briefing with sources.',
      model: 'gpt-4o',
      instructions:
        'You are a meticulous research analyst.\n' +
        '1. Decompose the request into sub-questions.\n' +
        '2. Gather evidence with the available tools before answering.\n' +
        '3. Return a briefing: Summary, Key findings, Sources, Open questions.\n' +
        'Cite every factual claim. Flag uncertainty instead of guessing.',
      tools: ['web_search', 'http_request'],
      memoryEnabled: true,
      maxSteps: 16,
    },
  },
  {
    id: 'code',
    name: 'Code Assistant',
    glyph: '⚒',
    blurb: 'Reads, explains, and edits code; runs tools to verify changes.',
    draft: {
      name: 'Code assistant',
      description: 'Reviews and edits code, explains changes, verifies with tools.',
      model: 'claude-3-7-sonnet',
      instructions:
        'You are a senior software engineer.\n' +
        'Read the relevant files before proposing changes.\n' +
        'Prefer minimal, focused diffs. Explain the reasoning for each change.\n' +
        'Run the project test suite when a verification tool is available.',
      tools: ['http_request'],
      requireApproval: true,
      maxSteps: 20,
    },
  },
  {
    id: 'support',
    name: 'Customer Support',
    glyph: '✓',
    blurb: 'Triages tickets with a tight tone guide and escalation rules.',
    draft: {
      name: 'Support triage',
      description: 'Triages inbound support questions and drafts replies.',
      model: 'gpt-4o-mini',
      instructions:
        'You are a friendly support agent.\n' +
        'Answer only from the knowledge documents provided; never invent policy.\n' +
        'Tone: concise, warm, no jargon.\n' +
        'If the request is about billing disputes or security, escalate to a human.',
      tools: [],
      memoryEnabled: true,
      knowledgeDocIds: [],
      budgetCentsPerRun: 500,
      maxSteps: 8,
    },
  },
  {
    id: 'analyst',
    name: 'Data Analyst',
    glyph: '◫',
    blurb: 'Queries metrics, computes summaries, explains the numbers.',
    draft: {
      name: 'Data analyst',
      description: 'Answers data questions from platform metrics and documents.',
      model: 'gpt-4o',
      instructions:
        'You are a data analyst.\n' +
        'Prefer exact numbers from tools over estimates.\n' +
        'Structure answers as: Question, Method, Result, Caveats.\n' +
        'State the time window of every figure you report.',
      tools: ['http_request'],
      stepTimeoutSeconds: 120,
      runTimeoutSeconds: 900,
    },
  },
  {
    id: 'orchestrator',
    name: 'Orchestrator',
    glyph: '⑂',
    blurb: 'Plans multi-step work, delegates to tools, tracks the budget.',
    draft: {
      name: 'Task orchestrator',
      description: 'Breaks goals into steps and drives them to completion.',
      model: 'gpt-4o',
      instructions:
        'You are an orchestrator.\n' +
        '1. Draft a numbered plan and show it before executing.\n' +
        '2. Execute steps one at a time with the available tools.\n' +
        '3. After each step, record what changed and what is next.\n' +
        'Stop and ask when a step would exceed its budget or permissions.',
      tools: ['http_request'],
      memoryEnabled: true,
      requireApproval: true,
      monthlyBudgetCents: 5000,
      maxSteps: 30,
      stepTimeoutSeconds: 120,
      runTimeoutSeconds: 1200,
    },
  },
]

const STEP_TITLES = [
  'Basic info',
  'Model selection',
  'System instructions',
  'Tool selection',
  'Memory & knowledge',
  'Permissions & policies',
  'Resource limits',
  'Review & deploy',
] as const

/** Steps gate Next: the first three map to server-side required fields. */
function stepComplete(step: number, draft: AgentDraft): boolean {
  switch (step) {
    case 0:
      return draft.name.trim() !== ''
    case 1:
      return draft.model.trim() !== ''
    case 2:
      return draft.instructions.trim() !== ''
    default:
      return true
  }
}

function firstIncompleteStep(draft: AgentDraft): number {
  for (let index = 0; index < STEP_TITLES.length; index += 1) {
    if (!stepComplete(index, draft)) return index
  }
  return STEP_TITLES.length - 1
}

/** Cheap token estimate for the instructions size hint (~4 chars/token). */
function estimateTokens(text: string): number {
  return Math.ceil(text.length / 4)
}

/** Backend rejects oversized instructions (INSTRUCTIONS_TOO_LARGE). */
const INSTRUCTIONS_WARN_CHARS = 8000

// ---------------------------------------------------------------------------
// JSON-aware diff helpers (no deps — used by the Compare configs view)
// ---------------------------------------------------------------------------

/** Recursively key-sorts an object so both sides compare field-for-field. */
function sortedCopy(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sortedCopy)
  if (value !== null && typeof value === 'object') {
    const source = value as Record<string, unknown>
    const out: Record<string, unknown> = {}
    for (const key of Object.keys(source).sort()) out[key] = sortedCopy(source[key])
    return out
  }
  return value
}

/** Flattens JSON into `dot.path → display value` rows (arrays stay JSON). */
function flattenJson(value: unknown, prefix = '', out: Record<string, string> = {}): Record<string, string> {
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
    const entries = Object.entries(value as Record<string, unknown>)
    if (entries.length === 0) {
      out[prefix || '(root)'] = '{}'
      return out
    }
    for (const [key, child] of entries) flattenJson(child, prefix ? `${prefix}.${key}` : key, out)
    return out
  }
  out[prefix || '(root)'] = value === undefined ? 'undefined' : (JSON.stringify(value) ?? 'undefined')
  return out
}

type DiffRow = {
  path: string
  left?: string
  right?: string
  kind: 'added' | 'removed' | 'changed' | 'same'
}

function diffJson(left: unknown, right: unknown): DiffRow[] {
  const leftFlat = flattenJson(sortedCopy(left))
  const rightFlat = flattenJson(sortedCopy(right))
  const paths = Array.from(new Set([...Object.keys(leftFlat), ...Object.keys(rightFlat)])).sort()
  return paths.map((path) => {
    const inLeft = path in leftFlat
    const inRight = path in rightFlat
    if (inLeft && !inRight) return { path, left: leftFlat[path], kind: 'removed' as const }
    if (!inLeft && inRight) return { path, right: rightFlat[path], kind: 'added' as const }
    const a = leftFlat[path]
    const b = rightFlat[path]
    return { path, left: a, right: b, kind: a === b ? ('same' as const) : ('changed' as const) }
  })
}

// ---------------------------------------------------------------------------
// Small presentational helpers
// ---------------------------------------------------------------------------

function CheckItem({
  checked,
  onToggle,
  title,
  detail,
}: {
  checked: boolean
  onToggle: () => void
  title: string
  detail?: string
}) {
  return (
    <label className="check-item">
      <input type="checkbox" checked={checked} onChange={onToggle} />
      <span>
        <strong>{title}</strong>
        {detail ? <span>{detail}</span> : null}
      </span>
    </label>
  )
}

function toggleIn(list: string[], value: string): string[] {
  return list.includes(value) ? list.filter((entry) => entry !== value) : [...list, value]
}

// ---------------------------------------------------------------------------
// Top-level view
// ---------------------------------------------------------------------------

export function AgentBuilderView({
  canWrite,
  onNavigate,
  onOpenRun,
}: {
  canWrite: boolean
  onNavigate: (view: ViewName) => void
  onOpenRun: (runId: string) => void
}) {
  const [mode, setMode] = useState<'build' | 'diff'>('build')

  return (
    <>
      <PageHeader
        eyebrow="Builder"
        title="Agent Builder"
        actions={
          <div className="builder-modes">
            <button type="button" className="ghost-button" aria-pressed={mode === 'build'} onClick={() => setMode('build')}>
              Build agent
            </button>
            <button type="button" className="ghost-button" aria-pressed={mode === 'diff'} onClick={() => setMode('diff')}>
              Compare configs
            </button>
          </div>
        }
      />

      {!canWrite ? (
        <p className="form-note">Viewer role — deploying an agent needs MEMBER and above; browsing the wizard is fine.</p>
      ) : null}

      {mode === 'build' ? <BuilderWizard canWrite={canWrite} onNavigate={onNavigate} onOpenRun={onOpenRun} /> : <AgentDiffPanel />}
    </>
  )
}

// ---------------------------------------------------------------------------
// Template library
// ---------------------------------------------------------------------------

function TemplateLibrary({ onPick }: { onPick: (template: Template) => void }) {
  return (
    <section className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Start fast</p>
          <h3>Template library</h3>
        </div>
      </div>
      <div className="tpl-grid">
        {TEMPLATES.map((template) => (
          <article key={template.id} className="tpl-card">
            <header>
              <span className="tpl-glyph" aria-hidden="true">
                {template.glyph}
              </span>
              <h4>{template.name}</h4>
            </header>
            <p>{template.blurb}</p>
            <button type="button" className="ghost-button small" onClick={() => onPick(template)}>
              Use template
            </button>
          </article>
        ))}
        <article className="tpl-card blank">
          <header>
            <span className="tpl-glyph" aria-hidden="true">
              ⬡
            </span>
            <h4>Blank agent</h4>
          </header>
          <p>Start from an empty draft and walk all eight steps yourself.</p>
          <button type="button" className="ghost-button small" onClick={() => onPick({ id: 'blank', name: 'Blank', glyph: '⬡', blurb: '', draft: {} })}>
            Start blank
          </button>
        </article>
      </div>
    </section>
  )
}

// ---------------------------------------------------------------------------
// 8-step wizard
// ---------------------------------------------------------------------------

function BuilderWizard({
  canWrite,
  onNavigate,
  onOpenRun,
}: {
  canWrite: boolean
  onNavigate: (view: ViewName) => void
  onOpenRun: (runId: string) => void
}) {
  const [draft, setDraft] = useState<AgentDraft>(EMPTY_DRAFT)
  const [step, setStep] = useState(0)
  const [created, setCreated] = useState<Agent | null>(null)

  const patch = (changes: Partial<AgentDraft>) => setDraft((current) => ({ ...current, ...changes }))

  const applyTemplate = (template: Template) => {
    setDraft({ ...EMPTY_DRAFT, ...template.draft })
    setStep(0)
    setCreated(null)
  }

  const resetWizard = () => {
    setDraft(EMPTY_DRAFT)
    setStep(0)
    setCreated(null)
  }

  if (created) {
    return <DeploySuccess agent={created} canWrite={canWrite} onNavigate={onNavigate} onOpenRun={onOpenRun} onBuildAnother={resetWizard} />
  }

  const maxReach = firstIncompleteStep(draft)

  return (
    <>
      <TemplateLibrary onPick={applyTemplate} />

      <section className="wizard-shell">
        <nav className="wizard-rail" aria-label="Wizard steps">
          {STEP_TITLES.map((title, index) => {
            const reachable = index <= maxReach
            const classes = [
              'wizard-step',
              index === step ? 'active' : '',
              stepComplete(index, draft) && index < step ? 'done' : '',
            ]
              .filter(Boolean)
              .join(' ')
            return (
              <button key={title} type="button" className={classes} disabled={!reachable} onClick={() => setStep(index)}>
                <span className="step-no">{index < step && stepComplete(index, draft) ? '✓' : index + 1}</span>
                {title}
              </button>
            )
          })}
        </nav>

        <div className="wizard-body">
          <article className="panel">
            <div className="panel-header">
              <div>
                <p className="eyebrow">
                  Step {step + 1} of {STEP_TITLES.length}
                </p>
                <h3>{STEP_TITLES[step]}</h3>
              </div>
              {step === 3 ? <span className="form-note">{formatNumber(draft.tools.length)} selected</span> : null}
            </div>

            {step === 0 ? <BasicInfoStep draft={draft} patch={patch} /> : null}
            {step === 1 ? <ModelStep draft={draft} patch={patch} /> : null}
            {step === 2 ? <InstructionsStep draft={draft} patch={patch} /> : null}
            {step === 3 ? <ToolsStep draft={draft} patch={patch} /> : null}
            {step === 4 ? <MemoryStep draft={draft} patch={patch} /> : null}
            {step === 5 ? <PermissionsStep draft={draft} patch={patch} /> : null}
            {step === 6 ? <LimitsStep draft={draft} patch={patch} /> : null}
            {step === 7 ? <ReviewStep draft={draft} canWrite={canWrite} onDeployed={setCreated} /> : null}

            {step < STEP_TITLES.length - 1 ? (
              <div className="wizard-nav">
                <button type="button" className="ghost-button" onClick={() => setStep((current) => Math.max(0, current - 1))} disabled={step === 0}>
                  ← Back
                </button>
                <button
                  type="button"
                  className="primary-button"
                  onClick={() => setStep((current) => Math.min(STEP_TITLES.length - 1, current + 1))}
                  disabled={!stepComplete(step, draft)}
                >
                  Next: {STEP_TITLES[Math.min(step + 1, STEP_TITLES.length - 1)]} →
                </button>
              </div>
            ) : null}
          </article>
        </div>
      </section>
    </>
  )
}

// --- individual steps ------------------------------------------------------

function BasicInfoStep({ draft, patch }: { draft: AgentDraft; patch: (changes: Partial<AgentDraft>) => void }) {
  return (
    <div className="form-grid">
      <div className="field">
        <label htmlFor="builder-name">Name</label>
        <input
          id="builder-name"
          value={draft.name}
          onChange={(event) => patch({ name: event.target.value })}
          placeholder="Support triage"
          required
        />
      </div>
      <div className="field span-2">
        <label htmlFor="builder-description">Description</label>
        <input
          id="builder-description"
          value={draft.description}
          onChange={(event) => patch({ description: event.target.value })}
          placeholder="What does this agent do?"
        />
      </div>
    </div>
  )
}

function ModelStep({ draft, patch }: { draft: AgentDraft; patch: (changes: Partial<AgentDraft>) => void }) {
  // Real backend-derived options: models already used by org agents.
  const agentsQuery = useAgents()
  const orgModels = useMemo(() => {
    const models = new Set<string>()
    for (const agent of agentsQuery.data ?? []) {
      if (agent.model) models.add(agent.model)
    }
    return Array.from(models).sort()
  }, [agentsQuery.data])
  const options = useMemo(() => Array.from(new Set([...orgModels, ...MODEL_PRESETS])), [orgModels])

  return (
    <div className="stack-gap">
      <div className="field">
        <label htmlFor="builder-model">Model id</label>
        <input
          id="builder-model"
          list="builder-model-options"
          value={draft.model}
          onChange={(event) => patch({ model: event.target.value })}
          placeholder="gpt-4o-mini"
          required
        />
        <datalist id="builder-model-options">
          {options.map((option) => (
            <option key={option} value={option} />
          ))}
        </datalist>
        <span className="form-note">Free text is allowed — the API validates the id against the platform catalog (UNKNOWN_MODEL).</span>
      </div>
      <div>
        <p className="eyebrow">Presets</p>
        <div className="chip-row">
          {MODEL_PRESETS.map((preset) => (
            <button key={preset} type="button" className="chip" onClick={() => patch({ model: preset })}>
              {preset}
            </button>
          ))}
        </div>
      </div>
      {orgModels.length > 0 ? (
        <div>
          <p className="eyebrow">Already in use in this org</p>
          <div className="chip-row">
            {orgModels.map((preset) => (
              <button key={preset} type="button" className="chip" onClick={() => patch({ model: preset })}>
                {preset}
              </button>
            ))}
          </div>
        </div>
      ) : null}
      {agentsQuery.isError ? <ErrorBanner error={agentsQuery.error} onRetry={() => void agentsQuery.refetch()} /> : null}
    </div>
  )
}

function InstructionsStep({ draft, patch }: { draft: AgentDraft; patch: (changes: Partial<AgentDraft>) => void }) {
  const chars = draft.instructions.length
  const warn = chars > INSTRUCTIONS_WARN_CHARS
  return (
    <div className="field">
      <label htmlFor="builder-instructions">System instructions</label>
      <textarea
        id="builder-instructions"
        rows={10}
        value={draft.instructions}
        onChange={(event) => patch({ instructions: event.target.value })}
        placeholder="System instructions sent to the model on every run"
        required
      />
      <span className={warn ? 'field-hint warn' : 'field-hint'}>
        {formatNumber(chars)} chars ≈ {formatNumber(estimateTokens(draft.instructions))} tokens
        {warn ? ' — the backend rejects oversized instructions (INSTRUCTIONS_TOO_LARGE)' : ''}
      </span>
    </div>
  )
}

function ToolsStep({ draft, patch }: { draft: AgentDraft; patch: (changes: Partial<AgentDraft>) => void }) {
  const toolsQuery = useTools()
  const tools = useMemo(() => toolsQuery.data ?? [], [toolsQuery.data])

  return (
    <div className="stack-gap">
      <p className="detail-copy muted">
        Picked from the live registry (GET /tools). Selected tools seed the allowlist on the next policy step.
      </p>
      {toolsQuery.isPending ? (
        <div className="check-grid">
          {[0, 1, 2, 3].map((index) => (
            <span key={index} className="check-item">
              <Skeleton height={12} width={14} />
              <Skeleton height={12} width="60%" />
            </span>
          ))}
        </div>
      ) : toolsQuery.isError ? (
        <ErrorBanner error={toolsQuery.error} onRetry={() => void toolsQuery.refetch()} />
      ) : tools.length === 0 ? (
        <EmptyState title="No tools registered" hint="The runtime registry is empty — you can still create a tool-free agent." />
      ) : (
        <div className="check-grid">
          {tools.map((tool) => (
            <CheckItem
              key={tool.name}
              checked={draft.tools.includes(tool.name)}
              onToggle={() => {
                const nextTools = toggleIn(draft.tools, tool.name)
                // Keep the allowlist a subset of the selection.
                const allowlist = draft.toolAllowlist.filter((name) => nextTools.includes(name))
                patch({ tools: nextTools, toolAllowlist: allowlist })
              }}
              title={tool.name}
              detail={tool.description || 'No description published.'}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function MemoryStep({ draft, patch }: { draft: AgentDraft; patch: (changes: Partial<AgentDraft>) => void }) {
  const docsQuery = useKnowledgeDocuments()
  const docs = useMemo(() => docsQuery.data ?? [], [docsQuery.data])

  return (
    <div className="stack-gap">
      <CheckItem
        checked={draft.memoryEnabled}
        onToggle={() => patch({ memoryEnabled: !draft.memoryEnabled })}
        title="Enable long-term memory"
        detail="Persist conversation snippets across runs so the agent can recall earlier context."
      />

      <div>
        <p className="eyebrow">Knowledge sources (GET /knowledge)</p>
        {docsQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={14} />
            <Skeleton height={14} width="70%" />
          </div>
        ) : docsQuery.isError ? (
          <ErrorBanner error={docsQuery.error} onRetry={() => void docsQuery.refetch()} />
        ) : docs.length === 0 ? (
          <p className="detail-copy muted">No knowledge documents uploaded yet — the Knowledge view can add some.</p>
        ) : (
          <div className="check-grid">
            {docs.map((doc) => (
              <CheckItem
                key={doc.id}
                checked={draft.knowledgeDocIds.includes(doc.id)}
                onToggle={() => patch({ knowledgeDocIds: toggleIn(draft.knowledgeDocIds, doc.id) })}
                title={doc.title}
                detail={`${formatNumber(doc.chunkCount)} chunks${doc.source ? ` · ${doc.source}` : ''}`}
              />
            ))}
          </div>
        )}
        {!draft.memoryEnabled && draft.knowledgeDocIds.length === 0 ? (
          <p className="form-note">Nothing selected — the agent will answer from instructions and tools alone.</p>
        ) : null}
      </div>
    </div>
  )
}

function PermissionsStep({ draft, patch }: { draft: AgentDraft; patch: (changes: Partial<AgentDraft>) => void }) {
  const allowlist = draft.toolAllowlist.length > 0 ? draft.toolAllowlist : draft.tools
  return (
    <div className="stack-gap">
      <div>
        <p className="eyebrow">Tool allowlist</p>
        {draft.tools.length === 0 ? (
          <p className="detail-copy muted">No tools selected on the previous step, so there is nothing to restrict.</p>
        ) : (
          <>
            <p className="detail-copy muted">Uncheck a tool to remove it from the execution allowlist (defaults to everything you picked).</p>
            <div className="check-grid">
              {draft.tools.map((tool) => (
                <CheckItem
                  key={tool}
                  checked={allowlist.includes(tool)}
                  onToggle={() => patch({ toolAllowlist: toggleIn(allowlist, tool) })}
                  title={tool}
                />
              ))}
            </div>
          </>
        )}
      </div>

      <CheckItem
        checked={draft.requireApproval}
        onToggle={() => patch({ requireApproval: !draft.requireApproval })}
        title="Require human approval for tool calls"
        detail="Route tool executions through the Approvals queue before they run."
      />

      <div className="form-grid">
        <div className="field">
          <label htmlFor="builder-budget-run">Budget per run (cents)</label>
          <input
            id="builder-budget-run"
            type="number"
            min={0}
            value={draft.budgetCentsPerRun}
            onChange={(event) => patch({ budgetCentsPerRun: Math.max(0, Number(event.target.value) || 0) })}
          />
        </div>
        <div className="field">
          <label htmlFor="builder-budget-month">Monthly budget (cents)</label>
          <input
            id="builder-budget-month"
            type="number"
            min={0}
            value={draft.monthlyBudgetCents}
            onChange={(event) => patch({ monthlyBudgetCents: Math.max(0, Number(event.target.value) || 0) })}
          />
        </div>
      </div>
      <p className="form-note">0 means unset (platform defaults apply). These budgets are part of the reviewed blueprint.</p>
    </div>
  )
}

function LimitsStep({ draft, patch }: { draft: AgentDraft; patch: (changes: Partial<AgentDraft>) => void }) {
  return (
    <div className="stack-gap">
      <div className="form-grid">
        <div className="field">
          <label htmlFor="builder-max-steps">Max steps per run</label>
          <input
            id="builder-max-steps"
            type="number"
            min={1}
            value={draft.maxSteps}
            onChange={(event) => patch({ maxSteps: Math.max(1, Number(event.target.value) || 1) })}
          />
        </div>
        <div className="field">
          <label htmlFor="builder-step-timeout">Step timeout (seconds)</label>
          <input
            id="builder-step-timeout"
            type="number"
            min={1}
            value={draft.stepTimeoutSeconds}
            onChange={(event) => patch({ stepTimeoutSeconds: Math.max(1, Number(event.target.value) || 1) })}
          />
        </div>
        <div className="field">
          <label htmlFor="builder-run-timeout">Run timeout (seconds)</label>
          <input
            id="builder-run-timeout"
            type="number"
            min={1}
            value={draft.runTimeoutSeconds}
            onChange={(event) => patch({ runTimeoutSeconds: Math.max(1, Number(event.target.value) || 1) })}
          />
        </div>
      </div>
      <p className="form-note">Guards against runaway loops: a run stops after max steps or when either timeout is exceeded.</p>
    </div>
  )
}

function ReviewStep({
  draft,
  canWrite,
  onDeployed,
}: {
  draft: AgentDraft
  canWrite: boolean
  onDeployed: (agent: Agent) => void
}) {
  const createAgent = useCreateAgent()

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (!canWrite || createAgent.isPending) return
    createAgent.mutate(
      {
        name: draft.name.trim(),
        description: draft.description.trim(),
        instructions: draft.instructions.trim(),
        model: draft.model.trim(),
      },
      { onSuccess: onDeployed },
    )
  }

  const createPayload = {
    name: draft.name.trim(),
    description: draft.description.trim(),
    instructions: draft.instructions.trim(),
    model: draft.model.trim(),
  }
  const blueprint = {
    ...draft,
    name: draft.name.trim(),
    description: draft.description.trim(),
    model: draft.model.trim(),
    instructions: draft.instructions.trim(),
    toolAllowlist: draft.toolAllowlist.length > 0 ? draft.toolAllowlist : draft.tools,
  }

  return (
    <form onSubmit={submit} className="stack-gap">
      <section className="summary-grid">
        <SummaryStat label="Model" value={createPayload.model || '—'} accent="info" />
        <SummaryStat label="Tools" value={formatNumber(draft.tools.length)} accent="success" />
        <SummaryStat label="Memory" value={draft.memoryEnabled ? 'On' : 'Off'} accent="warning" />
        <SummaryStat label="Max steps" value={formatNumber(draft.maxSteps)} />
      </section>

      <div>
        <p className="eyebrow">Create payload — POST /agents/create</p>
        <pre className="payload-block">{JSON.stringify(createPayload, null, 2)}</pre>
      </div>

      <div>
        <p className="eyebrow">Design blueprint (reviewed, not sent on create)</p>
        <pre className="payload-block builder-blueprint">{JSON.stringify(blueprint, null, 2)}</pre>
        <p className="form-note">
          The create endpoint persists name, description, instructions and model only. The blueprint documents the
          tools / memory / policy / limits you configured until the API accepts them.
        </p>
      </div>

      {createAgent.isError ? <div className="form-error">{describeError(createAgent.error)}</div> : null}

      <div className="form-actions">
        <span className="form-note">{canWrite ? 'Creates the agent immediately' : 'Viewer role — deploying is disabled'}</span>
        <button type="submit" className="primary-button" disabled={!canWrite || createAgent.isPending}>
          {createAgent.isPending ? 'Deploying…' : 'Deploy agent'}
        </button>
      </div>
    </form>
  )
}

// ---------------------------------------------------------------------------
// Deploy success + live test-run preview (POST /runs + SSE stream)
// ---------------------------------------------------------------------------

function DeploySuccess({
  agent,
  canWrite,
  onNavigate,
  onOpenRun,
  onBuildAnother,
}: {
  agent: Agent
  canWrite: boolean
  onNavigate: (view: ViewName) => void
  onOpenRun: (runId: string) => void
  onBuildAnother: () => void
}) {
  return (
    <>
      <section className="summary-grid">
        <SummaryStat label="Deployed" value={agent.name} accent="success" />
        <SummaryStat label="Model" value={agent.model || '—'} accent="info" />
        <SummaryStat label="Agent ID" value={shortenId(agent.id)} />
        <SummaryStat label="Status" value={agent.status} accent={agent.status.toUpperCase() === 'DRAFT' ? 'info' : 'success'} />
      </section>

      <TestRunPanel agent={agent} canWrite={canWrite} onOpenRun={onOpenRun} />

      <div className="wizard-nav">
        <button type="button" className="ghost-button" onClick={onBuildAnother}>
          Build another agent
        </button>
        <button type="button" className="primary-button" onClick={() => onNavigate('Agents')}>
          Open agent catalog →
        </button>
      </div>
    </>
  )
}

type LogLine = { id: number; tag: string; text: string }

function TestRunPanel({ agent, canWrite, onOpenRun }: { agent: Agent; canWrite: boolean; onOpenRun: (runId: string) => void }) {
  const runAgent = useRunAgent()
  const [runInput, setRunInput] = useState('')
  const [runId, setRunId] = useState<string | null>(null)
  const [lines, setLines] = useState<LogLine[]>([])
  const lineSeq = useRef(0)
  const logRef = useRef<HTMLDivElement | null>(null)

  const runQuery = useRun(runId)
  const status = runQuery.data?.status

  const appendLine = (tag: string, text: string) => {
    lineSeq.current += 1
    const line: LogLine = { id: lineSeq.current, tag, text }
    setLines((current) => [...current.slice(-199), line])
  }

  // Stream the run's SSE events into the preview log (same hook the Runs
  // detail uses; it also patches run status so the pill stays live).
  useRunEvents(runId, (event: RunEvent) => {
    const output = eventOutput(event)
    const eventStatusValue = eventStatus(event)
    if (output) appendLine(event.type ?? 'output', output)
    else if (eventStatusValue) appendLine('status', eventStatusValue)
    else if (Object.keys(event.payload).length > 0) appendLine(event.type ?? 'event', JSON.stringify(event.payload))
  })

  // Keep the newest line visible.
  useEffect(() => {
    if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight
  }, [lines])

  const startRun = (event: FormEvent) => {
    event.preventDefault()
    const input = runInput.trim()
    if (!input || runAgent.isPending || !canWrite) return
    runAgent.mutate(
      { agentId: agent.id, input },
      {
        onSuccess: (run) => {
          setRunId(run.id)
          setLines([])
          setRunInput('')
          appendLine('queued', `Run ${shortenId(run.id)} queued via POST /runs`)
        },
      },
    )
  }

  return (
    <section className="content-grid">
      <article className="panel">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Live preview</p>
            <h3>Test run</h3>
          </div>
          {status ? <StatusPill status={status} /> : null}
        </div>

        {!canWrite ? (
          <p className="detail-copy muted">Viewer role — running agents needs MEMBER and above.</p>
        ) : (
          <form onSubmit={startRun}>
            <div className="field">
              <label htmlFor="builder-run-input">Input</label>
              <textarea
                id="builder-run-input"
                rows={4}
                value={runInput}
                onChange={(event) => setRunInput(event.target.value)}
                placeholder="Give the new agent something to do…"
                required
              />
            </div>
            {runAgent.isError ? <div className="form-error">{describeError(runAgent.error)}</div> : null}
            <div className="form-actions">
              <span className="form-note">Streams GET /runs/{'{id}'}/events live</span>
              <button type="submit" className="primary-button" disabled={runAgent.isPending}>
                {runAgent.isPending ? 'Queuing…' : 'Start test run'}
              </button>
            </div>
          </form>
        )}
      </article>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Stream</p>
            <h3>Run output</h3>
          </div>
          {runId ? (
            <button type="button" className="ghost-button small" onClick={() => onOpenRun(runId)}>
              Open in Runs
            </button>
          ) : null}
        </div>

        {lines.length === 0 ? (
          <EmptyState
            title={runId ? 'Waiting for events…' : 'No test run yet'}
            hint={runId ? 'The stream connects automatically; output appears here as it arrives.' : 'Start a test run to watch this agent execute in real time.'}
          />
        ) : (
          <div className="run-log" ref={logRef}>
            {lines.map((line) => (
              <p key={line.id} className="run-log-line">
                <span className="log-tag">{line.tag}</span>
                {line.text}
              </p>
            ))}
          </div>
        )}

        {status && runQuery.data?.output ? (
          <div className="builder-final-output">
            <p className="eyebrow">Final output</p>
            <pre className="payload-block">{runQuery.data.output}</pre>
          </div>
        ) : null}
      </article>
    </section>
  )
}

// ---------------------------------------------------------------------------
// Compare configs — JSON-aware field diff between two existing agents
// ---------------------------------------------------------------------------

function AgentDiffPanel() {
  const agentsQuery = useAgents()
  const agents = useMemo(() => agentsQuery.data ?? [], [agentsQuery.data])

  const [leftPick, setLeftPick] = useState('')
  const [rightPick, setRightPick] = useState('')
  const [onlyDifferences, setOnlyDifferences] = useState(true)

  // Derived selection: default to the two first agents, and never allow the
  // two sides to collapse onto the same agent (picking A on the left bumps B
  // to the first other agent). No effect needed — this stays render-pure.
  const fallbackLeft = agents[0]?.id ?? ''
  const fallbackRight = agents.find((agent) => agent.id !== fallbackLeft)?.id ?? ''
  const leftId = leftPick || fallbackLeft
  let rightId = rightPick || fallbackRight
  if (rightId === leftId) rightId = agents.find((agent) => agent.id !== leftId)?.id ?? rightId

  const leftAgent = agents.find((agent) => agent.id === leftId)
  const rightAgent = agents.find((agent) => agent.id === rightId)

  const rows = useMemo(() => {
    if (!leftAgent || !rightAgent) return []
    const all = diffJson(leftAgent, rightAgent)
    return onlyDifferences ? all.filter((row) => row.kind !== 'same') : all
  }, [leftAgent, rightAgent, onlyDifferences])

  const sameCount = useMemo(() => {
    if (!leftAgent || !rightAgent) return 0
    return diffJson(leftAgent, rightAgent).filter((row) => row.kind === 'same').length
  }, [leftAgent, rightAgent])

  return (
    <section className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Diff</p>
          <h3>Compare agent configs</h3>
        </div>
        {agents.length >= 2 ? (
          <label className="diff-toggle">
            <input type="checkbox" checked={onlyDifferences} onChange={(event) => setOnlyDifferences(event.target.checked)} />
            Only differences
          </label>
        ) : null}
      </div>

      {agentsQuery.isPending ? (
        <div className="stack-gap">
          <Skeleton height={16} width="45%" />
          <Skeleton height={120} />
        </div>
      ) : agentsQuery.isError ? (
        <ErrorBanner error={agentsQuery.error} onRetry={() => void agentsQuery.refetch()} />
      ) : agents.length < 2 ? (
        <EmptyState
          title="Need at least two agents to compare"
          hint="Create another agent in the Builder, then come back to diff their configs."
        />
      ) : (
        <>
          <div className="filter-row">
            <div className="field">
              <label htmlFor="diff-left">Agent A</label>
              <select id="diff-left" value={leftId} onChange={(event) => setLeftPick(event.target.value)}>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name} ({shortenId(agent.id)})
                  </option>
                ))}
              </select>
            </div>
            <div className="field">
              <label htmlFor="diff-right">Agent B</label>
              <select id="diff-right" value={rightId} onChange={(event) => setRightPick(event.target.value)}>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name} ({shortenId(agent.id)})
                  </option>
                ))}
              </select>
            </div>
          </div>

          {leftId === rightId ? (
            <p className="form-note">Pick two different agents to see a diff.</p>
          ) : rows.length === 0 ? (
            <EmptyState
              title="Configs are identical"
              hint={`${formatNumber(sameCount)} shared fields match across both agents.`}
            />
          ) : (
            <div className="table-wrap">
              <table className="diff-table">
                <thead>
                  <tr>
                    <th>Field</th>
                    <th>A — {leftAgent?.name ?? '—'}</th>
                    <th>B — {rightAgent?.name ?? '—'}</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((row) => (
                    <tr key={row.path} className={`diff-row ${row.kind}`}>
                      <td className="path">{row.path}</td>
                      {row.kind === 'added' ? (
                        <>
                          <td className="val">—</td>
                          <td className="val">{row.right}</td>
                        </>
                      ) : row.kind === 'removed' ? (
                        <>
                          <td className="val">{row.left}</td>
                          <td className="val">—</td>
                        </>
                      ) : (
                        <>
                          <td className="val">{row.left}</td>
                          <td className="val">{row.right}</td>
                        </>
                      )}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  )
}
