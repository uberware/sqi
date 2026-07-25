// SPDX-License-Identifier: AGPL-3.0-or-later

// Client-side gating on the permissions GET /auth/me reports. Used to filter
// nav, gate routes, and hide mutating controls.
//
// The authorization matrix itself lives only on the server
// (internal/auth/policy/policy.go); this file deliberately does NOT mirror it.
// The names below are the permission vocabulary, kept as a union so call sites
// are typo-checked at compile time — not a second copy of who-gets-what.

import type { Principal } from '@/api/types'

export type Permission =
  | 'jobs.read'
  | 'jobs.read.all'
  | 'jobs.write'
  | 'jobs.submit_as'
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
  | 'isolation.manage'

// PERMISSION_SET exists only for compile-time exhaustiveness and to give a
// test something with a runtime representation to check: the Permission
// union above has none. Record<Permission, true> requires an object literal
// assigned to it to have EXACTLY one key per union member — add a member to
// Permission without adding a matching key here (or vice versa) and this
// file fails to typecheck, so the two cannot silently drift apart from each
// other. policy.test.ts separately pins ALL_PERMISSIONS (derived from this)
// against a hand-mirrored copy of internal/auth/policy/policy.go's grants, so
// a permission added on the server without ever reaching this file also
// fails a test, not just silently working with no client-side gate.
const PERMISSION_SET: Record<Permission, true> = {
  'jobs.read': true,
  'jobs.read.all': true,
  'jobs.write': true,
  'jobs.submit_as': true,
  'workers.read': true,
  'workers.manage': true,
  'infra.read': true,
  'infra.manage': true,
  'products.read': true,
  'products.manage': true,
  'diagnostics.read': true,
  'users.read': true,
  'users.manage': true,
  'apikeys.self': true,
  'apikeys.admin': true,
  'isolation.manage': true,
}

/** Every permission the server can grant, sorted. See PERMISSION_SET. */
export const ALL_PERMISSIONS: readonly Permission[] = (
  Object.keys(PERMISSION_SET) as Permission[]
).sort()

/**
 * Reports whether principal may perform perm, per the effective permissions the
 * server resolved for it. A null principal (identity not yet resolved) is
 * denied.
 *
 * There is no `kind === 'anonymous'` special case: with auth disabled the
 * server sends a Superuser principal whose permission list is the full set, so
 * gating vanishes when auth is off without the client deciding that for itself.
 *
 * A principal with no `permissions` array at all — a server predating the field
 * — denies rather than throwing, so a version skew degrades to a read-only-
 * looking UI instead of crashing the nav that renders it.
 */
export function can(principal: Principal | null, perm: Permission): boolean {
  if (!principal || !Array.isArray(principal.permissions)) return false
  return principal.permissions.includes(perm)
}
