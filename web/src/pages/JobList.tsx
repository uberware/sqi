// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useCallback, useRef, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import FilterToolbar from '@/components/FilterToolbar'
import Pagination from '@/components/Pagination'
import StatusBadge from '@/components/StatusBadge'
import TaskProgressBar from '@/components/TaskProgressBar'
import { ChevronDown, ChevronUp, ChevronUpDown, Rotate, Trash, X } from '@/components/icons'
import ErrorBanner from '@/components/ErrorBanner'
import BulkBar from '@/components/BulkBar'
import CopyableId from '@/components/CopyableId'
import RefreshControls from '@/components/RefreshControls'
import { useListJobs, queryKeys } from '@/api/queries'
import { useCancelJob, useDeleteJob, useRetryJob } from '@/api/mutations'
import { useListFilters } from '@/hooks/useListFilters'
import type { SortDirection } from '@/hooks/useListFilters'
import { useLiveNow } from '@/hooks/useLiveNow'
import { formatTimespan } from '@/lib/time'
import { useWebSocket } from '@/ws/context'
import { isJobEvent, isTaskEvent, JOB_REMOVED_STATUS } from '@/ws/events'
import type { Job, JobStatus, TaskCounts, TaskStatus, ListResponse } from '@/api/types'
import styles from './JobList.module.css'

// ── Constants ─────────────────────────────────────────────────────────────────

type JobSortField = 'name' | 'priority' | 'created_at' | 'status'

const JOB_STATUSES = new Set(['pending', 'running', 'paused', 'completed', 'failed', 'canceled'])
const JOB_SORT_FIELDS = new Set(['name', 'priority', 'created_at', 'status'])

/** Job statuses that can still be canceled. */
const CANCELABLE: ReadonlySet<JobStatus> = new Set(['pending', 'running', 'paused'])

/** Number of a job's tasks eligible for retry (failed or canceled). */
function retryableCount(job: Job): number {
  const c = job.task_counts
  if (!c) return 0
  return c.failed + c.canceled
}

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
  return <TaskProgressBar counts={counts} />
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

// ── Main component ────────────────────────────────────────────────────────────

export default function JobList() {
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
  }

  const { data, isLoading, isError, error, dataUpdatedAt } = useListJobs(queryParams)
  const jobs = data?.items ?? []
  const total = data?.total ?? 0

  const queryClient = useQueryClient()
  const cancelJob = useCancelJob()
  const deleteJob = useDeleteJob()
  const retryJob = useRetryJob()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())
  // Pending confirmation: either a single job id or the selected-bulk set.
  const [confirm, setConfirm] = useState<{ ids: string[]; bulk: boolean } | null>(null)

  // ── Selection state ── declared early so the WS callback below can reference
  // setSelectedIds without a forward-declaration lint error.
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set())
  const [cancelingIds, setCancelingIds] = useState<Set<string>>(new Set())
  const [retryingIds, setRetryingIds] = useState<Set<string>>(new Set())

  // ── WS-driven in-place updates ───────────────────────────────────

  // Tracks the last-known status of each task so we can apply accurate count
  // deltas when task events arrive.  Map is keyed by task_id.
  const taskStatusRef = useRef(new Map<string, TaskStatus>())

  // Debounce list invalidation so rapid task bursts only cause one background
  // re-fetch.
  const invalidateTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const scheduleListInvalidate = useCallback(() => {
    if (invalidateTimerRef.current) clearTimeout(invalidateTimerRef.current)
    invalidateTimerRef.current = setTimeout(() => {
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
      // Patch this job's status immediately in every cached list page.
      const { status, updated_at } = payload
      queryClient.setQueriesData<ListResponse<Job>>({ queryKey: ['jobs', 'list'] }, (old) => {
        if (!old) return old
        const idx = old.items.findIndex((j) => j.id === payload.job_id)
        if (idx === -1) return old
        const newItems = [...old.items]
        const prev = newItems[idx]
        if (!prev) return old
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
          newItems[idx] = {
            ...job,
            task_counts: applyTaskStatusDelta(job.task_counts, prevStatus, payload.status),
          }
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

  const cancelableJobs = jobs.filter((j) => CANCELABLE.has(j.status))
  const cancelableIds = new Set(cancelableJobs.map((j) => j.id))
  const selectedCancelable = [...selectedIds].filter((id) => cancelableIds.has(id))

  const retryableJobs = jobs.filter((j) => retryableCount(j) > 0)
  const retryableIds = new Set(retryableJobs.map((j) => j.id))
  const selectedRetryable = [...selectedIds].filter((id) => retryableIds.has(id))

  // A job is selectable if any bulk action applies to it.
  const selectableJobs = jobs.filter((j) => CANCELABLE.has(j.status) || retryableCount(j) > 0)

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

  // ── Per-row cancel ────────────────────────────────────────────────────────

  const handleCancelRow = useCallback(
    async (id: string) => {
      setCancelingIds((prev) => new Set(prev).add(id))
      try {
        await cancelJob.mutateAsync(id)
      } catch {
        // Error is visible via cancelJob.isError / cancelJob.error below.
        // Do not re-throw so bulk cancel continues past individual failures.
      } finally {
        setCancelingIds((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
        setSelectedIds((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
      }
    },
    [cancelJob],
  )

  // ── Per-row retry ─────────────────────────────────────────────────────────────

  const handleRetryRow = useCallback(
    async (id: string) => {
      setRetryingIds((prev) => new Set(prev).add(id))
      try {
        await retryJob.mutateAsync(id)
      } catch {
        // Surfaced via retryJob.isError below; swallow so bulk retry continues.
      } finally {
        setRetryingIds((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
        setSelectedIds((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
      }
    },
    [retryJob],
  )

  // ── Bulk cancel ───────────────────────────────────────────────────────────

  const handleBulkCancel = useCallback(async () => {
    for (const id of selectedCancelable) {
      await handleCancelRow(id)
    }
  }, [selectedCancelable, handleCancelRow])

  // ── Bulk retry ────────────────────────────────────────────────────────────

  const handleBulkRetry = useCallback(async () => {
    for (const id of selectedRetryable) {
      await handleRetryRow(id)
    }
  }, [selectedRetryable, handleRetryRow])

  // ── Per-row and bulk delete ───────────────────────────────────────────────

  const runDelete = useCallback(
    async (id: string) => {
      setDeletingIds((prev) => new Set(prev).add(id))
      try {
        await deleteJob.mutateAsync(id)
      } catch {
        // Surfaced via deleteJob.isError; continue past individual failures.
      } finally {
        setDeletingIds((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
        setSelectedIds((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
      }
    },
    [deleteJob],
  )

  const confirmDelete = useCallback(async () => {
    if (!confirm) return
    const ids = confirm.ids
    setConfirm(null)
    for (const id of ids) {
      await runDelete(id)
    }
  }, [confirm, runDelete])

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
      {cancelJob.isError && (
        <ErrorBanner>
          Cancel failed:{' '}
          {cancelJob.error instanceof Error ? cancelJob.error.message : 'Unknown error'}
        </ErrorBanner>
      )}
      {deleteJob.isError && (
        <ErrorBanner>
          Delete failed:{' '}
          {deleteJob.error instanceof Error ? deleteJob.error.message : 'Unknown error'}
        </ErrorBanner>
      )}
      {retryJob.isError && (
        <ErrorBanner>
          Retry failed: {retryJob.error instanceof Error ? retryJob.error.message : 'Unknown error'}
        </ErrorBanner>
      )}

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
                <td colSpan={11}>Loading…</td>
              </tr>
            )}
            {!isLoading && jobs.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={11}>No jobs found.</td>
              </tr>
            )}
            {jobs.map((job) => {
              const isSelected = selectedIds.has(job.id)
              const isCanceling = cancelingIds.has(job.id)
              const canCancel = CANCELABLE.has(job.status) && !isCanceling
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
                  <td>{formatTime(job.created_at)}</td>
                  <td>{formatTimespan(job.started_at, job.completed_at, now)}</td>
                  <td className={styles.actionsCell}>
                    {canCancel && (
                      <IconButton
                        icon={<X />}
                        className={styles.cancelBtn}
                        onClick={() => void handleCancelRow(job.id)}
                        busy={isCanceling}
                        title="Cancel"
                        label={`Cancel job ${job.name}`}
                      />
                    )}
                    {retryableCount(job) > 0 && (
                      <IconButton
                        icon={<Rotate />}
                        className={styles.retryBtn}
                        onClick={() => void handleRetryRow(job.id)}
                        busy={retryingIds.has(job.id)}
                        title="Retry failed and canceled tasks"
                        label={`Retry job ${job.name}`}
                      />
                    )}
                    <IconButton
                      icon={<Trash />}
                      className={styles.deleteBtn}
                      onClick={() => setConfirm({ ids: [job.id], bulk: false })}
                      busy={deletingIds.has(job.id)}
                      title="Delete"
                      label={`Delete job ${job.name}`}
                    />
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
      {selectedIds.size > 0 && (
        <BulkBar count={selectedIds.size} onClear={() => setSelectedIds(new Set())}>
          <button
            className={styles.bulkCancelBtn}
            onClick={() => void handleBulkCancel()}
            disabled={selectedCancelable.length === 0 || cancelJob.isPending}
            type="button"
            aria-label={`Cancel selected (${selectedCancelable.length})`}
          >
            <X />
            Cancel {selectedCancelable.length}
          </button>
          <button
            className={styles.bulkRetryBtn}
            onClick={() => void handleBulkRetry()}
            disabled={selectedRetryable.length === 0 || retryJob.isPending}
            type="button"
            aria-label={`Retry selected (${selectedRetryable.length})`}
          >
            <Rotate />
            Retry {selectedRetryable.length}
          </button>
          <button
            className={styles.bulkDeleteBtn}
            onClick={() => setConfirm({ ids: [...selectedIds], bulk: true })}
            disabled={selectedIds.size === 0 || deleteJob.isPending}
            type="button"
            aria-label={`Delete selected (${selectedIds.size})`}
          >
            <Trash />
            Delete {selectedIds.size}
          </button>
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
                className={styles.bulkDeleteBtn}
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
