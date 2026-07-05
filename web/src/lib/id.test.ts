// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { truncateId } from './id'

describe('truncateId', () => {
  it('truncates long ids to eight characters', () => {
    expect(truncateId('0123456789abcdef')).toBe('01234567')
  })

  it('returns short ids unchanged', () => {
    expect(truncateId('abc123')).toBe('abc123')
  })

  it('returns an exactly-eight-character id unchanged', () => {
    expect(truncateId('12345678')).toBe('12345678')
  })
})
