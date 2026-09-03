// Schedules view (track 2-f contract): list, create, pause/resume.
//
// Wired endpoints (no mocks):
// - GET  /schedules               -> {"schedules":[…]}   (schedules.read)
// - POST /schedules/create        -> {"schedule":{…}}    (schedules.write)
// - POST /schedules/{id}/pause    -> {"schedule":{…}}    (schedules.write)
// - POST /schedules/{id}/resume   -> {"schedule":{…}}    (schedules.write)
//
// Kinds mirror internal/scheduler: once (run_at RFC3339), recurring
// (interval_seconds >= 60), cron (cron_expr + IANA timezone).

import { useMemo, useState, type FormEvent } from 'react'
import { useAgents, useCreateSchedule, useScheduleTransition, useSchedules } from '../lib/hooks'
import type { CreateScheduleInput, Schedule } from '../lib/api/schedules'
import { formatDateTime, formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

const SCHEDULE_KINDS = ['cron', 'recurring', 'once'] as const

function describeTrigger(schedule: Schedule): string {
  if (schedule.kind === 'cron') {
    return `${schedule.cronExpr || '—'} (${schedule.timezone || 'UTC'})`
  }
  if (schedule.kind === 'recurring') {
    return `every ${schedule.intervalSeconds ?? '—'}s`
  }
  return schedule.runAt ? formatDateTime(schedule.runAt) : '—'
}

function ScheduleActions({ schedule, canWrite }: { schedule: Schedule; canWrite: boolean }) {
  const transition = useScheduleTransition()
  if (!canWrite) return <span className="form-note">write needs MEMBER+</span>
  const busy = transition.isPending
  const error = transition.isError ? describeError(transition.error) : null
  if (schedule.status === 'completed') return <span className="form-note">completed</span>
  return (
    <div className="table-actions">
      {error ? <span className="form-error inline">{error}</span> : null}
      {schedule.status === 'paused' ? (
        <button
          type="button"
          className="ghost-button small"
          disabled={busy}
          onClick={() => transition.mutate({ id: schedule.id, action: 'resume' })}
        >
          Resume
        </button>
      ) : (
        <button
          type="button"
          className="ghost-button small"
          disabled={busy}
          onClick={() => transition.mutate({ id: schedule.id, action: 'pause' })}
        >
          Pause
        </button>
      )}
    </div>
  )
}

function CreateScheduleForm({ onCreated }: { onCreated: (schedule: Schedule) => void }) {
  const createSchedule = useCreateSchedule()
  const agentsQuery = useAgents()
  const agents = agentsQuery.data ?? []
  const [agentId, setAgentId] = useState('')
  const [kind, setKind] = useState<string>(SCHEDULE_KINDS[0])
  const [cronExpr, setCronExpr] = useState('0 9 * * 1-5')
  const [timezone, setTimezone] = useState('UTC')
  const [intervalSeconds, setIntervalSeconds] = useState('300')
  const [runAt, setRunAt] = useState('')
  const [input, setInput] = useState('')
  const [message, setMessage] = useState<string | null>(null)

  const effectiveAgent = agentId || agents[0]?.id || ''

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (createSchedule.isPending || !effectiveAgent) return
    setMessage(null)
    const payload: CreateScheduleInput = { agent_id: effectiveAgent, input, kind }
    if (kind === 'cron') {
      payload.cron_expr = cronExpr.trim()
      payload.timezone = timezone.trim() || 'UTC'
    } else if (kind === 'recurring') {
      payload.interval_seconds = Number(intervalSeconds) || 0
    } else {
      const parsed = runAt ? new Date(runAt) : null
      payload.run_at = parsed && !Number.isNaN(parsed.getTime()) ? parsed.toISOString() : ''
    }
    createSchedule.mutate(payload, {
      onSuccess: (schedule) => {
        setInput('')
        setMessage(`Schedule ${shortenId(schedule.id)} created (${schedule.status}).`)
        onCreated(schedule)
      },
      onError: (error) => setMessage(describeError(error)),
    })
  }

  return (
    <article className="panel">
      <div className="panel-header">
        <div>
          <p className="eyebrow">New schedule</p>
          <h3>Create schedule</h3>
        </div>
        <span className="form-note">POST /schedules/create</span>
      </div>

      {message ? <div className={createSchedule.isError ? 'form-error' : 'form-note'}>{message}</div> : null}
      {agents.length === 0 ? (
        <EmptyState title="No agents to schedule" hint="Schedules trigger agent runs — create an agent first." />
      ) : (
        <form onSubmit={submit}>
          <div className="form-grid">
            <div className="field">
              <label htmlFor="schedule-agent">Agent</label>
              <select id="schedule-agent" value={effectiveAgent} onChange={(event) => setAgentId(event.target.value)} required>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name}
                  </option>
                ))}
              </select>
            </div>
            <div className="field">
              <label htmlFor="schedule-kind">Kind</label>
              <select id="schedule-kind" value={kind} onChange={(event) => setKind(event.target.value)}>
                {SCHEDULE_KINDS.map((candidate) => (
                  <option key={candidate} value={candidate}>
                    {candidate}
                  </option>
                ))}
              </select>
            </div>
            {kind === 'cron' ? (
              <>
                <div className="field">
                  <label htmlFor="schedule-cron">Cron expression</label>
                  <input id="schedule-cron" value={cronExpr} onChange={(event) => setCronExpr(event.target.value)} placeholder="0 9 * * 1-5" required />
                </div>
                <div className="field">
                  <label htmlFor="schedule-timezone">Timezone (IANA)</label>
                  <input id="schedule-timezone" value={timezone} onChange={(event) => setTimezone(event.target.value)} placeholder="UTC" required />
                </div>
              </>
            ) : null}
            {kind === 'recurring' ? (
              <div className="field">
                <label htmlFor="schedule-interval">Interval (seconds, ≥ 60)</label>
                <input id="schedule-interval" type="number" min="60" value={intervalSeconds} onChange={(event) => setIntervalSeconds(event.target.value)} required />
              </div>
            ) : null}
            {kind === 'once' ? (
              <div className="field">
                <label htmlFor="schedule-runat">Run at</label>
                <input id="schedule-runat" type="datetime-local" value={runAt} onChange={(event) => setRunAt(event.target.value)} required />
              </div>
            ) : null}
            <div className="field span-2">
              <label htmlFor="schedule-input">Input (sent to the agent on each fire)</label>
              <textarea id="schedule-input" rows={3} value={input} onChange={(event) => setInput(event.target.value)} placeholder="Daily digest request…" />
            </div>
          </div>
          <div className="form-actions">
            <span className="form-note">The scheduler fires due schedules and records next_run_at.</span>
            <button type="submit" className="primary-button" disabled={createSchedule.isPending}>
              {createSchedule.isPending ? 'Creating…' : 'Create schedule'}
            </button>
          </div>
        </form>
      )}
    </article>
  )
}

export function SchedulesView({ canWrite }: { canWrite: boolean }) {
  const schedulesQuery = useSchedules()
  const [showCreate, setShowCreate] = useState(false)
  const schedules = useMemo(() => schedulesQuery.data ?? [], [schedulesQuery.data])
  const active = schedules.filter((schedule) => schedule.status === 'active').length
  const paused = schedules.filter((schedule) => schedule.status === 'paused').length

  return (
    <>
      <PageHeader
        eyebrow="Automation"
        title="Schedules"
        actions={
          canWrite ? (
            <button type="button" className="primary-button" onClick={() => setShowCreate((open) => !open)}>
              New schedule
            </button>
          ) : (
            <span className="form-note">Viewer role — creating schedules needs MEMBER and above</span>
          )
        }
      />

      {showCreate && canWrite ? (
        <CreateScheduleForm onCreated={() => setShowCreate(false)} />
      ) : null}

      {schedulesQuery.isError ? <ErrorBanner error={schedulesQuery.error} onRetry={() => void schedulesQuery.refetch()} /> : null}

      <section className="summary-grid">
        {schedulesQuery.isPending ? (
          [0, 1, 2, 3].map((index) => (
            <article key={index} className="mini-stat">
              <Skeleton width={80} />
              <Skeleton height={26} width={60} />
            </article>
          ))
        ) : (
          <>
            <SummaryStat label="Total schedules" value={String(schedules.length)} />
            <SummaryStat label="Active" value={String(active)} accent="success" />
            <SummaryStat label="Paused" value={String(paused)} accent="warning" />
            <SummaryStat label="API" value="/schedules" accent="info" />
          </>
        )}
      </section>

      <article className="panel wide">
        <div className="panel-header">
          <div>
            <p className="eyebrow">Triggers</p>
            <h3>All schedules</h3>
          </div>
          <span className="form-note">GET /schedules</span>
        </div>

        {schedulesQuery.isPending ? (
          <div className="stack-gap">
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        ) : schedules.length === 0 ? (
          <EmptyState
            title="No schedules yet"
            hint="Schedules fire agent runs on a cron expression, a fixed interval, or a one-off timestamp."
            action={
              canWrite ? (
                <button type="button" className="primary-button" onClick={() => setShowCreate(true)}>
                  Create schedule
                </button>
              ) : undefined
            }
          />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Agent</th>
                  <th>Kind</th>
                  <th>Trigger</th>
                  <th>Status</th>
                  <th>Next run</th>
                  <th>Last fired</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {schedules.map((schedule) => (
                  <tr key={schedule.id}>
                    <td>
                      <strong>{shortenId(schedule.agentId)}</strong>
                    </td>
                    <td>
                      <span className={`step-type step-type-${schedule.kind}`}>{schedule.kind}</span>
                    </td>
                    <td>
                      <code>{describeTrigger(schedule)}</code>
                    </td>
                    <td>
                      <StatusPill status={schedule.status} />
                    </td>
                    <td>{schedule.nextRunAt ? formatRelativeTime(schedule.nextRunAt) : '—'}</td>
                    <td>{schedule.lastFiredAt ? formatRelativeTime(schedule.lastFiredAt) : '—'}</td>
                    <td>
                      <ScheduleActions schedule={schedule} canWrite={canWrite} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </article>
    </>
  )
}
