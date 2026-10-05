// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { ALL_PERMISSIONS, can, type Permission } from './policy'
import type { Principal } from '@/api/types'

function principal(permissions: Permission[], overrides: Partial<Principal> = {}): Principal {
  return {
    subject: 's',
    display_name: 'n',
    roles: [],
    kind: 'user',
    permissions,
    ...overrides,
  }
}

describe('can', () => {
  it('grants a permission the server listed', () => {
    expect(can(principal(['jobs.write']), 'jobs.write')).toBe(true)
  })

  it('denies a permission the server withheld', () => {
    expect(can(principal(['jobs.read']), 'jobs.write')).toBe(false)
  })

  it('denies when the server listed no permissions', () => {
    expect(can(principal([]), 'jobs.read')).toBe(false)
  })

  it('null principal (unresolved identity) denies', () => {
    expect(can(null, 'jobs.read')).toBe(false)
  })

  it('denies rather than throwing when permissions is absent (server version skew)', () => {
    const legacy = { subject: 's', display_name: 'n', roles: ['admin'], kind: 'user' } as Principal
    expect(() => can(legacy, 'jobs.read')).not.toThrow()
    expect(can(legacy, 'jobs.read')).toBe(false)
  })
})

describe('server-supplied permissions are the only input', () => {
  // The client never derives permissions from `roles`; it trusts GET
  // /auth/me's `permissions`, so the grants matrix lives in exactly one place
  // (internal/auth/policy/policy.go).
  it('ignores roles entirely — a privileged role with no permissions is denied', () => {
    const admin = principal([], { roles: ['admin'] })
    expect(can(admin, 'users.manage')).toBe(false)
    expect(can(admin, 'jobs.write')).toBe(false)
  })

  it('honours permissions that do not match the role name', () => {
    const oddball = principal(['users.manage'], { roles: ['read-only'] })
    expect(can(oddball, 'users.manage')).toBe(true)
  })
})

describe('permission union stays in lockstep with the server', () => {
  // internal/auth/policy/policy.go's module doc declares a three-way lockstep:
  // the Go Permission constants, this file's union, and docs/auth.md must all
  // agree. A permission missing from the union causes no compile error (an
  // unrelated permission set still typechecks) and no runtime effect until
  // something gates on it. This list is a hand-mirrored copy of every
  // `Permission = "..."` constant in policy.go — keep it in sync by hand
  // whenever that file changes; a permission added to one side and not the
  // other fails this test rather than working with no client-side gate (or, for a
  // Permission-union addition specifically, PERMISSION_SET's
  // Record<Permission, true> in policy.ts already forces the union and
  // ALL_PERMISSIONS to agree with EACH OTHER at compile time — this test is
  // what additionally pins that pair against the actual server list).
  const serverPermissions: Permission[] = [
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
    'users.read',
    'users.manage',
    'apikeys.self',
    'apikeys.admin',
    'isolation.manage',
    'workers.enroll',
  ]

  it('ALL_PERMISSIONS matches the server-declared permission set exactly', () => {
    expect([...ALL_PERMISSIONS].sort()).toEqual([...serverPermissions].sort())
  })
})

describe('auth-off', () => {
  // With auth disabled the server sends a Superuser principal whose permission
  // list is the full set (policy.PermissionsFor), so every control stays
  // enabled without the client special-casing `kind === 'anonymous'`.
  it('anonymous principal carrying the full set is allowed everything', () => {
    const anon = principal(['users.manage', 'jobs.write', 'apikeys.admin'], {
      roles: [],
      kind: 'anonymous',
    })
    expect(can(anon, 'users.manage')).toBe(true)
    expect(can(anon, 'jobs.write')).toBe(true)
  })

  it('does not blanket-allow on kind alone', () => {
    // The server keys its auth-off bypass off `Principal.Superuser`, not off
    // `kind === 'anonymous'`, so the client must not bypass on kind alone.
    // Trusting the permission list avoids a second source of truth.
    const anonWithoutPerms = principal([], { roles: [], kind: 'anonymous' })
    expect(can(anonWithoutPerms, 'users.manage')).toBe(false)
  })
})
