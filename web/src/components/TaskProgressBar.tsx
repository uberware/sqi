// SPDX-License-Identifier: AGPL-3.0-or-later

import type { TaskCounts } from '@/api/types'
import styles from './TaskProgressBar.module.css'

interface TaskProgressBarProps {
  /** Per-status task counts for the job. */
  counts: TaskCounts
  /** Extra class for the outer wrapper (e.g. to adjust layout in a table cell). */
  className?: string | undefined
}

/**
 * Task-completion meter: a `done/total` fraction above an accent-filled
 * progress bar. "Done" counts terminal tasks (succeeded + failed + canceled).
 */
export default function TaskProgressBar({ counts, className }: TaskProgressBarProps) {
  const done = counts.succeeded + counts.failed + counts.canceled
  const pct = counts.total > 0 ? Math.round((done / counts.total) * 100) : 0
  return (
    <div className={[styles.progress, className].filter(Boolean).join(' ')}>
      <div className={styles.progressFrac}>
        {done}/{counts.total}
      </div>
      <div
        className={styles.progressBar}
        role="progressbar"
        aria-label="Task progress"
        aria-valuenow={pct}
        aria-valuemin={0}
        aria-valuemax={100}
      >
        <div className={styles.progressFill} style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}
