// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import { useListFarms } from '@/api/queries'
import { useDeleteFarm } from '@/api/mutations'
import styles from './FarmList.module.css'

export default function FarmList() {
  const { data: farms, isLoading, isError, error } = useListFarms()
  const deleteFarm = useDeleteFarm()
  const { showToast } = useToast()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())

  const handleDelete = useCallback(
    async (id: string, name: string) => {
      if (!window.confirm(`Delete farm "${name}"? This cannot be undone.`)) return
      setDeletingIds((s) => new Set(s).add(id))
      try {
        await deleteFarm.mutateAsync(id)
        showToast(`Farm "${name}" deleted`, 'success')
      } catch (e) {
        showToast(e instanceof Error ? e.message : 'Failed to delete farm', 'error')
      } finally {
        setDeletingIds((s) => {
          const next = new Set(s)
          next.delete(id)
          return next
        })
      }
    },
    [deleteFarm, showToast],
  )

  const rows = farms ?? []

  return (
    <div className={styles.page}>
      <PageHeader
        title="Farms"
        subtitle={isLoading ? 'Loading…' : `${rows.length} farms`}
        action={
          <Link to="/farms/new" className={styles.newBtn}>
            + New Farm
          </Link>
        }
      />

      {isError && (
        <div className={styles.errorBanner} role="alert">
          Failed to load farms: {error instanceof Error ? error.message : 'Unknown error'}
        </div>
      )}

      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Farms">
          <thead>
            <tr>
              <th>Name</th>
              <th>Description</th>
              <th>Max Concurrent Tasks</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={styles.emptyRow}>
                <td colSpan={4}>Loading…</td>
              </tr>
            )}
            {!isLoading && rows.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={4}>No farms yet. Create one to start submitting jobs.</td>
              </tr>
            )}
            {rows.map((farm) => (
              <tr key={farm.id}>
                <td>
                  <Link to={`/farms/${farm.id}/edit`} className={styles.linkBtn}>
                    {farm.name}
                  </Link>
                </td>
                <td>{farm.description ?? '—'}</td>
                <td>{farm.max_concurrent_tasks === 0 ? 'Unlimited' : farm.max_concurrent_tasks}</td>
                <td>
                  <IconButton
                    icon={<Trash />}
                    className={styles.deleteBtn}
                    busy={deletingIds.has(farm.id)}
                    onClick={() => void handleDelete(farm.id, farm.name)}
                    title="Delete"
                    label={`Delete farm ${farm.name}`}
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
