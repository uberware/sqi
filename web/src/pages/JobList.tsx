// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useCallback, useRef, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import PageHeader from '@/components/PageHeader'
import StatusBadge from '@/components/StatusBadge'
import TaskProgressBar from '@/components/TaskProgressBar'
import { useListJobs, queryKeys } from '@/api/queries'
import { useCancelJob, useDeleteJob } from '@/api/mutations'
import { useJobListFilters } from '@/hooks/useJobListFilters'
import { useDebounce } from '@/hooks/useDebounce'
import { useLiveNow } from '@/hooks/useLiveNow'
import { formatTimespan } from '@/lib/time'
import { useWebSocket } from '@/ws/context'
import { isJobEvent, isTaskEvent, JOB_REMOVED_STATUS } from '@/ws/events'
import type { Job, JobStatus, TaskCounts, TaskStatus, ListResponse } from '@/api/types'
import type { JobSortField, SortDirection } from '@/hooks/useJobListFilters'
import styles from './JobList.module.css'

// ── Constants ─────────────────────────────────────────────────────────────────

const PAGE_SIZE = 50

/** Job statuses that can still be canceled. */
const CANCELABLE: ReadonlySet<JobStatus> = new Set(['pending', 'running', 'paused'])

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

function truncateId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}

function formatTime(iso: string | undefined): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
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
        {active ? (sortDir === 'asc' ? '↑' : '↓') : '↕'}
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

function IdCell({ id }: { id: string }) {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    },
    [],
  )

  const triggerCopy = useCallback(
    (e: React.SyntheticEvent) => {
      e.preventDefault()
      e.stopPropagation()
      void navigator.clipboard
        .writeText(id)
        .then(() => {
          setCopied(true)
          if (timerRef.current) clearTimeout(timerRef.current)
          timerRef.current = setTimeout(() => setCopied(false), 1500)
        })
        .catch(() => {
          // Clipboard write can fail in insecure contexts or when the document
          // is not focused. Silently ignore.
        })
    },
    [id],
  )

  return (
    <span
      className={styles.idCell}
      onClick={triggerCopy}
      title={`Click to copy: ${id}`}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') triggerCopy(e)
      }}
    >
      {truncateId(id)}
      <span className={styles.copyHint}>{copied ? '✓' : '⎘'}</span>
    </span>
  )
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
  const filters = useJobListFilters()
  const [inputSearch, setInputSearch] = useState(filters.search)
  const debouncedSearch = useDebounce(inputSearch, 300)

  // Sync debounced search value into URL params once it settles.
  // Skip the initial mount run so that existing page= params are not discarded
  // when the component first renders with a pre-set search value in the URL.
  const didMountRef = useRef(false)
  useEffect(() => {
    if (!didMountRef.current) {
      didMountRef.current = true
      return
    }
    filters.setSearch(debouncedSearch)
    // filters.setSearch is stable (useCallback wrapping a stable setSearchParams)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debouncedSearch])

  const queryParams: Parameters<typeof useListJobs>[0] = {
    ...(filters.status ? { status: filters.status } : {}),
    ...(debouncedSearch ? { search: debouncedSearch } : {}),
    sort_by: filters.sortField,
    sort_dir: filters.sortDir,
    limit: PAGE_SIZE,
    offset: (filters.page - 1) * PAGE_SIZE,
  }

  const { data, isLoading, isError, error, dataUpdatedAt } = useListJobs(queryParams)
  const jobs = data?.items ?? []
  const total = data?.total ?? 0

  const queryClient = useQueryClient()
  const cancelJob = useCancelJob()
  const deleteJob = useDeleteJob()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())
  // Pending confirmation: either a single job id or the selected-bulk set.
  const [confirm, setConfirm] = useState<{ ids: string[]; bulk: boolean } | null>(null)

  // ── Selection state ── declared early so the WS callback below can reference
  // setSelectedIds without a forward-declaration lint error.
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set())
  const [cancelingIds, setCancelingIds] = useState<Set<string>>(new Set())

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

  const toggleRow = useCallback((id: string) => {
    setSelectedIds((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])

  const toggleAll = useCallback(() => {
    if (cancelableJobs.length === 0) return
    const allSelected = cancelableJobs.every((j) => selectedIds.has(j.id))
    setSelectedIds(allSelected ? new Set() : new Set(cancelableJobs.map((j) => j.id)))
  }, [cancelableJobs, selectedIds])

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

  // ── Bulk cancel ───────────────────────────────────────────────────────────

  const handleBulkCancel = useCallback(async () => {
    for (const id of selectedCancelable) {
      await handleCancelRow(id)
    }
  }, [selectedCancelable, handleCancelRow])

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

  const allCancelableSelected =
    cancelableJobs.length > 0 && cancelableJobs.every((j) => selectedIds.has(j.id))

  return (
    <div className={styles.page}>
      <PageHeader
        title="Jobs"
        subtitle={isLoading ? 'Loading…' : `${total} jobs`}
        action={
          <div className={styles.headerActions}>
            {dataUpdatedAt > 0 && (
              <span className={styles.lastUpdated} aria-live="polite">
                Updated {formatAge(dataUpdatedAt, now)}
              </span>
            )}
            <button
              className={styles.refreshBtn}
              onClick={handleRefresh}
              type="button"
              aria-label="Refresh jobs"
            >
              ↻ Refresh
            </button>
          </div>
        }
      />

      {/* Status filter bar */}
      <div className={styles.toolbar}>
        <div className={styles.filterBar} role="toolbar" aria-label="Filter by status">
          {STATUS_FILTERS.map(({ label, value }) => (
            <button
              key={value}
              className={[
                styles.filterPill,
                filters.status === value ? styles['filterPill--active'] : '',
              ]
                .filter(Boolean)
                .join(' ')}
              onClick={() => filters.setStatus(value)}
              aria-pressed={filters.status === value}
              type="button"
            >
              {label}
            </button>
          ))}
        </div>

        {/* Search input */}
        <div className={styles.searchWrap}>
          <input
            className={styles.searchInput}
            type="search"
            placeholder="Search by name, ID, or owner…"
            value={inputSearch}
            onChange={(e) => setInputSearch(e.target.value)}
            aria-label="Search jobs"
          />
        </div>
      </div>

      {isError && (
        <div className={styles.errorBanner} role="alert">
          Failed to load jobs: {error instanceof Error ? error.message : 'Unknown error'}
        </div>
      )}
      {cancelJob.isError && (
        <div className={styles.errorBanner} role="alert">
          Cancel failed:{' '}
          {cancelJob.error instanceof Error ? cancelJob.error.message : 'Unknown error'}
        </div>
      )}
      {deleteJob.isError && (
        <div className={styles.errorBanner} role="alert">
          Delete failed:{' '}
          {deleteJob.error instanceof Error ? deleteJob.error.message : 'Unknown error'}
        </div>
      )}

      {/* Job table */}
      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Jobs">
          <thead>
            <tr>
              <th className={styles.checkCell}>
                <input
                  type="checkbox"
                  aria-label="Select all cancelable jobs"
                  checked={allCancelableSelected}
                  onChange={toggleAll}
                  disabled={cancelableJobs.length === 0}
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
                    <IdCell id={job.id} />
                  </td>
                  <td>{job.owner}</td>
                  <td>
                    <Link to="/queues">{job.queue_name ?? job.queue_id}</Link>
                  </td>
                  <td>
                    <StatusBadge status={job.status} />
                  </td>
                  <td>{job.priority}</td>
                  <td>
                    <ProgressCell job={job} />
                  </td>
                  <td>{formatTime(job.created_at)}</td>
                  <td>{formatTimespan(job.started_at, job.completed_at, now)}</td>
                  <td className={styles.actionsCell}>
                    {canCancel && (
                      <button
                        className={styles.cancelBtn}
                        onClick={() => void handleCancelRow(job.id)}
                        disabled={isCanceling}
                        type="button"
                        aria-label={`Cancel job ${job.name}`}
                      >
                        {isCanceling ? '…' : 'Cancel'}
                      </button>
                    )}
                    <button
                      className={styles.deleteBtn}
                      onClick={() => setConfirm({ ids: [job.id], bulk: false })}
                      disabled={deletingIds.has(job.id)}
                      type="button"
                      aria-label={`Delete job ${job.name}`}
                    >
                      {deletingIds.has(job.id) ? '…' : 'Delete'}
                    </button>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      {/* Bulk action bar — pinned below the list so selecting rows doesn't shift it */}
      {selectedIds.size > 0 && (
        <div className={styles.bulkBar}>
          <span className={styles.bulkBarCount}>{selectedIds.size} selected</span>
          <button
            className={styles.bulkCancelBtn}
            onClick={() => void handleBulkCancel()}
            disabled={selectedCancelable.length === 0 || cancelJob.isPending}
            type="button"
          >
            Cancel selected ({selectedCancelable.length})
          </button>
          <button
            className={styles.bulkDeleteBtn}
            onClick={() => setConfirm({ ids: [...selectedIds], bulk: true })}
            disabled={selectedIds.size === 0 || deleteJob.isPending}
            type="button"
          >
            Delete selected ({selectedIds.size})
          </button>
          <button
            className={styles.filterPill}
            onClick={() => setSelectedIds(new Set())}
            type="button"
          >
            Clear
          </button>
        </div>
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
              <button className={styles.filterPill} type="button" onClick={() => setConfirm(null)}>
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
