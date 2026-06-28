// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import { useListComputeLocations } from '@/api/queries'
import { useDeleteComputeLocation } from '@/api/mutations'
import styles from './ComputeLocationList.module.css'

export default function ComputeLocationList() {
  const { data: locations, isLoading, isError, error } = useListComputeLocations()
  const deleteLocation = useDeleteComputeLocation()
  const { showToast } = useToast()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())

  const handleDelete = useCallback(
    async (id: string, name: string, workerCount: number) => {
      const msg =
        workerCount > 0
          ? `Compute location "${name}" is in use by ${workerCount} online worker(s) and will reappear if a worker re-registers. Delete anyway?`
          : `Delete compute location "${name}"? This cannot be undone.`
      if (!window.confirm(msg)) return
      setDeletingIds((s) => new Set(s).add(id))
      try {
        await deleteLocation.mutateAsync(id)
        showToast(`Compute location "${name}" deleted`, 'success')
      } catch (e) {
        showToast(e instanceof Error ? e.message : 'Failed to delete compute location', 'error')
      } finally {
        setDeletingIds((s) => {
          const next = new Set(s)
          next.delete(id)
          return next
        })
      }
    },
    [deleteLocation, showToast],
  )

  const rows = locations ?? []

  return (
    <div className={styles.page}>
      <PageHeader
        title="Compute Locations"
        subtitle={isLoading ? 'Loading…' : `${rows.length} locations`}
        action={
          <Link to="/compute-locations/new" className={styles.newBtn}>
            + New Compute Location
          </Link>
        }
      />

      {isError && (
        <div className={styles.errorBanner} role="alert">
          Failed to load compute locations:{' '}
          {error instanceof Error ? error.message : 'Unknown error'}
        </div>
      )}

      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Compute locations">
          <thead>
            <tr>
              <th>Name</th>
              <th>Description</th>
              <th>Workers</th>
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
                <td colSpan={4}>
                  No compute locations yet. They group workers by physical or logical site so jobs
                  can resolve storage paths correctly for each location.
                </td>
              </tr>
            )}
            {rows.map((location) => (
              <tr key={location.id}>
                <td>
                  <Link to={`/compute-locations/${location.id}/edit`} className={styles.linkBtn}>
                    {location.name}
                  </Link>
                </td>
                <td>{location.description ?? '—'}</td>
                <td>{location.worker_count}</td>
                <td>
                  <IconButton
                    icon={<Trash />}
                    className={styles.deleteBtn}
                    busy={deletingIds.has(location.id)}
                    onClick={() =>
                      void handleDelete(location.id, location.name, location.worker_count)
                    }
                    title="Delete"
                    label={`Delete compute location ${location.name}`}
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
