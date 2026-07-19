// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import IconButton from '@/components/IconButton'
import RelativeTime from '@/components/RelativeTime'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import type { ApiKey } from '@/api/types'
import listStyles from '@/pages/entityList.module.css'

interface Props {
  keys: ApiKey[]
  isLoading: boolean
  /** Revokes one key by id. Rejections surface as an error toast. */
  onRevoke: (id: string) => Promise<unknown>
  /** Shown when the list is empty — the two call sites word this differently. */
  emptyMessage: string
  /** Extra class for the prefix `<code>`, when the page styles it. */
  prefixClassName?: string | undefined
}

/**
 * The API-key table shared by the self-service list (/api-keys) and the admin
 * per-user list (/users/{id}/api-keys). Both render the same six columns of
 * the same type; only the empty-state wording and the owning mutation differ,
 * so those are props and everything else — including the confirm prompt, the
 * per-row busy state, and the toast wording — lives here once.
 *
 * There is no create affordance: only the self-service page can mint a key,
 * and it owns that form.
 */
export default function ApiKeyTable({
  keys,
  isLoading,
  onRevoke,
  emptyMessage,
  prefixClassName,
}: Props) {
  const { showToast } = useToast()
  const [revokingIds, setRevokingIds] = useState<Set<string>>(new Set())

  const handleRevoke = useCallback(
    async (id: string, keyName: string) => {
      if (!window.confirm(`Revoke API key "${keyName}"? This cannot be undone.`)) return
      setRevokingIds((s) => new Set(s).add(id))
      try {
        await onRevoke(id)
        showToast(`API key "${keyName}" revoked`, 'success')
      } catch (e) {
        showToast(e instanceof Error ? e.message : 'Failed to revoke API key', 'error')
      } finally {
        setRevokingIds((s) => {
          const next = new Set(s)
          next.delete(id)
          return next
        })
      }
    },
    [onRevoke, showToast],
  )

  return (
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
          {!isLoading && keys.length === 0 && (
            <tr className={listStyles.emptyRow}>
              <td colSpan={6}>{emptyMessage}</td>
            </tr>
          )}
          {keys.map((key) => (
            <tr key={key.id}>
              <td>{key.name}</td>
              <td>
                <code className={prefixClassName}>{key.prefix}</code>
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
  )
}
