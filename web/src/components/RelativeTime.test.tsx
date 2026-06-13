// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import RelativeTime from './RelativeTime'

describe('RelativeTime', () => {
  it('renders seconds-ago for a very recent timestamp', () => {
    const tenSecondsAgo = new Date(Date.now() - 10_000).toISOString()
    render(<RelativeTime timestamp={tenSecondsAgo} />)
    // Intl "auto" yields e.g. "10 seconds ago"; assert the unit, not exact wording.
    expect(screen.getByText(/second/i)).toBeInTheDocument()
  })

  it('renders minutes for a few-minutes-ago timestamp', () => {
    const fiveMinAgo = new Date(Date.now() - 5 * 60_000).toISOString()
    render(<RelativeTime timestamp={fiveMinAgo} />)
    expect(screen.getByText(/minute/i)).toBeInTheDocument()
  })

  it('renders hours and days for older timestamps', () => {
    const { rerender } = render(
      <RelativeTime timestamp={new Date(Date.now() - 3 * 3_600_000).toISOString()} />,
    )
    expect(screen.getByText(/hour/i)).toBeInTheDocument()
    rerender(<RelativeTime timestamp={new Date(Date.now() - 3 * 86_400_000).toISOString()} />)
    expect(screen.getByText(/day/i)).toBeInTheDocument()
  })

  it('exposes a machine-readable dateTime and accepts Date and number inputs', () => {
    const d = new Date('2026-06-13T00:00:00.000Z')
    const { container } = render(<RelativeTime timestamp={d} />)
    expect(container.querySelector('time')).toHaveAttribute('dateTime', '2026-06-13T00:00:00.000Z')
    render(<RelativeTime timestamp={d.getTime()} />)
  })
})
