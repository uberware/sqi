// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { renderHook, act, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router'
import { usePaginatedList } from './usePaginatedList'
import type { ListResponse } from '@/api/types'

// ── Helpers ───────────────────────────────────────────────────────────────────

function makeWrapper(initialEntry = '/') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return function Wrapper({ children }: { children: React.ReactNode }) {
    return (
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={[initialEntry]}>{children}</MemoryRouter>
      </QueryClientProvider>
    )
  }
}

function makeQueryFn<T>(items: T[], total?: number) {
  const response: ListResponse<T> = {
    items,
    total: total ?? items.length,
    limit: 50,
    offset: 0,
  }
  return vi.fn().mockResolvedValue(response)
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('usePaginatedList', () => {
  it('defaults to page 1 and the specified defaultPageSize', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([]), 25), {
      wrapper: makeWrapper(),
    })
    expect(result.current.page).toBe(1)
    expect(result.current.pageSize).toBe(25)
  })

  it('falls back to DEFAULT_PAGE_SIZE (50) when no defaultPageSize is given', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([])), {
      wrapper: makeWrapper(),
    })
    expect(result.current.pageSize).toBe(50)
  })

  it('reads page and page_size from the URL search params', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 100), 10), {
      wrapper: makeWrapper('/?page=3&page_size=10'),
    })
    expect(result.current.page).toBe(3)
    expect(result.current.pageSize).toBe(10)
  })

  it('returns query data once loaded', async () => {
    const items = [{ id: 'a' }, { id: 'b' }]
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn(items, 2)), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    expect(result.current.items).toEqual(items)
    expect(result.current.total).toBe(2)
  })

  it('calculates totalPages from total and pageSize', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 25), 10), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    expect(result.current.totalPages).toBe(3)
  })

  it('totalPages is at least 1 even when total is 0', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 0), 10), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    expect(result.current.totalPages).toBe(1)
  })

  it('hasNextPage is true when not on the last page', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 30), 10), {
      wrapper: makeWrapper('/?page=2&page_size=10'),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    expect(result.current.hasNextPage).toBe(true)
    expect(result.current.hasPrevPage).toBe(true)
  })

  it('hasPrevPage is false on page 1', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 30), 10), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    expect(result.current.hasPrevPage).toBe(false)
  })

  it('hasNextPage is false on the last page', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 10), 10), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    expect(result.current.hasNextPage).toBe(false)
  })

  it('defaults to empty items and zero total while loading', () => {
    const neverResolves = vi.fn(() => new Promise<ListResponse<{ id: string }>>(() => {}))
    const { result } = renderHook(() => usePaginatedList(['test'], neverResolves), {
      wrapper: makeWrapper(),
    })
    expect(result.current.items).toEqual([])
    expect(result.current.total).toBe(0)
    expect(result.current.isLoading).toBe(true)
  })

  it('goToPage updates the page URL param', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 100), 10), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    act(() => {
      result.current.goToPage(4)
    })
    expect(result.current.page).toBe(4)
  })

  it('goToPage clamps to totalPages', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 30), 10), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    act(() => {
      result.current.goToPage(999)
    })
    expect(result.current.page).toBe(3)
  })

  it('goToPage clamps to 1 at minimum', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 30), 10), {
      wrapper: makeWrapper('/?page=2&page_size=10'),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    act(() => {
      result.current.goToPage(-5)
    })
    expect(result.current.page).toBe(1)
  })

  it('goToNextPage increments the page', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 30), 10), {
      wrapper: makeWrapper('/?page=1&page_size=10'),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    act(() => {
      result.current.goToNextPage()
    })
    expect(result.current.page).toBe(2)
  })

  it('goToPrevPage decrements the page', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 30), 10), {
      wrapper: makeWrapper('/?page=3&page_size=10'),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    act(() => {
      result.current.goToPrevPage()
    })
    expect(result.current.page).toBe(2)
  })

  it('setPageSize updates pageSize and resets page to 1', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 100), 10), {
      wrapper: makeWrapper('/?page=5&page_size=10'),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    act(() => {
      result.current.setPageSize(25)
    })
    expect(result.current.pageSize).toBe(25)
    expect(result.current.page).toBe(1)
  })

  it('setPageSize clamps to minimum of 1', async () => {
    const { result } = renderHook(() => usePaginatedList(['test'], makeQueryFn([], 10), 10), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    act(() => {
      result.current.setPageSize(0)
    })
    expect(result.current.pageSize).toBe(1)
  })

  it('passes correct offset to queryFn based on page and pageSize', async () => {
    const queryFn = makeQueryFn([], 100)
    renderHook(() => usePaginatedList(['test'], queryFn, 10), {
      wrapper: makeWrapper('/?page=3&page_size=10'),
    })
    await waitFor(() => expect(queryFn).toHaveBeenCalledWith({ limit: 10, offset: 20 }))
  })
})
