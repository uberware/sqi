// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import { useAuth } from '@/auth/context'
import { can, type Permission } from '@/auth/policy'
import PageHeader from '@/components/PageHeader'

/**
 * Route/section guard: renders children only if the current principal holds
 * `permission`, otherwise a friendly 403. Used to wrap admin-only routes so
 * deep-linking (e.g. /users as a plain user) shows a clean message rather than
 * a broken fetch. With auth disabled the anonymous principal passes everything.
 */
export default function RequireRole({
  permission,
  children,
}: {
  permission: Permission
  children: ReactNode
}) {
  const { principal } = useAuth()
  if (can(principal, permission)) return <>{children}</>
  return (
    <div>
      <PageHeader title="Not authorized" />
      <p>You do not have permission to view this page.</p>
    </div>
  )
}
