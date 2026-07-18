// SPDX-License-Identifier: AGPL-3.0-or-later

// Client-side mirror of the server authorization matrix
// (internal/auth/policy/policy.go). Used to filter nav, gate routes, and hide
// mutating controls. Keep in lockstep with the Go grants map and docs/auth.md.

import type { Principal } from '@/api/types'

export type Permission =
  | 'jobs.read'
  | 'jobs.write'
  | 'workers.read'
  | 'workers.manage'
  | 'infra.read'
  | 'infra.manage'
  | 'products.read'
  | 'products.manage'
  | 'diagnostics.read'
  | 'users.read'
  | 'users.manage'
  | 'apikeys.self'
  | 'apikeys.admin'

const GRANTS: Record<string, ReadonlySet<Permission>> = {
  'read-only': new Set(['jobs.read', 'workers.read', 'infra.read', 'products.read', 'apikeys.self']),
  user: new Set(['jobs.read', 'jobs.write', 'workers.read', 'infra.read', 'products.read', 'apikeys.self']),
  operator: new Set([
    'jobs.read', 'jobs.write', 'workers.read', 'workers.manage', 'infra.read',
    'infra.manage', 'products.read', 'products.manage', 'diagnostics.read', 'apikeys.self',
  ]),
  admin: new Set([
    'jobs.read', 'jobs.write', 'workers.read', 'workers.manage', 'infra.read',
    'infra.manage', 'products.read', 'products.manage', 'diagnostics.read',
    'users.read', 'users.manage', 'apikeys.self', 'apikeys.admin',
  ]),
}

/**
 * Reports whether principal may perform perm. The anonymous principal returned
 * by an auth-disabled server (`kind === 'anonymous'`) is allowed everything, so
 * gating vanishes exactly as it should when auth is off. A null principal
 * (identity not yet resolved) is denied.
 */
export function can(principal: Principal | null, perm: Permission): boolean {
  if (!principal) return false
  if (principal.kind === 'anonymous') return true
  return principal.roles.some((role) => GRANTS[role]?.has(perm) ?? false)
}
