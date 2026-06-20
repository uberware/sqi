// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { formatDuration, formatTimespan, formatUptime } from './time'

describe('formatDuration', () => {
  it('returns — for negative durations', () => {
    expect(formatDuration(-1)).toBe('—')
  })
  it('formats seconds', () => {
    expect(formatDuration(45_000)).toBe('45s')
  })
  it('formats minutes and seconds', () => {
    expect(formatDuration(90_000)).toBe('1m 30s')
  })
  it('formats hours and minutes', () => {
    expect(formatDuration(2 * 3_600_000 + 5 * 60_000)).toBe('2h 5m')
  })
})

describe('formatTimespan', () => {
  const start = '2026-06-19T00:00:00.000Z'
  const now = Date.parse('2026-06-19T00:01:30.000Z')

  it('returns — when there is no start timestamp', () => {
    expect(formatTimespan(undefined, undefined, now)).toBe('—')
  })
  it('measures from start to now while in progress', () => {
    expect(formatTimespan(start, undefined, now)).toBe('1m 30s')
  })
  it('measures from start to end for a terminal span, ignoring now', () => {
    expect(formatTimespan(start, '2026-06-19T00:00:10.000Z', now)).toBe('10s')
  })
  it('returns — when the computed duration is negative', () => {
    const earlier = Date.parse('2026-06-18T23:59:00.000Z')
    expect(formatTimespan(start, undefined, earlier)).toBe('—')
  })
})

describe('formatUptime', () => {
  const base = Date.parse('2026-06-19T00:00:00.000Z')
  const reg = '2026-06-19T00:00:00.000Z'

  it('returns — for a future registration time', () => {
    expect(formatUptime(reg, base - 1_000)).toBe('—')
  })
  it('formats seconds', () => {
    expect(formatUptime(reg, base + 30_000)).toBe('30s')
  })
  it('formats whole minutes', () => {
    expect(formatUptime(reg, base + 5 * 60_000)).toBe('5m')
  })
  it('formats hours and minutes', () => {
    expect(formatUptime(reg, base + (2 * 3_600_000 + 5 * 60_000))).toBe('2h 5m')
  })
  it('formats days and hours', () => {
    expect(formatUptime(reg, base + 26 * 3_600_000)).toBe('1d 2h')
  })
})
