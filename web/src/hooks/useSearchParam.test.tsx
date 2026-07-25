// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router'
import type { ReactNode } from 'react'
import { useSearchParam } from './useSearchParam'

function wrapper(initial: string) {
  return ({ children }: { children: ReactNode }) => (
    <MemoryRouter initialEntries={[initial]}>{children}</MemoryRouter>
  )
}

describe('useSearchParam', () => {
  it('reads an existing ?search= value', () => {
    const { result } = renderHook(() => useSearchParam(), { wrapper: wrapper('/x?search=nuke') })
    expect(result.current.search).toBe('nuke')
  })

  it('defaults to an empty string when the param is absent', () => {
    const { result } = renderHook(() => useSearchParam(), { wrapper: wrapper('/x') })
    expect(result.current.search).toBe('')
  })

  it('setSearch writes the param', () => {
    const { result } = renderHook(() => useSearchParam(), { wrapper: wrapper('/x') })
    act(() => result.current.setSearch('maya'))
    expect(result.current.search).toBe('maya')
  })

  it('setSearch("") removes the param and preserves other params', () => {
    const { result } = renderHook(
      () => {
        const location = useLocation()
        return { ...useSearchParam(), location }
      },
      { wrapper: wrapper('/x?search=nuke&foo=1') },
    )
    act(() => result.current.setSearch(''))
    expect(result.current.search).toBe('')
    expect(result.current.location.search).toBe('?foo=1')
  })
})
