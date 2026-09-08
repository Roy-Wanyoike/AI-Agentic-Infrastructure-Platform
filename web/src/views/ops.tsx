// Ops view (issue #81): queue introspection + dead-letter operations over the
// issue #77 backend surface.
//
//   GET  /queue/stats                       depth by status
//   GET  /queue/tasks?status=&limit=&cursor= task pages (keyset pagination)
//   GET  /queue/tasks/{id}                  task detail (attempts, last error)
//   POST /queue/tasks/{id}/requeue          OWNER/ADMIN; 409 unless dead-letter
//
// Zero-infra mode: the /queue/* surface may be absent (404) on a deployment
// where the queue-ops wiring has not landed — the view renders that as an
// honest "not available on this deployment" empty state, never as a crash.
//
// RBAC: reads are operator-facing but the contract does not gate them beyond
// authentication; the requeue action is OWNER/ADMIN (canManageQueueOps).

import { useMemo, useState } from 'react'
import { useQueueStats, useQueueTask, useQueueTasks, useRequeueTask } from '../lib/hooks'
import { QUEUE_TASK_STATUSES, type QueueTask } from '../lib/api/queue'
import { ApiError } from '../lib/api/client'
import { formatDateTime, formatRelativeTime, shortenId } from '../lib/format'
import { EmptyState, ErrorBanner, PageHeader, Skeleton, StatusPill, SummaryStat } from './shared'
import { describeError } from './uiHelpers'

const TASK_PAGE_SIZE = 25

const TASK_FILTERS = ['all', ...QUEUE_TASK_STATUSES] as const

function isMissingSurface(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404
}

function TaskPayloadCell({ task }: { task: QueueTask }) {
  let text: string | null = null
  if (task.payload) {
    try {
      text = JSON.stringify(task.payload)
    } catch {
      text = null
    }
  }
  return <code className="payload-cell">{text ?? '—'}</code>
}

function TaskDetailPanel({ taskId }: { taskId: string }) {
  const taskQuery = useQueueTask(taskId)
  const task = taskQuery.data

  return (
    <article className="panel wide" aria-label="Task detail">
      <div className="panel-header">
        <div>
          <p className="eyebrow">Queue</p>
          <h3>Task {shortenId(taskId)}</h3>
        </div>
        <span className="form-note">GET /queue/tasks/{shortenId(taskId)}</span>
      </div>

      {taskQuery.isPending ? (
        <div className="stack-gap">
          <Skeleton height={16} />
          <Skeleton height={16} />
        </div>
      ) : taskQuery.isError ? (
        isMissingSurface(taskQuery.error) ? (
          <EmptyState title="Task not found" hint="The queue API answered 404 — the task may have been consumed, pruned, or the surface is absent on this deployment." />
        ) : (
          <ErrorBanner error={taskQuery.error} onRetry={() => void taskQuery.refetch()} />
        )
      ) : task ? (
        <>
          <section className="summary-grid">
            <SummaryStat label="Status" value={task.status} accent={task.status === 'dead_letter' ? 'warning' : task.status === 'running' ? 'info' : 'default'} />
            <SummaryStat label="Attempts" value={String(task.attempts)} accent={task.attempts > 1 ? 'warning' : 'default'} />
            <SummaryStat label="Type" value={task.type || '—'} accent="info" />
            <SummaryStat label="Updated" value={formatRelativeTime(task.updatedAt)} />
          </section>
          {task.lastError ? (
            <div className="task-error-block">
              <label>Last error</label>
              <p>{task.lastError}</p>
            </div>
          ) : null}
          {task.payload ? (
            <div className="task-error-block">
              <label>Payload</label>
              <code className="payload-block">{JSON.stringify(task.payload, null, 2)}</code>
            </div>
          ) : null}
          <p className="form-note">
            Created {formatDateTime(task.createdAt)} · updated {formatDateTime(task.updatedAt)} · ID {task.id || '—'}
          </p>
        </>
      ) : null}
    </article>
  )
}

function TaskRowActions({ task, canRequeue, onRequeued }: { task: QueueTask; canRequeue: boolean; onRequeued: (task: QueueTask) => void }) {
  const requeue = useRequeueTask()
  const [error, setError] = useState<string | null>(null)

  if (!canRequeue) return <span className="form-note">requeue needs OWNER/ADMIN</span>

  const confirmRequeue = () => {
    if (!window.confirm(`Requeue dead-letter task ${shortenId(task.id)}? It will be enqueued for execution again.`)) return
    setError(null)
    requeue.mutate(task.id, {
      onSuccess: (updated) => {
        setError(null)
        onRequeued(updated)
      },
      onError: (cause) => setError(describeError(cause)),
    })
  }

  return (
    <div className="table-actions">
      {error ? <span className="form-error inline">{error}</span> : null}
      <button
        type="button"
        className="ghost-button small"
        disabled={requeue.isPending}
        title={task.status === 'dead_letter' ? 'Enqueue the task again' : 'Only dead-lettered tasks can be requeued (the API answers 409)'}
        onClick={confirmRequeue}
      >
        {requeue.isPending ? 'Requeuing…' : 'Requeue'}
      </button>
    </div>
  )
}

export function OpsView({ canRequeue }: { canRequeue: boolean }) {
  const statsQuery = useQueueStats()
  const [statusFilter, setStatusFilter] = useState<string>('all')
  const [cursor, setCursor] = useState('')
  const [older, setOlder] = useState<QueueTask[]>([])
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null)
  const [requeuedNote, setRequeuedNote] = useState<string | null>(null)

  const filter = statusFilter === 'all' ? '' : statusFilter
  const tasksQuery = useQueueTasks({ status: filter, limit: TASK_PAGE_SIZE, cursor })
  const pageItems = useMemo(() => tasksQuery.data?.tasks ?? [], [tasksQuery.data])
  const nextCursor = tasksQuery.data?.nextCursor ?? ''

  const tasks = useMemo(() => {
    const merged = [...older, ...pageItems]
    const seen = new Set<string>()
    return merged.filter((task) => {
      if (!task.id || seen.has(task.id)) return false
      seen.add(task.id)
      return true
    })
  }, [older, pageItems])

  const loadMore = () => {
    if (!nextCursor) return
    setOlder((prev) => [...prev, ...pageItems])
    setCursor(nextCursor)
  }

  const changeFilter = (next: string) => {
    setStatusFilter(next)
    setCursor('')
    setOlder([])
    setSelectedTaskId(null)
    setRequeuedNote(null)
  }

  const stats = statsQuery.data
  const deadLetters = stats?.deadLetter ?? 0
  const missingQueueApi =
    (statsQuery.isError && isMissingSurface(statsQuery.error)) || (tasksQuery.isError && isMissingSurface(tasksQuery.error))

  return (
    <>
      <PageHeader
        eyebrow="Operations"
        title="Queue & dead-letter ops"
        actions={<span className="form-note">GET /queue/stats · GET /queue/tasks</span>}
      />

      {requeuedNote ? <div className="form-note">{requeuedNote}</div> : null}
      {statsQuery.isError && !isMissingSurface(statsQuery.error) ? (
        <ErrorBanner error={statsQuery.error} onRetry={() => void statsQuery.refetch()} />
      ) : null}
      {tasksQuery.isError && !isMissingSurface(tasksQuery.error) ? (
        <ErrorBanner error={tasksQuery.error} onRetry={() => void tasksQuery.refetch()} />
      ) : null}

      {missingQueueApi ? (
        <EmptyState
          title="Queue ops API not available on this deployment"
          hint="GET /queue/stats answered 404 — the queue-ops surface (issue #77) is not wired here. Everything else keeps working; this panel lights up once the backend ships."
        />
      ) : (
        <section className="summary-grid">
          {statsQuery.isPending ? (
            [0, 1, 2, 3, 4].map((index) => (
              <article key={index} className="mini-stat">
                <Skeleton width={80} />
                <Skeleton height={26} width={60} />
              </article>
            ))
          ) : stats ? (
            <>
              <SummaryStat label="Queued" value={String(stats.queued)} accent="info" />
              <SummaryStat label="Running" value={String(stats.running)} accent="info" />
              <SummaryStat label="Completed" value={String(stats.completed)} accent="success" />
              <SummaryStat label="Dead letter" value={String(stats.deadLetter)} accent={deadLetters > 0 ? 'warning' : 'default'} />
              <SummaryStat label="Total depth" value={String(stats.total)} />
            </>
          ) : null}
        </section>
      )}

      {!missingQueueApi && !statsQuery.isError ? (
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Introspection</p>
              <h3>Tasks</h3>
            </div>
            <label className="inline-label" htmlFor="ops-task-filter">
              Status
              <select id="ops-task-filter" value={statusFilter} onChange={(event) => changeFilter(event.target.value)}>
                {TASK_FILTERS.map((candidate) => (
                  <option key={candidate} value={candidate}>
                    {candidate}
                  </option>
                ))}
              </select>
            </label>
          </div>

          {tasksQuery.isPending ? (
            <div className="stack-gap">
              <Skeleton height={16} />
              <Skeleton height={16} />
              <Skeleton height={16} />
              <Skeleton height={16} />
            </div>
          ) : tasks.length === 0 ? (
            <EmptyState
              title="No tasks in this view"
              hint="Tasks appear here as runs and workflows are enqueued. Dead-lettered tasks need attention — requeue them once the underlying failure is fixed."
            />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Task</th>
                    <th>Type</th>
                    <th>Status</th>
                    <th>Attempts</th>
                    <th>Payload</th>
                    <th>Updated</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {tasks.map((task) => (
                    <tr key={task.id} className={selectedTaskId === task.id ? 'row-selected' : undefined} onClick={() => setSelectedTaskId(task.id === selectedTaskId ? null : task.id)}>
                      <td>{shortenId(task.id)}</td>
                      <td>{task.type || '—'}</td>
                      <td>
                        <StatusPill status={task.status} />
                      </td>
                      <td>{task.attempts}</td>
                      <td>
                        <TaskPayloadCell task={task} />
                      </td>
                      <td>{formatRelativeTime(task.updatedAt)}</td>
                      <td onClick={(event) => event.stopPropagation()}>
                        <TaskRowActions task={task} canRequeue={canRequeue} onRequeued={(updated) => setRequeuedNote(`Task ${shortenId(updated.id)} requeued (now ${updated.status}).`)} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {!tasksQuery.isPending && nextCursor ? (
            <div className="form-actions">
              <span className="form-note">More tasks are available (keyset pagination).</span>
              <button type="button" className="ghost-button" onClick={loadMore}>
                Load next page
              </button>
            </div>
          ) : null}
        </article>
      ) : null}

      {selectedTaskId ? <TaskDetailPanel taskId={selectedTaskId} /> : null}
    </>
  )
}
