// SPDX-License-Identifier: AGPL-3.0-or-later

import { QueryCache, QueryClient } from '@tanstack/react-query'
import { ApiError } from './client'
import { evictAllExceptAuthMe } from './queryEviction'

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
      // never existed). This is one of two paths that evict the cache and
      // transition the app to anon — the other is explicit logout
      // (`useLogout` in `mutations.ts`). Both call the shared
      // `evictAllExceptAuthMe` helper so a session that merely *expires*
      // (rather than being logged out of) gets the same cross-user cache
      // eviction, not just the auth.me invalidation that flips the app to
      // the login page. Skip when the failing query IS auth.me itself: its
      // own query state already carries the fresh error, and evicting here
      // would just queue an immediate, redundant refetch of a request
      // that's already known to fail (and would self-invalidate via its own
      // onError in a loop).
      //
      // `queryClient` isn't assigned yet at the point this closure is
      // *defined* (it's still inside its own initializer), but it is always
      // assigned by the time the closure actually *runs* — onError only
      // fires for a query that failed after the client exists — so this
      // self-reference is safe.
      const isAuthMeQuery = query.queryKey[0] === 'auth' && query.queryKey[1] === 'me'
      if (error instanceof ApiError && error.status === 401 && !isAuthMeQuery) {
        evictAllExceptAuthMe(queryClient)
      }
      console.error('[sqi] query error', error)
    },
  }),
})
