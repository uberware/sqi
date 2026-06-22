// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback } from 'react'
import { useSearchParams } from 'react-router-dom'

export type SortDirection = 'asc' | 'desc'

export const STATUS_PARAM = 'status'
export const SEARCH_PARAM = 'search'
export const SORT_FIELD_PARAM = 'sort'
export const SORT_DIR_PARAM = 'dir'
export const PAGE_PARAM = 'page'
export const SIZE_PARAM = 'size'

const DEFAULT_PAGE_SIZE = 50
const DEFAULT_PAGE_SIZES = [25, 50, 100]

export interface ListFilterConfig<F extends string> {
  statuses: ReadonlySet<string>
  sortFields: ReadonlySet<string>
  defaultSortField: NoInfer<F>
  defaultSortDir: SortDirection
  defaultPageSize?: number
  pageSizes?: number[]
}

export interface UseListFiltersResult<S extends string, F extends string> {
  status: S | ''
  search: string
  sortField: F
  sortDir: SortDirection
  page: number
  pageSize: number
  setStatus: (status: S | '') => void
  setSearch: (search: string) => void
  setSortFieldAndDir: (field: F, dir: SortDirection) => void
  setPage: (page: number) => void
  setPageSize: (size: number) => void
}

export function useListFilters<S extends string, F extends string>(
  config: ListFilterConfig<F>,
): UseListFiltersResult<S, F> {
  const [searchParams, setSearchParams] = useSearchParams()
  const defaultSize = config.defaultPageSize ?? DEFAULT_PAGE_SIZE
  const sizes = config.pageSizes ?? DEFAULT_PAGE_SIZES

  const rawStatus = searchParams.get(STATUS_PARAM)
  const status = (rawStatus && config.statuses.has(rawStatus) ? rawStatus : '') as S | ''
  const search = searchParams.get(SEARCH_PARAM) ?? ''

  const rawSort = searchParams.get(SORT_FIELD_PARAM)
  const sortField = (
    rawSort && config.sortFields.has(rawSort) ? rawSort : config.defaultSortField
  ) as F

  const rawDir = searchParams.get(SORT_DIR_PARAM)
  const sortDir: SortDirection =
    rawDir === 'asc' || rawDir === 'desc' ? rawDir : config.defaultSortDir

  const parsedPage = parseInt(searchParams.get(PAGE_PARAM) ?? '1', 10)
  const page = !isNaN(parsedPage) && parsedPage >= 1 ? parsedPage : 1

  const parsedSize = parseInt(searchParams.get(SIZE_PARAM) ?? '', 10)
  const pageSize = sizes.includes(parsedSize) ? parsedSize : defaultSize

  const mutate = useCallback(
    (fn: (next: URLSearchParams) => void) => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        fn(next)
        return next
      })
    },
    [setSearchParams],
  )

  const setStatus = useCallback(
    (value: S | '') =>
      mutate((next) => {
        if (value === '') next.delete(STATUS_PARAM)
        else next.set(STATUS_PARAM, value)
        next.delete(PAGE_PARAM)
      }),
    [mutate],
  )

  const setSearch = useCallback(
    (value: string) =>
      mutate((next) => {
        if (value === '') next.delete(SEARCH_PARAM)
        else next.set(SEARCH_PARAM, value)
        next.delete(PAGE_PARAM)
      }),
    [mutate],
  )

  const setSortFieldAndDir = useCallback(
    (field: F, dir: SortDirection) =>
      mutate((next) => {
        next.set(SORT_FIELD_PARAM, field)
        next.set(SORT_DIR_PARAM, dir)
      }),
    [mutate],
  )

  const setPage = useCallback(
    (value: number) => mutate((next) => next.set(PAGE_PARAM, String(Math.max(1, value)))),
    [mutate],
  )

  const setPageSize = useCallback(
    (value: number) =>
      mutate((next) => {
        next.set(SIZE_PARAM, String(value))
        next.delete(PAGE_PARAM)
      }),
    [mutate],
  )

  return {
    status,
    search,
    sortField,
    sortDir,
    page,
    pageSize,
    setStatus,
    setSearch,
    setSortFieldAndDir,
    setPage,
    setPageSize,
  }
}
