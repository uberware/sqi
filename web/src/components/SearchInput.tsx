// SPDX-License-Identifier: AGPL-3.0-or-later

import styles from './SearchInput.module.css'

export interface SearchInputProps {
  value: string
  onChange: (value: string) => void
  placeholder: string
  'aria-label': string
  className?: string
}

export default function SearchInput({
  value,
  onChange,
  placeholder,
  'aria-label': ariaLabel,
  className,
}: SearchInputProps) {
  return (
    <div className={[styles.searchWrap, className].filter(Boolean).join(' ')}>
      <input
        className={styles.searchInput}
        type="search"
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        aria-label={ariaLabel}
      />
    </div>
  )
}
