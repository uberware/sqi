// SPDX-License-Identifier: AGPL-3.0-or-later

import type { QueryClient } from '@tanstack/react-query'
import { queryKeys } from './queries'

/**
 * Evict every cached query except `auth.me`, then invalidate `auth.me` so the
 * AuthProvider re-resolves.
 *
 * This is the shared eviction used by **both** paths that transition the app
 * to anon:
 *
 *  1. Explicit logout — `useLogout` (`mutations.ts`) calls this from its
 *     `onSuccess`.
 *  2. Session expiry / revocation — the `QueryCache.onError` handler
 *     (`queryClient.ts`) calls this when any query fails with a 401 that
 *     isn't `auth.me` itself.
 *
 * Query keys in this app carry no principal, and the `QueryClient` is a
 * singleton shared for the whole SPA lifetime. Evicting is therefore a
 * security requirement, not tidiness: anything the outgoing user browsed
 * would otherwise stay cached and be served — stale but visible — to
 * whoever logs in next on the same browser, regardless of which of the two
 * paths above triggered the transition.
 *
 * `auth.me` is deliberately spared rather than swept up in a blanket
 * `clear()` — this was verified against TanStack query-core source:
 * `removeQueries` destroys the Query object without notifying observers, so
 * removing `auth.me` too would leave the `AuthProvider`'s observer pointing
 * at a destroyed query with nothing to trigger a refetch, stranding it
 * pending instead of refetching into the 401 that flips the app to the
 * login page.
 */
export function evictAllExceptAuthMe(qc: QueryClient): void {
  qc.removeQueries({
    predicate: (q) => !(q.queryKey[0] === 'auth' && q.queryKey[1] === 'me'),
  })
  void qc.invalidateQueries({ queryKey: queryKeys.auth.me })
}
