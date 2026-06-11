// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import DataTable from './DataTable'
import type { ColumnDef } from './DataTable'

// ── Fixtures ──────────────────────────────────────────────────────────────────

interface Row {
  id: string
  name: string
  value: number
}

function makeRow(overrides: Partial<Row> = {}): Row {
  return { id: 'row-1', name: 'Alpha', value: 42, ...overrides }
}

const COLUMNS: ColumnDef<Row>[] = [
  { key: 'name', header: 'Name', render: (r) => r.name },
  { key: 'value', header: 'Value', render: (r) => String(r.value) },
]

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('DataTable', () => {
  it('renders column headers', () => {
    render(<DataTable columns={COLUMNS} data={[]} rowKey={(r) => r.id} aria-label="Test table" />)

    expect(screen.getByRole('columnheader', { name: 'Name' })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: 'Value' })).toBeInTheDocument()
  })

  it('renders typed data in table cells', () => {
    const rows = [
      makeRow({ id: '1', name: 'Alpha', value: 1 }),
      makeRow({ id: '2', name: 'Beta', value: 2 }),
    ]

    render(<DataTable columns={COLUMNS} data={rows} rowKey={(r) => r.id} aria-label="Test table" />)

    expect(screen.getByRole('cell', { name: 'Alpha' })).toBeInTheDocument()
    expect(screen.getByRole('cell', { name: '1' })).toBeInTheDocument()
    expect(screen.getByRole('cell', { name: 'Beta' })).toBeInTheDocument()
    expect(screen.getByRole('cell', { name: '2' })).toBeInTheDocument()
  })

  it('renders the default "No data." empty state when data is empty', () => {
    render(<DataTable columns={COLUMNS} data={[]} rowKey={(r) => r.id} aria-label="Test table" />)

    expect(screen.getByText('No data.')).toBeInTheDocument()
  })

  it('renders a custom empty state slot when provided', () => {
    render(
      <DataTable
        columns={COLUMNS}
        data={[]}
        rowKey={(r) => r.id}
        aria-label="Test table"
        emptyState={<span>Nothing here yet</span>}
      />,
    )

    expect(screen.getByText('Nothing here yet')).toBeInTheDocument()
    expect(screen.queryByText('No data.')).not.toBeInTheDocument()
  })

  it('renders loading skeleton rows when isLoading is true', () => {
    render(
      <DataTable
        columns={COLUMNS}
        data={[]}
        rowKey={(r) => r.id}
        aria-label="Test table"
        isLoading
      />,
    )

    // Skeleton rows are aria-hidden so actual data rows don't appear
    const skeletonRows = document.querySelectorAll('[aria-hidden="true"]')
    expect(skeletonRows.length).toBeGreaterThan(0)

    // Empty state should NOT be shown while loading
    expect(screen.queryByText('No data.')).not.toBeInTheDocument()
  })

  it('does not render data rows while loading', () => {
    const rows = [makeRow({ id: '1', name: 'Alpha', value: 1 })]

    render(
      <DataTable
        columns={COLUMNS}
        data={rows}
        rowKey={(r) => r.id}
        aria-label="Test table"
        isLoading
      />,
    )

    expect(screen.queryByText('Alpha')).not.toBeInTheDocument()
  })

  it('calls onRowClick with the row data when a row is clicked', async () => {
    const user = userEvent.setup()
    const onRowClick = vi.fn()
    const row = makeRow({ id: '1', name: 'Alpha', value: 1 })

    render(
      <DataTable
        columns={COLUMNS}
        data={[row]}
        rowKey={(r) => r.id}
        aria-label="Test table"
        onRowClick={onRowClick}
      />,
    )

    await user.click(screen.getByRole('cell', { name: 'Alpha' }))

    expect(onRowClick).toHaveBeenCalledExactlyOnceWith(row)
  })

  it('calls onRowClick when Enter is pressed on a row', async () => {
    const user = userEvent.setup()
    const onRowClick = vi.fn()
    const row = makeRow({ id: '1', name: 'Alpha', value: 1 })

    render(
      <DataTable
        columns={COLUMNS}
        data={[row]}
        rowKey={(r) => r.id}
        aria-label="Test table"
        onRowClick={onRowClick}
      />,
    )

    const cell = screen.getByRole('cell', { name: 'Alpha' })
    const tableRow = cell.closest('tr')
    expect(tableRow).toBeInTheDocument()
    ;(tableRow as HTMLElement).focus()
    await user.keyboard('{Enter}')

    expect(onRowClick).toHaveBeenCalledOnce()
  })

  it('calls onRowClick when Space is pressed on a row', async () => {
    const user = userEvent.setup()
    const onRowClick = vi.fn()
    const row = makeRow({ id: '1', name: 'Alpha', value: 1 })

    render(
      <DataTable
        columns={COLUMNS}
        data={[row]}
        rowKey={(r) => r.id}
        aria-label="Test table"
        onRowClick={onRowClick}
      />,
    )

    const cell = screen.getByRole('cell', { name: 'Alpha' })
    const tableRow = cell.closest('tr')
    expect(tableRow).toBeInTheDocument()
    ;(tableRow as HTMLElement).focus()
    await user.keyboard(' ')

    expect(onRowClick).toHaveBeenCalledOnce()
  })

  it('does not register click handlers when onRowClick is not provided', async () => {
    const user = userEvent.setup()
    const row = makeRow({ id: '1', name: 'Alpha', value: 1 })

    render(
      <DataTable columns={COLUMNS} data={[row]} rowKey={(r) => r.id} aria-label="Test table" />,
    )

    // Should render without error and the row should not be focusable
    const cell = screen.getByRole('cell', { name: 'Alpha' })
    const tableRow = cell.closest('tr')
    expect(tableRow).toBeInTheDocument()
    expect(tableRow).not.toHaveAttribute('tabIndex')

    // Clicking should not throw
    await user.click(cell)
  })

  it('applies an extra className to the wrapper', () => {
    const { container } = render(
      <DataTable
        columns={COLUMNS}
        data={[]}
        rowKey={(r) => r.id}
        aria-label="Test table"
        className="extra-class"
      />,
    )

    expect(container.firstChild).toHaveClass('extra-class')
  })
})
