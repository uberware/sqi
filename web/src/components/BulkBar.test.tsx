// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import BulkBar from './BulkBar'

describe('BulkBar', () => {
  it('shows the selected count', () => {
    render(<BulkBar count={3} onClear={() => {}} />)
    expect(screen.getByText('3 selected')).toBeInTheDocument()
  })

  it('calls onClear when Clear is clicked', async () => {
    const onClear = vi.fn()
    const user = userEvent.setup()
    render(<BulkBar count={1} onClear={onClear} />)
    await user.click(screen.getByRole('button', { name: 'Clear' }))
    expect(onClear).toHaveBeenCalledTimes(1)
  })

  it('renders action buttons passed as children', () => {
    render(
      <BulkBar count={2} onClear={() => {}}>
        <button type="button">Cancel 2</button>
      </BulkBar>,
    )
    expect(screen.getByRole('button', { name: 'Cancel 2' })).toBeInTheDocument()
  })
})
