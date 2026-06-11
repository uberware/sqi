// SPDX-License-Identifier: AGPL-3.0-or-later

import { useId } from 'react'
import styles from './Pagination.module.css'

const DEFAULT_PAGE_SIZE_OPTIONS = [25, 50, 100]

export interface PaginationProps {
  page: number
  totalPages: number
  pageSize: number
  total: number
  hasNextPage: boolean
  hasPrevPage: boolean
  onGoToPage: (page: number) => void
  onGoToNextPage: () => void
  onGoToPrevPage: () => void
  onSetPageSize: (size: number) => void
  pageSizeOptions?: number[]
  className?: string
}

/**
 * Computes the page number window to display, inserting `null` where ellipsis
 * should appear. Always shows first and last pages; shows up to WINDOW pages
 * on each side of the current page.
 */
function getPageWindow(page: number, totalPages: number): (number | null)[] {
  if (totalPages <= 7) {
    return Array.from({ length: totalPages }, (_, i) => i + 1)
  }

  const WINDOW = 2
  const low = Math.max(2, page - WINDOW)
  const high = Math.min(totalPages - 1, page + WINDOW)

  const pages: (number | null)[] = [1]
  if (low > 2) pages.push(null)
  for (let i = low; i <= high; i++) pages.push(i)
  if (high < totalPages - 1) pages.push(null)
  pages.push(totalPages)

  return pages
}

export default function Pagination({
  page,
  totalPages,
  pageSize,
  total,
  hasNextPage,
  hasPrevPage,
  onGoToPage,
  onGoToNextPage,
  onGoToPrevPage,
  onSetPageSize,
  pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS,
  className,
}: PaginationProps) {
  const pageSizeId = useId()
  const offset = (page - 1) * pageSize
  const rangeStart = total === 0 ? 0 : offset + 1
  const rangeEnd = Math.min(offset + pageSize, total)
  const pageWindow = getPageWindow(page, totalPages)

  return (
    <div
      className={[styles.pagination, className].filter(Boolean).join(' ')}
      aria-label="Pagination"
    >
      <span className={styles.rangeInfo} aria-live="polite">
        {total === 0 ? 'No results' : `${rangeStart}–${rangeEnd} of ${total}`}
      </span>

      <div className={styles.controls} role="group" aria-label="Page navigation">
        <button
          className={styles.navBtn}
          onClick={onGoToPrevPage}
          disabled={!hasPrevPage}
          aria-label="Previous page"
          type="button"
        >
          ←
        </button>

        {pageWindow.map((p, i) =>
          p === null ? (
            <span key={`ellipsis-${i}`} className={styles.ellipsis} aria-hidden="true">
              …
            </span>
          ) : (
            <button
              key={p}
              className={[styles.pageBtn, p === page ? styles['pageBtn--active'] : '']
                .filter(Boolean)
                .join(' ')}
              onClick={() => onGoToPage(p)}
              disabled={p === page}
              aria-label={`Page ${p}`}
              aria-current={p === page ? 'page' : undefined}
              type="button"
            >
              {p}
            </button>
          ),
        )}

        <button
          className={styles.navBtn}
          onClick={onGoToNextPage}
          disabled={!hasNextPage}
          aria-label="Next page"
          type="button"
        >
          →
        </button>
      </div>

      <div className={styles.pageSizeWrap}>
        <label htmlFor={pageSizeId} className={styles.pageSizeLabel}>
          Per page:
        </label>
        <select
          id={pageSizeId}
          className={styles.pageSizeSelect}
          value={pageSize}
          onChange={(e) => onSetPageSize(Number(e.target.value))}
          aria-label="Items per page"
        >
          {pageSizeOptions.map((opt) => (
            <option key={opt} value={opt}>
              {opt}
            </option>
          ))}
        </select>
      </div>
    </div>
  )
}
