// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useEffect, useRef } from 'react'
import SearchInput from './SearchInput'
import { useDebounce } from '@/hooks/useDebounce'
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
  const [input, setInput] = useState(search)
  const debounced = useDebounce(input, 300)

  // Push the settled debounced value up once it changes. Skip the initial mount
  // run so an existing URL search value isn't re-emitted (which would clear the
  // page param) on first render.
  const didMount = useRef(false)
  useEffect(() => {
    if (!didMount.current) {
      didMount.current = true
      return
    }
    onSearchChange(debounced)
    // onSearchChange is a stable callback from the parent's filter hook.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debounced])

  return (
    <div className={styles.toolbar}>
      <div className={styles.filterBar} role="toolbar" aria-label="Filter by status">
        {statuses.map(({ label, value, count }) => (
          <button
            key={value}
            className={[styles.filterPill, activeStatus === value ? styles['filterPill--active'] : '']
              .filter(Boolean)
              .join(' ')}
            onClick={() => onStatusChange(value)}
            aria-pressed={activeStatus === value}
            type="button"
          >
            {label}
            {count !== undefined && (
              <span className={styles.filterCount} aria-label={`${count}`}>
                {count}
              </span>
            )}
          </button>
        ))}
      </div>
      <SearchInput
        value={input}
        onChange={setInput}
        placeholder={searchPlaceholder}
        aria-label={searchLabel}
      />
    </div>
  )
}
