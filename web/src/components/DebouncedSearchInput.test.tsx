// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, act } from '@testing-library/react'
import DebouncedSearchInput from './DebouncedSearchInput'

describe('DebouncedSearchInput', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('seeds the input from value and does not emit on mount', () => {
    const onChange = vi.fn()
    render(
      <DebouncedSearchInput
        value="nuke"
        onChange={onChange}
        placeholder="Search…"
        aria-label="Search presets"
      />,
    )
    expect(screen.getByLabelText('Search presets')).toHaveValue('nuke')
    act(() => vi.advanceTimersByTime(1000))
    expect(onChange).not.toHaveBeenCalled()
  })

  it('debounces edits and emits only the settled value', () => {
    const onChange = vi.fn()
    render(
      <DebouncedSearchInput
        value=""
        onChange={onChange}
        placeholder="Search…"
        aria-label="Search items"
      />,
    )
    fireEvent.change(screen.getByLabelText('Search items'), { target: { value: 'fo' } })
    act(() => vi.advanceTimersByTime(200))
    fireEvent.change(screen.getByLabelText('Search items'), { target: { value: 'foo' } })
    expect(onChange).not.toHaveBeenCalled()
    act(() => vi.advanceTimersByTime(300))
    expect(onChange).toHaveBeenCalledTimes(1)
    expect(onChange).toHaveBeenCalledWith('foo')
  })
})
