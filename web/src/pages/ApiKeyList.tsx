// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import CopyButton from '@/components/CopyButton'
import RelativeTime from '@/components/RelativeTime'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import ErrorBanner from '@/components/ErrorBanner'
import { useApiKeys } from '@/api/queries'
import { useCreateApiKey, useRevokeApiKey } from '@/api/mutations'
import type { ApiKeyCreated } from '@/api/types'
import listStyles from './entityList.module.css'
import styles from './ApiKeyList.module.css'

export default function ApiKeyList() {
  const { data: apiKeys, isLoading, isError, error } = useApiKeys()
  const createApiKey = useCreateApiKey()
  const revokeApiKey = useRevokeApiKey()
  const { showToast } = useToast()

  const [formOpen, setFormOpen] = useState(false)
  const [name, setName] = useState('')
  const [expiresAt, setExpiresAt] = useState('')
  // The raw secret only ever lives here, transiently, for the one render
  // after creation — never persisted, never sent back to the query cache.
  const [created, setCreated] = useState<ApiKeyCreated | null>(null)
  const [revokingIds, setRevokingIds] = useState<Set<string>>(new Set())

  const closeForm = useCallback(() => {
    setFormOpen(false)
    setName('')
    setExpiresAt('')
  }, [])

  const dismissSecret = useCallback(() => {
    setCreated(null)
  }, [])

  const canSubmit = name.trim() !== '' && !createApiKey.isPending

  const handleCreate = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      if (!canSubmit) return
      const trimmedName = name.trim()
      const isoExpiry = expiresAt !== '' ? new Date(expiresAt).toISOString() : undefined
      try {
        const result = await createApiKey.mutateAsync({
          name: trimmedName,
          ...(isoExpiry !== undefined ? { expires_at: isoExpiry } : {}),
        })
        setCreated(result)
        closeForm()
      } catch (err) {
        showToast(err instanceof Error ? err.message : 'Failed to create API key', 'error')
      }
    },
    [canSubmit, name, expiresAt, createApiKey, closeForm, showToast],
  )

  const handleRevoke = useCallback(
    async (id: string, keyName: string) => {
      if (!window.confirm(`Revoke API key "${keyName}"? This cannot be undone.`)) return
      setRevokingIds((s) => new Set(s).add(id))
      try {
        await revokeApiKey.mutateAsync(id)
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
    [revokeApiKey, showToast],
  )

  const rows = apiKeys ?? []

  return (
    <div className={listStyles.page}>
      <PageHeader
        title="API Keys"
        backTo="/admin"
        subtitle={isLoading ? 'Loading…' : `${rows.length} keys`}
        action={
          <button
            type="button"
            className={listStyles.newBtn}
            onClick={() => setFormOpen((open) => !open)}
          >
            + New key
          </button>
        }
      />

      {isError && (
        <ErrorBanner>
          Failed to load API keys: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}

      {created && (
        <div className={styles.secretCallout} role="alert">
          <p className={styles.secretWarning}>
            Copy this key now — you won&rsquo;t be able to see it again.
          </p>
          <div className={styles.secretValue}>
            <CopyButton value={created.secret} />
          </div>
          <button type="button" className={styles.dismissBtn} onClick={dismissSecret}>
            Done
          </button>
        </div>
      )}

      {formOpen && (
        <form className={styles.form} onSubmit={(e) => void handleCreate(e)} noValidate>
          <div className={styles.field}>
            <label htmlFor="apikey-name" className={styles.label}>
              Name
            </label>
            <input
              id="apikey-name"
              className={styles.input}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
              aria-required="true"
            />
          </div>

          <div className={styles.field}>
            <label htmlFor="apikey-expires" className={styles.label}>
              Expires (optional)
            </label>
            <input
              id="apikey-expires"
              type="date"
              className={styles.input}
              value={expiresAt}
              onChange={(e) => setExpiresAt(e.target.value)}
            />
          </div>

          <div className={styles.footer}>
            <button type="submit" className={styles.submitBtn} disabled={!canSubmit}>
              Create key
            </button>
            <button type="button" className={styles.cancelBtn} onClick={closeForm}>
              Cancel
            </button>
          </div>
        </form>
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
            {!isLoading && rows.length === 0 && (
              <tr className={listStyles.emptyRow}>
                <td colSpan={6}>No API keys yet. Create one for headless SDK/submitter access.</td>
              </tr>
            )}
            {rows.map((key) => (
              <tr key={key.id}>
                <td>{key.name}</td>
                <td>
                  <code className={styles.prefix}>{key.prefix}</code>
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
