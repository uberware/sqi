// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useDebounce } from './useDebounce'

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('useDebounce', () => {
  it('returns the initial value immediately', () => {
    const { result } = renderHook(() => useDebounce('hello', 300))
    expect(result.current).toBe('hello')
  })

  it('does not update before the delay elapses', () => {
    const { result, rerender } = renderHook(({ value }) => useDebounce(value, 300), {
      initialProps: { value: 'initial' },
    })
    rerender({ value: 'updated' })
    act(() => vi.advanceTimersByTime(299))
    expect(result.current).toBe('initial')
  })

  it('updates after the delay elapses', () => {
    const { result, rerender } = renderHook(({ value }) => useDebounce(value, 300), {
      initialProps: { value: 'initial' },
    })
    rerender({ value: 'updated' })
    act(() => vi.advanceTimersByTime(300))
    expect(result.current).toBe('updated')
  })

  it('resets the timer on rapid value changes', () => {
    const { result, rerender } = renderHook(({ value }) => useDebounce(value, 300), {
      initialProps: { value: 'a' },
    })
    rerender({ value: 'b' })
    act(() => vi.advanceTimersByTime(200))
    rerender({ value: 'c' })
    act(() => vi.advanceTimersByTime(200))
    // Only 200ms have passed since the last change — still debouncing
    expect(result.current).toBe('a')
    act(() => vi.advanceTimersByTime(100))
    expect(result.current).toBe('c')
  })

  it('works with numeric values', () => {
    const { result, rerender } = renderHook(({ value }) => useDebounce(value, 100), {
      initialProps: { value: 0 },
    })
    rerender({ value: 42 })
    act(() => vi.advanceTimersByTime(100))
    expect(result.current).toBe(42)
  })

  it('clears the timer when the component unmounts', () => {
    const { result, rerender, unmount } = renderHook(({ value }) => useDebounce(value, 300), {
      initialProps: { value: 'a' },
    })
    rerender({ value: 'b' })
    unmount()
    // Advancing timers after unmount should not cause setState errors.
    // The debounced value must still be the pre-unmount snapshot 'a'.
    act(() => vi.advanceTimersByTime(300))
    expect(result.current).toBe('a')
  })

  it('delay = 0 updates on the next tick', () => {
    const { result, rerender } = renderHook(({ value }) => useDebounce(value, 0), {
      initialProps: { value: 'initial' },
    })
    rerender({ value: 'instant' })
    act(() => vi.advanceTimersByTime(0))
    expect(result.current).toBe('instant')
  })
})
