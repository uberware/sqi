// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { parseOptionalInt } from './parse'

describe('parseOptionalInt', () => {
  it('returns undefined for an empty string', () => {
    expect(parseOptionalInt('')).toBeUndefined()
  })

  it('returns undefined for whitespace-only input', () => {
    expect(parseOptionalInt('   ')).toBeUndefined()
  })

  it('parses a plain integer', () => {
    expect(parseOptionalInt('5')).toBe(5)
  })

  it('parses zero', () => {
    expect(parseOptionalInt('0')).toBe(0)
  })

  it('trims surrounding whitespace', () => {
    expect(parseOptionalInt(' 7 ')).toBe(7)
  })

  it('truncates decimals toward zero', () => {
    expect(parseOptionalInt('5.9')).toBe(5)
    expect(parseOptionalInt('-3.7')).toBe(-3)
  })

  it('parses exponent notation', () => {
    expect(parseOptionalInt('1e3')).toBe(1000)
  })

  it('returns undefined for non-numeric input', () => {
    expect(parseOptionalInt('abc')).toBeUndefined()
  })

  it('returns undefined for non-finite input', () => {
    expect(parseOptionalInt('Infinity')).toBeUndefined()
    expect(parseOptionalInt('NaN')).toBeUndefined()
  })
})
