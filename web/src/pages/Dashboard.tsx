// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useEffect, useRef } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import PageHeader from '@/components/PageHeader'
import StatusBadge from '@/components/StatusBadge'
import UsageBar from '@/components/UsageBar'
import {
  useListJobs,
  useListWorkers,
  useListFarms,
  useListUsagePools,
  queryKeys,
} from '@/api/queries'
import { useWebSocket, useConnectionState } from '@/ws/context'
import { isJobEvent, isWorkerEvent } from '@/ws/events'
import type { Job } from '@/api/types'
import styles from './Dashboard.module.css'

// ── Helpers ───────────────────────────────────────────────────────────────────

function formatFailureTime(iso: string | undefined): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

// ── Workers summary card ────────────────────────────────────────────

function WorkersCard() {
  const { data: onlineData } = useListWorkers({ status: 'online', limit: 1 })
  const { data: offlineData } = useListWorkers({ status: 'offline', limit: 1 })
  const { data: disabledData } = useListWorkers({ status: 'disabled', limit: 1 })
  const { data: farmsData } = useListFarms()

  const onlineCount = onlineData?.total ?? 0
  const offlineCount = offlineData?.total ?? 0
  const disabledCount = disabledData?.total ?? 0
  const totalWorkers = onlineCount + offlineCount + disabledCount

  const farmCapacity = (farmsData ?? []).reduce((sum, f) => sum + f.max_concurrent_tasks, 0)

  return (
    <div className={styles.card} data-testid="workers-card">
      <div className={styles.cardHeader}>
        <h2 className={styles.cardTitle}>Workers</h2>
      </div>
      <div className={styles.cardBody}>
        <div className={styles.bigStatRow}>
          <span className={styles.bigNum} aria-hidden="true">
            {onlineCount}
          </span>
          <span className={styles.bigDenominator}>/ {totalWorkers} total</span>
        </div>
        {farmCapacity > 0 && (
          <p className={styles.capacityLine}>Farm capacity: {farmCapacity} max concurrent tasks</p>
        )}
        <div className={styles.statBreakdown} role="list" aria-label="Worker status breakdown">
          <Link
            to="/workers?status=online"
            className={styles.statLink}
            role="listitem"
            aria-label={`${onlineCount} online workers`}
          >
            <span className={styles.statDot + ' ' + styles['statDot--online']} aria-hidden="true" />
            <span className={styles.statCount}>{onlineCount}</span>
            <span className={styles.statLabel}>Online</span>
          </Link>
          <Link
            to="/workers?status=offline"
            className={styles.statLink}
            role="listitem"
            aria-label={`${offlineCount} offline workers`}
          >
            <span
              className={styles.statDot + ' ' + styles['statDot--offline']}
              aria-hidden="true"
            />
            <span className={styles.statCount}>{offlineCount}</span>
            <span className={styles.statLabel}>Offline</span>
          </Link>
          <Link
            to="/workers?status=disabled"
            className={styles.statLink}
            role="listitem"
            aria-label={`${disabledCount} disabled workers`}
          >
            <span
              className={styles.statDot + ' ' + styles['statDot--disabled']}
              aria-hidden="true"
            />
            <span className={styles.statCount}>{disabledCount}</span>
            <span className={styles.statLabel}>Disabled</span>
          </Link>
        </div>
      </div>
    </div>
  )
}

// ── Jobs summary card ───────────────────────────────────────────────

function JobsCard() {
  const { data: runningData } = useListJobs({ status: 'running', limit: 1 })
  const { data: pendingData } = useListJobs({ status: 'pending', limit: 1 })
  const { data: completedData } = useListJobs({ status: 'completed', limit: 1 })
  const { data: failedData } = useListJobs({ status: 'failed', limit: 1 })

  const runningCount = runningData?.total ?? 0
  const pendingCount = pendingData?.total ?? 0
  const completedCount = completedData?.total ?? 0
  const failedCount = failedData?.total ?? 0

  const statuses = [
    {
      label: 'Running',
      count: runningCount,
      status: 'running' as const,
      to: '/jobs?status=running',
    },
    {
      label: 'Pending',
      count: pendingCount,
      status: 'pending' as const,
      to: '/jobs?status=pending',
    },
    {
      label: 'Completed',
      count: completedCount,
      status: 'completed' as const,
      to: '/jobs?status=completed',
    },
    { label: 'Failed', count: failedCount, status: 'failed' as const, to: '/jobs?status=failed' },
  ]

  return (
    <div className={styles.card} data-testid="jobs-card">
      <div className={styles.cardHeader}>
        <h2 className={styles.cardTitle}>Jobs</h2>
      </div>
      <div className={styles.cardBody}>
        <div className={styles.jobStatGrid} role="list" aria-label="Job status counts">
          {statuses.map(({ label, count, status, to }) => (
            <Link
              key={status}
              to={to}
              className={styles.jobStatItem}
              role="listitem"
              aria-label={`${count} ${label} jobs`}
            >
              <span className={styles.jobStatCount} aria-hidden="true">
                {count}
              </span>
              <StatusBadge status={status} />
            </Link>
          ))}
        </div>
      </div>
    </div>
  )
}

// ── Usage pools utilization card ──────────────────────────────────────

function UsagePoolsCard() {
  const { data, isLoading, isError } = useListUsagePools()
  const pools = data ?? []

  return (
    <div className={styles.card} data-testid="usage-pools-card">
      <div className={styles.cardHeader}>
        <h2 className={styles.cardTitle}>Usage Pools</h2>
        <Link to="/usage-pools" className={styles.cardHeaderLink}>
          Manage
        </Link>
      </div>
      <div className={styles.cardBody}>
        {isLoading && <p className={styles.emptyMessage}>Loading…</p>}
        {isError && (
          <p className={styles.errorMessage} role="alert">
            Failed to load usage pools.
          </p>
        )}
        {!isLoading && !isError && pools.length === 0 && (
          <p className={styles.emptyMessage}>No usage pools configured.</p>
        )}
        {pools.length > 0 && (
          <ul className={styles.poolUsageList} role="list" aria-label="Usage pool utilization">
            {pools.map((pool) => (
              <li key={pool.id} className={styles.poolUsageRow}>
                <span className={styles.poolUsageName} title={pool.name}>
                  {pool.name}
                </span>
                <UsageBar inUse={pool.in_use} max={pool.max_concurrent} />
                <span className={styles.poolUsageCount}>
                  {pool.in_use} / {pool.max_concurrent}
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  )
}

// ── Recent failures widget ──────────────────────────────────────────

const FAILURES_PARAMS = {
  status: 'failed' as const,
  sort_by: 'updated_at' as const,
  sort_dir: 'desc' as const,
  limit: 10,
}

function RecentFailuresWidget() {
  const { data, isLoading, isError } = useListJobs(FAILURES_PARAMS)
  const failures: Job[] = data?.items ?? []

  return (
    <div className={styles.card} data-testid="recent-failures-widget">
      <div className={styles.cardHeader}>
        <h2 className={styles.cardTitle}>Recent Failures</h2>
        <Link to="/jobs?status=failed" className={styles.cardHeaderLink}>
          View all
        </Link>
      </div>
      <div className={styles.failuresBody}>
        {isLoading && <p className={styles.emptyMessage}>Loading…</p>}
        {isError && (
          <p className={styles.errorMessage} role="alert">
            Failed to load recent failures.
          </p>
        )}
        {!isLoading && !isError && failures.length === 0 && (
          <p className={styles.emptyMessage}>No recent failures.</p>
        )}
        {failures.length > 0 && (
          <table className={styles.failureTable} aria-label="Recent failed jobs">
            <thead>
              <tr>
                <th>Job</th>
                <th>Owner</th>
                <th>Queue</th>
                <th>Failed</th>
              </tr>
            </thead>
            <tbody>
              {failures.map((job) => (
                <tr key={job.id}>
                  <td>
                    <Link to={`/jobs/${job.id}`} className={styles.jobLink}>
                      {job.name}
                    </Link>
                  </td>
                  <td className={styles.metaText}>{job.owner}</td>
                  <td className={styles.metaText}>{job.queue_name ?? job.queue_id}</td>
                  <td className={styles.metaText}>
                    {formatFailureTime(job.completed_at ?? job.updated_at)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}

// ── Dashboard ──────────────────────────────────────────────────

export default function Dashboard() {
  const queryClient = useQueryClient()
  const connectionState = useConnectionState()

  // Debounce job invalidations so rapid task bursts cause at most one re-fetch.
  const jobInvalidateTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const scheduleJobInvalidate = useCallback(() => {
    if (jobInvalidateTimerRef.current) clearTimeout(jobInvalidateTimerRef.current)
    jobInvalidateTimerRef.current = setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
    }, 5_000)
  }, [queryClient])

  useEffect(
    () => () => {
      if (jobInvalidateTimerRef.current) clearTimeout(jobInvalidateTimerRef.current)
    },
    [],
  )

  // Subscribe to worker events — invalidate worker count queries.
  useWebSocket('workers', (payload) => {
    if (!isWorkerEvent(payload)) return
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

  // Subscribe to job events — invalidate job count and recent failures queries.
  useWebSocket('jobs', (payload) => {
    if (!isJobEvent(payload)) return
    // Invalidate the affected status count immediately so the card updates.
    void queryClient.invalidateQueries({
      queryKey: queryKeys.jobs.list({ status: payload.status, limit: 1 }),
    })
    // When a job fails, refresh the recent failures widget immediately so the new
    // entry appears without waiting for the debounced full invalidation.
    if (payload.status === 'failed') {
      void queryClient.invalidateQueries({
        queryKey: queryKeys.jobs.list(FAILURES_PARAMS),
      })
    }
    // Debounce a full job refresh to catch status transitions (e.g. running→failed
    // requires the failed count to be incremented and running decremented).
    scheduleJobInvalidate()
  })

  const isDisconnected = connectionState !== 'connected'

  return (
    <div className={styles.page}>
      <PageHeader title="Dashboard" subtitle="Farm overview" />

      {/* Prominent disconnect warning */}
      {isDisconnected && (
        <div className={styles.disconnectBanner} role="alert" data-testid="disconnect-banner">
          <span className={styles.disconnectIcon} aria-hidden="true">
            ⚠
          </span>
          <span>Live updates paused ({connectionState}) — dashboard data may be stale.</span>
        </div>
      )}

      <div className={styles.content}>
        {/* Summary cards grid */}
        <div className={styles.grid}>
          <WorkersCard />
          <JobsCard />
          <UsagePoolsCard />
        </div>

        {/* Recent failures widget (full width) */}
        <RecentFailuresWidget />
      </div>
    </div>
  )
}
