// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import CopyButton from './CopyButton'

// userEvent.setup() installs its own navigator.clipboard stub, overriding any
// mock defined here. The clipboard tests use fireEvent.click() so that the
// component calls the mock directly rather than user-event's clipboard.

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

  it('keeps the value out of its tooltip, which may be a secret', () => {
    render(<CopyButton value="sqg_RAWSECRETVALUE" display="token" />)
    const button = screen.getByRole('button', { name: /copy token/i })
    expect(button).toHaveAttribute('title', 'Click to copy')
    expect(button.outerHTML).not.toContain('sqg_RAWSECRETVALUE')
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
    // The promise rejected, so there is no "Copied!" state.
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
  })
})
