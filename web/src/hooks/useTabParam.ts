// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback } from 'react'
import { useSearchParams } from 'react-router'

const TAB_PARAM = 'tab'

export interface UseTabParamResult {
  tab: string
  setTab: (value: string) => void
}

/**
 * URL-persisted `?tab=` selection for pages with a tabbed region — the cousin
 * of useSearchParam for tab state.
 *
 * Living in the URL is what lets a list row deep-link straight to a specific
 * tab, and keeps the browser Back button stepping through tab changes.
 *
 * A param naming a tab outside `valid` falls back to `fallback` rather than
 * selecting nothing, so a stale link cannot render a page with no panel.
 * Selecting `fallback` removes the param instead of pinning it, keeping shared
 * URLs clean.
 */
export function useTabParam(valid: readonly string[], fallback: string): UseTabParamResult {
  const [searchParams, setSearchParams] = useSearchParams()
  const raw = searchParams.get(TAB_PARAM)
  const tab = raw !== null && valid.includes(raw) ? raw : fallback

  const setTab = useCallback(
    (value: string) =>
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        if (value === fallback) next.delete(TAB_PARAM)
        else next.set(TAB_PARAM, value)
        return next
      }),
    [setSearchParams, fallback],
  )

  return { tab, setTab }
}
