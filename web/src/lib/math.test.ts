// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { clamp } from './math'

describe('clamp', () => {
  it('returns the value when it is in range', () => {
    expect(clamp(5, 0, 10)).toBe(5)
  })

  it('limits to the lower and upper bounds', () => {
    expect(clamp(-3, 0, 10)).toBe(0)
    expect(clamp(42, 0, 10)).toBe(10)
  })
})
