// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { can, type Permission } from './policy'
import type { Principal } from '@/api/types'

function principal(roles: string[], kind = 'user'): Principal {
  return { subject: 's', display_name: 'n', roles, kind } as Principal
}

const cases: Array<[string, Permission, boolean]> = [
  ['read-only', 'jobs.read', true],
  ['read-only', 'jobs.write', false],
  ['read-only', 'diagnostics.read', false],
  ['read-only', 'apikeys.self', true],
  ['user', 'jobs.write', true],
  ['user', 'users.read', false],
  ['operator', 'infra.manage', true],
  ['operator', 'users.manage', false],
  ['admin', 'users.manage', true],
  ['admin', 'apikeys.admin', true],
]

describe('can', () => {
  it.each(cases)('%s → %s = %s', (role, perm, expected) => {
    expect(can(principal([role]), perm)).toBe(expected)
  })

  it('anonymous principal is allowed everything (auth-off)', () => {
    const anon = principal([], 'anonymous')
    expect(can(anon, 'users.manage')).toBe(true)
    expect(can(anon, 'jobs.write')).toBe(true)
  })

  it('null principal (unresolved) denies', () => {
    expect(can(null, 'jobs.read')).toBe(false)
  })
})
