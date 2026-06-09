// SPDX-License-Identifier: AGPL-3.0-only

import { useState, useCallback, useMemo, useRef, useEffect } from 'react'
import { Link, useParams } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import StatusBadge from '@/components/StatusBadge'
import { useGetJob, useListTasks } from '@/api/queries'
import { useRetryTask } from '@/api/mutations'
import type { JobDetail as JobDetailType, Step, Task, TaskStatus } from '@/api/types'
import styles from './JobDetail.module.css'

// ── Constants ─────────────────────────────────────────────────────────────────

/** Task statuses that can be retried. */
const RETRYABLE: ReadonlySet<TaskStatus> = new Set(['failed', 'canceled'])

// ── Helpers ───────────────────────────────────────────────────────────────────

function truncateId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}

function formatDateTime(iso: string | undefined): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

/** Computes a human-readable duration between two ISO timestamps.
 *  If endIso is undefined the duration is measured from startIso to now. */
function durationLabel(startIso: string | undefined, endIso: string | undefined): string {
  if (!startIso) return '—'
  const start = new Date(startIso).getTime()
  const end = endIso ? new Date(endIso).getTime() : Date.now()
  const s = Math.floor((end - start) / 1000)
  if (s < 0) return '—'
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

function isTerminalTask(status: TaskStatus): boolean {
  return status === 'succeeded' || status === 'failed' || status === 'canceled'
}

// ── IdCell ────────────────────────────────────────────────────────────────────

function IdCell({ id }: { id: string }) {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (timerRef.current !== null) clearTimeout(timerRef.current)
    },
    [],
  )

  const handleCopy = useCallback(
    (e: React.SyntheticEvent) => {
      e.preventDefault()
      e.stopPropagation()
      void navigator.clipboard
        .writeText(id)
        .then(() => {
          setCopied(true)
          if (timerRef.current !== null) clearTimeout(timerRef.current)
          timerRef.current = setTimeout(() => setCopied(false), 1500)
        })
        .catch(() => {
          // Clipboard write can fail in insecure contexts or when the document
          // is not focused. Silently ignore — no visual change shown.
        })
    },
    [id],
  )

  return (
    <span
      className={styles.idCell}
      onClick={handleCopy}
      title={`Click to copy: ${id}`}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') handleCopy(e)
      }}
    >
      {truncateId(id)}
      <span className={styles.copyHint}>{copied ? '✓' : '⎘'}</span>
    </span>
  )
}

// ── ParametersCell ────────────────────────────────────────────────────────────

function ParametersCell({ params }: { params: Record<string, string> | undefined }) {
  const [expanded, setExpanded] = useState(false)

  if (!params || Object.keys(params).length === 0) {
    return <span className={styles.muted}>—</span>
  }

  const entries = Object.entries(params)

  if (!expanded) {
    const preview = entries
      .slice(0, 2)
      .map(([k, v]) => `${k}=${v}`)
      .join(', ')
    const more = entries.length > 2 ? ` +${entries.length - 2}` : ''
    return (
      <button
        className={styles.paramsToggle}
        onClick={() => setExpanded(true)}
        type="button"
        title="Click to expand parameters"
      >
        {preview}
        {more}
      </button>
    )
  }

  return (
    <div className={styles.paramsExpanded}>
      {entries.map(([k, v]) => (
        <div key={k} className={styles.paramRow}>
          <span className={styles.paramKey}>{k}</span>
          <span className={styles.paramValue}>{v}</span>
        </div>
      ))}
      <button className={styles.paramsToggle} onClick={() => setExpanded(false)} type="button">
        Collapse
      </button>
    </div>
  )
}

// ── MetadataCard ──────────────────────────────────────────────────────────────

function MetadataCard({ job }: { job: JobDetailType }) {
  return (
    <div className={styles.metaCard}>
      <dl className={styles.metaGrid}>
        <div className={styles.metaField}>
          <dt>Job ID</dt>
          <dd>
            <IdCell id={job.id} />
          </dd>
        </div>
        <div className={styles.metaField}>
          <dt>Status</dt>
          <dd>
            <StatusBadge status={job.status} />
          </dd>
        </div>
        <div className={styles.metaField}>
          <dt>Owner</dt>
          <dd>{job.owner}</dd>
        </div>
        <div className={styles.metaField}>
          <dt>Submitter</dt>
          <dd>{job.submitter}</dd>
        </div>
        <div className={styles.metaField}>
          <dt>Priority</dt>
          <dd>{job.priority}</dd>
        </div>
        <div className={styles.metaField}>
          <dt>Queue</dt>
          <dd>{job.queue_name ?? job.queue_id}</dd>
        </div>
        {job.project !== undefined && (
          <div className={styles.metaField}>
            <dt>Project</dt>
            <dd>{job.project}</dd>
          </div>
        )}
        <div className={styles.metaField}>
          <dt>Submitted</dt>
          <dd>{formatDateTime(job.created_at)}</dd>
        </div>
        <div className={styles.metaField}>
          <dt>Started</dt>
          <dd>{formatDateTime(job.started_at)}</dd>
        </div>
        <div className={styles.metaField}>
          <dt>Ended</dt>
          <dd>{formatDateTime(job.completed_at)}</dd>
        </div>
      </dl>
    </div>
  )
}

// ── TaskRow ───────────────────────────────────────────────────────────────────

interface TaskRowProps {
  task: Task
  jobId: string
  isRetrying: boolean
  retryError: string | undefined
  onRetry: (taskId: string) => void
}

function TaskRow({ task, jobId, isRetrying, retryError, onRetry }: TaskRowProps) {
  // While a retry is in-flight, show pending to signal the task is queued.
  const displayStatus: TaskStatus = isRetrying ? 'pending' : task.status
  const canRetry = RETRYABLE.has(task.status) && !isRetrying
  const endTime = isTerminalTask(task.status) ? task.updated_at : undefined

  return (
    <>
      <tr className={isRetrying ? styles.retryingRow : undefined}>
        <td>
          <IdCell id={task.id} />
        </td>
        <td>
          <ParametersCell params={task.parameters} />
        </td>
        <td>
          <StatusBadge status={displayStatus} />
        </td>
        <td>
          {task.assigned_worker_id !== undefined ? (
            <Link
              to={`/workers/${task.assigned_worker_id}`}
              className={styles.workerLink}
              title={task.assigned_worker_id}
            >
              {truncateId(task.assigned_worker_id)}
            </Link>
          ) : (
            <span className={styles.muted}>—</span>
          )}
        </td>
        <td>{formatDateTime(task.assigned_at)}</td>
        <td>{durationLabel(task.assigned_at, endTime)}</td>
        <td>
          <div className={styles.actionCell}>
            {canRetry && (
              <button
                className={styles.retryBtn}
                onClick={() => onRetry(task.id)}
                disabled={isRetrying}
                type="button"
                aria-label={`Retry task ${task.name}`}
              >
                Retry
              </button>
            )}
            {isRetrying && (
              <button className={styles.retryBtn} disabled type="button">
                …
              </button>
            )}
            <Link
              to={`/jobs/${jobId}/tasks/${task.id}/logs`}
              className={styles.logsLink}
              aria-label={`View logs for task ${task.name}`}
            >
              Logs
            </Link>
          </div>
        </td>
      </tr>
      {retryError !== undefined && (
        <tr className={styles.inlineError}>
          <td colSpan={7}>Retry failed: {retryError}</td>
        </tr>
      )}
    </>
  )
}

// ── StepSection ───────────────────────────────────────────────────────────────

function stepCountsFromTasks(tasks: Task[]) {
  let running = 0
  let succeeded = 0
  let failed = 0
  for (const t of tasks) {
    if (t.status === 'running') running++
    else if (t.status === 'succeeded') succeeded++
    else if (t.status === 'failed') failed++
  }
  return { total: tasks.length, running, succeeded, failed }
}

interface StepSectionProps {
  step: Step
  tasks: Task[]
  jobId: string
  retryingIds: ReadonlySet<string>
  retryErrors: ReadonlyMap<string, string>
  onRetry: (taskId: string) => void
}

function StepSection({ step, tasks, jobId, retryingIds, retryErrors, onRetry }: StepSectionProps) {
  const counts = stepCountsFromTasks(tasks)
  const deps = step.depends_on ?? []

  return (
    <section className={styles.stepSection} aria-label={`Step: ${step.name}`}>
      <div className={styles.stepHeader}>
        <div className={styles.stepTitleRow}>
          <h2 className={styles.stepName}>{step.name}</h2>
          <StatusBadge status={step.status} />
        </div>
        {deps.length > 0 && <p className={styles.stepDependsOn}>Depends on: {deps.join(', ')}</p>}
        <div className={styles.stepCounts}>
          <span>
            <span className={styles.countValue}>{counts.total}</span> total
          </span>
          <span>
            <span className={styles.countValue}>{counts.running}</span> running
          </span>
          <span>
            <span className={styles.countValue}>{counts.succeeded}</span> succeeded
          </span>
          {counts.failed > 0 && (
            <span className={styles.countFailed}>
              <span className={styles.countValue}>{counts.failed}</span> failed
            </span>
          )}
        </div>
      </div>

      {tasks.length > 0 ? (
        <div className={styles.tableWrap}>
          <table className={styles.taskTable} aria-label={`Tasks for step ${step.name}`}>
            <thead>
              <tr>
                <th>Task ID</th>
                <th>Parameters</th>
                <th>Status</th>
                <th>Worker</th>
                <th>Start time</th>
                <th>Duration</th>
                <th aria-label="Actions" />
              </tr>
            </thead>
            <tbody>
              {tasks.map((task) => (
                <TaskRow
                  key={task.id}
                  task={task}
                  jobId={jobId}
                  isRetrying={retryingIds.has(task.id)}
                  retryError={retryErrors.get(task.id)}
                  onRetry={onRetry}
                />
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className={styles.emptyTasks}>No tasks for this step.</p>
      )}
    </section>
  )
}

// ── Main component ────────────────────────────────────────────────────────────

export default function JobDetail() {
  const { id } = useParams<{ id: string }>()
  const jobId = id ?? ''

  const { data: job, isLoading, isError, error } = useGetJob(jobId)
  const { data: tasksPage, isLoading: tasksLoading } = useListTasks(jobId, { limit: 1000 })

  const retryTask = useRetryTask()
  const [retryingIds, setRetryingIds] = useState<Set<string>>(new Set())
  const [retryErrors, setRetryErrors] = useState<Map<string, string>>(new Map())

  // Group tasks by step_id so each StepSection can access its tasks directly.
  const tasksByStepId = useMemo(() => {
    const map = new Map<string, Task[]>()
    for (const task of tasksPage?.items ?? []) {
      const arr = map.get(task.step_id) ?? []
      arr.push(task)
      map.set(task.step_id, arr)
    }
    return map
  }, [tasksPage])

  // Steps sorted by step_order (the server should already return them ordered).
  const sortedSteps = useMemo(
    () => [...(job?.steps ?? [])].sort((a, b) => a.step_order - b.step_order),
    [job?.steps],
  )

  const handleRetry = useCallback(
    async (taskId: string) => {
      setRetryingIds((prev) => new Set(prev).add(taskId))
      setRetryErrors((prev) => {
        const n = new Map(prev)
        n.delete(taskId)
        return n
      })

      try {
        await retryTask.mutateAsync(taskId)
      } catch (err) {
        setRetryErrors((prev) =>
          new Map(prev).set(taskId, err instanceof Error ? err.message : 'Retry failed'),
        )
      } finally {
        setRetryingIds((prev) => {
          const n = new Set(prev)
          n.delete(taskId)
          return n
        })
      }
    },
    [retryTask],
  )

  if (isLoading) {
    return (
      <div className={styles.page}>
        <PageHeader title="Job" subtitle="Loading…" />
        <p className={styles.loadingPlaceholder}>Loading job details…</p>
      </div>
    )
  }

  if (isError || job === undefined) {
    return (
      <div className={styles.page}>
        <PageHeader title="Job" />
        <div className={styles.errorBanner} role="alert">
          Failed to load job: {error instanceof Error ? error.message : 'Unknown error'}
        </div>
      </div>
    )
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={job.name}
        subtitle={`ID: ${truncateId(job.id)}`}
        action={<StatusBadge status={job.status} />}
      />

      <MetadataCard job={job} />

      <div className={styles.stepsContainer}>
        <h2 className={styles.sectionTitle}>Steps</h2>
        {tasksLoading && <p className={styles.muted}>Loading tasks…</p>}
        {sortedSteps.map((step) => (
          <StepSection
            key={step.id}
            step={step}
            tasks={tasksByStepId.get(step.id) ?? []}
            jobId={jobId}
            retryingIds={retryingIds}
            retryErrors={retryErrors}
            onRetry={(taskId) => void handleRetry(taskId)}
          />
        ))}
        {sortedSteps.length === 0 && !tasksLoading && (
          <p className={styles.muted}>No steps found.</p>
        )}
      </div>
    </div>
  )
}
