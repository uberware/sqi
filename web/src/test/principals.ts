// SPDX-License-Identifier: AGPL-3.0-or-later

// Canned GET /auth/me principals for component tests.
//
// Production gating reads `permissions` straight off the server response
// (`@/auth/policy`), so these are *fixtures standing in for a server reply* —
// not a policy mirror the app consults. Nothing here is imported by src code.
// If the server matrix changes, update these lists so tests keep describing a
// real deployment; the app itself needs no change.

import { vi } from 'vitest'
import { useAuth } from '@/auth/context'
import type { Principal } from '@/api/types'
import type { Permission } from '@/auth/policy'

export const READ_ONLY_PERMISSIONS: Permission[] = [
  'jobs.read',
  'jobs.read.all',
  'workers.read',
  'infra.read',
  'products.read',
  'apikeys.self',
]

export const USER_PERMISSIONS: Permission[] = [
  'jobs.read',
  'jobs.write',
  'workers.read',
  'infra.read',
  'products.read',
  'apikeys.self',
]

export const OPERATOR_PERMISSIONS: Permission[] = [
  'jobs.read',
  'jobs.read.all',
  'jobs.write',
  'jobs.submit_as',
  'workers.read',
  'workers.manage',
  'infra.read',
  'infra.manage',
  'products.read',
  'products.manage',
  'diagnostics.read',
  'apikeys.self',
]

export const ADMIN_PERMISSIONS: Permission[] = [
  ...OPERATOR_PERMISSIONS,
  'users.read',
  'users.manage',
  'apikeys.admin',
]

/** Every permission — what an auth-disabled server reports for its superuser. */
export const ALL_PERMISSIONS: Permission[] = [...ADMIN_PERMISSIONS]

function make(
  subject: string,
  displayName: string,
  role: string,
  permissions: Permission[],
  kind = 'user',
): Principal {
  return {
    subject,
    display_name: displayName,
    roles: role ? [role] : [],
    kind,
    permissions,
  }
}

export const readOnlyPrincipal = (): Principal =>
  make('u-readonly', 'Read Only', 'read-only', READ_ONLY_PERMISSIONS)

export const userPrincipal = (): Principal => make('u-user', 'User', 'user', USER_PERMISSIONS)

export const operatorPrincipal = (): Principal =>
  make('u-operator', 'Operator', 'operator', OPERATOR_PERMISSIONS)

export const adminPrincipal = (): Principal => make('u-admin', 'Admin', 'admin', ADMIN_PERMISSIONS)

/** Auth disabled: a superuser principal carrying the full permission set. */
export const anonymousPrincipal = (): Principal =>
  make('anonymous', 'Anonymous', '', ALL_PERMISSIONS, 'anonymous')

/**
 * Points the mocked `useAuth` at `principal`. Test files must still declare
 * `vi.mock('@/auth/context', () => ({ useAuth: vi.fn() }))` at module scope —
 * only that hoisted call can replace the module — but the cast-heavy return
 * shape lives here so adding a field to the auth context is a one-file change
 * rather than a sweep across every page test.
 */
export function mockAuth(principal: Principal | null): void {
  vi.mocked(useAuth).mockReturnValue({
    principal,
    status: principal === null ? 'anon' : 'authed',
    refresh: vi.fn(),
  } as unknown as ReturnType<typeof useAuth>)
}
