// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import ErrorBanner from '@/components/ErrorBanner'
import { useListStorageLocations } from '@/api/queries'
import { useDeleteStorageLocation } from '@/api/mutations'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import styles from './entityList.module.css'

export default function StorageLocationList() {
  const { principal } = useAuth()
  const canManage = can(principal, 'infra.manage')

  const { data: locations, isLoading, isError, error } = useListStorageLocations()
  const deleteLocation = useDeleteStorageLocation()
  const { showToast } = useToast()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())

  const handleDelete = useCallback(
    async (id: string, name: string) => {
      if (!window.confirm(`Delete storage location "${name}"? This cannot be undone.`)) return
      setDeletingIds((s) => new Set(s).add(id))
      try {
        await deleteLocation.mutateAsync(id)
        showToast(`Storage location "${name}" deleted`, 'success')
      } catch (e) {
        showToast(e instanceof Error ? e.message : 'Failed to delete storage location', 'error')
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
        title="Storage Locations"
        backTo="/admin"
        subtitle={isLoading ? 'Loading…' : `${rows.length} locations`}
        action={
          canManage ? (
            <Link to="/storage-locations/new" className={styles.newBtn}>
              + New Storage Location
            </Link>
          ) : undefined
        }
      />

      {isError && (
        <ErrorBanner>
          Failed to load storage locations:{' '}
          {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}

      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Storage locations">
          <thead>
            <tr>
              <th>Name</th>
              <th>Type</th>
              <th>Default root</th>
              <th>Roots</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={styles.emptyRow}>
                <td colSpan={5}>Loading…</td>
              </tr>
            )}
            {!isLoading && rows.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={5}>
                  No storage locations yet. They let jobs reference data by name so the same job
                  runs on Linux, Windows, and cloud workers without hard-coded paths.{' '}
                  <a
                    href="https://github.com/uberware/sqi/blob/main/docs/storage-locations.md"
                    target="_blank"
                    rel="noreferrer"
                  >
                    Learn more
                  </a>
                  .
                </td>
              </tr>
            )}
            {rows.map((location) => {
              const roots = location.roots ?? {}
              const defaultRoot = roots['default']
              const rootCount = Object.keys(roots).length
              return (
                <tr key={location.id}>
                  <td>
                    <Link to={`/storage-locations/${location.id}/edit`} className={styles.linkBtn}>
                      {location.name}
                    </Link>
                  </td>
                  <td>{location.type}</td>
                  <td>{defaultRoot ? defaultRoot : '—'}</td>
                  <td>{rootCount}</td>
                  <td>
                    {canManage && (
                      <IconButton
                        icon={<Trash />}
                        className={styles.deleteBtn}
                        busy={deletingIds.has(location.id)}
                        onClick={() => void handleDelete(location.id, location.name)}
                        title="Delete"
                        label={`Delete storage location ${location.name}`}
                      />
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
