// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useCallback, useMemo, useRef, useEffect } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import PageHeader from '@/components/PageHeader'
import StatusBadge from '@/components/StatusBadge'
import TaskProgressBar from '@/components/TaskProgressBar'
import { Check, Copy, Document, Refresh, Rotate, Spinner, X } from '@/components/icons'
import { useGetJob, useListTasks, useListWorkers, queryKeys } from '@/api/queries'
import { useRetryTask, useCancelTask } from '@/api/mutations'
import { useWebSocket } from '@/ws/context'
import { useLiveNow } from '@/hooks/useLiveNow'
import { formatTimespan } from '@/lib/time'
import { isJobEvent, isTaskEvent, JOB_REMOVED_STATUS } from '@/ws/events'
import type {
  JobDetail as JobDetailType,
  Step,
  StepStatus,
  Task,
  TaskStatus,
  ListResponse,
} from '@/api/types'
import styles from './JobDetail.module.css'

// ── Constants ─────────────────────────────────────────────────────────────────

/** Task statuses that can be retried. */
const RETRYABLE: ReadonlySet<TaskStatus> = new Set(['failed', 'canceled'])

/** Task statuses that can be canceled (all non-terminal). */
const CANCELABLE: ReadonlySet<TaskStatus> = new Set(['pending', 'ready', 'assigned', 'running'])

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

function isTerminalTask(status: TaskStatus): boolean {
  return status === 'succeeded' || status === 'failed' || status === 'canceled'
}

/** Returns true when all of the step's declared dependencies have status "completed". */
function stepDepsSatisfied(step: Step, statusByName: Map<string, StepStatus>): boolean {
  return (step.depends_on ?? []).every((name) => statusByName.get(name) === 'completed')
}

/** Returns a human-readable age string for a past timestamp. */
function formatAge(ts: number, now: number): string {
  if (ts === 0) return '—'
  const diff = now - ts
  if (diff < 5000) return 'just now'
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`
  return new Date(ts).toLocaleTimeString()
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
      <span className={styles.copyHint}>{copied ? <Check size={12} /> : <Copy size={12} />}</span>
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
          <dt>Progress</dt>
          <dd>
            <TaskProgressBar counts={job.task_counts} />
          </dd>
        </div>
        <div className={styles.metaField}>
          <dt>Status</dt>
          <dd>
            <StatusBadge status={job.status} />
          </dd>
        </div>
        <div className={styles.metaField}>
          <dt>Priority</dt>
          <dd>{job.priority}</dd>
        </div>
        <div className={styles.metaField}>
          <dt>Queue</dt>
          <dd>
            <Link to="/queues">{job.queue_name ?? job.queue_id}</Link>
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
  isCanceling: boolean
  cancelError: string | undefined
  onCancel: (taskId: string) => void
  workerNamesById: ReadonlyMap<string, string>
  now: number
  isSelected: boolean
  onToggleSelect: (id: string) => void
  /** True when this task's enclosing step has all its dependencies completed. */
  depsSatisfied: boolean
}

function TaskRow({
  task,
  jobId,
  isRetrying,
  retryError,
  onRetry,
  isCanceling,
  cancelError,
  onCancel,
  workerNamesById,
  now,
  isSelected,
  onToggleSelect,
  depsSatisfied,
}: TaskRowProps) {
  // While a retry is in-flight, show pending to signal the task is queued.
  const displayStatus: TaskStatus = isRetrying ? 'pending' : task.status
  const canRetry = RETRYABLE.has(task.status) && !isRetrying && depsSatisfied
  const canCancel = CANCELABLE.has(task.status) && !isCanceling
  const endTime = isTerminalTask(task.status) ? task.updated_at : undefined

  return (
    <>
      <tr
        className={
          [isRetrying ? styles.retryingRow : '', isCanceling ? styles.cancelingRow : '']
            .filter(Boolean)
            .join(' ') || undefined
        }
      >
        <td className={styles.checkCell}>
          {(CANCELABLE.has(task.status) || (RETRYABLE.has(task.status) && depsSatisfied)) && (
            <input
              type="checkbox"
              aria-label={`Select task ${task.name}`}
              checked={isSelected}
              onChange={() => onToggleSelect(task.id)}
            />
          )}
        </td>
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
              {workerNamesById.get(task.assigned_worker_id) ?? truncateId(task.assigned_worker_id)}
            </Link>
          ) : (
            <span className={styles.muted}>—</span>
          )}
        </td>
        <td>{formatDateTime(task.assigned_at)}</td>
        <td>{formatTimespan(task.assigned_at, endTime, now)}</td>
        <td>
          <div className={styles.actionCell}>
            <Link
              to={`/jobs/${jobId}/tasks/${task.id}/logs`}
              className={styles.logsLink}
              title="Logs"
              aria-label={`View logs for task ${task.name}`}
            >
              <Document />
            </Link>
            {(canRetry || isRetrying) && (
              <button
                className={styles.retryBtn}
                onClick={() => onRetry(task.id)}
                disabled={isRetrying}
                type="button"
                title="Retry"
                aria-label={`Retry task ${task.name}`}
              >
                {isRetrying ? <Spinner /> : <Rotate />}
              </button>
            )}
            {(canCancel || isCanceling) && (
              <button
                className={styles.cancelBtn}
                onClick={() => onCancel(task.id)}
                disabled={isCanceling}
                type="button"
                title="Cancel"
                aria-label={`Cancel task ${task.name}`}
              >
                {isCanceling ? <Spinner /> : <X />}
              </button>
            )}
          </div>
        </td>
      </tr>
      {(retryError !== undefined || cancelError !== undefined) && (
        <tr className={styles.inlineError}>
          <td colSpan={8}>
            {retryError !== undefined && <>Retry failed: {retryError}</>}
            {retryError !== undefined && cancelError !== undefined && ' '}
            {cancelError !== undefined && <>Cancel failed: {cancelError}</>}
          </td>
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
  cancelingIds: ReadonlySet<string>
  cancelErrors: ReadonlyMap<string, string>
  onCancel: (taskId: string) => void
  workerNamesById: ReadonlyMap<string, string>
  now: number
  selectedTaskIds: ReadonlySet<string>
  onToggleSelect: (id: string) => void
  onToggleStep: () => void
  /** True when all of this step's declared dependencies have status "completed". */
  depsSatisfied: boolean
}

function StepSection({
  step,
  tasks,
  jobId,
  retryingIds,
  retryErrors,
  onRetry,
  cancelingIds,
  cancelErrors,
  onCancel,
  workerNamesById,
  now,
  selectedTaskIds,
  onToggleSelect,
  onToggleStep,
  depsSatisfied,
}: StepSectionProps) {
  const counts = stepCountsFromTasks(tasks)
  const deps = step.depends_on ?? []
  const selectableInStep = tasks.filter(
    (t) => CANCELABLE.has(t.status) || (RETRYABLE.has(t.status) && depsSatisfied),
  )

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
                <th className={styles.checkCell}>
                  <input
                    type="checkbox"
                    aria-label={`Select all tasks in step ${step.name}`}
                    checked={
                      selectableInStep.length > 0 &&
                      selectableInStep.every((t) => selectedTaskIds.has(t.id))
                    }
                    disabled={selectableInStep.length === 0}
                    onChange={onToggleStep}
                  />
                </th>
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
                  isCanceling={cancelingIds.has(task.id)}
                  cancelError={cancelErrors.get(task.id)}
                  onCancel={onCancel}
                  workerNamesById={workerNamesById}
                  now={now}
                  isSelected={selectedTaskIds.has(task.id)}
                  onToggleSelect={onToggleSelect}
                  depsSatisfied={depsSatisfied}
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

  const { data: job, isLoading, isError, error, dataUpdatedAt: jobUpdatedAt } = useGetJob(jobId)
  const {
    data: tasksPage,
    isLoading: tasksLoading,
    dataUpdatedAt: tasksUpdatedAt,
  } = useListTasks(jobId, { limit: 1000 })

  // Worker display names for the task list (tasks only carry assigned_worker_id).
  // Prefer the human-readable name, falling back to hostname — matching the
  // worker list/detail pages.
  const { data: workersPage } = useListWorkers({ limit: 1000 })
  const workerNamesById = useMemo(() => {
    const map = new Map<string, string>()
    for (const worker of workersPage?.items ?? []) {
      map.set(worker.id, worker.name || worker.hostname)
    }
    return map
  }, [workersPage])

  const queryClient = useQueryClient()
  const retryTask = useRetryTask()
  const [retryingIds, setRetryingIds] = useState<Set<string>>(new Set())
  const [retryErrors, setRetryErrors] = useState<Map<string, string>>(new Map())
  const cancelTask = useCancelTask()
  const [cancelingIds, setCancelingIds] = useState<Set<string>>(new Set())
  const [cancelErrors, setCancelErrors] = useState<Map<string, string>>(new Map())

  const [selectedTaskIds, setSelectedTaskIds] = useState<Set<string>>(new Set())

  const toggleTask = useCallback((id: string) => {
    setSelectedTaskIds((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])

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

  // Map of step name → step status, used to evaluate cross-step dependency satisfaction.
  const stepStatusByName = useMemo(() => {
    const map = new Map<string, StepStatus>()
    for (const step of job?.steps ?? []) {
      map.set(step.name, step.status)
    }
    return map
  }, [job?.steps])

  // Per-step: whether all declared dependencies are completed (mirrors backend allDepsCompleted).
  const depsSatisfiedByStepId = useMemo(() => {
    const map = new Map<string, boolean>()
    for (const step of sortedSteps) {
      map.set(step.id, stepDepsSatisfied(step, stepStatusByName))
    }
    return map
  }, [sortedSteps, stepStatusByName])

  // Per-task: inherit the enclosing step's dependency-satisfied flag.
  const depsSatisfiedByTaskId = useMemo(() => {
    const map = new Map<string, boolean>()
    for (const [stepId, tasks] of tasksByStepId) {
      const sat = depsSatisfiedByStepId.get(stepId) ?? true
      for (const t of tasks) {
        map.set(t.id, sat)
      }
    }
    return map
  }, [tasksByStepId, depsSatisfiedByStepId])

  // ── WS-driven task-level updates ─────────────────────────────────

  // Debounce job-detail invalidation so rapid task bursts only trigger one
  // background re-fetch (which refreshes step statuses, job aggregate status, etc.)
  const invalidateTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (invalidateTimerRef.current) clearTimeout(invalidateTimerRef.current)
    },
    [],
  )

  const scheduleDetailInvalidate = useCallback(() => {
    if (invalidateTimerRef.current) clearTimeout(invalidateTimerRef.current)
    invalidateTimerRef.current = setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.detail(jobId) })
      // Use a 3-element prefix — queryKeys.tasks.list(jobId) appends an
      // undefined 4th element that partialDeepEqual treats as a mismatch
      // against the active key's { limit: 1000 } params object.
      void queryClient.invalidateQueries({ queryKey: ['tasks', 'list', jobId] })
    }, 5_000)
  }, [queryClient, jobId])

  // Subscribe to per-job task updates.
  useWebSocket(`jobs/${jobId}/tasks`, (payload) => {
    if (!isTaskEvent(payload)) return

    // Patch the task status in the tasks query cache immediately.
    queryClient.setQueriesData<ListResponse<Task>>({ queryKey: queryKeys.tasks.all }, (old) => {
      if (!old) return old
      const idx = old.items.findIndex((t) => t.id === payload.task_id)
      if (idx === -1) return old
      const prev = old.items[idx]
      if (!prev) return old
      const newItems = [...old.items]
      newItems[idx] = {
        ...prev,
        status: payload.status,
        updated_at: payload.updated_at,
        ...(payload.worker_id ? { assigned_worker_id: payload.worker_id } : {}),
      }
      return { ...old, items: newItems }
    })

    // Schedule a background sync to refresh step statuses and job aggregate status.
    scheduleDetailInvalidate()
  })

  // Also subscribe to the "jobs" subject to capture job-level status changes
  // (e.g. the job transitioning from running → completed).
  useWebSocket('jobs', (payload) => {
    if (!isJobEvent(payload) || payload.job_id !== jobId) return
    // 'removed' means the job was hard-deleted; skip the optimistic patch and
    // let the query invalidation (triggered by the delete mutation) handle it.
    if (payload.status === JOB_REMOVED_STATUS) return

    const { status, updated_at } = payload
    queryClient.setQueriesData<JobDetailType>(
      { queryKey: queryKeys.jobs.detail(jobId), exact: true },
      (old) => {
        if (!old) return old
        return { ...old, status, updated_at }
      },
    )
  })

  // ── Live clock ──────────────────────────────────────────────────
  // Tick every second while the job is active so task durations and the
  // "Updated X ago" label stay alive; otherwise 30s.
  const jobActive = job?.status === 'running' || job?.status === 'pending'
  const now = useLiveNow(jobActive)

  const lastUpdated = Math.max(jobUpdatedAt, tasksUpdatedAt)

  // ── Manual refresh ───────────────────────────────────────────────

  const handleRefresh = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.detail(jobId) })
    void queryClient.invalidateQueries({ queryKey: ['tasks', 'list', jobId] })
  }, [queryClient, jobId])

  // ── Retry ─────────────────────────────────────────────────────────────────

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
        setSelectedTaskIds((prev) => {
          const n = new Set(prev)
          n.delete(taskId)
          return n
        })
      }
    },
    [retryTask],
  )

  // ── Cancel ────────────────────────────────────────────────────────────────

  const handleCancel = useCallback(
    async (taskId: string) => {
      setCancelingIds((prev) => new Set(prev).add(taskId))
      setCancelErrors((prev) => {
        const n = new Map(prev)
        n.delete(taskId)
        return n
      })

      try {
        await cancelTask.mutateAsync(taskId)
      } catch (err) {
        setCancelErrors((prev) =>
          new Map(prev).set(taskId, err instanceof Error ? err.message : 'Cancel failed'),
        )
      } finally {
        setCancelingIds((prev) => {
          const n = new Set(prev)
          n.delete(taskId)
          return n
        })
        setSelectedTaskIds((prev) => {
          const n = new Set(prev)
          n.delete(taskId)
          return n
        })
      }
    },
    [cancelTask],
  )

  // ── Bulk selection ────────────────────────────────────────────────────────

  const selectedTasks = useMemo(
    () => (tasksPage?.items ?? []).filter((t) => selectedTaskIds.has(t.id)),
    [tasksPage, selectedTaskIds],
  )
  const selectedCancelable = useMemo(
    () => selectedTasks.filter((t) => CANCELABLE.has(t.status)),
    [selectedTasks],
  )
  const selectedRetryable = useMemo(
    () =>
      selectedTasks.filter(
        (t) => RETRYABLE.has(t.status) && (depsSatisfiedByTaskId.get(t.id) ?? true),
      ),
    [selectedTasks, depsSatisfiedByTaskId],
  )

  // Count of selected tasks that are still actionable (excludes ghost selections
  // for tasks that have since transitioned to a non-selectable terminal state,
  // and dep-blocked retryable tasks that can't meaningfully run in isolation).
  const activeSelectedCount = useMemo(
    () =>
      selectedTasks.filter(
        (t) =>
          CANCELABLE.has(t.status) ||
          (RETRYABLE.has(t.status) && (depsSatisfiedByTaskId.get(t.id) ?? true)),
      ).length,
    [selectedTasks, depsSatisfiedByTaskId],
  )

  const handleBulkCancel = useCallback(async () => {
    for (const t of selectedCancelable) {
      await handleCancel(t.id)
    }
  }, [selectedCancelable, handleCancel])

  const handleBulkRetry = useCallback(async () => {
    for (const t of selectedRetryable) {
      await handleRetry(t.id)
    }
  }, [selectedRetryable, handleRetry])

  if (isLoading) {
    return (
      <div className={styles.page}>
        <PageHeader title="Job Details" subtitle="Loading…" />
        <p className={styles.loadingPlaceholder}>Loading job details…</p>
      </div>
    )
  }

  if (isError || job === undefined) {
    return (
      <div className={styles.page}>
        <PageHeader title="Job Details" />
        <div className={styles.errorBanner} role="alert">
          Failed to load job: {error instanceof Error ? error.message : 'Unknown error'}
        </div>
      </div>
    )
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title="Job Details"
        action={
          <div className={styles.headerActions}>
            <StatusBadge status={job.status} />
            {lastUpdated > 0 && (
              <span className={styles.lastUpdated} aria-live="polite">
                Updated {formatAge(lastUpdated, now)}
              </span>
            )}
            <button
              className={styles.refreshBtn}
              onClick={handleRefresh}
              type="button"
              aria-label="Refresh job data"
            >
              <Refresh />
              Refresh
            </button>
          </div>
        }
      />

      <div className={styles.jobNameArea}>
        <h2 className={styles.jobName}>{job.name}</h2>
      </div>

      <MetadataCard job={job} />

      <div className={styles.stepsContainer}>
        <h2 className={styles.sectionTitle}>Steps</h2>
        {tasksLoading && <p className={styles.muted}>Loading tasks…</p>}
        {sortedSteps.map((step) => {
          const stepTasks = tasksByStepId.get(step.id) ?? []
          const depsSatisfied = depsSatisfiedByStepId.get(step.id) ?? true
          const selectable = stepTasks.filter(
            (t) => CANCELABLE.has(t.status) || (RETRYABLE.has(t.status) && depsSatisfied),
          )
          const allSelected =
            selectable.length > 0 && selectable.every((t) => selectedTaskIds.has(t.id))
          return (
            <StepSection
              key={step.id}
              step={step}
              tasks={stepTasks}
              jobId={jobId}
              retryingIds={retryingIds}
              retryErrors={retryErrors}
              onRetry={(taskId) => void handleRetry(taskId)}
              cancelingIds={cancelingIds}
              cancelErrors={cancelErrors}
              onCancel={(taskId) => void handleCancel(taskId)}
              workerNamesById={workerNamesById}
              now={now}
              selectedTaskIds={selectedTaskIds}
              onToggleSelect={toggleTask}
              onToggleStep={() => {
                setSelectedTaskIds((prev) => {
                  const next = new Set(prev)
                  if (allSelected) {
                    selectable.forEach((t) => next.delete(t.id))
                  } else {
                    selectable.forEach((t) => next.add(t.id))
                  }
                  return next
                })
              }}
              depsSatisfied={depsSatisfied}
            />
          )
        })}
        {sortedSteps.length === 0 && !tasksLoading && (
          <p className={styles.muted}>No steps found.</p>
        )}
      </div>

      {activeSelectedCount > 0 && (
        <div className={styles.bulkBar}>
          <span className={styles.bulkBarCount}>{activeSelectedCount} selected</span>
          <button
            className={styles.bulkCancelBtn}
            onClick={() => void handleBulkCancel()}
            disabled={selectedCancelable.length === 0 || cancelTask.isPending}
            type="button"
            aria-label={`Cancel selected (${selectedCancelable.length})`}
          >
            <X />
            Cancel {selectedCancelable.length}
          </button>
          <button
            className={styles.bulkRetryBtn}
            onClick={() => void handleBulkRetry()}
            disabled={selectedRetryable.length === 0 || retryTask.isPending}
            type="button"
            aria-label={`Retry selected (${selectedRetryable.length})`}
          >
            <Rotate />
            Retry {selectedRetryable.length}
          </button>
          <button
            className={styles.clearBtn}
            onClick={() => setSelectedTaskIds(new Set())}
            type="button"
          >
            Clear
          </button>
        </div>
      )}
    </div>
  )
}
