// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback } from 'react'
import { useSearchParams } from 'react-router-dom'
import type { JobStatus } from '@/api/types'

export type JobSortField = 'name' | 'priority' | 'created_at' | 'status'
export type SortDirection = 'asc' | 'desc'

export const JOB_STATUS_PARAM = 'status'
export const JOB_SEARCH_PARAM = 'search'
export const JOB_SORT_FIELD_PARAM = 'sort'
export const JOB_SORT_DIR_PARAM = 'dir'
export const JOB_PAGE_PARAM = 'page'

const DEFAULT_SORT_FIELD: JobSortField = 'created_at'
const DEFAULT_SORT_DIR: SortDirection = 'desc'

const JOB_STATUSES = new Set<string>([
  'pending',
  'running',
  'paused',
  'completed',
  'failed',
  'canceled',
])
const SORT_FIELDS = new Set<string>(['name', 'priority', 'created_at', 'status'])

function parseStatus(raw: string | null): JobStatus | '' {
  if (!raw || !JOB_STATUSES.has(raw)) return ''
  return raw as JobStatus
}

function parseSortField(raw: string | null): JobSortField {
  if (!raw || !SORT_FIELDS.has(raw)) return DEFAULT_SORT_FIELD
  return raw as JobSortField
}

function parseSortDir(raw: string | null): SortDirection {
  if (raw === 'asc' || raw === 'desc') return raw
  return DEFAULT_SORT_DIR
}

function parsePage(raw: string | null): number {
  const n = parseInt(raw ?? '1', 10)
  return !isNaN(n) && n >= 1 ? n : 1
}

export type UseJobListFiltersResult = {
  status: JobStatus | ''
  search: string
  sortField: JobSortField
  sortDir: SortDirection
  page: number
  setStatus: (status: JobStatus | '') => void
  setSearch: (search: string) => void
  setSortField: (field: JobSortField) => void
  setSortDir: (dir: SortDirection) => void
  /** Update sort field and direction atomically in a single navigation. */
  setSortFieldAndDir: (field: JobSortField, dir: SortDirection) => void
  setPage: (page: number) => void
}

/**
 * Manages job list filter state (status, search, sort, page) in URL search
 * params so the view is bookmarkable and survives page reload.
 *
 * Changing status or search deletes the page param (treated as page 1) to
 * avoid landing on an empty page after the result set shrinks.
 *
 * Changing sort field or direction intentionally does NOT reset the page —
 * re-ordering a large list while on a deep page is a valid UX action.
 */
export function useJobListFilters(): UseJobListFiltersResult {
  const [searchParams, setSearchParams] = useSearchParams()

  const status = parseStatus(searchParams.get(JOB_STATUS_PARAM))
  const search = searchParams.get(JOB_SEARCH_PARAM) ?? ''
  const sortField = parseSortField(searchParams.get(JOB_SORT_FIELD_PARAM))
  const sortDir = parseSortDir(searchParams.get(JOB_SORT_DIR_PARAM))
  const page = parsePage(searchParams.get(JOB_PAGE_PARAM))

  const setStatus = useCallback(
    (value: JobStatus | '') => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        if (value === '') {
          next.delete(JOB_STATUS_PARAM)
        } else {
          next.set(JOB_STATUS_PARAM, value)
        }
        next.delete(JOB_PAGE_PARAM)
        return next
      })
    },
    [setSearchParams],
  )

  const setSearch = useCallback(
    (value: string) => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        if (value === '') {
          next.delete(JOB_SEARCH_PARAM)
        } else {
          next.set(JOB_SEARCH_PARAM, value)
        }
        next.delete(JOB_PAGE_PARAM)
        return next
      })
    },
    [setSearchParams],
  )

  const setSortField = useCallback(
    (value: JobSortField) => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        next.set(JOB_SORT_FIELD_PARAM, value)
        return next
      })
    },
    [setSearchParams],
  )

  const setSortDir = useCallback(
    (value: SortDirection) => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        next.set(JOB_SORT_DIR_PARAM, value)
        return next
      })
    },
    [setSearchParams],
  )

  const setSortFieldAndDir = useCallback(
    (field: JobSortField, dir: SortDirection) => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        next.set(JOB_SORT_FIELD_PARAM, field)
        next.set(JOB_SORT_DIR_PARAM, dir)
        return next
      })
    },
    [setSearchParams],
  )

  const setPage = useCallback(
    (value: number) => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        next.set(JOB_PAGE_PARAM, String(Math.max(1, value)))
        return next
      })
    },
    [setSearchParams],
  )

  return {
    status,
    search,
    sortField,
    sortDir,
    page,
    setStatus,
    setSearch,
    setSortField,
    setSortDir,
    setSortFieldAndDir,
    setPage,
  }
}
