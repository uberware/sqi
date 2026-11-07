// SPDX-License-Identifier: AGPL-3.0-or-later

import { clamp } from '@/lib/math'
import { formatDuration, timeLeftMs } from '@/lib/time'
import styles from './TaskRunProgress.module.css'

interface TaskRunProgressProps {
  /** Percent complete (0-100) reported by the running task. */
  progress?: number | undefined
  /** Free-text status reported by the running task. */
  statusText?: string | undefined
  /** ISO start of the current attempt, for the time-left estimate. */
  startedAt?: string | undefined
  /** Current epoch ms (a ticking clock). */
  now: number
}

/** Slim progress bar, percent, status text and time-left estimate for one running task. */
export default function TaskRunProgress({
  progress,
  statusText,
  startedAt,
  now,
}: TaskRunProgressProps) {
  const hasText = statusText !== undefined && statusText !== ''
  if (progress === undefined && !hasText) return null

  const pct = progress === undefined ? undefined : clamp(progress, 0, 100)
  const left = timeLeftMs(startedAt, now, progress)
  const leftText =
    left === undefined ? undefined : `≈ ${formatDuration(Math.round(left / 1000) * 1000)} left`

  return (
    <div className={styles.root}>
      <div
        className={`${styles.bar} ${pct === undefined ? styles.indeterminate : ''}`}
        role="progressbar"
        aria-label="Task progress"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct}
      >
        {pct !== undefined && <div className={styles.fill} style={{ width: `${pct}%` }} />}
      </div>
      <div className={styles.meta}>
        {pct !== undefined && <span className={styles.pct}>{pct}%</span>}
        {hasText && (
          <span className={styles.text} title={statusText}>
            {statusText}
          </span>
        )}
        {leftText !== undefined && <span className={styles.left}>{leftText}</span>}
      </div>
    </div>
  )
}
