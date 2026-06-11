// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode, KeyboardEvent } from 'react'
import styles from './DataTable.module.css'

export interface ColumnDef<T> {
  key: string
  header: ReactNode
  render: (row: T) => ReactNode
  className?: string
  headerClassName?: string
}

interface DataTableProps<T> {
  columns: ColumnDef<T>[]
  data: T[]
  rowKey: (row: T) => string
  onRowClick?: (row: T) => void
  isLoading?: boolean
  emptyState?: ReactNode
  className?: string
  'aria-label'?: string
}

const SKELETON_ROW_COUNT = 5

export default function DataTable<T>({
  columns,
  data,
  rowKey,
  onRowClick,
  isLoading = false,
  emptyState,
  className,
  'aria-label': ariaLabel,
}: DataTableProps<T>) {
  const showEmpty = !isLoading && data.length === 0

  function handleRowKeyDown(e: KeyboardEvent<HTMLTableRowElement>, row: T): void {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      onRowClick?.(row)
    }
  }

  return (
    <div className={[styles.wrap, className].filter(Boolean).join(' ')}>
      <table className={styles.table} aria-label={ariaLabel} role={onRowClick ? 'grid' : undefined}>
        <thead>
          <tr>
            {columns.map((col) => (
              <th key={col.key} className={col.headerClassName}>
                {col.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {isLoading &&
            Array.from({ length: SKELETON_ROW_COUNT }, (_, i) => (
              <tr key={i} className={styles.skeletonRow} aria-hidden="true">
                {columns.map((col) => (
                  <td key={col.key} className={col.className}>
                    <div className={styles.skeletonCell} />
                  </td>
                ))}
              </tr>
            ))}
          {showEmpty && (
            <tr>
              <td className={styles.emptyCell} colSpan={columns.length}>
                {emptyState ?? 'No data.'}
              </td>
            </tr>
          )}
          {!isLoading &&
            data.map((row) => (
              <tr
                key={rowKey(row)}
                className={onRowClick ? styles.clickableRow : undefined}
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                tabIndex={onRowClick ? 0 : undefined}
                onKeyDown={onRowClick ? (e) => handleRowKeyDown(e, row) : undefined}
              >
                {columns.map((col) => (
                  <td key={col.key} className={col.className}>
                    {col.render(row)}
                  </td>
                ))}
              </tr>
            ))}
        </tbody>
      </table>
    </div>
  )
}
