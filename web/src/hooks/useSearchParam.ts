// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback } from 'react'
import { useSearchParams } from 'react-router-dom'
import { SEARCH_PARAM } from '@/hooks/useListFilters'

export interface UseSearchParamResult {
  search: string
  setSearch: (value: string) => void
}

/**
 * URL-persisted `?search=` value for pages that filter client-side and carry
 * no other list-filter state (status/sort/pagination) — a lightweight cousin
 * of useListFilters for the catalog pages.
 */
export function useSearchParam(): UseSearchParamResult {
  const [searchParams, setSearchParams] = useSearchParams()
  const search = searchParams.get(SEARCH_PARAM) ?? ''

  const setSearch = useCallback(
    (value: string) =>
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        if (value === '') next.delete(SEARCH_PARAM)
        else next.set(SEARCH_PARAM, value)
        return next
      }),
    [setSearchParams],
  )

  return { search, setSearch }
}
