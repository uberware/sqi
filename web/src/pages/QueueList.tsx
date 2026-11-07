// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import ErrorBanner from '@/components/ErrorBanner'
import RemainingTime from '@/components/RemainingTime'
import { queryKeys, useFarmsWithQueues } from '@/api/queries'
import type { Queue } from '@/api/types'
import { useDeleteQueue } from '@/api/mutations'
import { useQueryClient } from '@tanstack/react-query'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import { useLiveNow } from '@/hooks/useLiveNow'
import { countDownSeconds, etaMs, formatClock } from '@/lib/time'
import { useWebSocket } from '@/ws/context'
import styles from './entityList.module.css'

/** Queue timing is known when the server reported it (all four fields arrive together). */
function hasTiming(q: Queue): boolean {
  return q.work_remaining_seconds !== undefined
}

/** True when the queue's remaining figure covers no job at all (every unfinished job lacks an estimate). */
function nothingKnown(q: Queue): boolean {
  return (q.work_remaining_seconds ?? 0) === 0 && (q.jobs_without_estimate ?? 0) > 0
}

interface TimingCellsProps {
  queue: Queue
  anchorMs: number
  now: number
}

function TimingCells({ queue, anchorMs, now }: TimingCellsProps) {
  if (!hasTiming(queue)) {
    return (
      <>
        <td>—</td>
        <td>—</td>
        <td>{queue.paused ? 'paused' : '—'}</td>
      </>
    )
  }
  const running = queue.running_tasks ?? 0
  const known = !nothingKnown(queue)
  const left = countDownSeconds(queue.work_remaining_seconds ?? 0, running, anchorMs, now)
  const missing = queue.jobs_without_estimate ?? 0
  let eta = '—'
  if (queue.paused) eta = 'paused'
  else if (known && (queue.unfinished_jobs ?? 0) > 0) {
    eta = formatClock(etaMs(Math.ceil(left), running, now), now)
  }
  return (
    <>
      <td>{queue.unfinished_jobs ?? 0}</td>
      <td>
        {known ? (
          <RemainingTime
            seconds={queue.work_remaining_seconds}
            running={running}
            anchorMs={anchorMs}
            now={now}
          />
        ) : (
          '—'
        )}
        {missing > 0 && <div>+{missing} without an estimate</div>}
      </td>
      <td>{eta}</td>
    </>
  )
}

/** Delay before a burst of job/task events refreshes the queue figures. */
const REFRESH_DELAY_MS = 5_000

export default function QueueList() {
  const { principal } = useAuth()
  const canManage = can(principal, 'infra.manage')

  const { data: farmsWithQueues, isLoading, isError, error, dataUpdatedAt } = useFarmsWithQueues()
  const queryClient = useQueryClient()
  const deleteQueue = useDeleteQueue()
  const { showToast } = useToast()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())

  const rows = (farmsWithQueues ?? []).flatMap(({ farm, queues }) =>
    queues.map((queue) => ({ queue, farmName: farm.name })),
  )

  const refreshTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(
    () => () => {
      if (refreshTimerRef.current) clearTimeout(refreshTimerRef.current)
    },
    [],
  )

  // Any job or task event may change the queue figures. The timer is never
  // reset, so a steady event stream cannot starve the refresh.
  useWebSocket('jobs', () => {
    if (refreshTimerRef.current) return
    refreshTimerRef.current = setTimeout(() => {
      refreshTimerRef.current = null
      void queryClient.invalidateQueries({ queryKey: [...queryKeys.farms.all, 'with-queues'] })
    }, REFRESH_DELAY_MS)
  })

  const counting = rows.some(({ queue }) => (queue.running_tasks ?? 0) > 0)
  const now = useLiveNow(counting)

  const handleDelete = useCallback(
    async (id: string, name: string) => {
      if (!window.confirm(`Delete queue "${name}"? This cannot be undone.`)) return
      setDeletingIds((s) => new Set(s).add(id))
      try {
        await deleteQueue.mutateAsync(id)
        showToast(`Queue "${name}" deleted`, 'success')
      } catch (e) {
        showToast(e instanceof Error ? e.message : 'Failed to delete queue', 'error')
      } finally {
        setDeletingIds((s) => {
          const next = new Set(s)
          next.delete(id)
          return next
        })
      }
    },
    [deleteQueue, showToast],
  )

  return (
    <div className={styles.page}>
      <PageHeader
        title="Queues"
        backTo="/admin"
        subtitle={isLoading ? 'Loading…' : `${rows.length} queues`}
        action={
          canManage ? (
            <Link to="/queues/new" className={styles.newBtn}>
              + New Queue
            </Link>
          ) : undefined
        }
      />

      {isError && (
        <ErrorBanner>
          Failed to load queues: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}

      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Queues">
          <thead>
            <tr>
              <th>Name</th>
              <th>Farm</th>
              <th>Priority</th>
              <th>Max Concurrent</th>
              <th>Paused</th>
              <th>Unfinished</th>
              <th>Remaining</th>
              <th>ETA</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={styles.emptyRow}>
                <td colSpan={9}>Loading…</td>
              </tr>
            )}
            {!isLoading && rows.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={9}>No queues yet. Create one to submit jobs to it.</td>
              </tr>
            )}
            {rows.map(({ queue, farmName }) => (
              <tr key={queue.id}>
                <td>
                  <Link to={`/queues/${queue.id}/edit`} className={styles.linkBtn}>
                    {queue.name}
                  </Link>
                </td>
                <td>
                  <Link to="/farms" className={styles.linkBtn}>
                    {farmName}
                  </Link>
                </td>
                <td>{queue.priority}</td>
                <td>
                  {queue.max_concurrent_tasks === 0 ? 'Unlimited' : queue.max_concurrent_tasks}
                </td>
                <td>{queue.paused ? 'Yes' : 'No'}</td>
                <TimingCells queue={queue} anchorMs={dataUpdatedAt} now={now} />
                <td>
                  {canManage && (
                    <IconButton
                      icon={<Trash />}
                      className={styles.deleteBtn}
                      busy={deletingIds.has(queue.id)}
                      onClick={() => void handleDelete(queue.id, queue.name)}
                      title="Delete"
                      label={`Delete queue ${queue.name}`}
                    />
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
