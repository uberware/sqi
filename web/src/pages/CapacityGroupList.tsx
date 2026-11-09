// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { Link } from 'react-router'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import ErrorBanner from '@/components/ErrorBanner'
import { useListCapacityGroups } from '@/api/queries'
import { useDeleteCapacityGroup } from '@/api/mutations'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import type { CapacityGroup } from '@/api/types'
import styles from './CapacityGroupList.module.css'

/** The members and desired-vs-online cells, which both come from the last evaluation. */
function StatusCells({ group }: { group: CapacityGroup }) {
  const s = group.status
  if (s === null) {
    const note = group.enabled ? 'not evaluated yet' : 'disabled'
    return (
      <>
        <td>—</td>
        <td className={styles.muted}>{note}</td>
      </>
    )
  }
  return (
    <>
      <td className={styles.num}>
        {s.online_members} online / {s.offline_members} offline
      </td>
      <td className={styles.num}>
        {s.desired_machines} desired / {s.online_members} online
        {s.degraded !== null && (
          <>
            {' '}
            <span className={styles.degraded} title={s.degraded.reason}>
              degraded
            </span>
          </>
        )}
      </td>
    </>
  )
}

export default function CapacityGroupList() {
  const { principal } = useAuth()
  const canManage = can(principal, 'infra.manage')

  const { data: groups, isLoading, isError, error } = useListCapacityGroups()
  const deleteGroup = useDeleteCapacityGroup()
  const { showToast } = useToast()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())

  const handleDelete = useCallback(
    async (id: string, name: string) => {
      if (!window.confirm(`Delete capacity group "${name}"? This cannot be undone.`)) return
      setDeletingIds((s) => new Set(s).add(id))
      try {
        await deleteGroup.mutateAsync(id)
        showToast(`Capacity group "${name}" deleted`, 'success')
      } catch (e) {
        showToast(e instanceof Error ? e.message : 'Failed to delete capacity group', 'error')
      } finally {
        setDeletingIds((s) => {
          const next = new Set(s)
          next.delete(id)
          return next
        })
      }
    },
    [deleteGroup, showToast],
  )

  const rows = groups ?? []

  return (
    <div className={styles.page}>
      <PageHeader
        title="Capacity Groups"
        backTo="/admin"
        subtitle={isLoading ? 'Loading…' : `${rows.length} groups`}
        action={
          canManage ? (
            <Link to="/capacity-groups/new" className={styles.newBtn}>
              + New Capacity Group
            </Link>
          ) : undefined
        }
      />

      {isError && (
        <ErrorBanner>
          Failed to load capacity groups: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}

      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Capacity groups">
          <thead>
            <tr>
              <th>Name</th>
              <th>Kind</th>
              <th>Provider</th>
              <th>Priority</th>
              <th>Members</th>
              <th>Desired / online</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={styles.emptyRow}>
                <td colSpan={7}>Loading…</td>
              </tr>
            )}
            {!isLoading && rows.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={7}>
                  No capacity groups yet. Create one to have sqi follow demand with machines.
                </td>
              </tr>
            )}
            {rows.map((group) => (
              <tr key={group.id}>
                <td>
                  <Link to={`/capacity-groups/${group.id}`} className={styles.linkBtn}>
                    {group.name}
                  </Link>
                </td>
                <td>{group.kind}</td>
                <td>{group.provider}</td>
                <td className={styles.num}>{group.priority}</td>
                <StatusCells group={group} />
                <td>
                  {canManage && (
                    <IconButton
                      icon={<Trash />}
                      className={styles.deleteBtn}
                      busy={deletingIds.has(group.id)}
                      onClick={() => void handleDelete(group.id, group.name)}
                      title="Delete"
                      label={`Delete capacity group ${group.name}`}
                    />
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <p className={styles.note}>
        Desired is the total number of machines a group should have, including machines marked for
        retirement; it is a target, not an amount to add.
      </p>
    </div>
  )
}
