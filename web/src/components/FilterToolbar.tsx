// SPDX-License-Identifier: AGPL-3.0-or-later

import DebouncedSearchInput from './DebouncedSearchInput'
import styles from './FilterToolbar.module.css'

export interface StatusFilterOption {
  label: string
  value: string
  count?: number
}

export interface FilterToolbarProps {
  statuses: StatusFilterOption[]
  activeStatus: string
  onStatusChange: (value: string) => void
  search: string
  onSearchChange: (value: string) => void
  searchPlaceholder: string
  searchLabel: string
}

export default function FilterToolbar({
  statuses,
  activeStatus,
  onStatusChange,
  search,
  onSearchChange,
  searchPlaceholder,
  searchLabel,
}: FilterToolbarProps) {
  return (
    <div className={styles.toolbar}>
      <div className={styles.filterBar} role="toolbar" aria-label="Filter by status">
        {statuses.map(({ label, value, count }) => (
          <button
            key={value}
            className={[
              styles.filterPill,
              activeStatus === value ? styles['filterPill--active'] : '',
            ]
              .filter(Boolean)
              .join(' ')}
            onClick={() => onStatusChange(value)}
            aria-pressed={activeStatus === value}
            aria-label={count !== undefined ? `${label} (${count})` : undefined}
            type="button"
          >
            {label}
            {count !== undefined && (
              <span className={styles.filterCount} aria-hidden="true">
                {count}
              </span>
            )}
          </button>
        ))}
      </div>
      <DebouncedSearchInput
        value={search}
        onChange={onSearchChange}
        placeholder={searchPlaceholder}
        aria-label={searchLabel}
      />
    </div>
  )
}
