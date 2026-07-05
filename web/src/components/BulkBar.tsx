// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import styles from './BulkBar.module.css'

interface BulkBarProps {
  /** Number of selected rows shown as "N selected". */
  count: number
  onClear: () => void
  /** Page-specific bulk action buttons. */
  children?: ReactNode
}

/** Bulk-selection action bar pinned below a list: count, actions, Clear. */
export default function BulkBar({ count, onClear, children }: BulkBarProps) {
  return (
    <div className={styles.bulkBar}>
      <span className={styles.count}>{count} selected</span>
      {children}
      <button className={styles.clearBtn} onClick={onClear} type="button">
        Clear
      </button>
    </div>
  )
}
