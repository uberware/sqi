// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import TaskRunProgress from './TaskRunProgress'

const now = Date.parse('2024-01-15T10:10:00Z')
const startAgo = (ms: number): string => new Date(now - ms).toISOString()

describe('TaskRunProgress', () => {
  it('shows percent, status text with title, and time left', () => {
    render(
      <TaskRunProgress progress={43} statusText="frame 3" startedAt={startAgo(60_000)} now={now} />,
    )
    const bar = screen.getByRole('progressbar', { name: 'Task progress' })
    expect(bar).toHaveAttribute('aria-valuenow', '43')
    expect(bar).toHaveAttribute('aria-valuemin', '0')
    expect(bar).toHaveAttribute('aria-valuemax', '100')
    expect(screen.getByText('43%')).toBeInTheDocument()
    expect(screen.getByText('frame 3')).toHaveAttribute('title', 'frame 3')
    expect(screen.getByText('≈ 1m 20s left')).toBeInTheDocument()
  })

  it('omits time left 20s after start', () => {
    render(<TaskRunProgress progress={43} startedAt={startAgo(20_000)} now={now} />)
    expect(screen.queryByText(/left/)).not.toBeInTheDocument()
  })

  it('shows an indeterminate bar for a status-only task', () => {
    render(<TaskRunProgress statusText="warming up" now={now} />)
    const bar = screen.getByRole('progressbar', { name: 'Task progress' })
    expect(bar).not.toHaveAttribute('aria-valuenow')
    expect(screen.getByText('warming up')).toBeInTheDocument()
  })

  it('renders nothing with neither progress nor status text', () => {
    const { container } = render(<TaskRunProgress now={now} />)
    expect(container).toBeEmptyDOMElement()
  })
})
