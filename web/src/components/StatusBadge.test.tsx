// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import StatusBadge from './StatusBadge'
import type { BadgeStatus } from './StatusBadge'

const STATUS_LABELS: [BadgeStatus, string][] = [
  ['pending', 'Pending'],
  ['ready', 'Ready'],
  ['assigned', 'Assigned'],
  ['running', 'Running'],
  ['succeeded', 'Succeeded'],
  ['completed', 'Completed'],
  ['failed', 'Failed'],
  ['canceled', 'Canceled'],
  ['paused', 'Paused'],
  ['blocked', 'Blocked'],
]

describe('StatusBadge', () => {
  it.each(STATUS_LABELS)('renders correct label for status "%s"', (status, expectedLabel) => {
    render(<StatusBadge status={status} />)
    const badge = screen.getByLabelText(`Status: ${expectedLabel}`)
    expect(badge).toBeInTheDocument()
    expect(badge).toHaveTextContent(expectedLabel)
  })

  it.each([
    ['awake', 'Awake'],
    ['waking', 'Waking'],
    ['retiring', 'Retiring'],
    ['asleep', 'Asleep'],
    ['wake_failed', 'Wake failed'],
  ] satisfies [BadgeStatus, string][])(
    'names power state "%s" as power',
    (status, expectedLabel) => {
      render(<StatusBadge status={status} kind="Power" />)
      const badge = screen.getByLabelText(`Power: ${expectedLabel}`)
      expect(badge).toHaveTextContent(expectedLabel)
    },
  )

  it('applies an extra className when provided', () => {
    const { container } = render(<StatusBadge status="pending" className="extra" />)
    expect(container.firstChild).toHaveClass('extra')
  })
})
