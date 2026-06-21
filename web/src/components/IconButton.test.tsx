// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import IconButton from './IconButton'

describe('IconButton', () => {
  it('renders the icon and exposes the accessible name', () => {
    render(<IconButton icon={<svg data-testid="icon" />} label="Delete farm render" />)
    const btn = screen.getByRole('button', { name: 'Delete farm render' })
    expect(btn).toBeInTheDocument()
    expect(screen.getByTestId('icon')).toBeInTheDocument()
  })

  it('calls onClick when clicked', () => {
    const onClick = vi.fn()
    render(<IconButton icon={<svg />} label="Delete" onClick={onClick} />)
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
    expect(onClick).toHaveBeenCalledTimes(1)
  })

  it('shows a spinner and disables the button while busy', () => {
    render(<IconButton icon={<svg data-testid="icon" />} label="Delete" busy />)
    const btn = screen.getByRole('button', { name: 'Delete' })
    expect(btn).toBeDisabled()
    // The resting icon is replaced by the spinner.
    expect(screen.queryByTestId('icon')).not.toBeInTheDocument()
  })

  it('does not fire onClick while busy', () => {
    const onClick = vi.fn()
    render(<IconButton icon={<svg />} label="Delete" busy onClick={onClick} />)
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
    expect(onClick).not.toHaveBeenCalled()
  })

  it('respects an explicit disabled prop', () => {
    render(<IconButton icon={<svg />} label="Delete" disabled />)
    expect(screen.getByRole('button', { name: 'Delete' })).toBeDisabled()
  })

  it('uses an explicit title, falling back to the label', () => {
    const { rerender } = render(<IconButton icon={<svg />} label="Delete farm a" title="Delete" />)
    expect(screen.getByRole('button', { name: 'Delete farm a' })).toHaveAttribute('title', 'Delete')
    rerender(<IconButton icon={<svg />} label="Delete farm a" />)
    expect(screen.getByRole('button', { name: 'Delete farm a' })).toHaveAttribute(
      'title',
      'Delete farm a',
    )
  })

  it('applies the provided className and defaults to a button type', () => {
    render(<IconButton icon={<svg />} label="Delete" className="custom" />)
    const btn = screen.getByRole('button', { name: 'Delete' })
    expect(btn).toHaveClass('custom')
    expect(btn).toHaveAttribute('type', 'button')
  })
})
