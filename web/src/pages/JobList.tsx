// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useCallback, useRef, useEffect } from 'react'
import type { ReactNode } from 'react'
import { Link } from 'react-router'
import { useQueryClient } from '@tanstack/react-query'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import FilterToolbar from '@/components/FilterToolbar'
import Pagination from '@/components/Pagination'
import StatusBadge from '@/components/StatusBadge'
import TaskProgressBar from '@/components/TaskProgressBar'
import RemainingTime from '@/components/RemainingTime'
import {
  ChevronDown,
  ChevronUp,
  ChevronUpDown,
  Pause,
  Play,
  Rotate,
  Trash,
  X,
} from '@/components/icons'
import ErrorBanner from '@/components/ErrorBanner'
import BulkBar from '@/components/BulkBar'
import CopyableId from '@/components/CopyableId'
import RefreshControls from '@/components/RefreshControls'
import { useListJobs, queryKeys } from '@/api/queries'
import { useJobActions } from '@/api/mutations'
import { useActionRunner } from '@/hooks/useActionRunner'
import { useListFilters } from '@/hooks/useListFilters'
import type { SortDirection } from '@/hooks/useListFilters'
import { useLiveNow } from '@/hooks/useLiveNow'
import { countDownSeconds, formatTimespan } from '@/lib/time'
import { useWebSocket } from '@/ws/context'
import {
  isJobEvent,
  isStaleEvent,
  isTaskEvent,
  JOB_CHANGE_ESTIMATE,
  JOB_REMOVED_STATUS,
} from '@/ws/events'
import { canCancelJob, canPauseJob, canResumeJob, canRetryJob } from '@/lib/jobStatus'
import type { JobAction } from '@/lib/jobStatus'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import type { Job, JobStatus, TaskCounts, TaskStatus, ListResponse } from '@/api/types'
import styles from './JobList.module.css'

// ── Constants ─────────────────────────────────────────────────────────────────

type JobSortField = 'name' | 'priority' | 'created_at' | 'status'

const JOB_STATUSES = new Set(['pending', 'running', 'paused', 'completed', 'failed', 'canceled'])
const JOB_SORT_FIELDS = new Set(['name', 'priority', 'created_at', 'status'])

/** Status filter groups shown above the table. */
const STATUS_FILTERS: { label: string; value: JobStatus | '' }[] = [
  { label: 'All', value: '' },
  { label: 'Running', value: 'running' },
  { label: 'Pending', value: 'pending' },
  { label: 'Paused', value: 'paused' },
  { label: 'Completed', value: 'completed' },
  { label: 'Failed', value: 'failed' },
  { label: 'Canceled', value: 'canceled' },
]

// ── Helpers ───────────────────────────────────────────────────────────────────

function formatTime(iso: string | undefined): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

// ── Sub-components ────────────────────────────────────────────────────────────

/**
 * Renders a sortable column header button. The parent `<th>` must supply
 * `aria-sort` — this component exposes `ariaSortValue` for that purpose.
 */
function SortableHeader({
  field,
  label,
  sortField,
  sortDir,
  onSort,
}: {
  field: JobSortField
  label: string
  sortField: JobSortField
  sortDir: SortDirection
  onSort: (field: JobSortField, dir: SortDirection) => void
}) {
  const active = sortField === field
  const nextDir: SortDirection = active && sortDir === 'asc' ? 'desc' : 'asc'
  return (
    <span
      className={styles.sortableHeader}
      onClick={() => onSort(field, nextDir)}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onSort(field, nextDir)
        }
      }}
    >
      {label}
      <span className={styles.sortIcon} aria-hidden="true">
        {active ? (
          sortDir === 'asc' ? (
            <ChevronUp size={12} />
          ) : (
            <ChevronDown size={12} />
          )
        ) : (
          <ChevronUpDown size={12} />
        )}
      </span>
    </span>
  )
}

function sortAriaValue(
  field: JobSortField,
  activeField: JobSortField,
  dir: SortDirection,
): 'ascending' | 'descending' | 'none' {
  if (field !== activeField) return 'none'
  return dir === 'asc' ? 'ascending' : 'descending'
}

function ProgressCell({ job }: { job: Job }) {
  const counts = job.task_counts
  if (!counts) return <span>—</span>
  return <TaskProgressBar counts={counts} progressPercent={job.progress_percent} />
}

// ── Task-count delta helper ───────────────────────────────────────────────────

function applyTaskStatusDelta(
  counts: TaskCounts,
  prevStatus: TaskStatus | undefined,
  newStatus: TaskStatus,
): TaskCounts {
  const c = { ...counts }
  if (prevStatus !== undefined) {
    c[prevStatus] = Math.max(0, c[prevStatus] - 1)
  }
  c[newStatus] = c[newStatus] + 1
  return c
}

/**
 * Applies a task-status delta to a list row. When the delta changes how many
 * tasks are running, the remaining-time countdown is rebased first: what it
 * has counted down so far, at the old rate, becomes the row's figure anchored
 * at `nowMs`, so the new rate applies from now on rather than to time already
 * counted.
 */
function applyTaskDelta(
  job: Job,
  counts: TaskCounts,
  prevStatus: TaskStatus,
  newStatus: TaskStatus,
  nowMs: number,
): Job {
  const next = applyTaskStatusDelta(counts, prevStatus, newStatus)
  const rem = job.remaining_seconds
  if (
    rem === null ||
    rem === undefined ||
    job.received_at === undefined ||
    next.running === counts.running
  ) {
    return { ...job, task_counts: next }
  }
  return {
    ...job,
    task_counts: next,
    remaining_seconds: countDownSeconds(rem, counts.running, job.received_at, nowMs),
    received_at: nowMs,
  }
}

// ── Main component ────────────────────────────────────────────────────────────

/** One action in the bulk bar: disabled while it runs or when no selected job applies. */
function BulkButton({
  verb,
  icon,
  className,
  count,
  running,
  onClick,
}: {
  verb: JobAction
  icon: ReactNode
  className: string | undefined
  count: number
  running: boolean
  onClick: () => void
}) {
  return (
    <button
      className={`${styles.bulkBtn} ${className ?? ''}`}
      onClick={onClick}
      disabled={count === 0 || running}
      type="button"
      aria-label={`${verb} selected (${count})`}
    >
      {icon}
      {verb} {count}
    </button>
  )
}

export default function JobList() {
  const { principal } = useAuth()
  const canWrite = can(principal, 'jobs.write')

  const filters = useListFilters<JobStatus, JobSortField>({
    statuses: JOB_STATUSES,
    sortFields: JOB_SORT_FIELDS,
    defaultSortField: 'created_at',
    defaultSortDir: 'desc',
  })

  const queryParams: Parameters<typeof useListJobs>[0] = {
    ...(filters.status ? { status: filters.status } : {}),
    ...(filters.search ? { search: filters.search } : {}),
    sort_by: filters.sortField,
    sort_dir: filters.sortDir,
    limit: filters.pageSize,
    offset: (filters.page - 1) * filters.pageSize,
    include_timing: true,
  }

  const { data, isLoading, isError, error, dataUpdatedAt } = useListJobs(queryParams)
  const jobs = data?.items ?? []
  const total = data?.total ?? 0

  const queryClient = useQueryClient()
  const jobActions = useJobActions()
  // Pending confirmation: either a single job id or the selected-bulk set.
  const [confirm, setConfirm] = useState<{ ids: string[]; bulk: boolean } | null>(null)

  // ── Selection state ── declared early so the WS callback below can reference
  // setSelectedIds without a forward-declaration lint error.
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set())

  const deselect = useCallback((id: string) => {
    setSelectedIds((prev) => {
      if (!prev.has(id)) return prev
      const next = new Set(prev)
      next.delete(id)
      return next
    })
  }, [])
  // Every action deselects each job as it finishes.
  const action = useActionRunner<JobAction>('jobs', deselect)

  // ── WS-driven in-place updates ───────────────────────────────────

  // Tracks the last-known status of each task so we can apply accurate count
  // deltas when task events arrive.  Map is keyed by task_id.
  const taskStatusRef = useRef(new Map<string, TaskStatus>())

  // Debounce list invalidation so rapid task bursts only cause one background
  // re-fetch.
  const invalidateTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const scheduleListInvalidate = useCallback(() => {
    // Never reset a pending timer: a steady event stream would starve it.
    if (invalidateTimerRef.current) return
    invalidateTimerRef.current = setTimeout(() => {
      invalidateTimerRef.current = null
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
    }, 5_000)
  }, [queryClient])

  useEffect(
    () => () => {
      if (invalidateTimerRef.current) clearTimeout(invalidateTimerRef.current)
    },
    [],
  )

  // WS subscription — updates job rows in-place without a full list re-fetch.
  useWebSocket('jobs', (payload) => {
    if (isJobEvent(payload)) {
      if (payload.status === JOB_REMOVED_STATUS) {
        // Hard-deleted: evict the job row from every cached list page immediately.
        queryClient.setQueriesData<ListResponse<Job>>({ queryKey: ['jobs', 'list'] }, (old) => {
          if (!old) return old
          const items = old.items.filter((j) => j.id !== payload.job_id)
          if (items.length === old.items.length) return old
          return { ...old, items, total: Math.max(0, old.total - 1) }
        })
        setSelectedIds((prev) => {
          if (!prev.has(payload.job_id)) return prev
          const next = new Set(prev)
          next.delete(payload.job_id)
          return next
        })
        return
      }
      if (payload.change === JOB_CHANGE_ESTIMATE) {
        // Only the estimate moved; the status it carries may be stale, so
        // refetch the lists (which also recomputes remaining time).
        void queryClient.invalidateQueries({ queryKey: ['jobs', 'list'] })
        return
      }
      // Patch this job's status immediately in every cached list page.
      const { status, updated_at } = payload
      queryClient.setQueriesData<ListResponse<Job>>({ queryKey: ['jobs', 'list'] }, (old) => {
        if (!old) return old
        const idx = old.items.findIndex((j) => j.id === payload.job_id)
        if (idx === -1) return old
        const newItems = [...old.items]
        const prev = newItems[idx]
        if (!prev || isStaleEvent(payload, prev)) return old
        newItems[idx] = { ...prev, status, updated_at }
        return { ...old, items: newItems }
      })
    } else if (isTaskEvent(payload)) {
      // Apply a task-count delta to the parent job row.
      const prevStatus = taskStatusRef.current.get(payload.task_id)
      taskStatusRef.current.set(payload.task_id, payload.status)

      if (prevStatus !== undefined) {
        queryClient.setQueriesData<ListResponse<Job>>({ queryKey: ['jobs', 'list'] }, (old) => {
          if (!old) return old
          const idx = old.items.findIndex((j) => j.id === payload.job_id)
          if (idx === -1) return old
          const job = old.items[idx]
          if (!job?.task_counts) return old
          const newItems = [...old.items]
          newItems[idx] = applyTaskDelta(
            job,
            job.task_counts,
            prevStatus,
            payload.status,
            Date.now(),
          )
          return { ...old, items: newItems }
        })
      }

      // Schedule a background sync so task_counts stay accurate even when
      // prevStatus is unknown (first event for a task) or we missed events.
      scheduleListInvalidate()
    }
  })

  // ── Live clock ──────────────────────────────────────────────────
  // Tick every second while a job on this page is active so the
  // "Elapsed" column and "Updated X ago" label stay alive; otherwise 30s.
  const hasActiveJob = jobs.some((j) => j.status === 'running' || j.status === 'pending')
  const now = useLiveNow(hasActiveJob)

  // ── Manual refresh ───────────────────────────────────────────────

  const handleRefresh = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
  }, [queryClient])

  // ── Derived selection values ──────────────────────────────────────────────

  const selectedWhere = (pred: (job: Job) => boolean) =>
    jobs.filter((j) => selectedIds.has(j.id) && pred(j)).map((j) => j.id)
  const selectedCancelable = selectedWhere(canCancelJob)
  const selectedRetryable = selectedWhere(canRetryJob)
  const selectedPausable = selectedWhere(canPauseJob)
  const selectedResumable = selectedWhere(canResumeJob)

  // A job is selectable if any bulk action applies to it. Every pausable or
  // resumable status is also cancelable, so pause and resume add nothing here.
  const selectableJobs = jobs.filter((j) => canCancelJob(j) || canRetryJob(j))

  const toggleRow = useCallback((id: string) => {
    setSelectedIds((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])

  const toggleAll = useCallback(() => {
    if (selectableJobs.length === 0) return
    const allSelected = selectableJobs.every((j) => selectedIds.has(j.id))
    setSelectedIds(allSelected ? new Set() : new Set(selectableJobs.map((j) => j.id)))
  }, [selectableJobs, selectedIds])

  // ── Delete ────────────────────────────────────────────────────────────────

  // Per-row and bulk delete both go through the confirm dialog.
  const confirmDelete = async () => {
    if (!confirm) return
    const ids = confirm.ids
    setConfirm(null)
    await action.run('Delete', ids, jobActions.Delete)
  }

  // ── Sort handler ──────────────────────────────────────────────────────────

  const { setSortFieldAndDir } = filters
  const handleSort = useCallback(
    (field: JobSortField, dir: SortDirection) => {
      setSortFieldAndDir(field, dir)
    },
    [setSortFieldAndDir],
  )

  const allSelectableSelected =
    selectableJobs.length > 0 && selectableJobs.every((j) => selectedIds.has(j.id))

  return (
    <div className={styles.page}>
      <PageHeader
        title="Jobs"
        subtitle={isLoading ? 'Loading…' : `${total} jobs`}
        action={
          <RefreshControls
            onRefresh={handleRefresh}
            label="Refresh jobs"
            updatedAt={dataUpdatedAt}
            now={now}
          />
        }
      />

      <FilterToolbar
        statuses={STATUS_FILTERS}
        activeStatus={filters.status}
        onStatusChange={(v) => filters.setStatus(v as JobStatus | '')}
        search={filters.search}
        onSearchChange={filters.setSearch}
        searchPlaceholder="Search by name, ID, or owner…"
        searchLabel="Search jobs"
      />

      {isError && (
        <ErrorBanner>
          Failed to load jobs: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}
      {[...action.errors].map(([verb, message]) => (
        <ErrorBanner key={verb}>{message}</ErrorBanner>
      ))}

      {/* Job table */}
      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Jobs">
          <thead>
            <tr>
              <th className={styles.checkCell}>
                <input
                  type="checkbox"
                  aria-label="Select all jobs"
                  checked={allSelectableSelected}
                  onChange={toggleAll}
                  disabled={selectableJobs.length === 0}
                />
              </th>
              <th aria-sort={sortAriaValue('name', filters.sortField, filters.sortDir)}>
                <SortableHeader
                  field="name"
                  label="Name"
                  sortField={filters.sortField}
                  sortDir={filters.sortDir}
                  onSort={handleSort}
                />
              </th>
              <th>ID</th>
              <th>Owner</th>
              <th>Queue</th>
              <th aria-sort={sortAriaValue('status', filters.sortField, filters.sortDir)}>
                <SortableHeader
                  field="status"
                  label="Status"
                  sortField={filters.sortField}
                  sortDir={filters.sortDir}
                  onSort={handleSort}
                />
              </th>
              <th aria-sort={sortAriaValue('priority', filters.sortField, filters.sortDir)}>
                <SortableHeader
                  field="priority"
                  label="Priority"
                  sortField={filters.sortField}
                  sortDir={filters.sortDir}
                  onSort={handleSort}
                />
              </th>
              <th>Progress</th>
              <th>Remaining</th>
              <th aria-sort={sortAriaValue('created_at', filters.sortField, filters.sortDir)}>
                <SortableHeader
                  field="created_at"
                  label="Submitted"
                  sortField={filters.sortField}
                  sortDir={filters.sortDir}
                  onSort={handleSort}
                />
              </th>
              <th>Elapsed</th>
              <th aria-label="Actions" className={styles.actionsCell} />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={styles.emptyRow}>
                <td colSpan={12}>Loading…</td>
              </tr>
            )}
            {!isLoading && jobs.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={12}>No jobs found.</td>
              </tr>
            )}
            {jobs.map((job) => {
              const isSelected = selectedIds.has(job.id)
              // Cancel hides while in flight, so a second click can't send it twice.
              const isCanceling = action.isBusy('Cancel', job.id)
              return (
                <tr key={job.id} className={isSelected ? styles.rowSelected : undefined}>
                  <td className={styles.checkCell}>
                    <input
                      type="checkbox"
                      aria-label={`Select job ${job.name}`}
                      checked={isSelected}
                      onChange={() => toggleRow(job.id)}
                    />
                  </td>
                  <td>
                    <Link to={`/jobs/${job.id}`}>{job.name}</Link>
                  </td>
                  <td>
                    <CopyableId id={job.id} />
                  </td>
                  <td>{job.owner}</td>
                  <td>
                    <Link to="/queues">{job.queue_name ?? job.queue_id}</Link>
                  </td>
                  <td>
                    <StatusBadge status={job.status} />
                    {(job.task_counts?.unschedulable ?? 0) > 0 && (
                      <span
                        className={styles.unschedulableIndicator}
                        title={`${job.task_counts?.unschedulable} unschedulable`}
                      >
                        {job.task_counts?.unschedulable} unschedulable
                      </span>
                    )}
                  </td>
                  <td>{job.priority}</td>
                  <td>
                    <ProgressCell job={job} />
                  </td>
                  <td>
                    <RemainingTime
                      seconds={job.remaining_seconds}
                      running={job.task_counts?.running ?? 0}
                      anchorMs={job.received_at ?? dataUpdatedAt}
                      now={now}
                    />
                  </td>
                  <td>{formatTime(job.created_at)}</td>
                  <td>{formatTimespan(job.started_at, job.completed_at, now)}</td>
                  <td className={styles.actionsCell}>
                    {canWrite && canPauseJob(job) && (
                      <IconButton
                        icon={<Pause />}
                        className={styles.pauseBtn}
                        onClick={() => void action.run('Pause', [job.id], jobActions.Pause)}
                        busy={action.isBusy('Pause', job.id)}
                        title="Pause"
                        label={`Pause job ${job.name}`}
                      />
                    )}
                    {canWrite && canResumeJob(job) && (
                      <IconButton
                        icon={<Play />}
                        className={styles.resumeBtn}
                        onClick={() => void action.run('Resume', [job.id], jobActions.Resume)}
                        busy={action.isBusy('Resume', job.id)}
                        title="Resume"
                        label={`Resume job ${job.name}`}
                      />
                    )}
                    {canWrite && canCancelJob(job) && !isCanceling && (
                      <IconButton
                        icon={<X />}
                        className={styles.cancelBtn}
                        onClick={() => void action.run('Cancel', [job.id], jobActions.Cancel)}
                        title="Cancel"
                        label={`Cancel job ${job.name}`}
                      />
                    )}
                    {canWrite && canRetryJob(job) && (
                      <IconButton
                        icon={<Rotate />}
                        className={styles.retryBtn}
                        onClick={() => void action.run('Retry', [job.id], jobActions.Retry)}
                        busy={action.isBusy('Retry', job.id)}
                        title="Retry failed and canceled tasks"
                        label={`Retry job ${job.name}`}
                      />
                    )}
                    {canWrite && (
                      <IconButton
                        icon={<Trash />}
                        className={styles.deleteBtn}
                        onClick={() => setConfirm({ ids: [job.id], bulk: false })}
                        busy={action.isBusy('Delete', job.id)}
                        title="Delete"
                        label={`Delete job ${job.name}`}
                      />
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      {total > 0 && (
        <Pagination
          page={filters.page}
          totalPages={Math.max(1, Math.ceil(total / filters.pageSize))}
          pageSize={filters.pageSize}
          total={total}
          hasNextPage={filters.page * filters.pageSize < total}
          hasPrevPage={filters.page > 1}
          onGoToPage={filters.setPage}
          onGoToNextPage={() => filters.setPage(filters.page + 1)}
          onGoToPrevPage={() => filters.setPage(filters.page - 1)}
          onSetPageSize={filters.setPageSize}
        />
      )}

      {/* Bulk action bar — pinned below the list so selecting rows doesn't shift it */}
      {canWrite && selectedIds.size > 0 && (
        <BulkBar count={selectedIds.size} onClear={() => setSelectedIds(new Set())}>
          <BulkButton
            verb="Cancel"
            icon={<X />}
            className={styles.bulkCancelBtn}
            count={selectedCancelable.length}
            running={action.isRunning('Cancel')}
            onClick={() => void action.run('Cancel', selectedCancelable, jobActions.Cancel)}
          />
          <BulkButton
            verb="Retry"
            icon={<Rotate />}
            className={styles.bulkRetryBtn}
            count={selectedRetryable.length}
            running={action.isRunning('Retry')}
            onClick={() => void action.run('Retry', selectedRetryable, jobActions.Retry)}
          />
          <BulkButton
            verb="Pause"
            icon={<Pause />}
            className={styles.bulkPauseBtn}
            count={selectedPausable.length}
            running={action.isRunning('Pause')}
            onClick={() => void action.run('Pause', selectedPausable, jobActions.Pause)}
          />
          <BulkButton
            verb="Resume"
            icon={<Play />}
            className={styles.bulkResumeBtn}
            count={selectedResumable.length}
            running={action.isRunning('Resume')}
            onClick={() => void action.run('Resume', selectedResumable, jobActions.Resume)}
          />
          <BulkButton
            verb="Delete"
            icon={<Trash />}
            className={styles.bulkDeleteBtn}
            count={selectedIds.size}
            running={action.isRunning('Delete')}
            onClick={() => setConfirm({ ids: [...selectedIds], bulk: true })}
          />
        </BulkBar>
      )}

      {confirm && (
        <div className={styles.dialogBackdrop} role="presentation" onClick={() => setConfirm(null)}>
          <div
            className={styles.dialog}
            role="dialog"
            aria-modal="true"
            aria-label="Confirm delete"
            onClick={(e) => e.stopPropagation()}
          >
            <p>
              {confirm.bulk
                ? `Delete ${confirm.ids.length} job${confirm.ids.length === 1 ? '' : 's'} and all their data? This cannot be undone.`
                : 'Delete this job and all its data? This cannot be undone.'}
            </p>
            <div className={styles.dialogActions}>
              <button className={styles.clearBtn} type="button" onClick={() => setConfirm(null)}>
                Cancel
              </button>
              <button
                className={`${styles.bulkBtn} ${styles.bulkDeleteBtn}`}
                type="button"
                onClick={() => void confirmDelete()}
              >
                Delete
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
