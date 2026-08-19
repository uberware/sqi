// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router'
import type { ReactNode } from 'react'
import { useTabParam } from './useTabParam'

function wrapper(initial: string) {
  return ({ children }: { children: ReactNode }) => (
    <MemoryRouter initialEntries={[initial]}>{children}</MemoryRouter>
  )
}

const VALID = ['readme', 'template']

describe('useTabParam', () => {
  it('reads an existing ?tab= value', () => {
    const { result } = renderHook(() => useTabParam(VALID, 'template'), {
      wrapper: wrapper('/x?tab=readme'),
    })
    expect(result.current.tab).toBe('readme')
  })

  it('falls back to the default when the param is absent', () => {
    const { result } = renderHook(() => useTabParam(VALID, 'template'), { wrapper: wrapper('/x') })
    expect(result.current.tab).toBe('template')
  })

  // A ?tab= value from a stale link or a hand-edited URL must not select a tab
  // that does not exist — the page would render no panel at all.
  it('falls back to the default when the param names an unknown tab', () => {
    const { result } = renderHook(() => useTabParam(VALID, 'template'), {
      wrapper: wrapper('/x?tab=nonsense'),
    })
    expect(result.current.tab).toBe('template')
  })

  it('setTab writes the param', () => {
    const { result } = renderHook(() => useTabParam(VALID, 'template'), { wrapper: wrapper('/x') })
    act(() => result.current.setTab('readme'))
    expect(result.current.tab).toBe('readme')
  })

  it('setTab preserves other query params', () => {
    const { result } = renderHook(
      () => ({ tabs: useTabParam(VALID, 'template'), loc: useLocation() }),
      { wrapper: wrapper('/x?search=nuke') },
    )
    act(() => result.current.tabs.setTab('readme'))
    expect(result.current.loc.search).toContain('search=nuke')
    expect(result.current.loc.search).toContain('tab=readme')
  })

  // Selecting the default tab drops the param rather than pinning it, so a
  // shared URL stays clean and the page keeps following its own default if
  // that default later changes.
  it('setTab to the default removes the param', () => {
    const { result } = renderHook(
      () => ({ tabs: useTabParam(VALID, 'template'), loc: useLocation() }),
      { wrapper: wrapper('/x?tab=readme') },
    )
    act(() => result.current.tabs.setTab('template'))
    expect(result.current.loc.search).not.toContain('tab=')
    expect(result.current.tabs.tab).toBe('template')
  })
})
