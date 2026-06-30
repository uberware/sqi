// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ProductPathDelivery } from '@/pages/ProductPathDelivery'

describe('ProductPathDelivery', () => {
  it('renders the five delivery checkboxes', () => {
    render(<ProductPathDelivery value={null} onChange={() => {}} />)
    expect(screen.getByRole('checkbox', { name: /swap paths in place/i })).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: /write translation file/i })).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: /pass as command flags/i })).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: /set environment variables/i })).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: /stage files locally/i })).toBeInTheDocument()
  })

  it('reveals the flag pattern field only when command flags is checked', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<ProductPathDelivery value={null} onChange={onChange} />)
    expect(screen.queryByLabelText(/flag pattern/i)).not.toBeInTheDocument()
    await user.click(screen.getByRole('checkbox', { name: /pass as command flags/i }))
    expect(screen.getByLabelText(/flag pattern/i)).toBeInTheDocument()
    expect(onChange).toHaveBeenCalled()
  })

  it('checks boxes from an incoming value', () => {
    render(
      <ProductPathDelivery
        value={{ deliveries: [{ kind: 'environment', variable: 'PROJECT_ROOT' }] }}
        onChange={() => {}}
      />,
    )
    expect(screen.getByRole('checkbox', { name: /set environment variables/i })).toBeChecked()
    expect(screen.getByLabelText(/variable name/i)).toHaveValue('PROJECT_ROOT')
  })
})
