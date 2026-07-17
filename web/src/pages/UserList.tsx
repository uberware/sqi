// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import ErrorBanner from '@/components/ErrorBanner'
import { useListUsers } from '@/api/queries'
import { useDeleteUser } from '@/api/mutations'
import styles from './entityList.module.css'

export default function UserList() {
  const { data: users, isLoading, isError, error } = useListUsers()
  const deleteUser = useDeleteUser()
  const { showToast } = useToast()
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())

  const handleDelete = useCallback(
    async (id: string, username: string) => {
      if (!window.confirm(`Delete user "${username}"? This cannot be undone.`)) return
      setDeletingIds((s) => new Set(s).add(id))
      try {
        await deleteUser.mutateAsync(id)
        showToast(`User "${username}" deleted`, 'success')
      } catch (e) {
        showToast(e instanceof Error ? e.message : 'Failed to delete user', 'error')
      } finally {
        setDeletingIds((s) => {
          const next = new Set(s)
          next.delete(id)
          return next
        })
      }
    },
    [deleteUser, showToast],
  )

  const rows = users ?? []

  return (
    <div className={styles.page}>
      <PageHeader
        title="Users"
        backTo="/admin"
        subtitle={isLoading ? 'Loading…' : `${rows.length} users`}
        action={
          <Link to="/users/new" className={styles.newBtn}>
            + New User
          </Link>
        }
      />

      {isError && (
        <ErrorBanner>
          Failed to load users: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}

      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Users">
          <thead>
            <tr>
              <th>Username</th>
              <th>Display Name</th>
              <th>Role</th>
              <th>Status</th>
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
                  No user accounts yet. Create one to let someone else sign in to this server.
                </td>
              </tr>
            )}
            {rows.map((user) => (
              <tr key={user.id}>
                <td>
                  <Link to={`/users/${user.id}/edit`} className={styles.linkBtn}>
                    {user.username}
                  </Link>
                </td>
                <td>{user.display_name ?? '—'}</td>
                <td>{user.role}</td>
                <td>{user.disabled ? 'Disabled' : 'Active'}</td>
                <td>
                  <IconButton
                    icon={<Trash />}
                    className={styles.deleteBtn}
                    busy={deletingIds.has(user.id)}
                    onClick={() => void handleDelete(user.id, user.username)}
                    title="Delete"
                    label={`Delete user ${user.username}`}
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
