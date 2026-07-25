// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { ReactNode } from 'react'
import { useListFilters } from './useListFilters'

const config = {
  statuses: new Set(['running', 'pending']),
  sortFields: new Set(['name', 'created_at']),
  defaultSortField: 'created_at' as const,
  defaultSortDir: 'desc' as const,
}

function wrapper(initial = '/') {
  return ({ children }: { children: ReactNode }) => (
    <MemoryRouter initialEntries={[initial]}>{children}</MemoryRouter>
  )
}

describe('useListFilters', () => {
  it('parses defaults from an empty URL', () => {
    const { result } = renderHook(() => useListFilters(config), { wrapper: wrapper() })
    expect(result.current.status).toBe('')
    expect(result.current.search).toBe('')
    expect(result.current.sortField).toBe('created_at')
    expect(result.current.sortDir).toBe('desc')
    expect(result.current.page).toBe(1)
    expect(result.current.pageSize).toBe(50)
  })

  it('rejects unknown status and sort values', () => {
    const { result } = renderHook(() => useListFilters(config), {
      wrapper: wrapper('/?status=bogus&sort=bogus&size=999'),
    })
    expect(result.current.status).toBe('')
    expect(result.current.sortField).toBe('created_at')
    expect(result.current.pageSize).toBe(50)
  })

  it('setStatus clears page', () => {
    const { result } = renderHook(() => useListFilters(config), {
      wrapper: wrapper('/?page=4'),
    })
    act(() => result.current.setStatus('running'))
    expect(result.current.status).toBe('running')
    expect(result.current.page).toBe(1)
  })

  it('setSearch clears page; setSortFieldAndDir keeps it', () => {
    const { result } = renderHook(() => useListFilters(config), {
      wrapper: wrapper('/?page=3'),
    })
    act(() => result.current.setSortFieldAndDir('name', 'asc'))
    expect(result.current.page).toBe(3)
    expect(result.current.sortField).toBe('name')
    act(() => result.current.setSearch('foo'))
    expect(result.current.search).toBe('foo')
    expect(result.current.page).toBe(1)
  })

  it('setPageSize clears page and persists', () => {
    const { result } = renderHook(() => useListFilters(config), {
      wrapper: wrapper('/?page=2'),
    })
    act(() => result.current.setPageSize(100))
    expect(result.current.pageSize).toBe(100)
    expect(result.current.page).toBe(1)
  })
})
