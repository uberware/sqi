// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import { Pause, Play, Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import ErrorBanner from '@/components/ErrorBanner'
import RemainingTime from '@/components/RemainingTime'
import { queryKeys, useFarmsWithQueues } from '@/api/queries'
import type { Queue } from '@/api/types'
import { useDeleteQueue, useSetQueuePaused } from '@/api/mutations'
import { useQueryClient } from '@tanstack/react-query'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import { useLiveNow } from '@/hooks/useLiveNow'
import { countDownCeil, etaMs, formatClock } from '@/lib/time'
import { useWebSocket } from '@/ws/context'
import styles from './entityList.module.css'

/** Queue timing is known when the server reported it (all five fields arrive together, counting as_of). */
function hasTiming(q: Queue): boolean {
  return q.work_remaining_seconds !== undefined
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
  const {
    running_tasks: running = 0,
    work_remaining_seconds: work = 0,
    jobs_without_estimate: missing = 0,
    unfinished_jobs: unfinished = 0,
  } = queue
  // No job in the figure has an estimate: nothing is known, not "zero left".
  const known = !(work === 0 && missing > 0)
  let eta = '—'
  if (queue.paused) eta = 'paused'
  else if (known && unfinished > 0) {
    eta = formatClock(etaMs(countDownCeil(work, running, anchorMs, now), running, now), now)
  }
  return (
    <>
      <td>{unfinished}</td>
      <td>
        {known ? (
          <RemainingTime seconds={work} running={running} anchorMs={anchorMs} now={now} />
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

  const {
    data: farmsWithQueues,
    isLoading,
    isError,
    error,
    dataUpdatedAt,
  } = useFarmsWithQueues({
    includeTiming: true,
  })
  const queryClient = useQueryClient()
  const { mutateAsync: deleteQueue } = useDeleteQueue()
  const { mutateAsync: setQueuePaused } = useSetQueuePaused()
  const { showToast } = useToast()
  const [busyKeys, setBusyKeys] = useState<Set<string>>(new Set())

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
      void queryClient.invalidateQueries({ queryKey: queryKeys.farms.withQueues(true) })
    }, REFRESH_DELAY_MS)
  })

  const counting = rows.some(({ queue }) => (queue.running_tasks ?? 0) > 0)
  const now = useLiveNow(counting)

  /** Runs one row action, showing `key` busy meanwhile and toasting the outcome. */
  async function runRowAction(
    key: string,
    action: () => Promise<unknown>,
    done: string,
    failed: string,
  ) {
    setBusyKeys((s) => new Set(s).add(key))
    try {
      await action()
      showToast(done, 'success')
    } catch (e) {
      showToast(e instanceof Error ? e.message : failed, 'error')
    } finally {
      setBusyKeys((s) => {
        const next = new Set(s)
        next.delete(key)
        return next
      })
    }
  }

  function handleDelete(queue: Queue) {
    if (!window.confirm(`Delete queue "${queue.name}"? This cannot be undone.`)) return
    void runRowAction(
      `delete:${queue.id}`,
      () => deleteQueue(queue.id),
      `Queue "${queue.name}" deleted`,
      'Failed to delete queue',
    )
  }

  function handleSetPaused(queue: Queue, paused: boolean) {
    const verb = paused ? 'pause' : 'resume'
    void runRowAction(
      `pause:${queue.id}`,
      () => setQueuePaused({ id: queue.id, paused }),
      `Queue "${queue.name}" ${verb}d`,
      `Failed to ${verb} queue`,
    )
  }

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
                <td className={styles.actionsCell}>
                  {canManage && !queue.paused && (
                    <IconButton
                      icon={<Pause />}
                      className={styles.pauseBtn}
                      busy={busyKeys.has(`pause:${queue.id}`)}
                      onClick={() => handleSetPaused(queue, true)}
                      title="Pause"
                      label={`Pause queue ${queue.name}`}
                    />
                  )}
                  {canManage && queue.paused && (
                    <IconButton
                      icon={<Play />}
                      className={styles.resumeBtn}
                      busy={busyKeys.has(`pause:${queue.id}`)}
                      onClick={() => handleSetPaused(queue, false)}
                      title="Resume"
                      label={`Resume queue ${queue.name}`}
                    />
                  )}
                  {canManage && (
                    <IconButton
                      icon={<Trash />}
                      className={styles.deleteBtn}
                      busy={busyKeys.has(`delete:${queue.id}`)}
                      onClick={() => handleDelete(queue)}
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
