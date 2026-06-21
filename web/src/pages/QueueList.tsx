// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import { useFarmsWithQueues } from '@/api/queries'
import { useDeleteQueue } from '@/api/mutations'
import styles from './QueueList.module.css'

export default function QueueList() {
  const { data: farmsWithQueues, isLoading, isError, error } = useFarmsWithQueues()
  const deleteQueue = useDeleteQueue()
  const { showToast } = useToast()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())

  const rows = (farmsWithQueues ?? []).flatMap(({ farm, queues }) =>
    queues.map((queue) => ({ queue, farmName: farm.name })),
  )

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
        subtitle={isLoading ? 'Loading…' : `${rows.length} queues`}
        action={
          <Link to="/queues/new" className={styles.newBtn}>
            + New Queue
          </Link>
        }
      />

      {isError && (
        <div className={styles.errorBanner} role="alert">
          Failed to load queues: {error instanceof Error ? error.message : 'Unknown error'}
        </div>
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
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={styles.emptyRow}>
                <td colSpan={6}>Loading…</td>
              </tr>
            )}
            {!isLoading && rows.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={6}>No queues yet. Create one to submit jobs to it.</td>
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
                <td>
                  <IconButton
                    icon={<Trash />}
                    className={styles.deleteBtn}
                    busy={deletingIds.has(queue.id)}
                    onClick={() => void handleDelete(queue.id, queue.name)}
                    title="Delete"
                    label={`Delete queue ${queue.name}`}
                  />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
