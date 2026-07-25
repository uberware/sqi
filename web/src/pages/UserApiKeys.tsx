// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback } from 'react'
import { useParams } from 'react-router'
import PageHeader from '@/components/PageHeader'
import ErrorBanner from '@/components/ErrorBanner'
import ApiKeyTable from '@/components/ApiKeyTable'
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

  const handleRevoke = useCallback((keyId: string) => revokeKey.mutateAsync(keyId), [revokeKey])

  const rows = keys ?? []

  return (
    <div className={listStyles.page}>
      <PageHeader
        title="API keys"
        backTo="/users"
        backLabel="Users"
        subtitle={isLoading ? 'Loading…' : `${rows.length} keys`}
      />

      {isError && (
        <ErrorBanner>
          Failed to load API keys: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}

      <ApiKeyTable
        keys={rows}
        isLoading={isLoading}
        onRevoke={handleRevoke}
        emptyMessage="This user has no API keys."
      />
    </div>
  )
}
