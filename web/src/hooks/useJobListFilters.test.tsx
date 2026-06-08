// SPDX-License-Identifier: AGPL-3.0-only

import { describe, it, expect } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { MemoryRouter, useSearchParams } from 'react-router-dom'
import { useJobListFilters } from './useJobListFilters'

function makeWrapper(initialEntry = '/') {
  return function Wrapper({ children }: { children: React.ReactNode }) {
    return <MemoryRouter initialEntries={[initialEntry]}>{children}</MemoryRouter>
  }
}

describe('useJobListFilters — defaults', () => {
  it('returns default values when URL has no params', () => {
    const { result } = renderHook(() => useJobListFilters(), { wrapper: makeWrapper() })
    expect(result.current.status).toBe('')
    expect(result.current.search).toBe('')
    expect(result.current.sortField).toBe('created_at')
    expect(result.current.sortDir).toBe('desc')
    expect(result.current.page).toBe(1)
  })
})

describe('useJobListFilters — reading URL params', () => {
  it('reads a valid status from URL', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?status=running'),
    })
    expect(result.current.status).toBe('running')
  })

  it('ignores an invalid status value', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?status=bogus'),
    })
    expect(result.current.status).toBe('')
  })

  it('reads search from URL', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?search=render'),
    })
    expect(result.current.search).toBe('render')
  })

  it('reads a valid sortField from URL', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?sort=priority'),
    })
    expect(result.current.sortField).toBe('priority')
  })

  it('falls back to default sortField for an invalid value', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?sort=unknown'),
    })
    expect(result.current.sortField).toBe('created_at')
  })

  it('reads sortDir asc from URL', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?dir=asc'),
    })
    expect(result.current.sortDir).toBe('asc')
  })

  it('falls back to desc for an invalid sortDir value', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?dir=sideways'),
    })
    expect(result.current.sortDir).toBe('desc')
  })

  it('reads page from URL', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?page=5'),
    })
    expect(result.current.page).toBe(5)
  })

  it('falls back to page 1 for invalid page value', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?page=abc'),
    })
    expect(result.current.page).toBe(1)
  })

  it('falls back to page 1 for zero page', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?page=0'),
    })
    expect(result.current.page).toBe(1)
  })

  it('reads all params simultaneously', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?status=failed&search=shader&sort=name&dir=asc&page=3'),
    })
    expect(result.current.status).toBe('failed')
    expect(result.current.search).toBe('shader')
    expect(result.current.sortField).toBe('name')
    expect(result.current.sortDir).toBe('asc')
    expect(result.current.page).toBe(3)
  })
})

describe('useJobListFilters — setStatus', () => {
  it('sets a valid status and resets page to 1', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?page=4'),
    })
    act(() => {
      result.current.setStatus('failed')
    })
    expect(result.current.status).toBe('failed')
    expect(result.current.page).toBe(1)
  })

  it('clears status when empty string is passed', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?status=running'),
    })
    act(() => {
      result.current.setStatus('')
    })
    expect(result.current.status).toBe('')
  })

  it('preserves other params when setting status', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?search=test&sort=priority&dir=asc'),
    })
    act(() => {
      result.current.setStatus('canceled')
    })
    expect(result.current.search).toBe('test')
    expect(result.current.sortField).toBe('priority')
    expect(result.current.sortDir).toBe('asc')
    expect(result.current.status).toBe('canceled')
  })
})

describe('useJobListFilters — setSearch', () => {
  it('sets search text and resets page to 1', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?page=3'),
    })
    act(() => {
      result.current.setSearch('arnold')
    })
    expect(result.current.search).toBe('arnold')
    expect(result.current.page).toBe(1)
  })

  it('clears search when empty string is passed', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?search=hello'),
    })
    act(() => {
      result.current.setSearch('')
    })
    expect(result.current.search).toBe('')
  })
})

describe('useJobListFilters — setSortField', () => {
  it('updates sort field without resetting page', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?page=2'),
    })
    act(() => {
      result.current.setSortField('name')
    })
    expect(result.current.sortField).toBe('name')
    expect(result.current.page).toBe(2)
  })

  it('accepts all valid sort fields', () => {
    const fields = ['name', 'priority', 'created_at', 'status'] as const
    for (const field of fields) {
      const { result } = renderHook(() => useJobListFilters(), { wrapper: makeWrapper() })
      act(() => {
        result.current.setSortField(field)
      })
      expect(result.current.sortField).toBe(field)
    }
  })
})

describe('useJobListFilters — setSortDir', () => {
  it('sets sort direction to asc', () => {
    const { result } = renderHook(() => useJobListFilters(), { wrapper: makeWrapper() })
    act(() => {
      result.current.setSortDir('asc')
    })
    expect(result.current.sortDir).toBe('asc')
  })

  it('sets sort direction to desc', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?dir=asc'),
    })
    act(() => {
      result.current.setSortDir('desc')
    })
    expect(result.current.sortDir).toBe('desc')
  })
})

describe('useJobListFilters — setSortFieldAndDir', () => {
  it('sets both field and direction atomically', () => {
    const { result } = renderHook(() => useJobListFilters(), { wrapper: makeWrapper() })
    act(() => {
      result.current.setSortFieldAndDir('priority', 'asc')
    })
    expect(result.current.sortField).toBe('priority')
    expect(result.current.sortDir).toBe('asc')
  })

  it('does not reset page', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?page=3'),
    })
    act(() => {
      result.current.setSortFieldAndDir('status', 'asc')
    })
    expect(result.current.page).toBe(3)
  })
})

describe('useJobListFilters — setPage', () => {
  it('updates page number', () => {
    const { result } = renderHook(() => useJobListFilters(), { wrapper: makeWrapper() })
    act(() => {
      result.current.setPage(7)
    })
    expect(result.current.page).toBe(7)
  })

  it('clamps page to minimum of 1', () => {
    const { result } = renderHook(() => useJobListFilters(), { wrapper: makeWrapper() })
    act(() => {
      result.current.setPage(-3)
    })
    expect(result.current.page).toBe(1)
  })

  it('preserves other params when changing page', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?status=running&search=test'),
    })
    act(() => {
      result.current.setPage(2)
    })
    expect(result.current.status).toBe('running')
    expect(result.current.search).toBe('test')
    expect(result.current.page).toBe(2)
  })
})

describe('useJobListFilters — URL cleanliness', () => {
  it('setStatus removes page param entirely (no ?page=1 residue)', () => {
    const { result } = renderHook(
      () => ({ filters: useJobListFilters(), params: useSearchParams()[0] }),
      { wrapper: makeWrapper('/?status=running&page=3') },
    )
    act(() => {
      result.current.filters.setStatus('failed')
    })
    expect(result.current.params.has('page')).toBe(false)
  })

  it('setSearch removes page param entirely (no ?page=1 residue)', () => {
    const { result } = renderHook(
      () => ({ filters: useJobListFilters(), params: useSearchParams()[0] }),
      { wrapper: makeWrapper('/?search=hello&page=2') },
    )
    act(() => {
      result.current.filters.setSearch('world')
    })
    expect(result.current.params.has('page')).toBe(false)
  })

  it('treats leading-numeric page param (e.g. 3abc) as the numeric prefix', () => {
    const { result } = renderHook(() => useJobListFilters(), {
      wrapper: makeWrapper('/?page=3abc'),
    })
    expect(result.current.page).toBe(3)
  })
})
