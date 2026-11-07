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

  describe('segments', () => {
    const width = (name: string): string =>
      (document.querySelector(`[data-segment="${name}"]`) as HTMLElement).style.width

    it('draws one segment per terminal status', () => {
      render(
        <TaskProgressBar
          counts={makeCounts({ total: 10, running: 3, succeeded: 4, failed: 1, canceled: 1 })}
        />,
      )
      expect(width('succeeded')).toBe('40%')
      expect(width('failed')).toBe('10%')
      expect(width('canceled')).toBe('10%')
      expect(width('running')).toBe('0%')
      expect(screen.getByText('6/10')).toBeInTheDocument()
      expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '60')
    })

    it('five succeeded and five failed read as half and half', () => {
      render(<TaskProgressBar counts={makeCounts({ total: 10, succeeded: 5, failed: 5 })} />)
      expect(width('succeeded')).toBe('50%')
      expect(width('failed')).toBe('50%')
      const bar = screen.getByRole('progressbar')
      expect(bar).toHaveAttribute('aria-valuenow', '100')
      expect(bar).toHaveAttribute(
        'aria-valuetext',
        '5 succeeded, 5 failed, 0 canceled, 0 running of 10',
      )
    })

    it('a single running task moves with its progress', () => {
      render(<TaskProgressBar counts={makeCounts({ total: 1, running: 1 })} progressPercent={43} />)
      expect(width('running')).toBe('43%')
      expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '43')
      expect(screen.getByText('0/1')).toBeInTheDocument()
    })

    it('clamps the running segment to the running count', () => {
      render(
        <TaskProgressBar
          counts={makeCounts({ total: 4, succeeded: 1, running: 1 })}
          progressPercent={100}
        />,
      )
      expect(width('running')).toBe('25%')
    })

    it('treats null progressPercent as no running progress', () => {
      render(
        <TaskProgressBar
          counts={makeCounts({ total: 4, succeeded: 1, running: 2 })}
          progressPercent={null}
        />,
      )
      expect(width('running')).toBe('0%')
    })
  })
})
