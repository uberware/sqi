// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import { Refresh } from '@/components/icons'
import { formatAge } from '@/lib/time'
import styles from './RefreshControls.module.css'

interface RefreshControlsProps {
  /** Accessible label for the refresh button, e.g. "Refresh jobs". */
  label: string
  onRefresh: () => void
  /** Query dataUpdatedAt timestamp (ms); omitted or 0 hides the age text. */
  updatedAt?: number
  /** Live "now" (ms) the age is computed against, from useLiveNow. */
  now?: number
  /** Extra header controls (status badge, toggles) rendered before the age. */
  children?: ReactNode
}

/** PageHeader action block: optional extras, "Updated N ago", refresh button. */
export default function RefreshControls({
  label,
  onRefresh,
  updatedAt,
  now,
  children,
}: RefreshControlsProps) {
  return (
    <div className={styles.headerActions}>
      {children}
      {updatedAt !== undefined && updatedAt > 0 && now !== undefined && (
        <span className={styles.lastUpdated} aria-live="polite">
          Updated {formatAge(updatedAt, now)}
        </span>
      )}
      <button className={styles.refreshBtn} onClick={onRefresh} type="button" aria-label={label}>
        <Refresh />
        Refresh
      </button>
    </div>
  )
}
