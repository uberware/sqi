// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useLiveNow } from './useLiveNow'

function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    get: () => state,
  })
  document.dispatchEvent(new Event('visibilitychange'))
}

beforeEach(() => {
  vi.useFakeTimers()
  // jsdom defaults to 'visible'; make it explicit and resettable.
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    get: () => 'visible',
  })
})
afterEach(() => {
  vi.useRealTimers()
})

describe('useLiveNow', () => {
  it('advances about once per second when active', () => {
    const { result } = renderHook(() => useLiveNow(true))
    const start = result.current
    act(() => vi.advanceTimersByTime(1_000))
    expect(result.current).toBeGreaterThanOrEqual(start + 1_000)
  })

  it('does not advance within a second when inactive', () => {
    const { result } = renderHook(() => useLiveNow(false))
    const start = result.current
    act(() => vi.advanceTimersByTime(1_000))
    expect(result.current).toBe(start)
  })

  it('advances after 30 seconds when inactive', () => {
    const { result } = renderHook(() => useLiveNow(false))
    const start = result.current
    act(() => vi.advanceTimersByTime(30_000))
    expect(result.current).toBeGreaterThanOrEqual(start + 30_000)
  })

  it('does not advance while the tab is hidden', () => {
    setVisibility('hidden')
    const { result } = renderHook(() => useLiveNow(true))
    const start = result.current
    act(() => vi.advanceTimersByTime(5_000))
    expect(result.current).toBe(start)
  })

  it('snaps to current time and resumes when the tab becomes visible', () => {
    setVisibility('hidden')
    const { result } = renderHook(() => useLiveNow(true))
    const start = result.current
    act(() => vi.advanceTimersByTime(10_000))
    expect(result.current).toBe(start)
    act(() => setVisibility('visible'))
    expect(result.current).toBeGreaterThanOrEqual(start + 10_000)
  })

  // eslint-disable-next-line vitest/expect-expect
  it('clears the interval on unmount', () => {
    const { unmount } = renderHook(() => useLiveNow(true))
    unmount()
    // Advancing after unmount must not throw a setState-after-unmount warning.
    act(() => vi.advanceTimersByTime(5_000))
  })
})
