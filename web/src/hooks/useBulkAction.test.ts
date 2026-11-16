// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useBulkAction } from './useBulkAction'

type Verb = 'Cancel' | 'Retry'

function failFor(...failing: string[]) {
  return (id: string) =>
    failing.includes(id) ? Promise.reject(new Error(`no ${id}`)) : Promise.resolve()
}

describe('useBulkAction', () => {
  it('runs the action for every id in order and settles each one', async () => {
    const onSettled = vi.fn()
    const seen: string[] = []
    const { result } = renderHook(() => useBulkAction<Verb>('jobs', onSettled))

    await act(() =>
      result.current.run('Cancel', ['a', 'b', 'c'], (id) => {
        seen.push(id)
        return Promise.resolve()
      }),
    )

    expect(seen).toEqual(['a', 'b', 'c'])
    expect(onSettled.mock.calls).toEqual([['a'], ['b'], ['c']])
    expect(result.current.errors.size).toBe(0)
  })

  it('keeps going past a failure and reports it even when a later id succeeds', async () => {
    const onSettled = vi.fn()
    const { result } = renderHook(() => useBulkAction<Verb>('jobs', onSettled))

    await act(() => result.current.run('Cancel', ['a', 'b', 'c'], failFor('b')))

    expect(onSettled).toHaveBeenCalledTimes(3)
    expect([...result.current.errors]).toEqual([['Cancel', 'Cancel failed for 1 of 3 jobs: no b']])
  })

  it('reports the last failure message when several fail', async () => {
    const { result } = renderHook(() => useBulkAction<Verb>('jobs'))

    await act(() => result.current.run('Cancel', ['a', 'b', 'c'], failFor('a', 'c')))

    expect(result.current.errors.get('Cancel')).toBe('Cancel failed for 2 of 3 jobs: no c')
  })

  it('reports a single-id failure without a count', async () => {
    const { result } = renderHook(() => useBulkAction<Verb>('jobs'))

    await act(() => result.current.run('Cancel', ['a'], failFor('a')))

    expect(result.current.errors.get('Cancel')).toBe('Cancel failed: no a')
  })

  it('reports a non-Error rejection as an unknown error', async () => {
    const { result } = renderHook(() => useBulkAction<Verb>('jobs'))

    await act(() =>
      result.current.run('Cancel', ['a'], () => Promise.reject(new Error('x').message)),
    )

    expect(result.current.errors.get('Cancel')).toBe('Cancel failed: Unknown error')
  })

  it("clears a verb's failure on its next run and leaves other verbs' failures", async () => {
    const { result } = renderHook(() => useBulkAction<Verb>('jobs'))

    await act(() => result.current.run('Cancel', ['a'], failFor('a')))
    await act(() => result.current.run('Retry', ['a'], failFor('a')))
    await act(() => result.current.run('Cancel', ['a'], failFor()))

    expect([...result.current.errors]).toEqual([['Retry', 'Retry failed: no a']])
  })

  it('marks an id busy and its verb running only while its action is in flight', async () => {
    let release = () => {}
    const { result } = renderHook(() => useBulkAction<Verb>('jobs'))

    let done: Promise<void> = Promise.resolve()
    act(() => {
      done = result.current.run(
        'Retry',
        ['a'],
        () => new Promise<void>((resolve) => (release = resolve)),
      )
    })

    expect(result.current.isBusy('Retry', 'a')).toBe(true)
    expect(result.current.isBusy('Cancel', 'a')).toBe(false)
    expect(result.current.isRunning('Retry')).toBe(true)
    expect(result.current.isRunning('Cancel')).toBe(false)

    await act(async () => {
      release()
      await done
    })

    expect(result.current.isBusy('Retry', 'a')).toBe(false)
    expect(result.current.isRunning('Retry')).toBe(false)
  })
})
