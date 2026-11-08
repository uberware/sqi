// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import RemainingTime from './RemainingTime'

describe('RemainingTime', () => {
  it('shows a dash when the remaining time is unknown', () => {
    render(<RemainingTime seconds={null} running={1} anchorMs={0} now={0} />)
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('shows a dash when the field is absent', () => {
    render(<RemainingTime seconds={undefined} running={1} anchorMs={0} now={0} />)
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('formats the remaining seconds', () => {
    render(<RemainingTime seconds={600} running={1} anchorMs={1000} now={1000} />)
    expect(screen.getByText('10m 0s')).toBeInTheDocument()
  })

  it('counts down by running tasks since the anchor', () => {
    render(<RemainingTime seconds={600} running={2} anchorMs={0} now={60_000} />)
    expect(screen.getByText('8m 0s')).toBeInTheDocument()
  })
})
