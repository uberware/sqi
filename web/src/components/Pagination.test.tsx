// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import Pagination, { type PaginationProps } from './Pagination'

function setup(overrides: Partial<PaginationProps> = {}) {
  const props: PaginationProps = {
    page: 1,
    totalPages: 1,
    pageSize: 25,
    total: 10,
    hasNextPage: false,
    hasPrevPage: false,
    onGoToPage: vi.fn(),
    onGoToNextPage: vi.fn(),
    onGoToPrevPage: vi.fn(),
    onSetPageSize: vi.fn(),
    ...overrides,
  }
  render(<Pagination {...props} />)
  return props
}

describe('Pagination', () => {
  it('renders the result range text', () => {
    setup({ page: 1, pageSize: 25, total: 10 })
    expect(screen.getByText('1–10 of 10')).toBeInTheDocument()
  })

  it('renders "No results" when total is zero', () => {
    setup({ total: 0 })
    expect(screen.getByText('No results')).toBeInTheDocument()
  })

  it('inserts ellipses for large page counts and keeps first/last', () => {
    setup({ page: 10, totalPages: 20, total: 500, hasNextPage: true, hasPrevPage: true })
    expect(screen.getByRole('button', { name: 'Page 1' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Page 20' })).toBeInTheDocument()
    expect(screen.getAllByText('…').length).toBeGreaterThan(0)
  })

  it('fires nav callbacks and disables controls at the edges', async () => {
    const user = userEvent.setup()
    const props = setup({
      page: 2,
      totalPages: 3,
      total: 75,
      hasNextPage: true,
      hasPrevPage: true,
    })
    await user.click(screen.getByRole('button', { name: 'Previous page' }))
    expect(props.onGoToPrevPage).toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: 'Next page' }))
    expect(props.onGoToNextPage).toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: 'Page 3' }))
    expect(props.onGoToPage).toHaveBeenCalledWith(3)
  })

  it('changes page size via the select', async () => {
    const user = userEvent.setup()
    const props = setup({ pageSize: 25 })
    await user.selectOptions(screen.getByLabelText('Items per page'), '50')
    expect(props.onSetPageSize).toHaveBeenCalledWith(50)
  })
})
