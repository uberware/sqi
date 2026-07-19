// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { useParams } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import RelativeTime from '@/components/RelativeTime'
import ErrorBanner from '@/components/ErrorBanner'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import { useUserApiKeys } from '@/api/queries'
import { useRevokeUserApiKey } from '@/api/mutations'
import listStyles from './entityList.module.css'

/**
 * Admin view of another user's API keys, gated on `apikeys.admin` by the
 * route. There is deliberately no create form: admins may list and revoke,
 * but minting a credential someone else is accountable for is a materially
 * different act, and the server exposes no route for it.
 */
export default function UserApiKeys() {
  const { id } = useParams<{ id: string }>()
  const userId = id ?? ''
  const { data: keys, isLoading, isError, error } = useUserApiKeys(userId)
  const revokeKey = useRevokeUserApiKey(userId)
  const { showToast } = useToast()
  const [revokingIds, setRevokingIds] = useState<Set<string>>(new Set())

  const handleRevoke = useCallback(
    async (keyId: string, name: string) => {
      if (!window.confirm(`Revoke API key "${name}"? This cannot be undone.`)) return
      setRevokingIds((prev) => new Set(prev).add(keyId))
      try {
        await revokeKey.mutateAsync(keyId)
        showToast(`API key "${name}" revoked`, 'success')
      } catch (err) {
        showToast(
          `Could not revoke "${name}": ${err instanceof Error ? err.message : 'Unknown error'}`,
          'error',
        )
      } finally {
        setRevokingIds((prev) => {
          const next = new Set(prev)
          next.delete(keyId)
          return next
        })
      }
    },
    [revokeKey, showToast],
  )

  const rows = keys ?? []

  return (
    <div>
      <PageHeader
        title="API keys"
        subtitle="Keys belonging to this user"
        backTo="/users"
        backLabel="Users"
      />

      {isError && (
        <ErrorBanner>
          Failed to load API keys: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}

      <div className={listStyles.tableWrap}>
        <table className={listStyles.table} aria-label="API Keys">
          <thead>
            <tr>
              <th>Name</th>
              <th>Prefix</th>
              <th>Created</th>
              <th>Last used</th>
              <th>Expires</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={listStyles.emptyRow}>
                <td colSpan={6}>Loading…</td>
              </tr>
            )}
            {!isLoading && !isError && rows.length === 0 && (
              <tr className={listStyles.emptyRow}>
                <td colSpan={6}>No API keys for this user.</td>
              </tr>
            )}
            {rows.map((key) => (
              <tr key={key.id}>
                <td>{key.name}</td>
                <td>
                  <code>{key.prefix}</code>
                </td>
                <td>
                  <RelativeTime timestamp={key.created_at} />
                </td>
                <td>{key.last_used_at ? <RelativeTime timestamp={key.last_used_at} /> : '—'}</td>
                <td>{key.expires_at ? <RelativeTime timestamp={key.expires_at} /> : '—'}</td>
                <td>
                  <IconButton
                    icon={<Trash />}
                    className={listStyles.deleteBtn}
                    busy={revokingIds.has(key.id)}
                    onClick={() => void handleRevoke(key.id, key.name)}
                    title="Revoke"
                    label={`Revoke API key ${key.name}`}
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
