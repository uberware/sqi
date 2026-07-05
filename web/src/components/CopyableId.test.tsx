// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import CopyableId from './CopyableId'

// Note: userEvent.setup() installs its own navigator.clipboard stub, overriding
// any mock we define. We use fireEvent.click() for clipboard tests so that the
// component calls our mock directly rather than user-event's internal clipboard.

function mockClipboard(writeText: ReturnType<typeof vi.fn>) {
  Object.defineProperty(navigator, 'clipboard', {
    value: { writeText },
    configurable: true,
    writable: true,
  })
}

describe('CopyableId', () => {
  it('renders the truncated id with a copy affordance', () => {
    render(<CopyableId id="0123456789abcdef" />)
    const cell = screen.getByRole('button', { name: /01234567/ })
    expect(cell).toHaveAttribute('title', 'Click to copy: 0123456789abcdef')
  })

  it('copies the full id to the clipboard on click', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    mockClipboard(writeText)
    render(<CopyableId id="0123456789abcdef" />)
    fireEvent.click(screen.getByRole('button'))
    expect(writeText).toHaveBeenCalledWith('0123456789abcdef')
    await waitFor(() => expect(screen.getByRole('button')).toHaveAttribute('data-copied', 'true'))
  })

  it('copies on Enter for keyboard users', () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    mockClipboard(writeText)
    render(<CopyableId id="worker-1" />)
    fireEvent.keyDown(screen.getByRole('button'), { key: 'Enter' })
    expect(writeText).toHaveBeenCalledWith('worker-1')
  })

  it('does not throw when the clipboard write rejects', () => {
    const writeText = vi.fn().mockRejectedValue(new Error('insecure'))
    mockClipboard(writeText)
    render(<CopyableId id="worker-1" />)
    fireEvent.click(screen.getByRole('button'))
    expect(writeText).toHaveBeenCalled()
    expect(screen.getByRole('button')).not.toHaveAttribute('data-copied', 'true')
  })
})
