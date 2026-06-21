// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useCallback, useRef, useEffect } from 'react'
import { useParams, Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import DiagnosticsPanel from '@/components/DiagnosticsPanel'
import PageHeader from '@/components/PageHeader'
import StatusBadge from '@/components/StatusBadge'
import { Check, Copy, Refresh } from '@/components/icons'
import { useGetWorker, queryKeys } from '@/api/queries'
import { useDisableWorker, useEnableWorker } from '@/api/mutations'
import { useWebSocket } from '@/ws/context'
import { isWorkerEvent, WORKER_REMOVED_STATUS } from '@/ws/events'
import type { WorkerDetail as WorkerDetailType } from '@/api/types'
import { useLiveNow } from '@/hooks/useLiveNow'
import { formatTimespan, formatUptime } from '@/lib/time'
import styles from './WorkerDetail.module.css'

// ── Helpers ───────────────────────────────────────────────────────────────────

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

function truncateId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}

// ── IdDisplay ─────────────────────────────────────────────────────────────────

function IdDisplay({ id }: { id: string }) {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    },
    [],
  )

  const handleCopy = useCallback(
    (e: React.SyntheticEvent) => {
      e.preventDefault()
      void navigator.clipboard
        .writeText(id)
        .then(() => {
          setCopied(true)
          if (timerRef.current) clearTimeout(timerRef.current)
          timerRef.current = setTimeout(() => setCopied(false), 1500)
        })
        .catch(() => {
          // Clipboard write can fail in insecure contexts; silently ignore.
        })
    },
    [id],
  )

  return (
    <span
      className={styles.idDisplay}
      onClick={handleCopy}
      title={`Click to copy: ${id}`}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') handleCopy(e)
      }}
    >
      <span className={styles.idFull}>{id}</span>
      <span className={styles.idCopyHint}>
        {copied ? (
          <>
            <Check size={12} /> Copied
          </>
        ) : (
          <Copy size={12} />
        )}
      </span>
    </span>
  )
}

// ── Capabilities ─────────────────────────────────────────────────────

function CapabilitiesSection({ worker }: { worker: WorkerDetailType }) {
  const hasSystem =
    worker.os ||
    worker.os_version ||
    worker.cpu_count !== undefined ||
    worker.ram_mb !== undefined ||
    worker.gpu?.model ||
    worker.gpu?.vendor ||
    worker.gpu?.count ||
    (worker.gpu?.vram_mb !== undefined && worker.gpu.vram_mb > 0)

  const customTags = worker.tags ? Object.entries(worker.tags) : []

  if (!hasSystem && customTags.length === 0) {
    return (
      <section className={styles.capSection} aria-label="Capabilities">
        <h2 className={styles.sectionTitle}>Capabilities</h2>
        <p className={styles.empty}>No capabilities reported.</p>
      </section>
    )
  }

  return (
    <section className={styles.capSection} aria-label="Capabilities">
      <h2 className={styles.sectionTitle}>Capabilities</h2>

      {hasSystem && (
        <div className={styles.capGroup}>
          <h3 className={styles.capGroupTitle}>System</h3>
          <dl className={styles.capGrid}>
            {worker.os && (
              <div className={styles.capField}>
                <dt>OS</dt>
                <dd>{worker.os_version ? `${worker.os} ${worker.os_version}` : worker.os}</dd>
              </div>
            )}
            {worker.os_version && !worker.os && (
              <div className={styles.capField}>
                <dt>OS Version</dt>
                <dd>{worker.os_version}</dd>
              </div>
            )}
            {worker.cpu_count !== undefined && (
              <div className={styles.capField}>
                <dt>CPU Cores</dt>
                <dd>{worker.cpu_count}</dd>
              </div>
            )}
            {worker.ram_mb !== undefined && (
              <div className={styles.capField}>
                <dt>RAM</dt>
                <dd>
                  {worker.ram_mb >= 1024
                    ? `${Math.round(worker.ram_mb / 1024)} GB`
                    : `${worker.ram_mb} MB`}
                </dd>
              </div>
            )}
            {worker.gpu?.vendor && (
              <div className={styles.capField}>
                <dt>GPU Vendor</dt>
                <dd>{worker.gpu.vendor}</dd>
              </div>
            )}
            {worker.gpu?.model && (
              <div className={styles.capField}>
                <dt>GPU Model</dt>
                <dd>{worker.gpu.model}</dd>
              </div>
            )}
            {worker.gpu?.vram_mb !== undefined && worker.gpu.vram_mb > 0 && (
              <div className={styles.capField}>
                <dt>GPU VRAM</dt>
                <dd>
                  {worker.gpu.vram_mb >= 1024
                    ? `${Math.round(worker.gpu.vram_mb / 1024)} GB`
                    : `${worker.gpu.vram_mb} MB`}
                </dd>
              </div>
            )}
            {worker.gpu?.count !== undefined && worker.gpu.count > 0 && (
              <div className={styles.capField}>
                <dt>GPU Count</dt>
                <dd>{worker.gpu.count}</dd>
              </div>
            )}
          </dl>
        </div>
      )}

      {customTags.length > 0 && (
        <div className={styles.capGroup}>
          <h3 className={styles.capGroupTitle}>Tags</h3>
          <dl className={styles.capGrid}>
            {customTags.map(([k, v]) => (
              <div key={k} className={styles.capField}>
                <dt>{k}</dt>
                <dd>{v || <span className={styles.emptyValue}>—</span>}</dd>
              </div>
            ))}
          </dl>
        </div>
      )}
    </section>
  )
}

// ── Assigned Tasks ───────────────────────────────────────────────────

function AssignedTasksSection({ worker, now }: { worker: WorkerDetailType; now: number }) {
  const tasks = worker.current_tasks

  return (
    <section className={styles.tasksSection} aria-label="Assigned tasks">
      <h2 className={styles.sectionTitle}>Active Tasks</h2>

      {tasks.length === 0 ? (
        <p className={styles.empty}>No active tasks.</p>
      ) : (
        <ul className={styles.taskCardList} aria-label="Active tasks">
          {tasks.map((task) => (
            <li key={task.id} className={styles.taskCard}>
              <div className={styles.taskCardRow}>
                <span className={styles.taskCardLabel}>Job</span>
                <Link to={`/jobs/${task.job_id}`} className={styles.taskCardJobLink}>
                  {truncateId(task.job_id)}
                </Link>
              </div>
              <div className={styles.taskCardRow}>
                <span className={styles.taskCardLabel}>Task</span>
                <span className={styles.taskCardValue}>{task.name}</span>
              </div>
              <div className={styles.taskCardRow}>
                <span className={styles.taskCardLabel}>Task ID</span>
                <span className={styles.taskCardMono}>{truncateId(task.id)}</span>
              </div>
              <div className={styles.taskCardRow}>
                <span className={styles.taskCardLabel}>Status</span>
                <StatusBadge status={task.status} />
              </div>
              <div className={styles.taskCardRow}>
                <span className={styles.taskCardLabel}>Start Time</span>
                <span className={styles.taskCardValue}>{formatDateTime(task.assigned_at)}</span>
              </div>
              <div className={styles.taskCardRow}>
                <span className={styles.taskCardLabel}>Elapsed</span>
                <span className={styles.taskCardValue}>
                  {formatTimespan(task.assigned_at, undefined, now)}
                </span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

// ── Toggle button ────────────────────────────────────────────────────

function ToggleButton({
  worker,
  isToggling,
  onDisable,
  onEnable,
}: {
  worker: WorkerDetailType
  isToggling: boolean
  onDisable: () => void
  onEnable: () => void
}) {
  if (worker.status === 'online') {
    return (
      <button
        className={`${styles.toggleBtn} ${styles['toggleBtn--disable']}`}
        onClick={onDisable}
        disabled={isToggling}
        type="button"
        aria-label={`Disable worker ${worker.name || worker.hostname}`}
      >
        {isToggling ? '…' : 'Disable'}
      </button>
    )
  }
  if (worker.status === 'disabled') {
    return (
      <button
        className={`${styles.toggleBtn} ${styles['toggleBtn--enable']}`}
        onClick={onEnable}
        disabled={isToggling}
        type="button"
        aria-label={`Enable worker ${worker.name || worker.hostname}`}
      >
        {isToggling ? '…' : 'Enable'}
      </button>
    )
  }
  return null
}

// ── Main component ────────────────────────────────────────────────────────────

export default function WorkerDetail() {
  const { id } = useParams<{ id: string }>()
  const workerId = id ?? ''

  const { data: worker, isLoading, isError, error } = useGetWorker(workerId)

  // ── Enable/disable mutations ────────────────────────────────────
  const queryClient = useQueryClient()
  const disableWorker = useDisableWorker()
  const enableWorker = useEnableWorker()
  const [isToggling, setIsToggling] = useState(false)

  const handleDisable = useCallback(async () => {
    enableWorker.reset()
    setIsToggling(true)
    try {
      await disableWorker.mutateAsync(workerId)
    } catch {
      // Error shown via disableWorker.isError
    } finally {
      setIsToggling(false)
    }
  }, [disableWorker, enableWorker, workerId])

  const handleEnable = useCallback(async () => {
    disableWorker.reset()
    setIsToggling(true)
    try {
      await enableWorker.mutateAsync(workerId)
    } catch {
      // Error shown via enableWorker.isError
    } finally {
      setIsToggling(false)
    }
  }, [enableWorker, disableWorker, workerId])

  // ── WS in-place status update ─────────────────────────────────────────────
  useWebSocket('workers', (payload) => {
    if (!isWorkerEvent(payload) || payload.worker_id !== workerId) return

    // The worker being viewed was removed (manual remove or retention sweep):
    // drop the cached detail and invalidate so the view reflects its absence.
    if (payload.status === WORKER_REMOVED_STATUS) {
      queryClient.removeQueries({ queryKey: queryKeys.workers.detail(workerId) })
      void queryClient.invalidateQueries({ queryKey: queryKeys.workers.all })
      return
    }

    const status = payload.status
    queryClient.setQueryData<WorkerDetailType>(queryKeys.workers.detail(workerId), (old) => {
      if (!old) return old
      return {
        ...old,
        status,
        ...(payload.name !== undefined ? { name: payload.name } : {}),
        ...(payload.hostname !== undefined ? { hostname: payload.hostname } : {}),
      }
    })
  })

  // ── Manual refresh ────────────────────────────────────────────────────────
  const handleRefresh = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.workers.detail(workerId) })
  }, [queryClient, workerId])

  // ── Live clock ────────────────────────────────────────────────────────────
  // Tick every second while the worker has any active task so their Elapsed and
  // the Uptime stay alive; otherwise 30s.
  const now = useLiveNow((worker?.current_tasks?.length ?? 0) > 0)

  // ── Loading / error states ────────────────────────────────────────────────

  if (isLoading) {
    return (
      <div className={styles.page}>
        <PageHeader title="Worker Details" subtitle="Loading…" />
        <div className={styles.loadingPlaceholder}>Loading worker details…</div>
      </div>
    )
  }

  if (isError || !worker) {
    return (
      <div className={styles.page}>
        <PageHeader title="Worker Details" />
        <div className={styles.errorBanner} role="alert">
          Failed to load worker:{' '}
          {error instanceof Error ? error.message : 'Worker not found or unavailable.'}
        </div>
      </div>
    )
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title="Worker Details"
        action={
          <div className={styles.headerActions}>
            <StatusBadge status={worker.status} />
            <ToggleButton
              worker={worker}
              isToggling={isToggling}
              onDisable={() => void handleDisable()}
              onEnable={() => void handleEnable()}
            />
            <button
              className={styles.refreshBtn}
              onClick={handleRefresh}
              type="button"
              aria-label="Refresh worker"
            >
              <Refresh />
              Refresh
            </button>
          </div>
        }
      />

      <div className={styles.workerNameArea}>
        <h2 className={styles.workerName}>{worker.name || worker.hostname}</h2>
      </div>

      {(disableWorker.isError || enableWorker.isError) && (
        <div className={styles.errorBanner} role="alert">
          Action failed:{' '}
          {(disableWorker.error ?? enableWorker.error) instanceof Error
            ? ((disableWorker.error ?? enableWorker.error) as Error).message
            : 'Unknown error'}
        </div>
      )}

      {/* Metadata header card */}
      <div className={styles.metaCard}>
        <dl className={styles.metaGrid}>
          <div className={styles.metaField}>
            <dt>Worker ID</dt>
            <dd>
              <IdDisplay id={worker.id} />
            </dd>
          </div>
          {/* Only show the hostname separately when it differs from the
              displayed name; otherwise the heading already shows it. */}
          {worker.name && worker.name !== worker.hostname && (
            <div className={styles.metaField}>
              <dt>Hostname</dt>
              <dd className={styles.metaValue}>{worker.hostname}</dd>
            </div>
          )}
          <div className={styles.metaField}>
            <dt>Farm</dt>
            <dd className={styles.metaValue}>{worker.farm_id}</dd>
          </div>
          {worker.compute_location && (
            <div className={styles.metaField}>
              <dt>Compute Location</dt>
              <dd className={styles.metaValue}>{worker.compute_location}</dd>
            </div>
          )}
          {worker.queue_id && (
            <div className={styles.metaField}>
              <dt>Queue</dt>
              <dd className={styles.metaValue}>{worker.queue_id}</dd>
            </div>
          )}
          {worker.ip_address && (
            <div className={styles.metaField}>
              <dt>IP Address</dt>
              <dd className={styles.metaMono}>{worker.ip_address}</dd>
            </div>
          )}
          <div className={styles.metaField}>
            <dt>Registered</dt>
            <dd className={styles.metaValue}>{formatDateTime(worker.registered_at)}</dd>
          </div>
          <div className={styles.metaField}>
            <dt>Last Heartbeat</dt>
            <dd className={styles.metaValue}>{formatDateTime(worker.last_heartbeat_at)}</dd>
          </div>
          <div className={styles.metaField}>
            <dt>Uptime</dt>
            <dd className={styles.metaValue}>{formatUptime(worker.registered_at, now)}</dd>
          </div>
        </dl>
      </div>

      <div className={styles.body}>
        {/* Full capability list */}
        <CapabilitiesSection worker={worker} />

        {/* Assigned tasks */}
        <AssignedTasksSection worker={worker} now={now} />

        {/* Worker diagnostic logs — heading sits outside the log box to match
            the Capabilities and Active Tasks sections. */}
        <section className={styles.diagSection} aria-label="Worker diagnostics">
          <h2 className={styles.sectionTitle}>Worker diagnostics</h2>
          <DiagnosticsPanel
            component={`worker:${workerId}`}
            title="Worker diagnostics"
            showTitle={false}
          />
        </section>
      </div>
    </div>
  )
}
