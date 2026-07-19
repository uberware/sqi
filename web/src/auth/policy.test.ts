// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { can, type Permission } from './policy'
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
  // The client used to re-derive permissions from `roles` via a hand-maintained
  // mirror of the Go grants matrix. It now trusts GET /auth/me's `permissions`,
  // so the matrix lives in exactly one place (internal/auth/policy/policy.go).
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
    // Guards the A0/B1 mismatch: the client keyed its auth-off bypass off
    // `kind === 'anonymous'` while the server keys it off `Principal.Superuser`.
    // Trusting the permission list removes the second source of truth.
    const anonWithoutPerms = principal([], { roles: [], kind: 'anonymous' })
    expect(can(anonWithoutPerms, 'users.manage')).toBe(false)
  })
})
