// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import RefreshControls from './RefreshControls'

describe('RefreshControls', () => {
  it('calls onRefresh when the refresh button is clicked', async () => {
    const onRefresh = vi.fn()
    const user = userEvent.setup()
    render(<RefreshControls onRefresh={onRefresh} label="Refresh jobs" />)
    await user.click(screen.getByRole('button', { name: 'Refresh jobs' }))
    expect(onRefresh).toHaveBeenCalledTimes(1)
  })

  it('shows the last-updated age when updatedAt and now are provided', () => {
    const now = Date.now()
    render(
      <RefreshControls
        onRefresh={() => {}}
        label="Refresh workers"
        updatedAt={now - 30_000}
        now={now}
      />,
    )
    expect(screen.getByText('Updated 30s ago')).toBeInTheDocument()
  })

  it('hides the last-updated age for a zero timestamp', () => {
    render(
      <RefreshControls
        onRefresh={() => {}}
        label="Refresh workers"
        updatedAt={0}
        now={Date.now()}
      />,
    )
    expect(screen.queryByText(/Updated/)).not.toBeInTheDocument()
  })

  it('renders extra controls passed as children before the refresh button', () => {
    render(
      <RefreshControls onRefresh={() => {}} label="Refresh worker">
        <span>status-badge</span>
      </RefreshControls>,
    )
    expect(screen.getByText('status-badge')).toBeInTheDocument()
  })
})
