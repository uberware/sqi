// SPDX-License-Identifier: AGPL-3.0-or-later
/* eslint-disable react-refresh/only-export-components -- context files export both provider and hooks */

import { createContext, useContext, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useAuthMe, queryKeys } from '@/api/queries'
import { ApiError } from '@/api/client'
import type { Principal } from '@/api/types'

export type AuthStatus = 'loading' | 'authed' | 'anon'

interface AuthContextValue {
  /** The current principal once resolved; null while loading or anonymous/unauthenticated. */
  principal: Principal | null
  status: AuthStatus
  /** Force GET /auth/me to re-run (e.g. right after a successful login). */
  refresh: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

/**
 * Resolves the current authentication state from `GET /auth/me` — the one
 * signal that drives the whole web UI's auth gating. No token is stored
 * anywhere client-side: the session lives in an HttpOnly cookie the browser
 * attaches automatically, and identity is derived fresh from this endpoint.
 *
 * `status` is `'anon'` on a 401 (no/invalid session) and also, deliberately,
 * on any other error — a network failure or 5xx should not leave the app
 * stuck rendering the shell against an unconfirmed identity. When auth is
 * disabled server-side, `/auth/me` returns 200 with an anonymous principal
 * (`kind: 'anonymous'`), which resolves to `'authed'` here just like any
 * other principal — this is what makes the login page never appear in that
 * mode without the web needing a separate "is auth enabled?" flag.
 */
export function AuthProvider({ children }: { children: ReactNode }) {
  const qc = useQueryClient()
  const { data, isLoading, error } = useAuthMe()

  let status: AuthStatus
  if (error instanceof ApiError && error.status === 401) {
    // Checked ahead of `data`: a background refetch (e.g. after the global
    // 401 handler invalidates this query) can fail while React Query still
    // holds the previous successful result, so a stale `data` must not mask
    // a fresh 401.
    status = 'anon'
  } else if (data) {
    status = 'authed'
  } else if (!isLoading && error) {
    status = 'anon'
  } else {
    status = 'loading'
  }

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: queryKeys.auth.me })
  }

  return (
    <AuthContext.Provider value={{ principal: data ?? null, status, refresh }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (ctx === null) {
    throw new Error('useAuth must be used within an AuthProvider')
  }
  return ctx
}
