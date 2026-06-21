// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, act } from '@testing-library/react'
import FilterToolbar from './FilterToolbar'

const statuses = [
  { label: 'All', value: '' },
  { label: 'Online', value: 'online', count: 3 },
]

describe('FilterToolbar', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('renders pills with counts and fires onStatusChange', () => {
    const onStatusChange = vi.fn()
    render(
      <FilterToolbar
        statuses={statuses}
        activeStatus=""
        onStatusChange={onStatusChange}
        search=""
        onSearchChange={vi.fn()}
        searchPlaceholder="Search…"
        searchLabel="Search items"
      />,
    )
    expect(screen.getByText('3')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /online/i }))
    expect(onStatusChange).toHaveBeenCalledWith('online')
  })

  it('debounces search and emits the settled value', () => {
    const onSearchChange = vi.fn()
    render(
      <FilterToolbar
        statuses={statuses}
        activeStatus=""
        onStatusChange={vi.fn()}
        search=""
        onSearchChange={onSearchChange}
        searchPlaceholder="Search…"
        searchLabel="Search items"
      />,
    )
    fireEvent.change(screen.getByLabelText('Search items'), { target: { value: 'foo' } })
    expect(onSearchChange).not.toHaveBeenCalled()
    act(() => vi.advanceTimersByTime(300))
    expect(onSearchChange).toHaveBeenCalledWith('foo')
  })
})
