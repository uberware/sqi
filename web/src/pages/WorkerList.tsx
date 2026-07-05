// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useCallback, useMemo } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import FilterToolbar from '@/components/FilterToolbar'
import Pagination from '@/components/Pagination'
import StatusBadge from '@/components/StatusBadge'
import { Pause, Play, Trash } from '@/components/icons'
import ErrorBanner from '@/components/ErrorBanner'
import BulkBar from '@/components/BulkBar'
import CopyableId from '@/components/CopyableId'
import RefreshControls from '@/components/RefreshControls'
import { useListWorkers, queryKeys } from '@/api/queries'
import { useDisableWorker, useEnableWorker, useRemoveWorker } from '@/api/mutations'
import { useListFilters } from '@/hooks/useListFilters'
import { useLiveNow } from '@/hooks/useLiveNow'
import { useWebSocket } from '@/ws/context'
import { isWorkerEvent, WORKER_REMOVED_STATUS } from '@/ws/events'
import type { Worker, WorkerStatus, ListResponse } from '@/api/types'
import styles from './WorkerList.module.css'

// ── Constants ─────────────────────────────────────────────────────────────────

const WORKER_STATUSES = new Set(['online', 'offline', 'disabled'])
const WORKER_SORT_FIELDS = new Set(['hostname', 'status', 'registered_at', 'last_heartbeat_at'])

const STATUS_FILTERS: { label: string; value: WorkerStatus | '' }[] = [
  { label: 'All', value: '' },
  { label: 'Online', value: 'online' },
  { label: 'Offline', value: 'offline' },
  { label: 'Disabled', value: 'disabled' },
]

// ── Helpers ───────────────────────────────────────────────────────────────────

function formatHeartbeat(iso: string | undefined): string {
  if (!iso) return '—'
  const diff = Date.now() - new Date(iso).getTime()
  if (diff < 5000) return 'just now'
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`
  return new Date(iso).toLocaleDateString()
}

// ── Sub-components ────────────────────────────────────────────────────────────

// ── Capability tag extraction ─────────────────────────────────────────────────

// OS is shown in its own dedicated column, so capability tags cover CPU, RAM,
// GPU, and any custom tags only — no duplication with the OS column.
function buildCapabilityTags(worker: Worker): string[] {
  const tags: string[] = []

  if (worker.cpu_count !== undefined) {
    tags.push(`${worker.cpu_count} CPU`)
  }
  if (worker.ram_mb !== undefined) {
    const gb = worker.ram_mb / 1024
    const label = gb >= 1 ? `${Math.round(gb)}GB RAM` : `${worker.ram_mb}MB RAM`
    tags.push(label)
  }
  if (worker.gpu?.model) {
    const vram = worker.gpu.vram_mb
    const gpuLabel = vram ? `${worker.gpu.model} ${Math.round(vram / 1024)}GB` : worker.gpu.model
    tags.push(gpuLabel)
  }
  if (worker.tags) {
    for (const [k, v] of Object.entries(worker.tags)) {
      tags.push(v ? `${k}=${v}` : k)
    }
  }

  return tags
}

// Compact capability tag display with +N more popover
function CapabilityTags({ worker }: { worker: Worker }) {
  const allTags = buildCapabilityTags(worker)
  const visible = allTags.slice(0, 3)
  const overflow = allTags.slice(3)

  if (allTags.length === 0) return <span className={styles.metaText}>—</span>

  return (
    <div className={styles.tagList}>
      {visible.map((tag) => (
        <span key={tag} className={styles.tagPill}>
          {tag}
        </span>
      ))}
      {overflow.length > 0 && (
        <span
          className={styles.tagMore}
          aria-label={`${overflow.length} more tags: ${overflow.join(', ')}`}
          data-testid="tag-overflow"
        >
          +{overflow.length} more
          <span className={styles.tagTooltip} role="tooltip">
            {overflow.map((tag) => (
              <span key={tag} className={styles.tagTooltipItem}>
                {tag}
              </span>
            ))}
          </span>
        </span>
      )}
    </div>
  )
}

// ── Main component ────────────────────────────────────────────────────────────

export default function WorkerList() {
  // ── URL-driven filters ──────────────────────────────────────────────────────
  const filters = useListFilters<
    WorkerStatus,
    'hostname' | 'status' | 'registered_at' | 'last_heartbeat_at'
  >({
    statuses: WORKER_STATUSES,
    sortFields: WORKER_SORT_FIELDS,
    defaultSortField: 'hostname',
    defaultSortDir: 'asc',
  })

  // ── Main data query ───────────────────────────────────────────────────────
  const { data, isLoading, isError, error, dataUpdatedAt } = useListWorkers({
    ...(filters.status ? { status: filters.status } : {}),
    ...(filters.search ? { search: filters.search } : {}),
    limit: filters.pageSize,
    offset: (filters.page - 1) * filters.pageSize,
  })

  const workers = useMemo(() => data?.items ?? [], [data])
  const total = data?.total ?? 0

  // ── Per-status count queries (limit=1 to get totals cheaply) ────
  const { data: onlineCountData } = useListWorkers({
    status: 'online',
    limit: 1,
    ...(filters.search ? { search: filters.search } : {}),
  })
  const { data: offlineCountData } = useListWorkers({
    status: 'offline',
    limit: 1,
    ...(filters.search ? { search: filters.search } : {}),
  })
  const { data: disabledCountData } = useListWorkers({
    status: 'disabled',
    limit: 1,
    ...(filters.search ? { search: filters.search } : {}),
  })

  // Derive "All" by summing status counts so it remains accurate when a filter
  // is active — data.total reflects the filtered subset, not the grand total.
  const onlineCount = onlineCountData?.total ?? 0
  const offlineCount = offlineCountData?.total ?? 0
  const disabledCount = disabledCountData?.total ?? 0
  const statusCounts: Record<WorkerStatus | '', number> = {
    '': onlineCount + offlineCount + disabledCount,
    online: onlineCount,
    offline: offlineCount,
    disabled: disabledCount,
  }

  const statusOptions = STATUS_FILTERS.map((f) => ({ ...f, count: statusCounts[f.value] }))

  // ── Enable/disable/remove mutations ─────────────────────────────
  const queryClient = useQueryClient()
  const disableWorker = useDisableWorker()
  const enableWorker = useEnableWorker()
  const removeWorker = useRemoveWorker()
  const [togglingIds, setTogglingIds] = useState<Set<string>>(new Set())

  // ── Multi-select state ──────────────────────────────────────────
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set())

  // Eligible subsets for each bulk action (ineligible selections are skipped).
  const disableableIds = new Set(workers.filter((w) => w.status === 'online').map((w) => w.id))
  const removableIds = new Set(workers.filter((w) => w.removable).map((w) => w.id))
  const selectedDisableable = [...selectedIds].filter((id) => disableableIds.has(id))
  const selectedRemovable = [...selectedIds].filter((id) => removableIds.has(id))

  const toggleRow = useCallback((id: string) => {
    setSelectedIds((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])

  const toggleAll = useCallback(() => {
    if (workers.length === 0) return
    const allSelected = workers.every((w) => selectedIds.has(w.id))
    setSelectedIds(allSelected ? new Set() : new Set(workers.map((w) => w.id)))
  }, [workers, selectedIds])

  const deselect = useCallback((id: string) => {
    setSelectedIds((prev) => {
      if (!prev.has(id)) return prev
      const next = new Set(prev)
      next.delete(id)
      return next
    })
  }, [])

  const handleDisable = useCallback(
    async (id: string) => {
      enableWorker.reset() // clear any stale enable-error before starting a disable
      setTogglingIds((prev) => new Set(prev).add(id))
      try {
        await disableWorker.mutateAsync(id)
      } catch {
        // Error visible via disableWorker.isError
      } finally {
        setTogglingIds((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
      }
    },
    [disableWorker, enableWorker],
  )

  const handleEnable = useCallback(
    async (id: string) => {
      disableWorker.reset() // clear any stale disable-error before starting an enable
      setTogglingIds((prev) => new Set(prev).add(id))
      try {
        await enableWorker.mutateAsync(id)
      } catch {
        // Error visible via enableWorker.isError
      } finally {
        setTogglingIds((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
      }
    },
    [enableWorker, disableWorker],
  )

  const handleRemove = useCallback(
    async (id: string) => {
      setTogglingIds((prev) => new Set(prev).add(id))
      try {
        await removeWorker.mutateAsync(id)
      } catch {
        // Error visible via removeWorker.isError; do not re-throw so a bulk
        // remove continues past individual failures (e.g. a 409 race).
      } finally {
        setTogglingIds((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
        deselect(id)
      }
    },
    [removeWorker, deselect],
  )

  // ── Bulk actions ────────────────────────────────────────────────
  // Each applies only to its eligible subset; ineligible selections are
  // silently skipped. Runs sequentially so one failure doesn't abort the rest.
  const handleBulkDisable = useCallback(async () => {
    for (const id of selectedDisableable) {
      await handleDisable(id)
      deselect(id)
    }
  }, [selectedDisableable, handleDisable, deselect])

  const handleBulkRemove = useCallback(async () => {
    for (const id of selectedRemovable) {
      await handleRemove(id)
    }
  }, [selectedRemovable, handleRemove])

  // ── WS-driven worker updates ────────────────────────────────────
  useWebSocket('workers', (payload) => {
    if (!isWorkerEvent(payload)) return

    const workerId = payload.worker_id

    if (payload.status === WORKER_REMOVED_STATUS) {
      // A removed worker (manual remove or retention sweep) is dropped from the
      // list rather than updated in place, and cleared from the selection.
      queryClient.setQueriesData<ListResponse<Worker>>({ queryKey: ['workers', 'list'] }, (old) => {
        if (!old) return old
        const items = old.items.filter((w) => w.id !== workerId)
        if (items.length === old.items.length) return old
        return { ...old, items, total: Math.max(0, old.total - 1) }
      })
      deselect(workerId)
    } else {
      const status = payload.status
      queryClient.setQueriesData<ListResponse<Worker>>({ queryKey: ['workers', 'list'] }, (old) => {
        if (!old) return old
        const idx = old.items.findIndex((w) => w.id === workerId)
        if (idx === -1) return old
        const newItems = [...old.items]
        const prev = newItems[idx]
        if (!prev) return old
        newItems[idx] = {
          ...prev,
          status,
          // Update name/hostname if provided by the event
          ...(payload.name !== undefined ? { name: payload.name } : {}),
          ...(payload.hostname !== undefined ? { hostname: payload.hostname } : {}),
        }
        return { ...old, items: newItems }
      })
    }

    // Invalidate all worker list queries (including per-status counts) so
    // counts stay accurate, and refetch the main list for newly-seen workers.
    // A single prefix invalidation covers every variant, including those with
    // a search term embedded in the query key.
    void queryClient.invalidateQueries({ queryKey: ['workers', 'list'] })
  })

  // ── Last-updated timestamp ────────────────────────────────────────────────
  const now = useLiveNow(false)

  // ── Manual refresh ────────────────────────────────────────────────────────
  const handleRefresh = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.workers.all })
  }, [queryClient])

  return (
    <div className={styles.page}>
      <PageHeader
        title="Workers"
        subtitle={isLoading ? 'Loading…' : `${total} workers`}
        action={
          <RefreshControls
            onRefresh={handleRefresh}
            label="Refresh workers"
            updatedAt={dataUpdatedAt}
            now={now}
          />
        }
      />

      <FilterToolbar
        statuses={statusOptions}
        activeStatus={filters.status}
        onStatusChange={(v) => filters.setStatus(v as WorkerStatus | '')}
        search={filters.search}
        onSearchChange={filters.setSearch}
        searchPlaceholder="Search by name, host, ID, or location…"
        searchLabel="Search workers"
      />

      {isError && (
        <ErrorBanner>
          Failed to load workers: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}
      {(disableWorker.isError || enableWorker.isError || removeWorker.isError) && (
        <ErrorBanner>
          Action failed:{' '}
          {(disableWorker.error ?? enableWorker.error ?? removeWorker.error) instanceof Error
            ? ((disableWorker.error ?? enableWorker.error ?? removeWorker.error) as Error).message
            : 'Unknown error'}
        </ErrorBanner>
      )}

      {/* Workers table */}
      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Workers">
          <thead>
            <tr>
              <th className={styles.checkCell}>
                <input
                  type="checkbox"
                  aria-label="Select all workers"
                  checked={workers.length > 0 && workers.every((w) => selectedIds.has(w.id))}
                  onChange={toggleAll}
                  disabled={workers.length === 0}
                />
              </th>
              <th>Name</th>
              <th>ID</th>
              <th>Location</th>
              <th>Status</th>
              <th>OS</th>
              <th>Capabilities</th>
              <th>Last Heartbeat</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={styles.emptyRow}>
                <td colSpan={9}>Loading…</td>
              </tr>
            )}
            {!isLoading && workers.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={9}>No workers found.</td>
              </tr>
            )}
            {workers.map((worker) => {
              const isToggling = togglingIds.has(worker.id)
              const isSelected = selectedIds.has(worker.id)
              return (
                <tr key={worker.id} className={isSelected ? styles.rowSelected : undefined}>
                  <td className={styles.checkCell}>
                    <input
                      type="checkbox"
                      aria-label={`Select worker ${worker.name || worker.hostname}`}
                      checked={isSelected}
                      onChange={() => toggleRow(worker.id)}
                    />
                  </td>
                  <td>
                    <Link to={`/workers/${worker.id}`}>{worker.name || worker.hostname}</Link>
                  </td>
                  <td>
                    <CopyableId id={worker.id} />
                  </td>
                  <td>
                    <span className={styles.metaText}>{worker.compute_location ?? '—'}</span>
                  </td>
                  <td>
                    <StatusBadge status={worker.status} />
                  </td>
                  <td>
                    <span className={styles.metaText}>
                      {worker.os
                        ? worker.os_version
                          ? `${worker.os} ${worker.os_version}`
                          : worker.os
                        : '—'}
                    </span>
                  </td>
                  {/* Compact capability tags */}
                  <td>
                    <CapabilityTags worker={worker} />
                  </td>
                  <td>
                    <span className={styles.metaText}>
                      {formatHeartbeat(worker.last_heartbeat_at)}
                    </span>
                  </td>
                  {/* Per-row enable/disable */}
                  <td>
                    {worker.status === 'online' && (
                      <IconButton
                        icon={<Pause />}
                        className={`${styles.toggleBtn} ${styles['toggleBtn--disable']}`}
                        onClick={() => void handleDisable(worker.id)}
                        busy={isToggling}
                        title="Disable"
                        label={`Disable worker ${worker.name || worker.hostname}`}
                      />
                    )}
                    {worker.status === 'disabled' && (
                      <IconButton
                        icon={<Play />}
                        className={`${styles.toggleBtn} ${styles['toggleBtn--enable']}`}
                        onClick={() => void handleEnable(worker.id)}
                        busy={isToggling}
                        title="Enable"
                        label={`Enable worker ${worker.name || worker.hostname}`}
                      />
                    )}
                    {worker.removable && (
                      <IconButton
                        icon={<Trash />}
                        className={`${styles.toggleBtn} ${styles['toggleBtn--remove']}`}
                        onClick={() => void handleRemove(worker.id)}
                        busy={isToggling}
                        title="Remove"
                        label={`Remove worker ${worker.name || worker.hostname}`}
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
      {selectedIds.size > 0 && (
        <BulkBar count={selectedIds.size} onClear={() => setSelectedIds(new Set())}>
          <button
            className={`${styles.bulkBtn} ${styles['bulkBtn--disable']}`}
            onClick={() => void handleBulkDisable()}
            disabled={selectedDisableable.length === 0 || disableWorker.isPending}
            type="button"
            aria-label={`Disable selected (${selectedDisableable.length})`}
          >
            <Pause />
            Disable {selectedDisableable.length}
          </button>
          <button
            className={`${styles.bulkBtn} ${styles['bulkBtn--remove']}`}
            onClick={() => void handleBulkRemove()}
            disabled={selectedRemovable.length === 0 || removeWorker.isPending}
            type="button"
            aria-label={`Remove selected (${selectedRemovable.length})`}
          >
            <Trash />
            Remove {selectedRemovable.length}
          </button>
        </BulkBar>
      )}
    </div>
  )
}
