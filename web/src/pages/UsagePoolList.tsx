// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import UsageBar from '@/components/UsageBar'
import IconButton from '@/components/IconButton'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import ErrorBanner from '@/components/ErrorBanner'
import { useListUsagePools } from '@/api/queries'
import { useDeleteUsagePool } from '@/api/mutations'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import styles from './UsagePoolList.module.css'

export default function UsagePoolList() {
  const { principal } = useAuth()
  const canManage = can(principal, 'infra.manage')

  const { data: pools, isLoading, isError, error } = useListUsagePools()
  const deletePool = useDeleteUsagePool()
  const { showToast } = useToast()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())

  const handleDelete = useCallback(
    async (id: string, name: string) => {
      if (!window.confirm(`Delete usage pool "${name}"? This cannot be undone.`)) return
      setDeletingIds((s) => new Set(s).add(id))
      try {
        await deletePool.mutateAsync(id)
        showToast(`Usage pool "${name}" deleted`, 'success')
      } catch (e) {
        showToast(e instanceof Error ? e.message : 'Failed to delete usage pool', 'error')
      } finally {
        setDeletingIds((s) => {
          const next = new Set(s)
          next.delete(id)
          return next
        })
      }
    },
    [deletePool, showToast],
  )

  const rows = pools ?? []

  return (
    <div className={styles.page}>
      <PageHeader
        title="Usage Pools"
        backTo="/admin"
        subtitle={isLoading ? 'Loading…' : `${rows.length} pools`}
        action={
          canManage ? (
            <Link to="/usage-pools/new" className={styles.newBtn}>
              + New Usage Pool
            </Link>
          ) : undefined
        }
      />

      {isError && (
        <ErrorBanner>
          Failed to load usage pools: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}

      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Usage pools">
          <thead>
            <tr>
              <th>Name</th>
              <th>License server</th>
              <th>Usage</th>
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
                <td colSpan={4}>No usage pools yet. Create one to cap concurrent usage.</td>
              </tr>
            )}
            {rows.map((pool) => (
              <tr key={pool.id}>
                <td>
                  <Link to={`/usage-pools/${pool.id}/edit`} className={styles.linkBtn}>
                    {pool.name}
                  </Link>
                </td>
                <td>{pool.server_hint ? pool.server_hint : '—'}</td>
                <td>
                  <div
                    className={styles.usage}
                    title={`${pool.in_use} in use, ${pool.available} available`}
                  >
                    <span className={styles.usageLabel}>
                      {pool.in_use} / {pool.max_concurrent}
                    </span>
                    <UsageBar
                      inUse={pool.in_use}
                      max={pool.max_concurrent}
                      className={styles.usageBar}
                    />
                  </div>
                </td>
                <td>
                  {canManage && (
                    <IconButton
                      icon={<Trash />}
                      className={styles.deleteBtn}
                      busy={deletingIds.has(pool.id)}
                      onClick={() => void handleDelete(pool.id, pool.name)}
                      title="Delete"
                      label={`Delete usage pool ${pool.name}`}
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
