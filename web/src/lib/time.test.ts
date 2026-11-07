// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import {
  countDownSeconds,
  etaMs,
  formatAge,
  formatClock,
  formatDurationInput,
  formatDuration,
  formatTimespan,
  formatUptime,
  parseDurationInput,
  timeLeftMs,
} from './time'

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

describe('formatAge', () => {
  const now = Date.parse('2026-06-19T12:00:00.000Z')

  it('returns — for a zero timestamp', () => {
    expect(formatAge(0, now)).toBe('—')
  })
  it('returns "just now" under five seconds', () => {
    expect(formatAge(now - 4_000, now)).toBe('just now')
  })
  it('formats seconds', () => {
    expect(formatAge(now - 30_000, now)).toBe('30s ago')
  })
  it('formats minutes', () => {
    expect(formatAge(now - 5 * 60_000, now)).toBe('5m ago')
  })
  it('falls back to a clock time after an hour', () => {
    const ts = now - 2 * 3_600_000
    expect(formatAge(ts, now)).toBe(new Date(ts).toLocaleTimeString())
  })
})

describe('timeLeftMs', () => {
  const now = Date.parse('2024-01-15T10:10:00Z')
  const startAgo = (ms: number): string => new Date(now - ms).toISOString()

  it('is undefined at progress 0', () => {
    expect(timeLeftMs(startAgo(60_000), now, 0)).toBeUndefined()
  })
  it('is undefined before 30s have elapsed', () => {
    expect(timeLeftMs(startAgo(29_000), now, 1)).toBeUndefined()
  })
  it('extrapolates from elapsed time', () => {
    expect(timeLeftMs(startAgo(60_000), now, 50)).toBe(60_000)
    expect(timeLeftMs(startAgo(100_000), now, 25)).toBe(300_000)
  })
  it('is 0 at progress 100', () => {
    expect(timeLeftMs(startAgo(60_000), now, 100)).toBe(0)
  })
  it('is undefined without a start or progress', () => {
    expect(timeLeftMs(undefined, now, 50)).toBeUndefined()
    expect(timeLeftMs(startAgo(60_000), now, undefined)).toBeUndefined()
  })
})

describe('countDownSeconds', () => {
  const t = 1_000_000
  it('subtracts running x elapsed from the remaining work', () => {
    expect(countDownSeconds(600, 2, t, t + 10_000)).toBe(580)
  })
  it('floors at zero', () => {
    expect(countDownSeconds(10, 2, t, t + 60_000)).toBe(0)
  })
  it('counts nothing when now is before the anchor', () => {
    expect(countDownSeconds(600, 2, t, t - 5_000)).toBe(600)
  })
})

describe('etaMs', () => {
  const t = 1_000_000
  it('divides work across running tasks, at least one', () => {
    expect(etaMs(600, 0, t)).toBe(t + 600_000)
    expect(etaMs(600, 2, t)).toBe(t + 300_000)
  })
})

describe('formatClock', () => {
  it('shows HH:MM on the same local day', () => {
    const now = new Date(2026, 10, 3, 9, 0).getTime()
    expect(formatClock(new Date(2026, 10, 3, 14, 5).getTime(), now)).toBe('14:05')
  })
  it('prefixes the weekday on another day', () => {
    const now = new Date(2026, 10, 3, 23, 0).getTime()
    // 2026-11-04 is a Wednesday.
    expect(formatClock(new Date(2026, 10, 4, 2, 40).getTime(), now)).toBe('Wed 02:40')
  })
})

describe('parseDurationInput', () => {
  it.each([
    ['2h', 7200],
    ['1h 30m', 5400],
    ['90m', 5400],
    ['45s', 45],
    ['1h30m', 5400],
    ['1H 30M', 5400],
    [' 2h ', 7200],
  ])('parses %s', (text, want) => {
    expect(parseDurationInput(text)).toBe(want)
  })
  it.each(['90', '0m', '0', '1x', '', 'abc', '1h 1h'])('rejects %j', (text) => {
    expect(parseDurationInput(text)).toBeUndefined()
  })
})

describe('parseDurationInput range', () => {
  it('accepts the largest int32 and rejects beyond it', () => {
    expect(parseDurationInput('2147483647s')).toBe(2147483647)
    expect(parseDurationInput('2147483648s')).toBeUndefined()
    expect(parseDurationInput('99999999999h')).toBeUndefined()
  })
})

describe('formatDurationInput', () => {
  it.each([
    [3661, '1h 1m 1s'],
    [3600, '1h'],
    [59, '59s'],
    [90061, '25h 1m 1s'],
    [5405, '1h 30m 5s'],
  ])('formats %i as %s', (n, want) => {
    expect(formatDurationInput(n)).toBe(want)
  })
  it.each([3661, 90061, 59, 3600, 60, 5405, 1])('round-trips %i exactly', (n) => {
    expect(parseDurationInput(formatDurationInput(n))).toBe(n)
  })
})
