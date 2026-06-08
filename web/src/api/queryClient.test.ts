// SPDX-License-Identifier: AGPL-3.0-only

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
    queryClient.getQueryCache().config.onError?.(err, {} as never)
    expect(spy).toHaveBeenCalledWith('[sqi] query error', err)
  })
})
