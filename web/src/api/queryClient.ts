// SPDX-License-Identifier: AGPL-3.0-only

import { QueryCache, QueryClient } from '@tanstack/react-query'
import { ApiError } from './client'

/** Singleton QueryClient shared across the application. */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 10_000, // 10 s before a cached result is re-fetched in the background
      // Only retry server errors (5xx) and network failures, not client errors
      // (4xx) which are deterministic and won't change on a second attempt.
      retry: (count, error) => {
        if (error instanceof ApiError && error.status < 500) return false
        return count < 1
      },
    },
  },
  queryCache: new QueryCache({
    onError: (error) => {
      console.error('[sqi] query error', error)
    },
  }),
})
