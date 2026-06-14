// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useCallback, useRef, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import PageHeader from '@/components/PageHeader'
import StatusBadge from '@/components/StatusBadge'
import { useListWorkers, queryKeys } from '@/api/queries'
import { useDisableWorker, useEnableWorker } from '@/api/mutations'
import { useWebSocket } from '@/ws/context'
import { isWorkerEvent } from '@/ws/events'
import type { Worker, WorkerStatus, ListResponse } from '@/api/types'
import styles from './WorkerList.module.css'

// ── Constants ─────────────────────────────────────────────────────────────────

const PAGE_SIZE = 50

const STATUS_FILTERS: { label: string; value: WorkerStatus | '' }[] = [
  { label: 'All', value: '' },
  { label: 'Online', value: 'online' },
  { label: 'Offline', value: 'offline' },
  { label: 'Disabled', value: 'disabled' },
]

// ── Helpers ───────────────────────────────────────────────────────────────────

function truncateId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}

function formatHeartbeat(iso: string | undefined): string {
  if (!iso) return '—'
  const diff = Date.now() - new Date(iso).getTime()
  if (diff < 5000) return 'just now'
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`
  return new Date(iso).toLocaleDateString()
}

function formatAge(ts: number, now: number): string {
  if (ts === 0) return '—'
  const diff = now - ts
  if (diff < 5000) return 'just now'
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`
  return new Date(ts).toLocaleTimeString()
}

// ── Sub-components ────────────────────────────────────────────────────────────

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
          // Clipboard write can fail in insecure contexts. Silently ignore.
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
  // ── URL-driven status filter ────────────────────────────────────
  const [activeStatus, setActiveStatus] = useState<WorkerStatus | ''>('')

  // ── Main data query ───────────────────────────────────────────────────────
  const { data, isLoading, isError, error, dataUpdatedAt } = useListWorkers({
    ...(activeStatus ? { status: activeStatus } : {}),
    limit: PAGE_SIZE,
  })

  const workers = data?.items ?? []
  const total = data?.total ?? 0

  // ── Per-status count queries (limit=1 to get totals cheaply) ────
  const { data: onlineCountData } = useListWorkers({ status: 'online', limit: 1 })
  const { data: offlineCountData } = useListWorkers({ status: 'offline', limit: 1 })
  const { data: disabledCountData } = useListWorkers({ status: 'disabled', limit: 1 })

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

  // ── Enable/disable mutations ────────────────────────────────────
  const queryClient = useQueryClient()
  const disableWorker = useDisableWorker()
  const enableWorker = useEnableWorker()
  const [togglingIds, setTogglingIds] = useState<Set<string>>(new Set())

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

  // ── WS-driven in-place worker updates ───────────────────────────
  useWebSocket('workers', (payload) => {
    if (!isWorkerEvent(payload)) return

    queryClient.setQueriesData<ListResponse<Worker>>({ queryKey: ['workers', 'list'] }, (old) => {
      if (!old) return old
      const idx = old.items.findIndex((w) => w.id === payload.worker_id)
      if (idx === -1) return old
      const newItems = [...old.items]
      const prev = newItems[idx]
      if (!prev) return old
      newItems[idx] = {
        ...prev,
        status: payload.status,
        // Update hostname if provided by the event
        ...(payload.hostname !== undefined ? { hostname: payload.hostname } : {}),
      }
      return { ...old, items: newItems }
    })

    // Invalidate count queries so filter pill counts stay accurate
    void queryClient.invalidateQueries({
      queryKey: queryKeys.workers.list({ status: 'online', limit: 1 }),
    })
    void queryClient.invalidateQueries({
      queryKey: queryKeys.workers.list({ status: 'offline', limit: 1 }),
    })
    void queryClient.invalidateQueries({
      queryKey: queryKeys.workers.list({ status: 'disabled', limit: 1 }),
    })
  })

  // ── Last-updated timestamp ────────────────────────────────────────────────
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 30_000)
    return () => clearInterval(id)
  }, [])

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
              aria-label="Refresh workers"
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
                activeStatus === value ? styles['filterPill--active'] : '',
              ]
                .filter(Boolean)
                .join(' ')}
              onClick={() => setActiveStatus(value)}
              aria-pressed={activeStatus === value}
              type="button"
            >
              {label}
              <span className={styles.filterCount} aria-label={`${statusCounts[value]} workers`}>
                {statusCounts[value]}
              </span>
            </button>
          ))}
        </div>
      </div>

      {isError && (
        <div className={styles.errorBanner} role="alert">
          Failed to load workers: {error instanceof Error ? error.message : 'Unknown error'}
        </div>
      )}
      {(disableWorker.isError || enableWorker.isError) && (
        <div className={styles.errorBanner} role="alert">
          Action failed:{' '}
          {(disableWorker.error ?? enableWorker.error) instanceof Error
            ? ((disableWorker.error ?? enableWorker.error) as Error).message
            : 'Unknown error'}
        </div>
      )}

      {/* Workers table */}
      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Workers">
          <thead>
            <tr>
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
                <td colSpan={8}>Loading…</td>
              </tr>
            )}
            {!isLoading && workers.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={8}>No workers found.</td>
              </tr>
            )}
            {workers.map((worker) => {
              const isToggling = togglingIds.has(worker.id)
              return (
                <tr key={worker.id}>
                  <td>
                    <Link to={`/workers/${worker.id}`}>{worker.hostname}</Link>
                  </td>
                  <td>
                    <IdCell id={worker.id} />
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
                      <button
                        className={`${styles.toggleBtn} ${styles['toggleBtn--disable']}`}
                        onClick={() => void handleDisable(worker.id)}
                        disabled={isToggling}
                        type="button"
                        aria-label={`Disable worker ${worker.hostname}`}
                      >
                        {isToggling ? '…' : 'Disable'}
                      </button>
                    )}
                    {worker.status === 'disabled' && (
                      <button
                        className={`${styles.toggleBtn} ${styles['toggleBtn--enable']}`}
                        onClick={() => void handleEnable(worker.id)}
                        disabled={isToggling}
                        type="button"
                        aria-label={`Enable worker ${worker.hostname}`}
                      >
                        {isToggling ? '…' : 'Enable'}
                      </button>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
    </div>
  )
}
