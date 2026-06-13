// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import CopyButton from './CopyButton'

// Note: userEvent.setup() installs its own navigator.clipboard stub, overriding
// any mock we define. We use fireEvent.click() for clipboard tests so that the
// component calls our mock directly rather than user-event's internal clipboard.

describe('CopyButton', () => {
  it('writes the value to the clipboard on click', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    })
    render(<CopyButton value="job-abc123" />)
    fireEvent.click(screen.getByRole('button', { name: /copy job-abc123/i }))
    expect(writeText).toHaveBeenCalledWith('job-abc123')
    await waitFor(() => expect(screen.getByRole('button', { name: /copied/i })).toBeInTheDocument())
  })

  it('renders the display label instead of the value when provided', () => {
    render(<CopyButton value="job-abc123def456" display="job-abc1" />)
    expect(screen.getByText('job-abc1')).toBeInTheDocument()
    expect(screen.queryByText('job-abc123def456')).not.toBeInTheDocument()
  })

  it('does not throw when clipboard write rejects', () => {
    const writeText = vi.fn().mockRejectedValue(new Error('insecure'))
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    })
    render(<CopyButton value="x" />)
    fireEvent.click(screen.getByRole('button'))
    expect(writeText).toHaveBeenCalled()
    // No "Copied!" state because the promise rejected.
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
  })
})
