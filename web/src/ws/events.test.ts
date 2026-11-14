// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { isStaleEvent } from './events'

describe('isStaleEvent', () => {
  const current = { updated_at: '2024-01-01T10:05:00Z' }

  it.each([
    ['older than the held state', '2024-01-01T10:01:00Z', true],
    ['at the same instant', '2024-01-01T10:05:00Z', false],
    ['newer than the held state', '2024-01-01T10:06:00Z', false],
    ['sub-second older', '2024-01-01T10:04:59.999Z', true],
    ['unparseable', 'not-a-time', false],
  ])('event %s (%s) is stale: %s', (_label, eventAt, want) => {
    expect(isStaleEvent({ updated_at: eventAt }, current)).toBe(want)
  })

  it('is never stale when the held timestamp is unparseable', () => {
    expect(isStaleEvent({ updated_at: '2024-01-01T10:01:00Z' }, { updated_at: '' })).toBe(false)
  })
})
