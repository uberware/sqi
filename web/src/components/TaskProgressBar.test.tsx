// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import TaskProgressBar from './TaskProgressBar'
import type { TaskCounts } from '@/api/types'

function makeCounts(overrides: Partial<TaskCounts> = {}): TaskCounts {
  return {
    total: 0,
    pending: 0,
    ready: 0,
    assigned: 0,
    running: 0,
    succeeded: 0,
    failed: 0,
    canceled: 0,
    ...overrides,
  }
}

describe('TaskProgressBar', () => {
  it('counts terminal tasks (succeeded + failed + canceled) as done', () => {
    render(
      <TaskProgressBar
        counts={makeCounts({ total: 10, running: 3, succeeded: 4, failed: 1, canceled: 1 })}
      />,
    )
    expect(screen.getByText('6/10')).toBeInTheDocument()
    expect(screen.getByRole('progressbar', { name: /task progress/i })).toHaveAttribute(
      'aria-valuenow',
      '60',
    )
  })

  it('rounds the percentage to the nearest integer', () => {
    render(<TaskProgressBar counts={makeCounts({ total: 3, succeeded: 2 })} />)
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '67')
  })

  it('renders 0% without dividing by zero when there are no tasks', () => {
    render(<TaskProgressBar counts={makeCounts({ total: 0 })} />)
    expect(screen.getByText('0/0')).toBeInTheDocument()
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '0')
  })

  it('merges an extra className onto the wrapper', () => {
    const { container } = render(
      <TaskProgressBar counts={makeCounts({ total: 1 })} className="extra" />,
    )
    expect((container.firstElementChild as HTMLElement).className).toContain('extra')
  })
})
