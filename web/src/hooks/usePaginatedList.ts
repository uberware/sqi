// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import type { QueryKey } from '@tanstack/react-query'
import type { ListResponse } from '@/api/types'

export type PaginationParams = {
  limit: number
  offset: number
}

export type UsePaginatedListResult<T> = {
  items: T[]
  total: number
  page: number
  pageSize: number
  totalPages: number
  hasNextPage: boolean
  hasPrevPage: boolean
  isLoading: boolean
  isError: boolean
  error: Error | null
  goToPage: (page: number) => void
  goToNextPage: () => void
  goToPrevPage: () => void
  setPageSize: (size: number) => void
}

const PAGE_PARAM = 'page'
const PAGE_SIZE_PARAM = 'page_size'
const DEFAULT_PAGE_SIZE = 50

/**
 * Wraps a paginated list query with URL-driven page and pageSize state.
 *
 * Page and pageSize are stored in the URL search params so the active view is
 * preserved on reload and can be shared via URL. Requires React Router context.
 *
 * @param queryKey - TanStack Query key array; current page/pageSize is appended
 *   so each page has its own cache entry.
 * @param queryFn - Function receiving pagination params and returning a
 *   {@link ListResponse}.
 * @param defaultPageSize - Page size used when the URL param is absent.
 */
export function usePaginatedList<T>(
  queryKey: QueryKey,
  queryFn: (params: PaginationParams) => Promise<ListResponse<T>>,
  defaultPageSize = DEFAULT_PAGE_SIZE,
): UsePaginatedListResult<T> {
  const [searchParams, setSearchParams] = useSearchParams()

  const page = Math.max(1, parseInt(searchParams.get(PAGE_PARAM) ?? '1', 10) || 1)
  const pageSize = Math.max(
    1,
    parseInt(searchParams.get(PAGE_SIZE_PARAM) ?? String(defaultPageSize), 10) || defaultPageSize,
  )
  const offset = (page - 1) * pageSize

  // QueryKey in TanStack Query v5 is always readonly unknown[].
  const result = useQuery({
    queryKey: [...(queryKey as unknown[]), { page, pageSize }],
    queryFn: () => queryFn({ limit: pageSize, offset }),
  })

  const items = result.data?.items ?? []
  const total = result.data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))

  const goToPage = useCallback(
    (newPage: number): void => {
      const clamped = Math.max(1, Math.min(newPage, totalPages))
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        next.set(PAGE_PARAM, String(clamped))
        return next
      })
    },
    [totalPages, setSearchParams],
  )

  const setPageSize = useCallback(
    (size: number): void => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        next.set(PAGE_SIZE_PARAM, String(Math.max(1, size)))
        next.set(PAGE_PARAM, '1') // reset to first page on page size change
        return next
      })
    },
    [setSearchParams],
  )

  const goToNextPage = useCallback(() => goToPage(page + 1), [goToPage, page])
  const goToPrevPage = useCallback(() => goToPage(page - 1), [goToPage, page])

  return {
    items,
    total,
    page,
    pageSize,
    totalPages,
    hasNextPage: page < totalPages,
    hasPrevPage: page > 1,
    isLoading: result.isLoading,
    isError: result.isError,
    error: result.error,
    goToPage,
    goToNextPage,
    goToPrevPage,
    setPageSize,
  }
}
