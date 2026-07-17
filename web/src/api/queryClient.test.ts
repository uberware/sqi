// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, afterEach } from 'vitest'
import { QueryClient } from '@tanstack/react-query'
import { ApiError } from './client'

afterEach(() => {
  vi.restoreAllMocks()
})

async function getRetryFn() {
  const { queryClient } = await import('./queryClient')
  const retry = queryClient.getDefaultOptions().queries?.retry
  if (typeof retry !== 'function') throw new Error('retry should be a function')
  return retry
}

describe('queryClient', () => {
  it('is a QueryClient instance', async () => {
    const { queryClient } = await import('./queryClient')
    expect(queryClient).toBeInstanceOf(QueryClient)
  })

  it('has staleTime of 10 000 ms', async () => {
    const { queryClient } = await import('./queryClient')
    expect(queryClient.getDefaultOptions().queries?.staleTime).toBe(10_000)
  })

  describe('retry', () => {
    it('does not retry 4xx ApiErrors regardless of attempt count', async () => {
      const retry = await getRetryFn()
      const err = new ApiError({
        type: 'about:blank',
        title: 'Not Found',
        status: 404,
        detail: 'not found',
      })
      expect(retry(0, err)).toBe(false)
      expect(retry(1, err)).toBe(false)
    })

    it('retries non-4xx errors on the first attempt (count=0)', async () => {
      const retry = await getRetryFn()
      expect(retry(0, new TypeError('Failed to fetch'))).toBe(true)
    })

    it('does not retry non-4xx errors after the first attempt (count=1)', async () => {
      const retry = await getRetryFn()
      expect(retry(1, new TypeError('Failed to fetch'))).toBe(false)
    })

    it('retries 5xx ApiErrors on the first attempt', async () => {
      const retry = await getRetryFn()
      const err = new ApiError({
        type: 'about:blank',
        title: 'Server Error',
        status: 500,
        detail: 'error',
      })
      expect(retry(0, err)).toBe(true)
    })

    it('does not retry 5xx ApiErrors after the first attempt', async () => {
      const retry = await getRetryFn()
      const err = new ApiError({
        type: 'about:blank',
        title: 'Bad Gateway',
        status: 502,
        detail: 'error',
      })
      expect(retry(1, err)).toBe(false)
    })
  })

  it('logs query errors to console.error via the QueryCache onError handler', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const { queryClient } = await import('./queryClient')
    const err = new Error('boom')
    queryClient.getQueryCache().config.onError?.(err, { queryKey: ['jobs'] } as never)
    expect(spy).toHaveBeenCalledWith('[sqi] query error', err)
  })

  describe('401 handling', () => {
    it('invalidates the auth.me query when any other query fails with a 401 ApiError', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {})
      const { queryClient } = await import('./queryClient')
      const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')
      const err = new ApiError({
        type: 'about:blank',
        title: 'Unauthorized',
        status: 401,
        detail: 'authentication required',
      })
      queryClient.getQueryCache().config.onError?.(err, { queryKey: ['workers', 'list'] } as never)
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['auth', 'me'] })
    })

    it('does not invalidate auth.me for non-401 errors', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {})
      const { queryClient } = await import('./queryClient')
      const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')
      const err = new ApiError({
        type: 'about:blank',
        title: 'Not Found',
        status: 404,
        detail: 'not found',
      })
      queryClient.getQueryCache().config.onError?.(err, { queryKey: ['workers', 'list'] } as never)
      expect(invalidateSpy).not.toHaveBeenCalled()
    })

    it('does not re-invalidate auth.me when the auth.me query itself is the one failing (avoids a redundant refetch loop)', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {})
      const { queryClient } = await import('./queryClient')
      const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')
      const err = new ApiError({
        type: 'about:blank',
        title: 'Unauthorized',
        status: 401,
        detail: 'authentication required',
      })
      queryClient.getQueryCache().config.onError?.(err, { queryKey: ['auth', 'me'] } as never)
      expect(invalidateSpy).not.toHaveBeenCalled()
    })

    it('evicts other cached queries but spares auth.me when a non-auth.me query gets a 401 (session expiry, not explicit logout)', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {})
      const { queryClient } = await import('./queryClient')
      // Data left behind by the previously signed-in user, plus their resolved
      // principal — both must survive differently: the principal is spared so
      // the AuthProvider can refetch it into a 401, everything else is evicted.
      queryClient.setQueryData(['jobs', 'list', {}], [{ id: 'job-from-user-a' }])
      queryClient.setQueryData(['auth', 'me'], { username: 'user-a' })

      const err = new ApiError({
        type: 'about:blank',
        title: 'Unauthorized',
        status: 401,
        detail: 'authentication required',
      })
      // Simulates session expiry: some unrelated query (not auth.me) surfaces
      // the 401, e.g. because the session cookie expired server-side.
      queryClient.getQueryCache().config.onError?.(err, { queryKey: ['workers', 'list'] } as never)

      expect(queryClient.getQueryData(['jobs', 'list', {}])).toBeUndefined()
      expect(queryClient.getQueryData(['auth', 'me'])).toEqual({ username: 'user-a' })
    })
  })
})
