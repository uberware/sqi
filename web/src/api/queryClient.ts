// SPDX-License-Identifier: AGPL-3.0-or-later

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
    onError: (error, query) => {
      // A 401 from *any* query means the session is gone (expired, revoked,
      // never existed). Invalidating auth.me here — rather than requiring
      // every call site to notice — makes the AuthProvider re-resolve to
      // 'anon' and the app fall back to the login page, no matter which
      // query surfaced the 401. Skip when the failing query IS auth.me
      // itself: its own query state already carries the fresh error, and
      // re-invalidating it here would just queue an immediate, redundant
      // refetch of a request that's already known to fail.
      //
      // `queryClient` isn't assigned yet at the point this closure is
      // *defined* (it's still inside its own initializer), but it is always
      // assigned by the time the closure actually *runs* — onError only
      // fires for a query that failed after the client exists — so this
      // self-reference is safe.
      const isAuthMeQuery = query.queryKey[0] === 'auth' && query.queryKey[1] === 'me'
      if (error instanceof ApiError && error.status === 401 && !isAuthMeQuery) {
        void queryClient.invalidateQueries({ queryKey: ['auth', 'me'] })
      }
      console.error('[sqi] query error', error)
    },
  }),
})
